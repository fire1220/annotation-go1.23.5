// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Central free lists.
//
// See malloc.go for an overview.
//
// The mcentral doesn't actually contain the list of free objects; the mspan does.
// Each mcentral is two lists of mspans: those with free objects (c->nonempty)
// and those that are completely allocated (c->empty).

package runtime

import (
	"internal/runtime/atomic"
	"runtime/internal/sys"
)

// Central list of free objects of a given size.
type mcentral struct {
	_         sys.NotInHeap
	spanclass spanClass

	// partial and full contain two mspan sets: one of swept in-use
	// spans, and one of unswept in-use spans. These two trade
	// roles on each GC cycle. The unswept set is drained either by
	// allocation or by the background sweeper in every GC cycle,
	// so only two roles are necessary.
	//
	// sweepgen is increased by 2 on each GC cycle, so the swept
	// spans are in partial[sweepgen/2%2] and the unswept spans are in
	// partial[1-sweepgen/2%2]. Sweeping pops spans from the
	// unswept set and pushes spans that are still in-use on the
	// swept set. Likewise, allocating an in-use span pushes it
	// on the swept set.
	//
	// Some parts of the sweeper can sweep arbitrary spans, and hence
	// can't remove them from the unswept set, but will add the span
	// to the appropriate swept list. As a result, the parts of the
	// sweeper and mcentral that do consume from the unswept list may
	// encounter swept spans, and these should be ignored.
	partial [2]spanSet // 注释：【有空闲】的span链表(两个元素分别表示GC【已清理】和【未清理】两类数组，通过清理计数器来区分，清理开始的时候计数器自增2) //  list of spans with a free object
	full    [2]spanSet // 注释：【无空闲】的span链表(两个元素分别表示GC【已清理】和【未清理】两类数组，通过清理计数器来区分，清理开始的时候计数器自增2)  // list of spans with no free objects
}

// Initialize a single central free list.
func (c *mcentral) init(spc spanClass) {
	c.spanclass = spc
	lockInit(&c.partial[0].spineLock, lockRankSpanSetSpine)
	lockInit(&c.partial[1].spineLock, lockRankSpanSetSpine)
	lockInit(&c.full[0].spineLock, lockRankSpanSetSpine)
	lockInit(&c.full[1].spineLock, lockRankSpanSetSpine)
}

// partialUnswept returns the spanSet which holds partially-filled
// unswept spans for this sweepgen.
func (c *mcentral) partialUnswept(sweepgen uint32) *spanSet {
	return &c.partial[1-sweepgen/2%2]
}

// partialSwept returns the spanSet which holds partially-filled
// swept spans for this sweepgen.
// 注释：【有可用空间】【已清理】中拿span
func (c *mcentral) partialSwept(sweepgen uint32) *spanSet {
	return &c.partial[sweepgen/2%2]
}

// fullUnswept returns the spanSet which holds unswept spans without any
// free slots for this sweepgen.
func (c *mcentral) fullUnswept(sweepgen uint32) *spanSet {
	return &c.full[1-sweepgen/2%2]
}

// fullSwept returns the spanSet which holds swept spans without any
// free slots for this sweepgen.
func (c *mcentral) fullSwept(sweepgen uint32) *spanSet {
	return &c.full[sweepgen/2%2]
}

// Allocate a span to use in an mcache.
// 注释：从mcentral或mheap中拿出一个跨度类
func (c *mcentral) cacheSpan() *mspan {
	// Deduct credit for this span allocation and sweep if necessary.
	spanBytes := uintptr(class_to_allocnpages[c.spanclass.sizeclass()]) * _PageSize
	deductSweepCredit(spanBytes, 0)

	traceDone := false
	trace := traceAcquire()
	if trace.ok() {
		trace.GCSweepStart()
		traceRelease(trace)
	}

	// If we sweep spanBudget spans without finding any free
	// space, just allocate a fresh span. This limits the amount
	// of time we can spend trying to find free space and
	// amortizes the cost of small object sweeping over the
	// benefit of having a full free span to allocate from. By
	// setting this to 100, we limit the space overhead to 1%.
	//
	// TODO(austin,mknyszek): This still has bad worst-case
	// throughput. For example, this could find just one free slot
	// on the 100th swept span. That limits allocation latency, but
	// still has very poor throughput. We could instead keep a
	// running free-to-used budget and switch to fresh span
	// allocation if the budget runs low.
	spanBudget := 100

	var s *mspan
	var sl sweepLocker

	// Try partial swept spans first.
	sg := mheap_.sweepgen
	if s = c.partialSwept(sg).pop(); s != nil { // 【有可用空间】【已清理】中拿span
		goto havespan
	}

	sl = sweep.active.begin()
	if sl.valid {
		// Now try partial unswept spans.
		for ; spanBudget >= 0; spanBudget-- {
			s = c.partialUnswept(sg).pop() // 【有可用空间】【未清理】中拿span
			if s == nil {
				break
			}
			if s, ok := sl.tryAcquire(s); ok {
				// We got ownership of the span, so let's sweep it and use it.
				s.sweep(true)
				sweep.active.end(sl)
				goto havespan
			}
			// We failed to get ownership of the span, which means it's being or
			// has been swept by an asynchronous sweeper that just couldn't remove it
			// from the unswept list. That sweeper took ownership of the span and
			// responsibility for either freeing it to the heap or putting it on the
			// right swept list. Either way, we should just ignore it (and it's unsafe
			// for us to do anything else).
		}
		// Now try full unswept spans, sweeping them and putting them into the
		// right list if we fail to get a span.
		for ; spanBudget >= 0; spanBudget-- {
			s = c.fullUnswept(sg).pop() // 【无可用空间】【未清理】中拿span
			if s == nil {
				break
			}
			if s, ok := sl.tryAcquire(s); ok {
				// We got ownership of the span, so let's sweep it.
				s.sweep(true)
				// Check if there's any free space.
				freeIndex := s.nextFreeIndex()
				if freeIndex != s.nelems {
					s.freeindex = freeIndex
					sweep.active.end(sl)
					goto havespan
				}
				// Add it to the swept list, because sweeping didn't give us any free space.
				c.fullSwept(sg).push(s.mspan) // 【无可用空间】【已清理】中放入span
			}
			// See comment for partial unswept spans.
		}
		sweep.active.end(sl)
	}
	trace = traceAcquire()
	if trace.ok() {
		trace.GCSweepDone()
		traceDone = true
		traceRelease(trace)
	}

	// We failed to get a span from the mcentral so get one from mheap.
	s = c.grow() // 从mheap中拿出一个跨度类
	if s == nil {
		return nil
	}

	// At this point s is a span that should have free slots.
havespan:
	if !traceDone {
		trace := traceAcquire()
		if trace.ok() {
			trace.GCSweepDone()
			traceRelease(trace)
		}
	}
	n := int(s.nelems) - int(s.allocCount)
	if n == 0 || s.freeindex == s.nelems || s.allocCount == s.nelems {
		throw("span has no free objects")
	}
	freeByteBase := s.freeindex &^ (64 - 1)
	whichByte := freeByteBase / 8
	// Init alloc bits cache.
	s.refillAllocCache(whichByte)

	// Adjust the allocCache so that s.freeindex corresponds to the low bit in
	// s.allocCache.
	s.allocCache >>= s.freeindex % 64

	return s
}

// Return span from an mcache.
//
// s must have a span class corresponding to this
// mcentral and it must not be empty.
// 注释：把跨度类放到已分配(非缓存)的队列里(这里包括:有空闲链表和无空闲链表)
func (c *mcentral) uncacheSpan(s *mspan) {
	if s.allocCount == 0 {
		throw("uncaching span but s.allocCount == 0")
	}

	sg := mheap_.sweepgen
	stale := s.sweepgen == sg+1 // 是否需要清理

	// Fix up sweepgen.
	if stale { // 是否需要清理
		// Span was cached before sweep began. It's our
		// responsibility to sweep it.
		//
		// Set sweepgen to indicate it's not cached but needs
		// sweeping and can't be allocated from. sweep will
		// set s.sweepgen to indicate s is swept.
		atomic.Store(&s.sweepgen, sg-1) // 把 sweepgen 设置成 h->sweepgen - 1【正在清理、未缓存】span(跨度)正在扫描标记。(理解为重新清理过程中的状态，从新清理是-2,过程中会+1，所以状态为-1)
	} else {
		// Indicate that s is no longer cached.
		atomic.Store(&s.sweepgen, sg) // 把 sweepgen 设置成 h->sweepgen【已经清理、未缓存】span(跨度)清扫标记完成，准备使用。
	}

	// Put the span in the appropriate place.
	if stale {
		// It's stale, so just sweep it. Sweeping will put it on
		// the right list.
		//
		// We don't use a sweepLocker here. Stale cached spans
		// aren't in the global sweep lists, so mark termination
		// itself holds up sweep completion until all mcaches
		// have been swept.
		ss := sweepLocked{s}
		ss.sweep(false) // 开始清理
	} else {
		if int(s.nelems)-int(s.allocCount) > 0 { // 有空闲，总数对象数 > 使用对象数
			// Put it back on the partial swept list.
			c.partialSwept(sg).push(s) // 放到有空闲链表中
		} else {
			// There's no free space and it's not stale, so put it on the
			// full swept list.
			c.fullSwept(sg).push(s) // 放到无空闲链表中
		}
	}
}

// grow allocates a new empty span from the heap and initializes it for c's size class.
func (c *mcentral) grow() *mspan {
	npages := uintptr(class_to_allocnpages[c.spanclass.sizeclass()])
	size := uintptr(class_to_size[c.spanclass.sizeclass()])

	s := mheap_.alloc(npages, c.spanclass)
	if s == nil {
		return nil
	}

	// Use division by multiplication and shifts to quickly compute:
	// n := (npages << _PageShift) / size
	n := s.divideByElemSize(npages << _PageShift)
	s.limit = s.base() + size*n
	s.initHeapBits(false)
	return s
}

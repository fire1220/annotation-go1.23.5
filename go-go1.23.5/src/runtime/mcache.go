// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/runtime/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// Per-thread (in Go, per-P) cache for small objects.
// This includes a small object cache and local allocation stats.
// No locking needed because it is per-thread (per-P).
//
// mcaches are allocated from non-GC'd memory, so any heap pointers
// must be specially handled.
// 注释：线程缓存，线程内共享，所以无需上锁
type mcache struct {
	_ sys.NotInHeap

	// The following members are accessed on every malloc,
	// so they are grouped here for better caching.
	nextSample uintptr // trigger heap sample after allocating this many bytes
	scanAlloc  uintptr // 分配的可扫描字节数// bytes of scannable heap allocated

	// Allocator cache for tiny objects w/o pointers.
	// See "Tiny allocator" comment in malloc.go.

	// tiny points to the beginning of the current tiny block, or
	// nil if there is no current tiny block.
	//
	// tiny is a heap pointer. Since mcache is in non-GC'd memory,
	// we handle it by clearing it in releaseAll during mark
	// termination.
	//
	// tinyAllocs is the number of tiny allocations performed
	// by the P that owns this mcache.
	// 微对象处理，微对象（无指针类型的微对象）
	// 如果微对象没有可用空间会到alloc中拿
	tiny       uintptr // 注释：微对象的基地址
	tinyoffset uintptr // 注释：微对象下一个空闲位置的偏移量
	tinyAllocs uintptr // 注释：微对象分配的对象个数

	// The rest is not accessed on every malloc.

	// 对应的跨度类，每个跨度类有两条（共有双倍的跨度类的数据）
	// 偶数是有指针的跨度类，基数是无指针的跨度类
	// 有指针跨度类：numSpanClasses << 1
	// 无指针跨度类：numSpanClasses << 1 | 1
	// 例如微对象无指针的跨度类是：5 = 2 << 1 | 1，(微对象id是2)
	// mcache中的所有mspan的缓存，
	alloc [numSpanClasses]*mspan // 注释：存储有指针和无指针的跨度类 // spans to allocate from, indexed by spanClass

	stackcache [_NumStackOrders]stackfreelist

	// flushGen indicates the sweepgen during which this mcache
	// was last flushed. If flushGen != mheap_.sweepgen, the spans
	// in this mcache are stale and need to the flushed so they
	// can be swept. This is done in acquirep.
	flushGen atomic.Uint32
}

// A gclink is a node in a linked list of blocks, like mlink,
// but it is opaque to the garbage collector.
// The GC does not trace the pointers during collection,
// and the compiler does not emit write barriers for assignments
// of gclinkptr values. Code should store references to gclinks
// as gclinkptr, not as *gclink.
type gclink struct {
	next gclinkptr
}

// A gclinkptr is a pointer to a gclink, but it is opaque
// to the garbage collector.
type gclinkptr uintptr

// ptr returns the *gclink form of p.
// The result should be used for accessing fields, not stored
// in other data structures.
func (p gclinkptr) ptr() *gclink {
	return (*gclink)(unsafe.Pointer(p))
}

type stackfreelist struct {
	list gclinkptr // linked list of free stacks
	size uintptr   // total size of stacks in list
}

// dummy mspan that contains no free objects.
var emptymspan mspan

func allocmcache() *mcache {
	var c *mcache
	systemstack(func() {
		lock(&mheap_.lock)
		c = (*mcache)(mheap_.cachealloc.alloc())
		c.flushGen.Store(mheap_.sweepgen)
		unlock(&mheap_.lock)
	})
	for i := range c.alloc {
		c.alloc[i] = &emptymspan
	}
	c.nextSample = nextSample()
	return c
}

// freemcache releases resources associated with this
// mcache and puts the object onto a free list.
//
// In some cases there is no way to simply release
// resources, such as statistics, so donate them to
// a different mcache (the recipient).
func freemcache(c *mcache) {
	systemstack(func() {
		c.releaseAll()
		stackcache_clear(c)

		// NOTE(rsc,rlh): If gcworkbuffree comes back, we need to coordinate
		// with the stealing of gcworkbufs during garbage collection to avoid
		// a race where the workbuf is double-freed.
		// gcworkbuffree(c.gcworkbuf)

		lock(&mheap_.lock)
		mheap_.cachealloc.free(unsafe.Pointer(c))
		unlock(&mheap_.lock)
	})
}

// getMCache is a convenience function which tries to obtain an mcache.
//
// Returns nil if we're not bootstrapping or we don't have a P. The caller's
// P must not change, so we must be in a non-preemptible state.
// 注释：获取线程缓存mcache
func getMCache(mp *m) *mcache {
	// Grab the mcache, since that's where stats live.
	pp := mp.p.ptr()
	var c *mcache
	if pp == nil {
		// We will be called without a P while bootstrapping, 		// 启动时,我们将会被调用,在这期间没有一个P
		// in which case we use mcache0, which is set in mallocinit.// 在这种情况下,我们使用mcache0,也就是说在mallocinit初始化时设置
		// mcache0 is cleared when bootstrapping is complete,		// 当启动完成后mcache0将被清除
		// by procresize.
		c = mcache0
	} else {
		c = pp.mcache
	}
	return c
}

// refill acquires a new span of span class spc for c. This span will
// have at least one free object. The current span in c must be full.
//
// Must run in a non-preemptible context since otherwise the owner of
// c could change.
// 注释：重新装填，把mcentral(或mheap)装填到mcache中
func (c *mcache) refill(spc spanClass) {
	// Return the current cached span to the central lists.
	s := c.alloc[spc] // 获取对应的跨度类

	if s.allocCount != s.nelems {
		throw("refill of span with free space remaining")
	}
	if s != &emptymspan { // 如果跨度类不是首次分配（初始化跨度类的时候会赋值&emptymspan）
		// Mark this span as no longer cached.
		if s.sweepgen != mheap_.sweepgen+3 {
			throw("bad sweepgen in refill")
		}
		mheap_.central[spc].mcentral.uncacheSpan(s) // 注释：把跨度类放到已分配(非缓存)的队列里(这里包括:有空闲链表和无空闲链表)

		// Count up how many slots were used and record it.
		stats := memstats.heapStats.acquire()                             // 获取内存统计
		slotsUsed := int64(s.allocCount) - int64(s.allocCountBeforeCache) // 当前跨度类中已使用的对象数(所有分配对象数-申请前已分配对象数)
		atomic.Xadd64(&stats.smallAllocCount[spc.sizeclass()], slotsUsed) // 存储统计小对象分配的对象个数

		// Flush tinyAllocs.
		if spc == tinySpanClass { // 如果是微对象
			atomic.Xadd64(&stats.tinyAllocCount, int64(c.tinyAllocs)) // 存储统计微对象分配的对象个数
			c.tinyAllocs = 0                                          // 清空当前跨度类的微对线分配数量
		}
		memstats.heapStats.release() // 释放内存统计

		// Count the allocs in inconsistent, internal stats.
		bytesAllocated := slotsUsed * int64(s.elemsize)
		gcController.totalAlloc.Add(bytesAllocated) // 更新内存统计(更新分配的总内存大小)

		// Clear the second allocCount just to be safe.
		s.allocCountBeforeCache = 0 // 清空当前跨度类的旧的已使用的对象数
	}

	// Get a new cached span from the central lists.
	s = mheap_.central[spc].mcentral.cacheSpan() // 从mheap中拿出一个跨度类
	if s == nil {
		throw("out of memory")
	}

	if s.allocCount == s.nelems {
		throw("span has no free space")
	}

	// Indicate that this span is cached and prevent asynchronous
	// sweeping in the next sweep phase.
	s.sweepgen = mheap_.sweepgen + 3 // 设置成已清理状态

	// Store the current alloc count for accounting later.
	s.allocCountBeforeCache = s.allocCount // 备份跨度类中已使用的对象数

	// Update heapLive and flush scanAlloc.
	//
	// We have not yet allocated anything new into the span, but we
	// assume that all of its slots will get used, so this makes
	// heapLive an overestimate.
	//
	// When the span gets uncached, we'll fix up this overestimate
	// if necessary (see releaseAll).
	//
	// We pick an overestimate here because an underestimate leads
	// the pacer to believe that it's in better shape than it is,
	// which appears to lead to more memory used. See #53738 for
	// more details.
	usedBytes := uintptr(s.allocCount) * s.elemsize // 已使用的偏移量
	// int64(s.npages*pageSize)-int64(usedBytes) 页块数*页大小8KB-已使用的偏移量=剩余空闲内存
	gcController.update(int64(s.npages*pageSize)-int64(usedBytes), int64(c.scanAlloc))
	c.scanAlloc = 0

	c.alloc[spc] = s
}

// allocLarge allocates a span for a large object.
// 注释：大对象分配
// 计算分配所需的页块数，并调用makeSpanClass()创建一个跨度类id是0的跨度类,调用mheap.alloc()分配内存空间，并纳入mcentral中管理
func (c *mcache) allocLarge(size uintptr, noscan bool) *mspan {
	if size+_PageSize < size {
		throw("out of memory")
	}
	npages := size >> _PageShift // 计算所需的页数
	if size&_PageMask != 0 {     // 如果size不为8KB的倍数，则页块数+1
		npages++
	}

	// Deduct credit for this span allocation and sweep if
	// necessary. mHeap_Alloc will also sweep npages, so this only
	// pays the debt down to npage pages.
	// 译：
	// 为此跨度分配和清理扣除信用（如果有必要）。mHeap_Alloc也会对npages进行清理，因此这里仅将债务减少到npage页。
	deductSweepCredit(npages*_PageSize, npages)

	spc := makeSpanClass(0, noscan) // 创建一个跨度类id是0的跨度类(id=0表示大对象)
	s := mheap_.alloc(npages, spc)  // 分配内存空间(通过所需的页数和跨度类id申请内存空间)
	if s == nil {
		throw("out of memory")
	}

	// Count the alloc in consistent, external stats.
	stats := memstats.heapStats.acquire()
	atomic.Xadd64(&stats.largeAlloc, int64(npages*pageSize))
	atomic.Xadd64(&stats.largeAllocCount, 1)
	memstats.heapStats.release()

	// Count the alloc in inconsistent, internal stats.
	gcController.totalAlloc.Add(int64(npages * pageSize))

	// Update heapLive.
	gcController.update(int64(s.npages*pageSize), 0)

	// Put the large span in the mcentral swept list so that it's
	// visible to the background sweeper.
	mheap_.central[spc].mcentral.fullSwept(mheap_.sweepgen).push(s)
	s.limit = s.base() + size
	s.initHeapBits(false)
	return s
}

func (c *mcache) releaseAll() {
	// Take this opportunity to flush scanAlloc.
	scanAlloc := int64(c.scanAlloc)
	c.scanAlloc = 0

	sg := mheap_.sweepgen
	dHeapLive := int64(0)
	for i := range c.alloc {
		s := c.alloc[i]
		if s != &emptymspan {
			slotsUsed := int64(s.allocCount) - int64(s.allocCountBeforeCache)
			s.allocCountBeforeCache = 0

			// Adjust smallAllocCount for whatever was allocated.
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.smallAllocCount[spanClass(i).sizeclass()], slotsUsed)
			memstats.heapStats.release()

			// Adjust the actual allocs in inconsistent, internal stats.
			// We assumed earlier that the full span gets allocated.
			gcController.totalAlloc.Add(slotsUsed * int64(s.elemsize))

			if s.sweepgen != sg+1 {
				// refill conservatively counted unallocated slots in gcController.heapLive.
				// Undo this.
				//
				// If this span was cached before sweep, then gcController.heapLive was totally
				// recomputed since caching this span, so we don't do this for stale spans.
				dHeapLive -= int64(s.nelems-s.allocCount) * int64(s.elemsize)
			}

			// Release the span to the mcentral.
			mheap_.central[i].mcentral.uncacheSpan(s)
			c.alloc[i] = &emptymspan
		}
	}
	// Clear tinyalloc pool.
	c.tiny = 0
	c.tinyoffset = 0

	// Flush tinyAllocs.
	stats := memstats.heapStats.acquire()
	atomic.Xadd64(&stats.tinyAllocCount, int64(c.tinyAllocs))
	c.tinyAllocs = 0
	memstats.heapStats.release()

	// Update heapLive and heapScan.
	gcController.update(dHeapLive, scanAlloc)
}

// prepareForSweep flushes c if the system has entered a new sweep phase
// since c was populated. This must happen between the sweep phase
// starting and the first allocation from c.
func (c *mcache) prepareForSweep() {
	// Alternatively, instead of making sure we do this on every P
	// between starting the world and allocating on that P, we
	// could leave allocate-black on, allow allocation to continue
	// as usual, use a ragged barrier at the beginning of sweep to
	// ensure all cached spans are swept, and then disable
	// allocate-black. However, with this approach it's difficult
	// to avoid spilling mark bits into the *next* GC cycle.
	sg := mheap_.sweepgen
	flushGen := c.flushGen.Load()
	if flushGen == sg {
		return
	} else if flushGen != sg-2 {
		println("bad flushGen", flushGen, "in prepareForSweep; sweepgen", sg)
		throw("bad flushGen")
	}
	c.releaseAll()
	stackcache_clear(c)
	c.flushGen.Store(mheap_.sweepgen) // Synchronizes with gcStart
}

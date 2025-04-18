// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

const pageCachePages = 8 * unsafe.Sizeof(pageCache{}.cache) // 每个分配器缓存的页面大小

// pageCache represents a per-p cache of pages the allocator can
// allocate from without a lock. More specifically, it represents
// a pageCachePages*pageSize chunk of memory with 0 or more free
// pages in it.
// 每个P处理器缓存的页面
type pageCache struct {
	base  uintptr // 内存块的基地址 // base address of the chunk
	cache uint64  // 64位的位图，表示空闲页面（1表示空闲） // 64-bit bitmap representing free pages (1 means free)
	scav  uint64  // scavenged缩写，64位的位图，表示已回收(已清理)的页面（1表示已回收） // 64-bit bitmap representing scavenged pages (1 means scavenged)
}

// empty reports whether the page cache has no free pages.
// 缓存是否为空(无空闲页面)
func (c *pageCache) empty() bool {
	return c.cache == 0 // 缓存是否为空(无空闲页面)
}

// alloc allocates npages from the page cache and is the main entry
// point for allocation.
//
// Returns a base address and the amount of scavenged memory in the
// allocated region in bytes.
//
// Returns a base address of zero on failure, in which case the
// amount of scavenged memory should be ignored.
//
//	注释：在p的缓存中快速分配内存
//		返回值为base地址和已回收内存的大小
func (c *pageCache) alloc(npages uintptr) (uintptr, uintptr) {
	if c.cache == 0 {
		return 0, 0
	}
	if npages == 1 { // 要分配的页数，如果是1个时
		i := uintptr(sys.TrailingZeros64(c.cache)) // 右尾零个数(0表示已分配,1表示空闲)
		scav := (c.scav >> i) & 1                  // 获取i位置具体值
		c.cache &^= 1 << i                         // 重置空闲页,把已分配的清零 // set bit to mark in-use
		c.scav &^= 1 << i                          // 重置清理位，把已分配设置为未回收 // clear bit to mark unscavenged
		return c.base + i*pageSize, uintptr(scav) * pageSize
	}
	return c.allocN(npages) // 如果要分配多个页的内存时执行
}

// allocN is a helper which attempts to allocate npages worth of pages
// from the cache. It represents the general case for allocating from
// the page cache.
//
// Returns a base address and the amount of scavenged memory in the
// allocated region in bytes.
//
//	注释：在p的缓存中快速分配多个页的内存
//		返回值为base地址和已回收(空闲)内存的大小
func (c *pageCache) allocN(npages uintptr) (uintptr, uintptr) {
	i := findBitRange64(c.cache, uint(npages)) // 查找第一个连续npages个1的起始位置,没找到返回64
	if i >= 64 {                               // 表示没有连续的npages个1
		return 0, 0
	}
	mask := ((uint64(1) << npages) - 1) << i
	scav := sys.OnesCount64(c.scav & mask) // 1的个数(1表示已回收（空闲）)
	c.cache &^= mask                       // 清除已使用的缓存 // mark in-use bits
	c.scav &^= mask                        // 把已使用的内存标记成未清理 // clear scavenged bits
	return c.base + uintptr(i*pageSize), uintptr(scav) * pageSize
}

// flush empties out unallocated free pages in the given cache
// into s. Then, it clears the cache, such that empty returns
// true.
//
// p.mheapLock must be held.
//
// Must run on the system stack because p.mheapLock must be held.
//
//go:systemstack
func (c *pageCache) flush(p *pageAlloc) {
	assertLockHeld(p.mheapLock)

	if c.empty() {
		return
	}
	ci := chunkIndex(c.base)
	pi := chunkPageIndex(c.base)

	// This method is called very infrequently, so just do the
	// slower, safer thing by iterating over each bit individually.
	for i := uint(0); i < 64; i++ {
		if c.cache&(1<<i) != 0 {
			p.chunkOf(ci).free1(pi + i)

			// Update density statistics.
			p.scav.index.free(ci, pi+i, 1)
		}
		if c.scav&(1<<i) != 0 {
			p.chunkOf(ci).scavenged.setRange(pi+i, 1)
		}
	}

	// Since this is a lot like a free, we need to make sure
	// we update the searchAddr just like free does.
	if b := (offAddr{c.base}); b.lessThan(p.searchAddr) {
		p.searchAddr = b
	}
	p.update(c.base, pageCachePages, false, false)
	*c = pageCache{}
}

// allocToCache acquires a pageCachePages-aligned chunk of free pages which
// may not be contiguous, and returns a pageCache structure which owns the
// chunk.
//
// p.mheapLock must be held.
//
// Must run on the system stack because p.mheapLock must be held.
//
// 注释：重新填充页缓存到P处理器里
//
//go:systemstack
func (p *pageAlloc) allocToCache() pageCache {
	assertLockHeld(p.mheapLock)

	// If the searchAddr refers to a region which has a higher address than
	// any known chunk, then we know we're out of memory.
	if chunkIndex(p.searchAddr.addr()) >= p.end { // 判断页里块的索引是否大于等于p的缓存块结束下标，如果true表示缓存中没有内存了
		return pageCache{}
	}
	c := pageCache{}
	ci := chunkIndex(p.searchAddr.addr()) // 页里块的索引 // chunk index
	var chunk *pallocData
	if p.summary[len(p.summary)-1][ci] != 0 {
		// Fast path: there's free pages at or near the searchAddr address.
		chunk = p.chunkOf(ci)
		j, _ := chunk.find(1, chunkPageIndex(p.searchAddr.addr()))
		if j == ^uint(0) {
			throw("bad summary data")
		}
		c = pageCache{
			base:  chunkBase(ci) + alignDown(uintptr(j), 64)*pageSize,
			cache: ^chunk.pages64(j),
			scav:  chunk.scavenged.block64(j),
		}
	} else {
		// Slow path: the searchAddr address had nothing there, so go find
		// the first free page the slow way.
		addr, _ := p.find(1)
		if addr == 0 {
			// We failed to find adequate free space, so mark the searchAddr as OoM
			// and return an empty pageCache.
			p.searchAddr = maxSearchAddr()
			return pageCache{}
		}
		ci = chunkIndex(addr)
		chunk = p.chunkOf(ci)
		c = pageCache{
			base:  alignDown(addr, 64*pageSize),
			cache: ^chunk.pages64(chunkPageIndex(addr)),
			scav:  chunk.scavenged.block64(chunkPageIndex(addr)),
		}
	}

	// Set the page bits as allocated and clear the scavenged bits, but
	// be careful to only set and clear the relevant bits.
	cpi := chunkPageIndex(c.base)
	chunk.allocPages64(cpi, c.cache)
	chunk.scavenged.clearBlock64(cpi, c.cache&c.scav /* free and scavenged */)

	// Update as an allocation, but note that it's not contiguous.
	p.update(c.base, pageCachePages, false, true)

	// Update density statistics.
	p.scav.index.alloc(ci, uint(sys.OnesCount64(c.cache)))

	// Set the search address to the last page represented by the cache.
	// Since all of the pages in this block are going to the cache, and we
	// searched for the first free page, we can confidently start at the
	// next page.
	//
	// However, p.searchAddr is not allowed to point into unmapped heap memory
	// unless it is maxSearchAddr, so make it the last page as opposed to
	// the page after.
	p.searchAddr = offAddr{c.base + pageSize*(pageCachePages-1)}
	return c
}

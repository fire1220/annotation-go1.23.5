// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin || (openbsd && !mips64)

package runtime

import "unsafe"

// Call fn with arg as its argument. Return what fn returns.
// fn is the raw pc value of the entry point of the desired function.
// Switches to the system stack, if not already there.
// Preserves the calling point as the location where a profiler traceback will begin.
//
// 译：
// 使用 arg 作为参数调用函数 fn，并返回 fn 的返回值。
// fn 是目标函数入口点的原始 pc 值。
// 如果当前不在系统栈上，则切换到系统栈。
// 保留调用点的位置，以便作为性能分析回溯的起点。
// 禁止分裂（避免栈扩展）
//
//go:nosplit
func libcCall(fn, arg unsafe.Pointer) int32 {
	// Leave caller's PC/SP/G around for traceback.
	// 译：保留调用者的 PC/SP/G，以便进行回溯。
	gp := getg() // 获取当前 Goroutine
	var mp *m    // 定义线程指针
	if gp != nil {
		mp = gp.m // 获取当前线程
	}
	if mp != nil && mp.libcallsp == 0 {
		mp.libcallg.set(gp)          // 设置当前 Goroutine
		mp.libcallpc = getcallerpc() // 获取调用者的程序计数器
		// sp must be the last, because once async cpu profiler finds
		// all three values to be non-zero, it will use them
		// 译：
		//	sp 必须放在最后，因为一旦异步 CPU 分析器发现
		//	这三个值均非零时，它将使用这些值。
		mp.libcallsp = getcallersp() // 获取调用者的栈指针
	} else {
		// Make sure we don't reset libcallsp. This makes
		// libcCall reentrant; We remember the g/pc/sp for the
		// first call on an M, until that libcCall instance
		// returns.  Reentrance only matters for signals, as
		// libc never calls back into Go.  The tricky case is
		// where we call libcX from an M and record g/pc/sp.
		// Before that call returns, a signal arrives on the
		// same M and the signal handling code calls another
		// libc function.  We don't want that second libcCall
		// from within the handler to be recorded, and we
		// don't want that call's completion to zero
		// libcallsp.
		// We don't need to set libcall* while we're in a sighandler
		// (even if we're not currently in libc) because we block all
		// signals while we're handling a signal. That includes the
		// profile signal, which is the one that uses the libcall* info.
		// 译：
		// 确保我们不会重置 libcallsp。这使得 libcCall 可重入；
		// 我们记住第一次调用的 g/pc/sp，直到该调用完成。
		// 只有信号处理时才需要可重入性，因为 libc 不会回调 Go。
		// 复杂情况：当我们在一个 M 上调用 libcX 并记录 g/pc/sp，
		// 在该调用返回之前，信号到达同一个 M，信号处理代码调用另一个 libc 函数。
		// 我们不希望第二个 libcCall 被记录，也不希望它的完成清零 libcallsp。
		// 在信号处理期间（即使不在 libc 中），我们不需要设置 libcall*，
		// 因为我们会在处理信号时阻塞所有信号，包括使用 libcall* 信息的性能分析信号。
		mp = nil
	}
	res := asmcgocall(fn, arg) // 调用目标函数
	if mp != nil {
		mp.libcallsp = 0 // 清理 libcallsp
	}
	return res
}

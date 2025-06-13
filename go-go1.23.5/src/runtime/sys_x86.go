// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || 386

package runtime

import (
	"internal/goarch"
	"unsafe"
)

// adjust Gobuf as if it executed a call to fn with context ctxt
// and then stopped before the first instruction in fn.
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
	sp := buf.sp
	sp -= goarch.PtrSize
	// 注释：这里做的太巧妙了，SP伪寄存器就是硬寄存器的BP基地址，是高地址，然后每个平台的最小内存单位不同，这里在调用前buf.ps是goexit的PC加一个单位的内存地址
	// 注释：
	//                    ********************
	//      caller --->   *       bp         *  									  <--- (基地址)当前函数的伪SP地址
	//                    ********************
	//                    *   return addr    *  <--fn执行完后回到这里继续执行---<---|	  <--- 下一个函数的返回位置(通常由LR寄存器存储),这里把return addr设置程goexit函数句柄，所以当fn执行完后执行goexit函数
	//                    ********************			  	 					|
	//      callee --->   *      fn()        *  ->--执行完成后-->------>----------^	  <--- 下一个函数的PC
	//                    ********************
	// 为了保证栈针的连续性，fn执行会缩栈，然后回到return addr处继续执行下一个执行。
	*(*uintptr)(unsafe.Pointer(sp)) = buf.pc
	buf.sp = sp
	buf.pc = uintptr(fn)
	buf.ctxt = ctxt
}

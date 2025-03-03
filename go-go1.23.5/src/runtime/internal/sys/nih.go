// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sys

// NOTE: keep in sync with cmd/compile/internal/types.CalcSize
// to make the compiler recognize this as an intrinsic type.
type nih struct{}

// NotInHeap is a type must never be allocated from the GC'd heap or on the stack,
// and is called not-in-heap.
//
// Other types can embed NotInHeap to make it not-in-heap. Specifically, pointers
// to these types must always fail the `runtime.inheap` check. The type may be used
// for global variables, or for objects in unmanaged memory (e.g., allocated with
// `sysAlloc`, `persistentalloc`, r`fixalloc`, or from a manually-managed span).
//
// Specifically:
//
// 1. `new(T)`, `make([]T)`, `append([]T, ...)` and implicit heap
// allocation of T are disallowed. (Though implicit allocations are
// disallowed in the runtime anyway.)
//
// 2. A pointer to a regular type (other than `unsafe.Pointer`) cannot be
// converted to a pointer to a not-in-heap type, even if they have the
// same underlying type.
//
// 3. Any type that containing a not-in-heap type is itself considered as not-in-heap.
//
// - Structs and arrays are not-in-heap if their elements are not-in-heap.
// - Maps and channels contains no-in-heap types are disallowed.
//
// 4. Write barriers on pointers to not-in-heap types can be omitted.
//
// The last point is the real benefit of NotInHeap. The runtime uses
// it for low-level internal structures to avoid memory barriers in the
// scheduler and the memory allocator where they are illegal or simply
// inefficient. This mechanism is reasonably safe and does not compromise
// the readability of the runtime.
//
// 译：NotInHeap 是一种必须永远不在 GC 管理的堆或栈上分配的类型，称为 not-in-heap。
// 其他类型可以通过嵌入 NotInHeap 来使其成为 not-in-heap 类型。具体来说，指向这些类型的指针必须始终无法通过 `runtime.inheap` 检查。
// 该类型可以用于全局变量，或者用于未管理的内存中的对象（例如，使用 `sysAlloc`、`persistentalloc`、`fixalloc` 或来自手动管理的 span 分配的对象）。
// 具体规则如下：
// 1. 禁止使用 `new(T)`、`make([]T)`、`append([]T, ...)` 和隐式堆分配 T。（尽管隐式分配在 runtime 中无论如何都是不允许的。）
// 2. 普通类型的指针（除了 `unsafe.Pointer`）不能转换为 not-in-heap 类型的指针，即使它们具有相同的底层类型。
// 3. 包含 not-in-heap 类型的任何类型本身也被视为 not-in-heap。
// - 结构体和数组如果其元素是 not-in-heap，则它们也是 not-in-heap。
// - 包含 not-in-heap 类型的map和chan是不允许的。
// 4. 对 not-in-heap 类型的指针可以省略写屏障。
// 最后一点是 NotInHeap 的真正好处。运行时使用它来避免在调度器和内存分配器中设置内存屏障，因为在那里设置屏障是非法的或低效的。
// 这种机制相对安全，并且不会影响 runtime 的可读性。
//
//	注释：
//	NotInHeap 类型用于标记不能在 GC 管理的堆或栈上分配的对象。
//	该类型可以嵌入其他类型中，使这些类型也不在堆中分配。NotInHeap 的主要作用是避免写屏障，提高调度器和内存分配器的效率。
//	具体规则如下：
//	1.禁止使用 new、make、append 等操作分配 NotInHeap 类型。
//	2.普通指针不能转换为 NotInHeap 类型的指针。
//	3.包含 NotInHeap 类型的复合类型（如结构体、数组）也是 NotInHeap 类型。
//	4.对 NotInHeap 类型的指针可以省略写屏障。
type NotInHeap struct{ _ nih } // 不在 GC 管理的堆或栈上分配的类型，称为 not-in-heap。

// Package escape 用可对比的最小样例演示 Go 的逃逸分析：
// 同一份逻辑，写法不同，变量落在栈上还是堆上就不同，进而决定了 GC 压力。
//
// 观察编译器决策：
//
//	go build -gcflags='-m -l' ./topics/01-escape-analysis
//
// -m 打印优化决策，-l 关闭内联（内联会改变逃逸结论，见第 2 组）。
package escape

import (
	"fmt"
	"strconv"
)

// Point 是一个 16 字节的小结构体，用来观察它被分配在栈上还是堆上。
type Point struct {
	X, Y int
}

// ---------------------------------------------------------------------------
// 第 1 组：返回指针 vs 返回值
//
// 逃逸分析是「自底向上」的：编译器先分析被调函数，得出参数/返回值是否泄漏，
// 再把结论用到调用方。一个函数返回指向局部变量的指针时，该局部变量的生命周期
// 必然超出当前栈帧，只能分配到堆上。
// ---------------------------------------------------------------------------

// NewPointByPointer 返回局部变量的地址，&Point{} 逃逸到堆。
//
//go:noinline
func NewPointByPointer() *Point {
	p := &Point{X: 1, Y: 2}
	return p
}

// NewPointByValue 返回值拷贝，16 字节直接走寄存器/栈，零分配。
//
//go:noinline
func NewPointByValue() Point {
	return Point{X: 1, Y: 2}
}

// ---------------------------------------------------------------------------
// 第 2 组：内联如何消除逃逸
//
// 上面用 //go:noinline 强制隔离了两个栈帧。去掉它以后，编译器先把函数体内联到
// 调用方，"返回局部指针" 这件事就不存在了 —— 分析范围变成调用方那一个栈帧，
// 只要指针没有再往外泄漏，对象就能留在栈上。
// 这解释了一个常见困惑：同样是 &T{}，有时零分配有时不是。
// ---------------------------------------------------------------------------

// newPointInlinable 函数体足够小，满足内联预算，会被内联到调用方。
func newPointInlinable() *Point {
	return &Point{X: 1, Y: 2}
}

// SumInlinedPointer 内联后 &Point{} 不逃逸，整个函数零分配。
func SumInlinedPointer() int {
	p := newPointInlinable()
	return p.X + p.Y
}

// escapedPoint 是包级变量，任何被它引用的对象都必然逃逸。
var escapedPoint *Point

// SumLeakedPointer 把指针存到包级变量，即使内联也无法留在栈上。
func SumLeakedPointer() int {
	p := newPointInlinable()
	escapedPoint = p
	return p.X + p.Y
}

// ---------------------------------------------------------------------------
// 第 3 组：interface{} 装箱
//
// 把具体类型赋给 interface{} 需要「装箱」：接口值只存指针，所以被装箱的数据
// 通常要有一个堆上的副本。fmt 系列函数的可变参数 ...any 会把参数泄漏给
// 反射逻辑，编译器只能判定为逃逸。
// ---------------------------------------------------------------------------

// FormatWithSprintf 走 fmt + 反射，参数装箱逃逸，结果字符串也是新分配。
//
//go:noinline
func FormatWithSprintf(n int) string {
	return fmt.Sprintf("%d", n)
}

// FormatWithItoa 专用转换函数，无装箱，只分配结果字符串本身。
//
//go:noinline
func FormatWithItoa(n int) string {
	return strconv.Itoa(n)
}

// FormatWithAppend 追加进调用方复用的缓冲区，稳态下零分配。
//
//go:noinline
func FormatWithAppend(buf []byte, n int) []byte {
	return strconv.AppendInt(buf, int64(n), 10)
}

// ---------------------------------------------------------------------------
// 第 4 组：make 的长度是不是编译期常量
//
// make 只有在「长度是常量」且「不逃逸」时才可能落到栈上；长度是运行期变量时，
// 编译器无法为它预留固定大小的栈槽，只能堆分配。
// ---------------------------------------------------------------------------

// SumConstLenBuf 长度是常量，缓冲区留在栈上。
//
//go:noinline
func SumConstLenBuf() int {
	buf := make([]byte, 64)
	for i := range buf {
		buf[i] = byte(i)
	}
	total := 0
	for _, v := range buf {
		total += int(v)
	}
	return total
}

// SumVarLenBuf 长度来自参数，即使实参恒为 64 也会堆分配。
//
//go:noinline
func SumVarLenBuf(n int) int {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte(i)
	}
	total := 0
	for _, v := range buf {
		total += int(v)
	}
	return total
}

// ---------------------------------------------------------------------------
// 第 5 组：隐式栈变量的大小上限
//
// 即使长度是常量、也不逃逸，超过 maxImplicitStackVarSize（当前实现为 64KB）
// 的隐式变量仍会被移到堆上，避免栈帧爆炸。
// 见 cmd/compile/internal/ir/cfg.go 中的 MaxImplicitStackVarSize。
// ---------------------------------------------------------------------------

const (
	bufAtLimit     = 64 * 1024 // 恰好 64KB
	bufOverInLimit = 64*1024 + 1
)

// SumBufAtLimit 常量长度 64KB，仍在栈上。
//
//go:noinline
func SumBufAtLimit() int {
	buf := make([]byte, bufAtLimit)
	return touch(buf)
}

// SumBufOverLimit 常量长度 64KB+1，超过隐式栈变量上限，改为堆分配。
//
//go:noinline
func SumBufOverLimit() int {
	buf := make([]byte, bufOverInLimit)
	return touch(buf)
}

// touch 按页步长访问缓冲区，让「分配了 64KB」这件事真实发生。
//
// 这里的 //go:noinline 不是可有可无的装饰。去掉它，编译器会内联 touch，
// 发现每个 buf[i] 都是「刚写完立刻读」，于是做 store-to-load forwarding，
// 把整个 64KB 后备数组消除掉 —— SumBufAtLimit 会编译成 LEAF|NOFRAME、
// 栈帧为 0 的纯算术循环，基准跑出 ~5ns/op，而它其实什么都没分配。
// 详见 README「基准测试陷阱」一节。
//
//go:noinline
func touch(buf []byte) int {
	total := 0
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = byte(i)
		total += int(buf[i])
	}
	return total
}

// ---------------------------------------------------------------------------
// 第 6 组：闭包捕获
//
// 闭包捕获的变量，如果闭包本身不逃逸，捕获变量可以留在栈上；一旦闭包被返回
// 或存到外部，被捕获的变量就要搬到堆上，并且闭包本身也要在堆上组一个
// funcval（函数指针 + 捕获变量）。
// ---------------------------------------------------------------------------

// SumWithLocalClosure 闭包只在本函数内调用，捕获的 total 留在栈上。
//
//go:noinline
func SumWithLocalClosure(n int) int {
	total := 0
	add := func(v int) { total += v }
	for i := range n {
		add(i)
	}
	return total
}

// MakeAccumulator 返回闭包，被捕获的 total 逃逸到堆。
//
//go:noinline
func MakeAccumulator() func(int) int {
	total := 0
	return func(v int) int {
		total += v
		return total
	}
}

// MakeAccumulator2 捕获两个变量，用来验证 funcval 的内存构成。
// 和 MakeAccumulator 对照，可以推出 funcval 里每多捕获一个变量涨多少字节。
//
//go:noinline
func MakeAccumulator2() func(int) (int, int) {
	a, b := 0, 0
	return func(v int) (int, int) {
		a += v
		b -= v
		return a, b
	}
}

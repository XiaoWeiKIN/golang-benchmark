package escape

import "testing"

// 本文件统一使用 Go 1.24 引入的 b.Loop()。相比经典的 `for i := 0; i < b.N; i++`，
// 它有两个关键好处：
//  1. 循环体内被调用函数的参数和返回值会被自动保活，编译器不会把整个循环消除掉，
//     所以不再需要写 `var sink T` 这类样板来"吃掉"结果；
//  2. 循环外的准备代码天然不计入计时，不用手写 b.ResetTimer()。
//
// 注意 b.Loop() 的保活只覆盖「函数调用」的参数与返回值。如果被测代码是内联展开的
// 纯计算（比如第 2 组的 SumInlinedPointer），仍然可能被优化掉一部分 —— 这正是
// 第 2 组要观察的现象本身，见 README。

// --- 第 1 组：返回指针 vs 返回值 ---------------------------------------------

func BenchmarkPointer_Returned(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		NewPointByPointer()
	}
}

func BenchmarkPointer_Value(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		NewPointByValue()
	}
}

// --- 第 2 组：内联如何消除逃逸 -----------------------------------------------

func BenchmarkInline_NotEscaping(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		SumInlinedPointer()
	}
}

func BenchmarkInline_Leaked(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		SumLeakedPointer()
	}
}

// --- 第 3 组：interface{} 装箱 -----------------------------------------------

// 用一个大于 100 的数，绕开 strconv 对小整数的静态表快路径，保证三种写法可比。
//
// 写成包级变量而不是常量：常量转 any 时编译器能静态装箱，测不到装箱那次分配。
// 这些函数都是 //go:noinline，所以实参在被调方一定是运行期值 —— 用 var 只是把这件事写明白。
var formatInput = 1234567

func BenchmarkFormat_Sprintf(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		FormatWithSprintf(formatInput)
	}
}

func BenchmarkFormat_Itoa(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		FormatWithItoa(formatInput)
	}
}

func BenchmarkFormat_AppendReuseBuf(b *testing.B) {
	b.ReportAllocs()
	// 缓冲区在循环外一次性备好，稳态下每轮复用同一块内存。
	buf := make([]byte, 0, 32)
	for b.Loop() {
		buf = FormatWithAppend(buf[:0], formatInput)
	}
}

// --- 第 4 组：make 的长度是不是编译期常量 -------------------------------------

func BenchmarkMake_ConstLen(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		SumConstLenBuf()
	}
}

func BenchmarkMake_VarLen(b *testing.B) {
	b.ReportAllocs()
	// 实参恒为 64，和上面的常量版本工作量完全一致，差异只来自分配位置。
	n := 64
	for b.Loop() {
		SumVarLenBuf(n)
	}
}

// --- 第 5 组：隐式栈变量的 64KB 上限 ------------------------------------------

func BenchmarkMake_64KB(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		SumBufAtLimit()
	}
}

func BenchmarkMake_64KBPlus1(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		SumBufOverLimit()
	}
}

// --- 第 6 组：闭包捕获 --------------------------------------------------------

const closureIters = 100

func BenchmarkClosure_Local(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		SumWithLocalClosure(closureIters)
	}
}

func BenchmarkClosure_Returned(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		add := MakeAccumulator()
		for j := range closureIters {
			add(j)
		}
	}
}

// --- 验证组：拆解 B/op 的构成 -------------------------------------------------
//
// 这组基准不是用来比快慢的，只看 B/op 和 allocs/op —— 它们是编译期定死的，
// 不受机器负载影响。作用是验证 README 里对分配构成的拆解是否成立：
// 每条假说都能被这里的数字推翻。跑：make bench BENCH='Verify_' TOPIC=01-escape-analysis

// 假说：Sprintf 的 2 次分配 = 装箱的 int(8B) + 结果字符串(按 size class 取整)。
// 预测：结果字符串从 7 字节(class 8)变成 12 字节(class 16)时，Sprintf 的 B/op
// 应 16→24、Itoa 应 8→16，两者 allocs/op 都不变。
var (
	fmtShort = 1234567      // 十进制 7 位
	fmtLong  = 123456789012 // 十进制 12 位
)

func BenchmarkVerify_SprintfShort(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		FormatWithSprintf(fmtShort)
	}
}

func BenchmarkVerify_SprintfLong(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		FormatWithSprintf(fmtLong)
	}
}

func BenchmarkVerify_ItoaShort(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		FormatWithItoa(fmtShort)
	}
}

func BenchmarkVerify_ItoaLong(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		FormatWithItoa(fmtLong)
	}
}

// 假说：返回闭包的 24 B / 2 allocs = 堆上的 total(8B) + funcval{fn, *total}(16B)。
// 预测：多捕获一个 int，funcval 涨 8 字节(24B)，捕获变量合计仍是一次分配，所以
// B/op 应为 24+16=40。若 B/op != 40 或 allocs 变成 3，拆解就是错的。
func BenchmarkVerify_Closure1Var(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		MakeAccumulator()
	}
}

func BenchmarkVerify_Closure2Vars(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		MakeAccumulator2()
	}
}

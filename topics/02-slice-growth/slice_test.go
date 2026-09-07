package slicegrowth

import (
	"fmt"
	"testing"
)

// ===========================================================================
// 事前写定的假说与预测 —— 本段在跑任何基准之前定稿，之后不修改。
// 被推翻的条目会原样保留，结果写进 README 的「坑」和附录 B。
//
// H1（置信度：中）Go 1.18 起把翻倍的拐点从 1024 降到了 256。cap < 256 时翻倍，
//    超过后按 newcap += (newcap + 3*256)/4 逐步逼近 1.25 倍。最终容量还要经
//    roundupsize 按 size class 取整。
//    能推翻它的观察：拐点出现在 1024（Go 1.17 及以前的行为），
//    或者增长率稳定在 2.0 / 1.5 而不是趋向 1.25。
//
// H1′（置信度：低）makeslice 不做 size class 取整，growslice 做。
//    预测：cap(make([]byte, 0, 100)) == 100，而让 append 自然长到能装下
//    100 字节时，cap 会是 112（size class）。
//    若两者都是 112 或都是 100，这条就错了。
//
// H2（置信度：高）预分配把 O(log n) 次分配压成 1 次，同时消掉累计约 O(n) 的拷贝。
//    预测：往 nil slice 追加 1000 个 int，预分配版 allocs/op = 1；
//    不预分配版 allocs/op 在 10–20 之间，且 B/op 明显大于 8000。
//    若不预分配也是 1 alloc，说明编译器把整个循环优化了 —— 那是实验无效，不是 H2 错。
//
// H3（置信度：中）预分配后，append 版和索引版的分配行为完全相同，
//    差异只在循环体：索引版能做边界检查消除，append 版每轮要更新 len。
//    预测：B/op 和 allocs/op 完全一致，ns/op 索引版略快但差距在 2 倍以内。
//    若 B/op 不同，说明我对 makeslice 清零行为的理解有问题。
//
// H4（置信度：中高）扩容时 growslice 对含指针的元素类型要走带写屏障的拷贝，
//    不含指针的直接 memmove；而且 []*T 的内容还要被 GC 扫描。
//    预测：同样追加 1000 个元素，[]*T 的 ns/op 至少是 []int 的 1.5 倍，
//    而 allocs/op 相同。
//
// H5（置信度：高）append(dst, src...) 能一次算出所需容量，只扩容一次。
//    预测：dst 为 nil、src 有 1000 个元素时，批量版 allocs/op = 1，
//    逐个版 allocs/op 在 10–20 之间。
//
// H6（置信度：中）cap 而非 len 决定了内存占用、清零开销和 GC 扫描量。
//    预测：make([]int, 0, 10000) 只放 10 个元素时，B/op ≈ 80000 而不是 80，
//    ns/op 也随 cap 线性增长。
// ===========================================================================

// 四档规模一起跑：增长曲线的结论在小规模上可能完全不成立，这本身也要验证。
var sizes = []int{10, 100, 1000, 10000}

// --- 场景 1：增长曲线（用测试观察，不参与计时）--------------------------------

// sink 让被观察的 slice 逃逸。
//
// 这不是可有可无的：不逃逸的 slice 会被编译器分配一个 32 字节的栈上缓冲区，
// 序列开头会变成 []byte:[32 64 …] / []int:[4 8 …]，而真实的堆增长是从 8 字节起步的。
// 详见 README 的「坑」一节。
var sink any

// capSeq 从 nil 开始逐个 append，记录 cap 每次变化后的值。
func capSeq[T any](upTo int) []int {
	var s []T
	var seq []int
	last := 0
	for len(s) < upTo {
		var zero T
		s = append(s, zero)
		sink = s // 强制逃逸，绕开栈上缓冲区优化
		if c := cap(s); c != last {
			last = c
			seq = append(seq, c)
		}
	}
	return seq
}

// TestGrowthCurve 打印容量序列和相邻比值，用 go test -v 查看。
// 检验 H1：拐点在哪、增长率趋向多少。
func TestGrowthCurve(t *testing.T) {
	report := func(name string, seq []int) {
		t.Logf("%s 容量序列：%v", name, seq)
		for i := 1; i < len(seq); i++ {
			t.Logf("  %6d → %6d   ×%.3f", seq[i-1], seq[i], float64(seq[i])/float64(seq[i-1]))
		}
	}
	report("[]byte", capSeq[byte](5000))
	report("[]int", capSeq[int](5000))
}

// TestVerifyCapRounding 检验 H1′：makeslice 取不取整、growslice 取不取整。
// 只记录不断言 —— 预测错了要留在记录里，不该让 make check 变红。
func TestVerifyCapRounding(t *testing.T) {
	const want = 100

	made := cap(make([]byte, 0, want))

	var grown []byte
	for len(grown) < want {
		grown = append(grown, 0)
		sink = grown
	}

	t.Logf("预测：make 得到 cap=100，append 长到 100 得到 cap=112")
	t.Logf("实际：make → cap=%d   逐个 append → cap=%d", made, cap(grown))

	switch {
	case made == want && cap(grown) == 112:
		t.Log("结论：H1′ 完全命中")
	case made == want:
		t.Logf("结论：H1′ 前半命中（make 不取整），后半落空（逐个 append 得到 %d 而非 112）", cap(grown))
	default:
		t.Logf("结论：H1′ 被推翻（make 得到 %d 而非 %d）", made, want)
	}

	// 后续追查：逐个 append 是从 64 翻倍到 128，压根没机会去"凑" 100，
	// 所以上面那个测法看不到 roundupsize。改成一次性要 100 字节才能隔离出取整行为。
	var bulk []byte
	bulk = append(bulk, make([]byte, want)...)
	sink = bulk
	t.Logf("追查：一次性 append %d 字节 → cap=%d（roundupsize 的 size class）", want, cap(bulk))
}

// --- 场景 2：预分配 vs 不预分配 -----------------------------------------------

func BenchmarkPrealloc_No(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				AppendNoPrealloc(n)
			}
		})
	}
}

func BenchmarkPrealloc_Yes(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				AppendPrealloc(n)
			}
		})
	}
}

// --- 场景 3：append vs 索引赋值（都已预分配）----------------------------------

func BenchmarkFill_Append(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				FillByAppend(n)
			}
		})
	}
}

func BenchmarkFill_Index(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				FillByIndex(n)
			}
		})
	}
}

// --- 场景 4：元素含不含指针 ---------------------------------------------------

func makeIntSrc(n int) []int {
	src := make([]int, n)
	for i := range src {
		src[i] = i
	}
	return src
}

// makePtrSrc 让所有指针都指向同一块 backing 数组，
// 这样"准备源数据"只有两次分配，不会污染被测的扩容成本。
func makePtrSrc(n int) []*int {
	backing := make([]int, n)
	src := make([]*int, n)
	for i := range src {
		backing[i] = i
		src[i] = &backing[i]
	}
	return src
}

func BenchmarkElem_Int(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			src := makeIntSrc(n)
			b.ReportAllocs()
			for b.Loop() {
				AppendInts(src)
			}
		})
	}
}

func BenchmarkElem_Ptr(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			src := makePtrSrc(n)
			b.ReportAllocs()
			for b.Loop() {
				AppendPtrs(src)
			}
		})
	}
}

// --- 场景 5：批量 append vs 逐个 append ---------------------------------------

func BenchmarkBulk_OneByOne(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			src := makeIntSrc(n)
			b.ReportAllocs()
			for b.Loop() {
				AppendOneByOne(src)
			}
		})
	}
}

func BenchmarkBulk_Variadic(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			src := makeIntSrc(n)
			b.ReportAllocs()
			for b.Loop() {
				AppendBulk(src)
			}
		})
	}
}

// --- 场景 6：预分配过头 -------------------------------------------------------

// len 固定为 smallLen(10)，只有 cap 在变。
func BenchmarkOverAlloc(b *testing.B) {
	for _, capHint := range sizes {
		b.Run(fmt.Sprintf("cap=%d", capHint), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				FillSmall(capHint)
			}
		})
	}
}

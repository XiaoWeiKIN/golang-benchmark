// Package slicegrowth 研究 append 的扩容策略，以及预分配到底省下了什么。
//
// 六个场景对应 README 里的六节：
//  1. 增长曲线的形状（见 slice_test.go 的 TestGrowthCurve）
//  2. 预分配 vs 不预分配
//  3. 预分配后用 append 还是用索引赋值
//  4. 元素含不含指针
//  5. append(dst, src...) vs 逐个 append
//  6. 预分配过头的代价
package slicegrowth

// ---------------------------------------------------------------------------
// 场景 2：预分配 vs 不预分配
//
// 两个函数的循环体完全一样，唯一差异是初始 slice 有没有 cap。
// ---------------------------------------------------------------------------

// AppendNoPrealloc 从 nil 开始追加，扩容由 growslice 自己决定。
//
//go:noinline
func AppendNoPrealloc(n int) []int {
	var s []int
	for i := range n {
		s = append(s, i)
	}
	return s
}

// AppendPrealloc 一次性把 cap 开够，之后的 append 都不触发扩容。
//
//go:noinline
func AppendPrealloc(n int) []int {
	s := make([]int, 0, n)
	for i := range n {
		s = append(s, i)
	}
	return s
}

// ---------------------------------------------------------------------------
// 场景 3：预分配之后，append 还是索引赋值
//
// 分配行为应当完全相同，差异只在循环体：索引版可以做边界检查消除，
// append 版每轮要更新 len。
// ---------------------------------------------------------------------------

// FillByAppend 用 make([]T, 0, n) + append。
//
// 注意它和上面的 AppendPrealloc 函数体逐字相同 —— 这不是冗余，是个噪声地板对照组：
// 两个完全一样的函数分别测出来差多少，就是这台机器上"没有差异"的分辨率下限。
// 实测差 6.4%，所以场景「append vs 索引」里 1% 的差距不能当成结论。
//
//go:noinline
func FillByAppend(n int) []int {
	s := make([]int, 0, n)
	for i := range n {
		s = append(s, i)
	}
	return s
}

// FillByIndex 用 make([]T, n) + 下标写入。
//
//go:noinline
func FillByIndex(n int) []int {
	s := make([]int, n)
	for i := range n {
		s[i] = i
	}
	return s
}

// ---------------------------------------------------------------------------
// 场景 4：元素里有没有指针
//
// 两个函数都从 nil 开始追加同样数量的 8 字节元素，唯一差异是元素类型
// 含不含指针 —— 这决定了 growslice 搬运数据时走 memmove 还是带写屏障的拷贝，
// 也决定了这块内存要不要被 GC 扫描。
//
// 源数据由调用方预先准备，避免把"构造元素"的成本算进来。
// ---------------------------------------------------------------------------

// AppendInts 追加不含指针的元素。
//
//go:noinline
func AppendInts(src []int) []int {
	var s []int
	for _, v := range src {
		s = append(s, v)
	}
	return s
}

// AppendPtrs 追加含指针的元素。
//
//go:noinline
func AppendPtrs(src []*int) []*int {
	var s []*int
	for _, v := range src {
		s = append(s, v)
	}
	return s
}

// ---------------------------------------------------------------------------
// 场景 5：append(dst, src...) vs 逐个 append
// ---------------------------------------------------------------------------

// AppendOneByOne 逐个追加，每次只让 append 看到一个元素。
//
//go:noinline
func AppendOneByOne(src []int) []int {
	var s []int
	for _, v := range src {
		s = append(s, v)
	}
	return s
}

// AppendBulk 一次性追加整个源切片。
//
//go:noinline
func AppendBulk(src []int) []int {
	var s []int
	return append(s, src...)
}

// ---------------------------------------------------------------------------
// 场景 6：预分配过头的代价
//
// len 固定为 smallLen，只有 cap 在变 —— 用来看内存占用和耗时跟的是哪一个。
// ---------------------------------------------------------------------------

const smallLen = 10

// FillSmall 按 capHint 预分配，但只放 smallLen 个元素。
//
//go:noinline
func FillSmall(capHint int) []int {
	s := make([]int, 0, capHint)
	for i := range smallLen {
		s = append(s, i)
	}
	return s
}

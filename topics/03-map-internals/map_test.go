package mapinternals

import (
	"fmt"
	"runtime"
	"testing"
)

// ===========================================================================
// 事前写定的假说与预测 —— 本段在跑任何基准之前定稿，之后不修改。
// 被推翻的条目原样保留，结果写进 README 的「坑」和附录 B。
//
// H1（置信度：高）小 map 不上堆。
//    hint <= 8 且 map 不逃逸时，编译器把 Map 头和那唯一一个 group 都放栈上
//    （walk/builtin.go:walkMakeSwissMap 里两次 stackTempAddr）。
//    预测：装 8 条的非逃逸 map，allocs/op = 0；hint 改成常量 9 掉到堆上，
//         allocs/op >= 3（groups 数组 + table 结构体 + 目录切片）。
//    若 hint=8 有分配，或 hint=9 仍然 0 分配，这条就错了。
//
// H2（置信度：中高）make(map, hint) 的 hint 换算成容量是有台阶的，而且台阶会反着走。
//    target = hint*8/7；dirSize = alignUpPow2(ceil(target/1024))；每表 alignUpPow2(target/dirSize)。
//    预测：hint=896 → 单表 1024 槽、容量恰好 896，插 896 条零扩容；
//         hint=897 → target=1025 → 两张表各 512 槽、容量共 896 < 897，必然扩容
//         （897 条按最高位二分，两边不可能都 <= 448）；
//         于是 hint 从 896 加到 897，建表的 B/op 不降反升。
//    若两者 B/op 连续、没有台阶，这条就错了。
//
// H3（置信度：高）128 字节是内联存储的硬边界。
//    reflectdata/map_swiss.go:42,45 是严格大于。
//    预测：map[int64][128]byte 的 allocs/op 与条目数无关；
//         map[int64][129]byte 每条目多一次 newobject，allocs/op ≈ N。台阶正好在 128→129。
//    若 129 仍内联，或台阶不在这里，这条就错了。
//
// H4（置信度：中）8 条目的 map[string]int，key 越过 64 字节反而变快。
//    runtime_faststr_swiss.go:17 getWithoutKeySmallFastStr：dirLen<=0 且 len(key)>64 时
//    只比长度 + 首 8 字节 + 尾 8 字节，全程不算哈希。
//    预测 (a)：8 条目 map 的查找耗时，key 从 65B 涨到 512B 基本持平（<20% 波动）；
//    预测 (b)：9 条目 map 的同一条曲线随 key 长度单调线性上升。
//    若 8 条目那条也线性上升，或 9 条目那条也是平的，这条就错了。
//
//    H4c（在 H4 通过确认之后、跑基准之前补充的推论）：
//    快路径只在「恰好一个槽通过快速测试」时成立。若 8 个 key 的首 8 字节和尾 8 字节
//    全都相同、只有中间不同，代码会 goto dohash 退回算哈希。
//    预测：MiddleKeys 构造的 8 条目 map，曲线重新变成随长度线性上升。
//
// H5（置信度：高）删除和 clear 都不还内存。
//    table.rehash 只有「容量翻倍」和「切成两张表」两条出路，没有缩容路径；
//    Map.Clear 只清槽不释放 groups 数组。
//    预测：map[int64]int64 填 100 万后删到只剩 1 条，GC 后 map 占用下降 < 10%；
//         clear(m) 同样 < 10%；只有重建 map 才掉下来。
//    若任一方式下降 > 50%，这条就错了。
//
// H6（置信度：低 —— 这条最没底）墓碑会拖慢查找，但 pruneTombstones 会兜住。
//    Go 1.25 在 growthLeft 用尽时先试 pruneTombstones（table.go:508），
//    能回收 >=10% 容量才动手，回收不动才扩容。
//    预测：反复「删一条插一条」到稳态后，查找耗时比同尺寸新建 map 高 >= 30%，
//         但收敛，不会超过 2 倍。
//    若完全不退化，或退化超过 2 倍，这条就错了。
//
// H7（置信度：中高）map 里有没有指针，决定 GC 要不要扫它，成本差一个数量级。
//    预测：100 万条的 map[int64]int64，groups 数组 noscan，runtime.GC() 耗时与空堆同量级；
//         换成 map[int64]*int64（全部指向同一个对象，隔离掉对象数变量）后 >= 5 倍。
//    若两者 GC 耗时相当，这条就错了。
//
// H8（置信度：中）Swiss Table 的收益在查找和内存，不在插入。
//    预测：同一组 AB_ 基准在 GOEXPERIMENT=noswissmap 下重跑，
//         查找类 swiss 快 >= 15%；建表 B/op 少 >= 8%
//         （理论值 136B/8 槽 @ 7/8 = 19.4 字节每条，对 144B/8 槽 @ 6.5/8 = 22.2）；
//         插入类两者差异 < 10%。
//    若插入差异也很大，或查找没优势，这条就错了。
// ===========================================================================

var (
	sinkInt  int
	sinkI64  int64
	sinkIMap map[int64]int64
	sinkE128 map[int64]Elem128
	sinkE129 map[int64]Elem129
	sinkK128 map[Key128]int
	sinkK129 map[Key129]int
)

func heapAllocNow() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// ---------------------------------------------------------------------------
// 原则 7：先确认实验测到的真是你以为的东西
// ---------------------------------------------------------------------------

func TestSetup_Sanity(t *testing.T) {
	// 场景 4 的两种 key 形状必须真的如设计所述，否则整组数据没有意义。
	d := DistinctKeys(8, 128)
	for i := 1; i < len(d); i++ {
		if d[i][:8] == d[0][:8] {
			t.Fatalf("DistinctKeys 的首 8 字节应当互不相同，但 [%d] 和 [0] 一样", i)
		}
		if len(d[i]) != 128 {
			t.Fatalf("DistinctKeys 长度应为 128，实际 %d", len(d[i]))
		}
	}

	m := MiddleKeys(8, 128)
	for i := 1; i < len(m); i++ {
		if m[i][:8] != m[0][:8] || m[i][120:] != m[0][120:] {
			t.Fatalf("MiddleKeys 的首尾 8 字节应当全部相同，但 [%d] 不一样", i)
		}
		if m[i] == m[0] {
			t.Fatalf("MiddleKeys 整体应当互不相同，但 [%d] 和 [0] 一样", i)
		}
	}

	// 装进 map 之后条目数必须对得上：8 条走小 map 快路径，9 条不走。
	if got := len(StrMapOf(DistinctKeys(8, 128))); got != 8 {
		t.Fatalf("8 条 key 的 map 应有 8 条，实际 %d", got)
	}
	if got := len(StrMapOf(DistinctKeys(9, 128))); got != 9 {
		t.Fatalf("9 条 key 的 map 应有 9 条，实际 %d", got)
	}

	// 场景 5 的搅动 map 必须真的维持在 n 条。
	cm, ck := BuildChurned(1000, 5000)
	if len(cm) != 1000 || len(ck) != 1000 {
		t.Fatalf("搅动后应仍有 1000 条，实际 len(m)=%d len(keys)=%d", len(cm), len(ck))
	}
	for _, k := range ck {
		if _, ok := cm[k]; !ok {
			t.Fatalf("key %d 应当还在表里", k)
		}
	}
}

// ---------------------------------------------------------------------------
// H1：小 map 的特权
// ---------------------------------------------------------------------------

var strKeys8 = DistinctKeys(8, 16)

func BenchmarkSmall_NoHint(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sinkInt = SmallMapNoHint(strKeys8)
	}
}

func BenchmarkSmall_Hint8(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sinkInt = SmallMapHint8(strKeys8)
	}
}

func BenchmarkSmall_Hint9(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sinkInt = SmallMapHint9(strKeys8)
	}
}

func BenchmarkSmall_Escape(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sinkInt = SmallMapEscape(strKeys8)
	}
}

// ---------------------------------------------------------------------------
// H2：预分配的台阶
//
// 每个 op = 往一张全新的 map 里插入 n 条。hint 和 n 一起变 ——
// 因为要问的正是「我知道要插 n 条，那 make(map, n) 到底够不够」。
// ---------------------------------------------------------------------------

var presizeSizes = []int{8, 9, 896, 897, 898, 1000, 1792, 1793}

// 两种 key 形状。顺序 key 的哈希高位恰好均衡（见 IntKeys 的注释和 README 坑二），
// 多表时会让预分配显得比实际更管用；随机 key 才是真实分布。
var keyShapes = []struct {
	name string
	gen  func(int) []int64
}{
	{"seq", IntKeys},
	{"rand", RandKeys},
}

func benchPresize(b *testing.B, hintOf func(n int) int) {
	for _, sh := range keyShapes {
		for _, n := range presizeSizes {
			b.Run(fmt.Sprintf("%s/n=%d", sh.name, n), func(b *testing.B) {
				ks := sh.gen(n)
				hint := hintOf(n)
				b.ReportAllocs()
				for b.Loop() {
					sinkIMap = BuildIntMap(ks, hint)
				}
			})
		}
	}
}

// Exact：make(map, n)，即「我知道要装 n 条，就报 n」
func BenchmarkPresize_Exact(b *testing.B) { benchPresize(b, func(n int) int { return n }) }

// NoHint：完全不预分配，作为上界参照
func BenchmarkPresize_NoHint(b *testing.B) { benchPresize(b, func(n int) int { return -1 }) }

// Headroom：多报 1/7，正好抵消 7/8 载荷带来的向上取整
func BenchmarkPresize_Headroom(b *testing.B) { benchPresize(b, func(n int) int { return n + n/7 }) }

// TestH2_HintSteps 把源码算法的推算（PredictShape）和实测稳态占用摆在一起对账。
// groupBytes = 8 字节控制字 + 8 个 (int64,int64) 槽 = 136。
// 实测比预测高出的那一截是 size class 向上取整 + table 结构体 + 目录切片，
// 没有扩容时稳定在 1.06 倍左右；明显超过这个数就说明发生了扩容。
func TestH2_HintSteps(t *testing.T) {
	const groupBytes = 8 + 8*(8+8)
	t.Logf("预测：hint 从 896 加到 897，总容量仍是 896（两表各 448），必然扩容")
	t.Logf("%-6s %-6s %-8s %-9s %-8s %-10s %-12s %-8s", "key", "hint", "表数", "每表槽数", "总容量", "预测字节", "实测字节", "实测/预测")

	for _, sh := range keyShapes {
		for _, hint := range []int{8, 9, 64, 512, 896, 897, 898, 1000, 1792, 1793, 3584} {
			shape := PredictShape(hint, groupBytes)
			ks := sh.gen(hint)

			// 小 hint 的单张 map 只有几百字节，低于 ReadMemStats 的分辨率，
			// 所以一次建 reps 张再平摊（原则 6：先量出自己的分辨率）。
			reps := 1 + 1<<20/shape.Bytes
			ms := make([]map[int64]int64, reps)

			runtime.GC()
			runtime.GC()
			before := heapAllocNow()
			for i := range ms {
				ms[i] = BuildIntMap(ks, hint)
			}
			runtime.GC()
			runtime.GC()
			after := heapAllocNow()

			measured := int(after-before) / reps
			t.Logf("%-6s %-6d %-8d %-9d %-8d %-10d %-12d %.2fx",
				sh.name, hint, shape.DirSize, shape.TableCap, shape.Capacity,
				shape.Bytes, measured, float64(measured)/float64(shape.Bytes))

			if len(ms[0]) != hint {
				t.Fatalf("hint=%d 建出来的 map 有 %d 条，应为 %d", hint, len(ms[0]), hint)
			}
			runtime.KeepAlive(ms)
			runtime.KeepAlive(ks)
		}
	}
}

// TestH2_Headroom 回答实际问题：要装 n 条，hint 该报多少才真的不扩容？
// 用二分找出第一次多出分配的插入位置，就是这张表的真实容量。
func TestH2_Headroom(t *testing.T) {
	t.Logf("每行：给定 hint，实际能装多少条才第一次扩容")
	t.Logf("%-6s %-8s %-10s %-10s %-10s", "key", "hint", "预测容量", "实测容量", "实测/hint")

	for _, sh := range keyShapes {
		for _, hint := range []int{896, 898, 1000, 1792, 3584, 10000} {
			shape := PredictShape(hint, 8+8*(8+8))
			ks := sh.gen(hint * 3)
			baseline := allocsForInserts(ks, hint, 1)

			// 二分：找到第一个使分配次数超过基线的插入条数
			lo, hi := 1, hint*3
			for lo < hi {
				mid := (lo + hi) / 2
				if allocsForInserts(ks, hint, mid) > baseline {
					hi = mid
				} else {
					lo = mid + 1
				}
			}
			t.Logf("%-6s %-8d %-10d %-10d %.3f", sh.name, hint, shape.Capacity, lo-1, float64(lo-1)/float64(hint))
		}
	}
}

//go:noinline
func buildFirstN(keys []int64, hint, n int) map[int64]int64 {
	m := make(map[int64]int64, hint)
	for i := 0; i < n; i++ {
		m[keys[i]] = keys[i]
	}
	return m
}

func allocsForInserts(keys []int64, hint, n int) float64 {
	return testing.AllocsPerRun(20, func() { sinkIMap = buildFirstN(keys, hint, n) })
}

// ---------------------------------------------------------------------------
// H3：128 字节的分水岭
// ---------------------------------------------------------------------------

const bigN = 1000

func BenchmarkBigElem_128(b *testing.B) {
	ks := IntKeys(bigN)
	b.ReportAllocs()
	for b.Loop() {
		sinkE128 = BuildElem128(ks)
	}
}

func BenchmarkBigElem_129(b *testing.B) {
	ks := IntKeys(bigN)
	b.ReportAllocs()
	for b.Loop() {
		sinkE129 = BuildElem129(ks)
	}
}

func BenchmarkBigKey_128(b *testing.B) {
	ks := BigKeys128(bigN)
	b.ReportAllocs()
	for b.Loop() {
		sinkK128 = BuildKey128(ks)
	}
}

func BenchmarkBigKey_129(b *testing.B) {
	ks := BigKeys129(bigN)
	b.ReportAllocs()
	for b.Loop() {
		sinkK129 = BuildKey129(ks)
	}
}

// TestH3_Footprint 量稳态占用。建表的 B/op 里混着扩容produced的垃圾，
// 这里只看最终留在堆上的部分，才能回答「哪种更省内存」。
func TestH3_Footprint(t *testing.T) {
	ks := IntKeys(bigN)
	t.Logf("%d 条，槽内联 vs 槽存指针的稳态占用：", bigN)

	runtime.GC()
	runtime.GC()
	before := heapAllocNow()
	m128 := BuildElem128(ks)
	runtime.GC()
	runtime.GC()
	b128 := int(heapAllocNow() - before)

	runtime.GC()
	runtime.GC()
	before = heapAllocNow()
	m129 := BuildElem129(ks)
	runtime.GC()
	runtime.GC()
	b129 := int(heapAllocNow() - before)

	t.Logf("[128]byte（内联）  %6d KB，每条 %.0f 字节（净荷 128）", b128/1024, float64(b128)/bigN)
	t.Logf("[129]byte（存指针）%6d KB，每条 %.0f 字节（净荷 129）", b129/1024, float64(b129)/bigN)
	runtime.KeepAlive(m128)
	runtime.KeepAlive(m129)
}

// ---------------------------------------------------------------------------
// H4：长字符串 key 的小 map 会跳过哈希
//
// 三种 key 形状 × 两种探针，横轴都是 key 长度：
//   Small8_Distinct  8 条 + 首 8 字节互不相同 → 走快路径
//   Large9_Distinct  9 条（有目录了）        → 必须算哈希，作对照
//   Small8_Middle    8 条但首尾 8 字节全相同 → 快速测试区分不了，退回算哈希
//
// Hit  查一个表里有的 key
// Miss 查一个同样形状但不在表里的 key
//
// Miss 这一维是读源码时补上的：runtime_faststr_swiss.go:25 那段注释写明
// 这条快路径针对的是「8 个槽都 quick-match 失败」的情形，也就是查不中。
// 只测查中的话，测不到它设计出来要解决的那个问题。
// ---------------------------------------------------------------------------

var keyLens = []int{32, 64, 65, 128, 512, 1024, 4096}

func benchStrLookup(b *testing.B, count int, gen func(count, length int) []string, miss bool) {
	for _, l := range keyLens {
		b.Run(fmt.Sprintf("len=%d", l), func(b *testing.B) {
			// 多生成一个：下标 count 那个形状相同但不入表，用作 miss 探针。
			all := gen(count+1, l)
			ks := all[:count]
			m := StrMapOf(ks)
			if len(m) != count {
				b.Fatalf("map 应有 %d 条，实际 %d", count, len(m))
			}
			probe := ks[count/2]
			if miss {
				probe = all[count]
				if _, ok := m[probe]; ok {
					b.Fatalf("miss 探针不应该在表里")
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				sinkInt = LookupStrMap(m, probe)
			}
		})
	}
}

func BenchmarkStrKey_Small8_Distinct_Hit(b *testing.B)  { benchStrLookup(b, 8, DistinctKeys, false) }
func BenchmarkStrKey_Small8_Distinct_Miss(b *testing.B) { benchStrLookup(b, 8, DistinctKeys, true) }
func BenchmarkStrKey_Large9_Distinct_Hit(b *testing.B)  { benchStrLookup(b, 9, DistinctKeys, false) }
func BenchmarkStrKey_Large9_Distinct_Miss(b *testing.B) { benchStrLookup(b, 9, DistinctKeys, true) }
func BenchmarkStrKey_Small8_Middle_Hit(b *testing.B)    { benchStrLookup(b, 8, MiddleKeys, false) }
func BenchmarkStrKey_Small8_Middle_Miss(b *testing.B)   { benchStrLookup(b, 8, MiddleKeys, true) }

// ---------------------------------------------------------------------------
// H5：删除不缩容
// ---------------------------------------------------------------------------

func TestH5_NoShrink(t *testing.T) {
	const n = 1_000_000
	t.Logf("预测：删到只剩 1 条、或 clear，map 占用下降都 < 10%%；只有重建才降")

	measure := func(name string, shrink func(m map[int64]int64) map[int64]int64) {
		ks := IntKeys(n)
		runtime.GC()
		runtime.GC()
		before := heapAllocNow()

		m := BuildIntMap(ks, n)
		runtime.GC()
		runtime.GC()
		filled := heapAllocNow() - before

		m = shrink(m)
		runtime.GC()
		runtime.GC()
		after := heapAllocNow() - before

		t.Logf("%-12s 满表 %6d KB → %6d KB（剩 %d 条，占用降到 %.1f%%）",
			name, filled/1024, after/1024, len(m), 100*float64(after)/float64(filled))
		runtime.KeepAlive(m)
		runtime.KeepAlive(ks)
	}

	measure("delete 到剩 1 条", func(m map[int64]int64) map[int64]int64 {
		for k := range m {
			if k != 0 {
				delete(m, k)
			}
		}
		return m
	})

	measure("clear(m)", func(m map[int64]int64) map[int64]int64 {
		clear(m)
		m[0] = 0
		return m
	})

	measure("重建", func(m map[int64]int64) map[int64]int64 {
		fresh := make(map[int64]int64)
		fresh[0] = m[0]
		return fresh
	})
}

// ---------------------------------------------------------------------------
// H6：墓碑
//
// churnN 条稳定驻留，搅动 churnRounds 轮「删一条插一条」。
// 对照组是同一批 key 新建的 map。
// ---------------------------------------------------------------------------

const (
	churnN      = 100_000
	churnRounds = 1_000_000
)

func BenchmarkTombstone_Fresh(b *testing.B) {
	_, ks := BuildChurned(churnN, churnRounds)
	m := BuildFresh(ks)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		sinkI64 = LookupIntMap(m, ks[i&(len(ks)-1)])
		i++
	}
}

func BenchmarkTombstone_Churned(b *testing.B) {
	m, ks := BuildChurned(churnN, churnRounds)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		sinkI64 = LookupIntMap(m, ks[i&(len(ks)-1)])
		i++
	}
}

// TestH6_ChurnFootprint 分别量出「搅动过的表」和「同一批 key 新建的表」的稳态占用。
// 查找变慢到底是墓碑本身，还是表被撑大导致缓存变差，这两个数字能把它们分开。
func TestH6_ChurnFootprint(t *testing.T) {
	// 先量搅动表。keys 切片本身要扣掉。
	runtime.GC()
	runtime.GC()
	before := heapAllocNow()
	m, ks := BuildChurned(churnN, churnRounds)
	runtime.GC()
	runtime.GC()
	churnBytes := int(heapAllocNow()-before) - churnN*8

	// 再量新建表。此刻 m 和 ks 都还活着，测出来的就是新表自己的净增量。
	runtime.GC()
	runtime.GC()
	before2 := heapAllocNow()
	fresh := BuildFresh(ks)
	runtime.GC()
	runtime.GC()
	freshBytes := int(heapAllocNow() - before2)

	if len(m) != churnN || len(fresh) != churnN {
		t.Fatalf("两张表都应有 %d 条，实际 churned=%d fresh=%d", churnN, len(m), len(fresh))
	}
	t.Logf("搅动 %d 轮后：len=%d，占用 %d KB（每条 %.1f 字节）",
		churnRounds, len(m), churnBytes/1024, float64(churnBytes)/float64(churnN))
	t.Logf("同一批 key 新建：len=%d，占用 %d KB（每条 %.1f 字节）",
		len(fresh), freshBytes/1024, float64(freshBytes)/float64(churnN))
	t.Logf("搅动表 / 新建表 = %.2fx", float64(churnBytes)/float64(freshBytes))

	runtime.KeepAlive(m)
	runtime.KeepAlive(ks)
	runtime.KeepAlive(fresh)
}

// ---------------------------------------------------------------------------
// H7：map 里有没有指针，决定 GC 要不要扫它
//
// 每个 op = 一次 runtime.GC()。
// ---------------------------------------------------------------------------

const gcN = 1_000_000

func BenchmarkGC_Baseline(b *testing.B) {
	runtime.GC()
	for b.Loop() {
		runtime.GC()
	}
}

func BenchmarkGC_NoPtr(b *testing.B) {
	m := BuildNoPtr(gcN)
	runtime.GC()
	for b.Loop() {
		runtime.GC()
	}
	runtime.KeepAlive(m)
}

func BenchmarkGC_SharedPtr(b *testing.B) {
	m := BuildSharedPtr(gcN)
	runtime.GC()
	for b.Loop() {
		runtime.GC()
	}
	runtime.KeepAlive(m)
}

func BenchmarkGC_DistinctPtr(b *testing.B) {
	m := BuildDistinctPtr(gcN)
	runtime.GC()
	for b.Loop() {
		runtime.GC()
	}
	runtime.KeepAlive(m)
}

// ---------------------------------------------------------------------------
// H8：Swiss Table vs 老 bucket 实现
//
// 这一组用 make ab TOPIC=03-map-internals BENCH='AB_' 跑，
// 会在默认和 GOEXPERIMENT=noswissmap 两种实现下各跑一遍再 benchstat 对比。
// 只有在两种实现下含义都成立的基准才放进这一组。
// ---------------------------------------------------------------------------

var abSizes = []int{1000, 1_000_000}

func BenchmarkAB_LookupHit(b *testing.B) {
	for _, n := range abSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			ks := IntKeys(n)
			m := BuildIntMap(ks, n)
			i := 0
			b.ReportAllocs()
			for b.Loop() {
				sinkI64 = LookupIntMap(m, ks[i%n])
				i++
			}
		})
	}
}

func BenchmarkAB_LookupMiss(b *testing.B) {
	for _, n := range abSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			ks := IntKeys(n)
			m := BuildIntMap(ks, n)
			i := 0
			b.ReportAllocs()
			for b.Loop() {
				sinkI64 = LookupIntMap(m, int64(n+i%n))
				i++
			}
		})
	}
}

func BenchmarkAB_Build(b *testing.B) {
	for _, n := range abSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			ks := IntKeys(n)
			b.ReportAllocs()
			for b.Loop() {
				sinkIMap = BuildIntMap(ks, -1)
			}
		})
	}
}

func BenchmarkAB_BuildPresized(b *testing.B) {
	for _, n := range abSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			ks := IntKeys(n)
			b.ReportAllocs()
			for b.Loop() {
				sinkIMap = BuildIntMap(ks, n)
			}
		})
	}
}

func BenchmarkAB_Iterate(b *testing.B) {
	for _, n := range abSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			m := BuildIntMap(IntKeys(n), n)
			b.ReportAllocs()
			for b.Loop() {
				var s int64
				for _, v := range m {
					s += v
				}
				sinkI64 = s
			}
		})
	}
}

// TestAB_Footprint 报告每条目的稳态字节数，在两种实现下分别跑。
// 理论值：swiss 136 字节/8 槽 @ 7/8 载荷 = 19.4；老 bucket 144 字节/8 槽 @ 6.5/8 = 22.2。
func TestAB_Footprint(t *testing.T) {
	t.Logf("runtime.Version() = %s", runtime.Version())
	for _, n := range []int{1000, 100_000, 1_000_000} {
		ks := IntKeys(n)
		runtime.GC()
		runtime.GC()
		before := heapAllocNow()
		m := BuildIntMap(ks, n)
		runtime.GC()
		runtime.GC()
		bytes := heapAllocNow() - before
		t.Logf("n=%-8d 预分配建表占 %7d KB，每条目 %.2f 字节", n, bytes/1024, float64(bytes)/float64(n))
		runtime.KeepAlive(m)
		runtime.KeepAlive(ks)
	}
}

// ---------------------------------------------------------------------------
// 噪声地板（METHODOLOGY 原则 6）：两个函数体逐字相同
// ---------------------------------------------------------------------------

func benchNoise(b *testing.B, f func(map[int64]int64, int64) int64) {
	ks := IntKeys(1024)
	m := BuildIntMap(ks, 1024)
	i := 0
	b.ReportAllocs()
	for b.Loop() {
		sinkI64 = f(m, ks[i&1023])
		i++
	}
}

func BenchmarkNoiseFloor_A(b *testing.B) { benchNoise(b, NoiseFloorA) }
func BenchmarkNoiseFloor_B(b *testing.B) { benchNoise(b, NoiseFloorB) }

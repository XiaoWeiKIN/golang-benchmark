package strs

import (
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
	"unique"
)

// ===========================================================================
// 事前写定的假说与预测 —— 本段在跑任何基准之前定稿，之后不修改。
// 被推翻的条目原样保留，结果写进 README 的「坑」和附录 B。
//
// 【上半场：转换】
//
// H1（置信度：高）string(b) 和 []byte(s) 默认都要拷贝数据。
//    预测：各 1 alloc/op，B/op 随长度走 size class。
//
// H2（置信度：高）编译器对几种"转换结果活不过当前表达式"的写法有零拷贝特例：
//    m[string(b)]、string(b) == "字面量"、for range string(b)、append(b, s...)。
//    预测：这四种写法 0 allocs/op；把 string(b) 存进中间变量后变成 1 alloc/op。
//    若 LookupDirect 和 LookupViaVar 分配次数相同，这条就错了。
//
// H3（置信度：高）unsafe.String 零拷贝。
//    预测：0 allocs/op，ns/op 接近噪声地板；且能构造出"修改原 []byte 导致
//    string 内容改变"的实例，证明它破坏了 string 的不可变性。
//
// 【下半场：驻留】
//
// H4（置信度：中高）驻留的节省倍数只跟字符串长度有关，跟重复次数无关。
//    因为每个引用仍要占 16 字节的 string header，只有数据能共享：
//        不驻留 ≈ n×(16+len)   驻留 ≈ n×16 + k×len
//        节省倍数 ≈ (16+len)/16
//    预测：n=1000、k=1 时，len=16 省约 2×、len=64 省约 5×、len=256 省约 17×；
//    把 n 从 100 提到 10000，同一个 len 下的节省倍数不变。
//    若节省倍数随 n 上升，或短串也能省 3 倍以上，这条就错了。
//    （这条若成立，VictoriaMetrics 那篇文章"三份变一份省 3 倍"的配图是误导性的。）
//
// H5（置信度：中）命中率低时驻留是净亏损：命中省一次数据拷贝，未命中要多付
//    一次插入（写锁 + map 扩容）。
//    预测：存在一个命中率阈值，低于它驻留版本的 ns/op 和 B/op 都超过不驻留版本；
//    我猜阈值在 50% 附近。
//    若任何命中率下驻留都不亏，这条就错了。
//
// H6（置信度：中）16 分片的 map+RWMutex 在并发下接近 sync.Map，但保持 0 分配。
//    预测：10 核并发下 ShardedInterner 的 ns/op 与 SyncMapInterner 同量级
//    （2 倍以内），allocs/op = 0。
//    若分片版仍接近单锁版，说明瓶颈不在锁粒度，这条就错了。
//
// H7（置信度：低 —— 这条最没底）unique 的弱指针 + GC 联动清理真的会及时发生。
//    预测：驻留 10 万个唯一字符串后丢弃全部引用，连续 runtime.GC() 若干轮，
//    unique 版本的 HeapAlloc 回落到接近初始值；sync.Map 版本不回落。
//    若 unique 也不回落，或需要超过 5 轮 GC 才回落（实践中不能指望及时释放），
//    这条就错了。
//
// H8（置信度：中低 —— 我怀疑自己会被推翻）驻留降低 GC 标记开销。
//    预测：同样数据量下，驻留版本的 runtime.GC() 耗时至少降低 30%。
//    反面理由：字符串数据是 noscan 的，GC 不扫内容只扫 header 指针，
//    而 header 的数量并没有减少 —— 所以很可能测不出差别。
//    若耗时差异小于噪声地板，这条就被推翻。
//
// --- 第二轮（收进 VictoriaMetrics 的双 map 实现时补写，同样先于实验）---
//
// H9（置信度：中高）VM 的迁移触发条件 mutableReads > len(readonly) 让 O(N) 的
//    全量拷贝摊还成 O(1)。
//    预测：往空表插入 N 个互不相同的 key，每 key 平均耗时基本恒定 ——
//    N 从 1e3 涨到 1e5（100 倍），平均耗时变化不超过 2 倍。
//    若平均耗时随 N 明显上升，说明不是摊还 O(1)，这条就错了。
//
// H10（置信度：中）快路径的优势在插入密集时会消失：每次未命中 VM 都要
//    Clone + 加锁 + 可能触发迁移，而 unique 只需在自己的表里插一次。
//    预测：全未命中时 VM 慢于 unique；命中为主时 VM 更快 —— 存在交叉点。
//    若 VM 在任何命中率下都更快、或都更慢，这条就错了。
// ===========================================================================

func init() {
	for i := range 64 {
		s := "go1." + strconv.Itoa(i) + ".4"
		LookupTable[s] = i
	}
	LookupTable["go1.16.4"] = 16

	// 长 key：40 字节，超过 32 字节的栈上临时缓冲区上限
	LookupTableLong[string(keyLong)] = 1
}

// keyLong 是 40 字节，key64 是 8 字节。
var keyLong = []byte("go_info{version=go1.16.4,inst=host1234}xx")

// ---------------------------------------------------------------------------
// 上半场 H1：默认转换要拷贝
// ---------------------------------------------------------------------------

var lengths = []int{16, 64, 256}

func makeBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

func BenchmarkConv_BytesToString(b *testing.B) {
	for _, n := range lengths {
		b.Run(fmt.Sprintf("len=%d", n), func(b *testing.B) {
			src := makeBytes(n)
			b.ReportAllocs()
			for b.Loop() {
				BytesToString(src)
			}
		})
	}
}

func BenchmarkConv_StringToBytes(b *testing.B) {
	for _, n := range lengths {
		b.Run(fmt.Sprintf("len=%d", n), func(b *testing.B) {
			src := string(makeBytes(n))
			b.ReportAllocs()
			for b.Loop() {
				StringToBytes(src)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 上半场 H2：编译器的零拷贝特例
// ---------------------------------------------------------------------------

var key64 = []byte("go1.16.4")

func BenchmarkSpecial_LookupDirect(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		LookupDirect(key64)
	}
}

func BenchmarkSpecial_LookupViaVar(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		LookupViaVar(key64)
	}
}

// 追查：LookupViaVar 没分配，怀疑是 ≤32 字节的非逃逸 string(b) 走了栈上临时缓冲区。
// 预测：换成 40 字节的 key，LookupViaVarLong 会变成 1 alloc，
// 而 LookupDirectLong 仍然是 0 alloc。若两者都是 0，猜测错。
func BenchmarkSpecial_LookupViaVarLong(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		LookupViaVarLong(keyLong)
	}
}

func BenchmarkSpecial_LookupDirectLong(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		LookupDirectLong(keyLong)
	}
}

func BenchmarkSpecial_CompareLiteral(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		CompareLiteral(key64)
	}
}

func BenchmarkSpecial_RangeString(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		RangeString(key64)
	}
}

func BenchmarkSpecial_AppendString(b *testing.B) {
	dst := make([]byte, 0, 64)
	s := "go1.16.4"
	b.ReportAllocs()
	for b.Loop() {
		AppendStringToBytes(dst[:0], s)
	}
}

// ---------------------------------------------------------------------------
// 上半场 H3：unsafe 零拷贝，以及它破坏不可变性的证据
// ---------------------------------------------------------------------------

func BenchmarkConv_UnsafeString(b *testing.B) {
	for _, n := range lengths {
		b.Run(fmt.Sprintf("len=%d", n), func(b *testing.B) {
			src := makeBytes(n)
			b.ReportAllocs()
			for b.Loop() {
				UnsafeString(src)
			}
		})
	}
}

// TestUnsafeStringAliasing 演示 unsafe.String 的代价：
// 返回的 string 和原 []byte 共享底层数组，改 []byte 就改了"不可变"的 string。
func TestUnsafeStringAliasing(t *testing.T) {
	b := []byte("hello")

	safe := BytesToString(b)   // 拷贝
	unsafeS := UnsafeString(b) // 零拷贝，共享底层数组

	b[0] = 'J'

	t.Logf("改动原 []byte 之后：safe=%q  unsafe=%q", safe, unsafeS)
	if safe != "hello" {
		t.Errorf("拷贝版本不该受影响，实际 %q", safe)
	}
	if unsafeS == "hello" {
		t.Log("结论：H3 后半被推翻 —— unsafe.String 竟然没有跟着变")
	} else {
		t.Logf("结论：H3 后半命中 —— 一个 string 的内容被改掉了，值从 \"hello\" 变成 %q", unsafeS)
	}
}

// ---------------------------------------------------------------------------
// 下半场 H4：节省倍数只跟长度有关，跟重复次数无关
// ---------------------------------------------------------------------------

// liveHeap 连做两轮 GC 后读取存活堆大小。
// 注意测的是「存活内存」而不是 benchmark 的 B/op —— B/op 是分配流量，
// 和「驻留省内存」是两回事，这个区分本身就是本专题要讲的。
func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// makeBases 造 k 个互不相同、长度为 length 的字符串。
func makeBases(k, length int) []string {
	out := make([]string, k)
	for i := range out {
		b := makeBytes(length)
		tag := strconv.Itoa(i)
		copy(b, tag)
		out[i] = string(b)
	}
	return out
}

// buildPlain 造 n 个引用，每个都是独立副本（模拟"每次采集都重新解析")。
func buildPlain(n int, bases []string) []string {
	out := make([]string, n)
	for i := range out {
		b := []byte(bases[i%len(bases)])
		out[i] = string(b)
	}
	return out
}

// buildInterned 造 n 个引用，全部经过驻留。
func buildInterned(n int, bases []string, in Interner) []string {
	out := make([]string, n)
	for i := range out {
		b := []byte(bases[i%len(bases)])
		out[i] = in.Intern(string(b))
	}
	return out
}

func TestH4_SavingRatio(t *testing.T) {
	t.Logf("预测：节省倍数 ≈ (16+len)/16，只跟长度有关，跟 n 无关")
	t.Logf("%6s %6s %12s %12s %8s %8s", "n", "len", "不驻留(B)", "驻留(B)", "实测", "预测")

	for _, n := range []int{100, 1000, 10000} {
		for _, length := range []int{16, 64, 256} {
			bases := makeBases(1, length)

			before := liveHeap()
			plain := buildPlain(n, bases)
			plainHeap := liveHeap() - before
			runtime.KeepAlive(plain)
			plain = nil

			in := NewMutexInterner()
			before = liveHeap()
			interned := buildInterned(n, bases, in)
			internedHeap := liveHeap() - before
			runtime.KeepAlive(interned)
			runtime.KeepAlive(in)
			interned = nil

			got := float64(plainHeap) / float64(internedHeap)
			want := float64(16+length) / 16.0
			t.Logf("%6d %6d %12d %12d %7.2f× %7.2f×", n, length, plainHeap, internedHeap, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 下半场 H5：命中率低时驻留是净亏损
// ---------------------------------------------------------------------------

// hitRates 用「不同 key 的总数」来控制命中率：keyspace 越大，重复率越低。
var hitRates = []struct {
	name     string
	distinct int // 一批 batchSize 个 key 里有多少个不同的
}{
	{"hit=99.9%", 10},
	{"hit=90%", 1000},
	{"hit=50%", 5000},
	{"hit=0%", 10000},
}

// batchSize 是一批要处理的 key 数量。命中率 = 1 - distinct/batchSize。
//
// 为什么每批都新建一个驻留表：如果表建在 b.Loop() 外面，跑几万轮之后所有 key
// 都进过表了，四档"命中率"会全部收敛成 100% —— 那样测的是别的东西。
// 每批一张新表恰好对应文章自己提的 gotcha：驻留表必须定期轮换，否则无限增长。
const batchSize = 10000

func makeBatch(distinct int) [][]byte {
	keys := makeBases(distinct, 64)
	raw := make([][]byte, batchSize)
	for i := range raw {
		raw[i] = []byte(keys[i%len(keys)])
	}
	return raw
}

// 每个 op = 处理 batchSize 个 key，不是单个 key。
func BenchmarkHitRate_Interned(b *testing.B) {
	for _, hr := range hitRates {
		b.Run(hr.name, func(b *testing.B) {
			raw := makeBatch(hr.distinct)
			b.ReportAllocs()
			for b.Loop() {
				in := NewMutexInterner()
				for _, src := range raw {
					in.Intern(string(src))
				}
			}
		})
	}
}

func BenchmarkHitRate_Plain(b *testing.B) {
	for _, hr := range hitRates {
		b.Run(hr.name, func(b *testing.B) {
			raw := makeBatch(hr.distinct)
			b.ReportAllocs()
			for b.Loop() {
				for _, src := range raw {
					BytesToString(src)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 下半场 H6：四种容器的命中开销，单线程 + 并发
// ---------------------------------------------------------------------------

const internKey = `go_info{version="go1.16.4",instance="host-1234"}`

func benchIntern(b *testing.B, in Interner) {
	in.Intern(internKey) // 预热，保证走命中路径
	b.ReportAllocs()
	for b.Loop() {
		in.Intern(internKey)
	}
}

func benchInternParallel(b *testing.B, in Interner) {
	in.Intern(internKey)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			in.Intern(internKey)
		}
	})
}

func BenchmarkIntern_Mutex(b *testing.B)   { benchIntern(b, NewMutexInterner()) }
func BenchmarkIntern_SyncMap(b *testing.B) { benchIntern(b, NewSyncMapInterner()) }
func BenchmarkIntern_Sharded(b *testing.B) { benchIntern(b, NewShardedInterner()) }
func BenchmarkIntern_Unique(b *testing.B)  { benchIntern(b, NewUniqueInterner()) }
func BenchmarkIntern_VM(b *testing.B)      { benchIntern(b, NewVMInterner()) }

func BenchmarkPar_Mutex(b *testing.B)   { benchInternParallel(b, NewMutexInterner()) }
func BenchmarkPar_SyncMap(b *testing.B) { benchInternParallel(b, NewSyncMapInterner()) }
func BenchmarkPar_Sharded(b *testing.B) { benchInternParallel(b, NewShardedInterner()) }
func BenchmarkPar_Unique(b *testing.B)  { benchInternParallel(b, NewUniqueInterner()) }
func BenchmarkPar_VM(b *testing.B)      { benchInternParallel(b, NewVMInterner()) }

// 追查 H6：上面的并发基准让所有 goroutine 打同一个 key，全部落在同一个分片，
// 分片自然帮不上忙 —— 那是个无效实验。这一组换成 1024 个分散的 key 重测。
func benchInternParallelSpread(b *testing.B, in Interner) {
	keys := makeBases(1024, 48)
	for _, k := range keys {
		in.Intern(k)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			in.Intern(keys[i&1023])
			i++
		}
	})
}

func BenchmarkSpread_Mutex(b *testing.B)   { benchInternParallelSpread(b, NewMutexInterner()) }
func BenchmarkSpread_SyncMap(b *testing.B) { benchInternParallelSpread(b, NewSyncMapInterner()) }
func BenchmarkSpread_Sharded(b *testing.B) { benchInternParallelSpread(b, NewShardedInterner()) }
func BenchmarkSpread_Unique(b *testing.B)  { benchInternParallelSpread(b, NewUniqueInterner()) }
func BenchmarkSpread_VM(b *testing.B)      { benchInternParallelSpread(b, NewVMInterner()) }
func BenchmarkSpread_NoiseFloor(b *testing.B) {
	benchInternParallelSpread(b, NewMutexInternerNoise())
}

// 噪声地板（METHODOLOGY 原则 6）：函数体和 MutexInterner.Intern 逐字相同
func BenchmarkNoiseFloor(b *testing.B)     { benchIntern(b, NewMutexInternerNoise()) }
func BenchmarkPar_NoiseFloor(b *testing.B) { benchInternParallel(b, NewMutexInternerNoise()) }

// ---------------------------------------------------------------------------
// 下半场 H7：unique 的自动回收
// ---------------------------------------------------------------------------

const cleanupN = 100_000

func TestH7_UniqueCleanup(t *testing.T) {
	t.Logf("预测：丢弃引用后 unique 的堆回落，sync.Map 不回落；且 unique 应在 5 轮 GC 内完成")

	run := func(name string, intern func(string)) {
		base := liveHeap()
		for i := range cleanupN {
			intern("unique-string-payload-" + strconv.Itoa(i))
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		peak := ms.HeapAlloc

		t.Logf("%s: 驻留 %d 个唯一串后 HeapAlloc=%d KB（基线 %d KB）",
			name, cleanupN, peak/1024, base/1024)
		for round := 1; round <= 5; round++ {
			runtime.GC()
			runtime.ReadMemStats(&ms)
			t.Logf("  第 %d 轮 GC 后：%d KB（相对基线 %+d KB）",
				round, ms.HeapAlloc/1024, (int64(ms.HeapAlloc)-int64(base))/1024)
		}
	}

	run("unique", func(s string) { unique.Make(s) })

	var keep sync.Map
	run("sync.Map", func(s string) { keep.LoadOrStore(s, s) })
	runtime.KeepAlive(&keep)
}

// ---------------------------------------------------------------------------
// 下半场 H8：驻留是否降低 GC 标记开销
// ---------------------------------------------------------------------------

func TestH8_GCCost(t *testing.T) {
	t.Logf("预测：驻留版本的 runtime.GC() 耗时至少降低 30%%")
	t.Logf("反面理由：字符串数据是 noscan 的，GC 不扫内容只扫 header 指针，header 数量没变")

	const n = 200_000
	bases := makeBases(1, 64)

	timeGC := func(label string, held []string) time.Duration {
		runtime.GC()
		runtime.GC()
		start := time.Now()
		for range 10 {
			runtime.GC()
		}
		d := time.Since(start) / 10
		runtime.KeepAlive(held)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		t.Logf("%-10s 单轮 GC %v   HeapAlloc=%d KB   存活对象=%d",
			label, d, ms.HeapAlloc/1024, ms.HeapObjects)
		return d
	}

	plain := buildPlain(n, bases)
	dPlain := timeGC("不驻留", plain)
	plain = nil

	in := NewMutexInterner()
	interned := buildInterned(n, bases, in)
	dInterned := timeGC("驻留", interned)
	runtime.KeepAlive(in)
	interned = nil

	ratio := float64(dPlain) / float64(dInterned)
	t.Logf("耗时比 %.2f×（预测 ≥1.43×，即至少降 30%%）", ratio)
}

// ---------------------------------------------------------------------------
// H9：迁移的摊还成本
//
// 每个 op = 往一张全新的空表里插入 n 个互不相同的 key（全部未命中）。
// benchstat 报的是每 op 耗时，除以 n 才是每 key 的平均耗时。
// ---------------------------------------------------------------------------

var insertSizes = []int{1000, 10000, 100000}

func BenchmarkInsert_VM(b *testing.B) {
	for _, n := range insertSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			keys := makeBases(n, 48)
			b.ReportAllocs()
			for b.Loop() {
				in := NewVMInterner()
				for _, k := range keys {
					in.Intern(k)
				}
			}
		})
	}
}

func BenchmarkInsert_Mutex(b *testing.B) {
	for _, n := range insertSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			keys := makeBases(n, 48)
			b.ReportAllocs()
			for b.Loop() {
				in := NewMutexInterner()
				for _, k := range keys {
					in.Intern(k)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// H10：命中率变化时 VM 和 unique 的排名会不会翻转
//
// 每个 op = 处理 batchSize 个 key，其中 distinct 个互不相同。
// VM 每批用一张新表（对应真实场景里表被 TTL 清空后重建）；
// unique 是全局的，没法重置 —— 这个不对称在 README 里说明。
// ---------------------------------------------------------------------------

func BenchmarkCross_VM(b *testing.B) {
	for _, hr := range hitRates {
		b.Run(hr.name, func(b *testing.B) {
			raw := makeBatch(hr.distinct)
			b.ReportAllocs()
			for b.Loop() {
				in := NewVMInterner()
				for _, src := range raw {
					in.Intern(string(src))
				}
			}
		})
	}
}

func BenchmarkCross_Unique(b *testing.B) {
	for _, hr := range hitRates {
		b.Run(hr.name, func(b *testing.B) {
			raw := makeBatch(hr.distinct)
			in := NewUniqueInterner()
			b.ReportAllocs()
			for b.Loop() {
				for _, src := range raw {
					in.Intern(string(src))
				}
			}
		})
	}
}

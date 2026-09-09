package rcucache

import (
	"fmt"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// ===========================================================================
// 事前写定的假说与预测 —— 本段在跑任何基准之前定稿，之后不修改。
// 被推翻的条目原样保留，结果写进 README 的「坑」和附录 B。
//
// H1（置信度：中高）RWMutex.RLock 的代价是 cache line 争用，不是锁本身：
//    它要原子递增一个共享计数器，那条 cache line 在所有读者的核之间来回弹。
//    预测：并发度从 1 涨到 10，RWMutex 读耗时上升 ≥10 倍，RCU 读上升 <2 倍。
//    若 RWMutex 也保持平坦，或 RCU 也明显恶化，这条就错了。
//
// H2（置信度：高）纯 RCU（每次写都复制整张表）的写入是 O(N²)。
//    预测：插入 N 个 key 的总耗时呈二次增长 —— N 从 1e3 涨到 1e4（10 倍），
//    总耗时上升约 100 倍。若呈线性，这条就错了。
//
// H3（置信度：中高）`writes > len(snapshot)` 这个自适应阈值是必要的。
//    换成固定阈值 K：表有 N 个条目时插入 N 个 key 要合并 N/K 次、每次 O(N)，
//    总成本 O(N²/K) —— 仍是二次，只是常数小了。
//    预测：K=1000 时，N=1e5 的插入总耗时比自适应版慢 ≥3 倍，且随 N 增大继续拉开。
//    若两者相当，说明合并不是瓶颈、阈值怎么选无所谓，这条就错了。
//
// H4（置信度：中）合并瞬间新旧两份快照并存，有内存尖峰。
//    预测：表有 N 个条目时，合并后立刻读 HeapAlloc（不 GC）相对稳态出现 ≥1.8× 的尖峰。
//    若尖峰 <1.5×，这条就错了。
//
// H5（置信度：低 —— 这条最没底）GC 替 RCU 做 grace period。
//    预测：旧快照在下一轮 GC 就被回收；若人为持有读者引用则不回收。
//    若需要多轮 GC 才回收，或持有引用时也被回收，这条就错了。
//
// H6（置信度：中）写多时这个模式会输 —— 合并要拿全局锁且是 O(N)。
//    预测：存在一个写比例阈值，超过它 RCU+攒批输给 Go 1.24 的 sync.Map
//    和分片 map；我猜阈值在 5% 写附近。
//    若任何写比例下 RCU 都赢、或都输，这条就错了。
// ===========================================================================

func keys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "route:/api/v1/resource/" + strconv.Itoa(i)
	}
	return out
}

func fill(c Cache, ks []string) Cache {
	for i, k := range ks {
		c.Put(k, i)
	}
	return c
}

// ---------------------------------------------------------------------------
// H1：读侧代价随并发度怎么变
//
// 用 go test -cpu=1,2,4,10 跑，基准名后缀就是并发度。
// 表里有 1024 个 key，全部命中，只读不写。
// ---------------------------------------------------------------------------

const readKeys = 1024

func benchRead(b *testing.B, c Cache) {
	ks := keys(readKeys)
	fill(c, ks)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get(ks[i&(readKeys-1)])
			i++
		}
	})
}

func BenchmarkRead_RWMutex(b *testing.B)  { benchRead(b, NewRWMutexCache()) }
func BenchmarkRead_RCUPure(b *testing.B)  { benchRead(b, NewRCUPureCache()) }
func BenchmarkRead_RCUBatch(b *testing.B) { benchRead(b, NewRCUBatchedCache()) }
func BenchmarkRead_Sharded(b *testing.B)  { benchRead(b, NewShardedCache()) }
func BenchmarkRead_SyncMap(b *testing.B)  { benchRead(b, NewSyncMapCache()) }

// 噪声地板（METHODOLOGY 原则 6）：Get 函数体和 RWMutexCache.Get 逐字相同
func BenchmarkRead_NoiseFloor(b *testing.B) { benchRead(b, NewNoiseCache()) }

// ---------------------------------------------------------------------------
// H2 / H3：写入路径 —— 纯 RCU、固定阈值、自适应阈值
//
// 每个 op = 往一张全新的空表里插入 n 个互不相同的 key。
// benchstat 报每 op 耗时，除以 n 才是每 key 的平均。
// ---------------------------------------------------------------------------

// 纯 RCU 是 O(N²)，n 只能取小值，否则跑不完。
var pureSizes = []int{1000, 3000, 10000}

// 攒批方案是 O(N)，可以取到 1e5。
var batchSizes = []int{10000, 100000}

func BenchmarkInsert_RCUPure(b *testing.B) {
	for _, n := range pureSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			ks := keys(n)
			b.ReportAllocs()
			for b.Loop() {
				fill(NewRCUPureCache(), ks)
			}
		})
	}
}

func BenchmarkInsert_RCUBatch(b *testing.B) {
	for _, n := range batchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			ks := keys(n)
			b.ReportAllocs()
			for b.Loop() {
				fill(NewRCUBatchedCache(), ks)
			}
		})
	}
}

func BenchmarkInsert_RCUFixed(b *testing.B) {
	for _, n := range batchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			ks := keys(n)
			b.ReportAllocs()
			for b.Loop() {
				fill(NewRCUFixedCache(), ks)
			}
		})
	}
}

func BenchmarkInsert_RWMutex(b *testing.B) {
	for _, n := range batchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			ks := keys(n)
			b.ReportAllocs()
			for b.Loop() {
				fill(NewRWMutexCache(), ks)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// H4：合并的内存尖峰
// ---------------------------------------------------------------------------

func heapAllocNow() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

func TestH4_MergeSpike(t *testing.T) {
	t.Logf("预测：合并后立刻读 HeapAlloc（不 GC），相对稳态出现 ≥1.8× 的尖峰")

	for _, n := range []int{10_000, 100_000} {
		// 先单独量出 key 字符串 + 切片占多少，用来把 map 自身的占用分离出来。
		ks := keys(n)
		runtime.GC()
		runtime.GC()
		keysOnly := heapAllocNow()

		c := NewRCUBatchedCache()
		fill(c, ks)
		c.ForceMerge()

		runtime.GC()
		runtime.GC()
		steady := heapAllocNow()

		c.ForceMerge() // 复制整张表；旧快照立刻变垃圾但还没被回收
		peak := heapAllocNow()

		runtime.GC()
		after := heapAllocNow()
		runtime.KeepAlive(c)

		mapOnly := steady - keysOnly
		spike := peak - steady
		t.Logf("n=%-7d 稳态 %5d KB → 合并后 %5d KB（总堆 %.2f×）→ GC 后 %5d KB",
			n, steady/1024, peak/1024, float64(peak)/float64(steady), after/1024)
		t.Logf("          其中 key 字符串+切片 %d KB，map 自身 %d KB，合并多出来 %d KB（map 的 %.2f×）",
			keysOnly/1024, mapOnly/1024, spike/1024, float64(spike)/float64(mapOnly))
		runtime.KeepAlive(ks)
	}
}

// ---------------------------------------------------------------------------
// H5：GC 替 RCU 做 grace period
// ---------------------------------------------------------------------------

func TestH5_GCGracePeriod(t *testing.T) {
	t.Logf("预测：旧快照下一轮 GC 就被回收；人为持有读者引用则不回收")

	// 场景一：没人持有旧快照
	{
		c := NewRCUBatchedCache()
		fill(c, keys(1000))
		c.ForceMerge()

		collected := make(chan struct{})
		runtime.SetFinalizer(c.Snapshot(), func(*map[string]int) { close(collected) })

		c.ForceMerge() // 发布新快照，旧的不再被引用
		runtime.GC()
		runtime.GC()
		time.Sleep(20 * time.Millisecond) // 终结器是异步跑的

		select {
		case <-collected:
			t.Log("场景一（无读者）：旧快照已回收 ✅")
		default:
			t.Log("场景一（无读者）：旧快照未回收 ❌ —— H5 前半被推翻")
		}
		runtime.KeepAlive(c)
	}

	// 场景二：有读者持有旧快照
	{
		c := NewRCUBatchedCache()
		fill(c, keys(1000))
		c.ForceMerge()

		reader := c.Snapshot() // 模拟一个还在读的 goroutine 手里的引用
		collected := make(chan struct{})
		runtime.SetFinalizer(reader, func(*map[string]int) { close(collected) })

		c.ForceMerge()
		runtime.GC()
		runtime.GC()
		time.Sleep(20 * time.Millisecond)

		select {
		case <-collected:
			t.Log("场景二（有读者）：旧快照被回收 ❌ —— H5 后半被推翻")
		default:
			t.Log("场景二（有读者）：旧快照未回收 ✅ —— 读者引用确实起到了 grace period 的作用")
		}
		runtime.KeepAlive(reader)
		runtime.KeepAlive(c)
	}
}

// ---------------------------------------------------------------------------
// H6：读写混合，找写比例的交叉点
//
// 表里预填 1024 个 key。every=0 表示纯读；every=N 表示每 N 次操作有 1 次写。
// 写的是已存在的 key，所以表不增长 —— 测的是稳态行为。
// ---------------------------------------------------------------------------

var writeRatios = []struct {
	name  string
	every int
}{
	{"w=0%", 0},
	{"w=1%", 100},
	{"w=5%", 20},
	{"w=20%", 5},
	{"w=50%", 2},
}

func benchMixed(b *testing.B, mk func() Cache) {
	for _, wr := range writeRatios {
		b.Run(wr.name, func(b *testing.B) {
			ks := keys(readKeys)
			c := fill(mk(), ks)
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					k := ks[i&(readKeys-1)]
					if wr.every > 0 && i%wr.every == 0 {
						c.Put(k, i)
					} else {
						c.Get(k)
					}
					i++
				}
			})
		})
	}
}

func BenchmarkMixed_RCUBatch(b *testing.B) {
	benchMixed(b, func() Cache { return NewRCUBatchedCache() })
}
func BenchmarkMixed_Sharded(b *testing.B) { benchMixed(b, func() Cache { return NewShardedCache() }) }
func BenchmarkMixed_SyncMap(b *testing.B) { benchMixed(b, func() Cache { return NewSyncMapCache() }) }
func BenchmarkMixed_RWMutex(b *testing.B) { benchMixed(b, func() Cache { return NewRWMutexCache() }) }

// 噪声地板：NoiseCache 的 Get/Put 函数体和 RWMutexCache 逐字相同
func BenchmarkMixed_NoiseFloor(b *testing.B) { benchMixed(b, func() Cache { return NewNoiseCache() }) }

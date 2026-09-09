// Package rcucache 研究"读多写少的并发缓存"这个模式：
// 读侧用不可变快照 + 原子发布（RCU），写侧攒批合并（LSM 式的 memtable → immutable）。
//
// 这个组合出现在很多地方 —— 路由表、配置缓存、feature flag、指标 label 缓存，
// 以及 VictoriaMetrics 的字符串驻留表（见专题 04）。
// Go 老版本的 sync.Map 内部用的也是同一套：read（原子只读）+ dirty（有锁）+
// misses 计数，misses > len(dirty) 时提升。Go 1.24 把它换成了 HashTrieMap。
//
// 六种实现签名一致，唯一的变量是并发控制方式。
package rcucache

import (
	"hash/maphash"
	"maps"
	"sync"
	"sync/atomic"
)

// Cache 是六种实现的共同接口。读写分开，方便单独测读侧代价和混合读写。
type Cache interface {
	Get(k string) (int, bool)
	Put(k string, v int)
}

// ---------------------------------------------------------------------------
// 实现 1：map + RWMutex —— 基线
//
// 注意 RLock 并不是"免费的读"：它要原子递增一个共享计数器，
// 那条 cache line 会在所有读者的核之间来回弹。
// ---------------------------------------------------------------------------

type RWMutexCache struct {
	mu sync.RWMutex
	m  map[string]int
}

func NewRWMutexCache() *RWMutexCache { return &RWMutexCache{m: make(map[string]int)} }

//go:noinline
func (c *RWMutexCache) Get(k string) (int, bool) {
	c.mu.RLock()
	v, ok := c.m[k]
	c.mu.RUnlock()
	return v, ok
}

//go:noinline
func (c *RWMutexCache) Put(k string, v int) {
	c.mu.Lock()
	c.m[k] = v
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 实现 2：纯 RCU —— 每次写都复制整张表
//
// 读侧已经是最优形态了（一次原子加载 + 一次 map 查找，零写操作）。
// 但写侧每次 O(N) 拷贝，插入 N 个 key 就是 O(N²) —— 这正是需要攒批的原因。
// ---------------------------------------------------------------------------

type RCUPureCache struct {
	mu       sync.Mutex // 只保护写者之间
	snapshot atomic.Pointer[map[string]int]
}

func NewRCUPureCache() *RCUPureCache {
	c := &RCUPureCache{}
	m := make(map[string]int)
	c.snapshot.Store(&m)
	return c
}

//go:noinline
func (c *RCUPureCache) Get(k string) (int, bool) {
	v, ok := (*c.snapshot.Load())[k]
	return v, ok
}

//go:noinline
func (c *RCUPureCache) Put(k string, v int) {
	c.mu.Lock()
	old := *c.snapshot.Load()
	cp := make(map[string]int, len(old)+1)
	maps.Copy(cp, old)
	cp[k] = v
	c.snapshot.Store(&cp)
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 实现 3：RCU + 自适应阈值攒批 —— VictoriaMetrics / 老版 sync.Map 的方案
//
// 新 key 先进有锁的 pending，攒够 len(snapshot) 次才整体合并。
// 阈值随表增长而增长，于是 O(N) 的合并被摊薄成 O(1)。
// ---------------------------------------------------------------------------

type RCUBatchedCache struct {
	mu       sync.Mutex
	pending  map[string]int
	writes   uint64
	snapshot atomic.Pointer[map[string]int]
}

func NewRCUBatchedCache() *RCUBatchedCache {
	c := &RCUBatchedCache{pending: make(map[string]int)}
	m := make(map[string]int)
	c.snapshot.Store(&m)
	return c
}

//go:noinline
func (c *RCUBatchedCache) Get(k string) (int, bool) {
	if v, ok := (*c.snapshot.Load())[k]; ok {
		return v, true // 快路径：零锁零写
	}
	c.mu.Lock()
	v, ok := c.pending[k]
	c.mu.Unlock()
	return v, ok
}

//go:noinline
func (c *RCUBatchedCache) Put(k string, v int) {
	c.mu.Lock()
	c.pending[k] = v
	c.writes++
	// 自适应阈值：表越大，越少合并。这一行是整个设计的支点。
	if c.writes > uint64(len(*c.snapshot.Load())) {
		c.mergeLocked()
		c.writes = 0
	}
	c.mu.Unlock()
}

func (c *RCUBatchedCache) mergeLocked() {
	old := *c.snapshot.Load()
	cp := make(map[string]int, len(old)+len(c.pending))
	maps.Copy(cp, old)
	maps.Copy(cp, c.pending)
	c.pending = make(map[string]int)
	c.snapshot.Store(&cp)
}

// ForceMerge 供测试用，真实实现里不需要。
func (c *RCUBatchedCache) ForceMerge() {
	c.mu.Lock()
	c.mergeLocked()
	c.writes = 0
	c.mu.Unlock()
}

// Snapshot 供测试观察当前快照指针。
func (c *RCUBatchedCache) Snapshot() *map[string]int { return c.snapshot.Load() }

// ---------------------------------------------------------------------------
// 实现 4：RCU + 固定阈值攒批 —— 用来检验"自适应"是不是必要的
//
// 每 fixedThreshold 次写合并一次。表有 N 个条目时，插入 N 个 key 要合并
// N/K 次、每次 O(N)，总成本 O(N²/K) —— 仍是二次，只是常数小了。
// ---------------------------------------------------------------------------

const fixedThreshold = 1000

type RCUFixedCache struct {
	mu       sync.Mutex
	pending  map[string]int
	writes   uint64
	snapshot atomic.Pointer[map[string]int]
}

func NewRCUFixedCache() *RCUFixedCache {
	c := &RCUFixedCache{pending: make(map[string]int)}
	m := make(map[string]int)
	c.snapshot.Store(&m)
	return c
}

//go:noinline
func (c *RCUFixedCache) Get(k string) (int, bool) {
	if v, ok := (*c.snapshot.Load())[k]; ok {
		return v, true
	}
	c.mu.Lock()
	v, ok := c.pending[k]
	c.mu.Unlock()
	return v, ok
}

//go:noinline
func (c *RCUFixedCache) Put(k string, v int) {
	c.mu.Lock()
	c.pending[k] = v
	c.writes++
	if c.writes > fixedThreshold { // ← 唯一的差异：阈值不随表增长
		old := *c.snapshot.Load()
		cp := make(map[string]int, len(old)+len(c.pending))
		maps.Copy(cp, old)
		maps.Copy(cp, c.pending)
		c.pending = make(map[string]int)
		c.snapshot.Store(&cp)
		c.writes = 0
	}
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 实现 5：分片 map —— 把锁拆细，但每片内部仍有竞争
// ---------------------------------------------------------------------------

const shardCount = 16

type ShardedCache struct {
	seed   maphash.Seed
	shards [shardCount]struct {
		mu sync.RWMutex
		m  map[string]int
		_  [40]byte // 填充到 cache line，避免相邻分片伪共享
	}
}

func NewShardedCache() *ShardedCache {
	c := &ShardedCache{seed: maphash.MakeSeed()}
	for i := range c.shards {
		c.shards[i].m = make(map[string]int)
	}
	return c
}

func (c *ShardedCache) shard(k string) int {
	return int(maphash.String(c.seed, k) % shardCount)
}

//go:noinline
func (c *ShardedCache) Get(k string) (int, bool) {
	sh := &c.shards[c.shard(k)]
	sh.mu.RLock()
	v, ok := sh.m[k]
	sh.mu.RUnlock()
	return v, ok
}

//go:noinline
func (c *ShardedCache) Put(k string, v int) {
	sh := &c.shards[c.shard(k)]
	sh.mu.Lock()
	sh.m[k] = v
	sh.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 实现 6：sync.Map —— Go 1.24 起是 HashTrieMap，不再是 read/dirty 双 map
// ---------------------------------------------------------------------------

type SyncMapCache struct{ m sync.Map }

func NewSyncMapCache() *SyncMapCache { return &SyncMapCache{} }

//go:noinline
func (c *SyncMapCache) Get(k string) (int, bool) {
	v, ok := c.m.Load(k)
	if !ok {
		return 0, false
	}
	return v.(int), true
}

//go:noinline
func (c *SyncMapCache) Put(k string, v int) { c.m.Store(k, v) }

// ---------------------------------------------------------------------------
// 噪声地板对照组（METHODOLOGY 原则 6）：Get/Put 函数体和 RWMutexCache 逐字相同
// ---------------------------------------------------------------------------

type NoiseCache struct {
	mu sync.RWMutex
	m  map[string]int
}

func NewNoiseCache() *NoiseCache { return &NoiseCache{m: make(map[string]int)} }

//go:noinline
func (c *NoiseCache) Get(k string) (int, bool) {
	c.mu.RLock()
	v, ok := c.m[k]
	c.mu.RUnlock()
	return v, ok
}

//go:noinline
func (c *NoiseCache) Put(k string, v int) {
	c.mu.Lock()
	c.m[k] = v
	c.mu.Unlock()
}

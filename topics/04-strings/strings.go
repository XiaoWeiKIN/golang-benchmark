// Package strs 研究两件相关的事：
//
//	上半场 —— string 和 []byte 之间的转换什么时候拷贝、什么时候不拷贝；
//	下半场 —— 字符串驻留（string interning）省下的到底是哪部分内存，代价在哪。
//
// 之所以放在一起，是因为它们共用同一个底层事实：一个 string 值 = 16 字节的头
// （数据指针 + 长度）+ 一段只读的数据。转换的开销在于要不要复制"数据"，
// 驻留的收益上限也取决于"数据"能被多少个头共享 —— 而头本身省不掉。
package strs

import (
	"hash/maphash"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unique"
	"unsafe"
)

// ---------------------------------------------------------------------------
// 上半场之一：默认的转换要拷贝
// ---------------------------------------------------------------------------

// BytesToString 把 []byte 转成 string。string 必须不可变，所以要复制一份数据。
//
//go:noinline
func BytesToString(b []byte) string { return string(b) }

// StringToBytes 把 string 转成 []byte。[]byte 可写，同样要复制。
//
//go:noinline
func StringToBytes(s string) []byte { return []byte(s) }

// ---------------------------------------------------------------------------
// 上半场之二：编译器的零拷贝特例
//
// 有几种写法里，转换出来的 string 生命周期不超过该表达式，编译器能证明
// 没人能观察到它，于是直接复用 []byte 的底层数组，不做拷贝。
// ---------------------------------------------------------------------------

// LookupTable 是被查的表，内容在 init 里填。
var LookupTable = map[string]int{}

// LookupDirect 直接用 string(b) 作下标 —— 编译器特例，不为查表分配。
//
//go:noinline
func LookupDirect(b []byte) int { return LookupTable[string(b)] }

// LookupViaVar 先把 string(b) 存进变量再查 —— 变量可能被别处观察到，只能真拷贝。
// 和 LookupDirect 只差一个中间变量。
//
//go:noinline
func LookupViaVar(b []byte) int {
	s := string(b)
	return LookupTable[s]
}

// CompareLiteral 和字面量比较，编译器特例。
//
//go:noinline
func CompareLiteral(b []byte) bool { return string(b) == "go1.16.4" }

// RangeString 对 string(b) 做 range，编译器特例。
//
//go:noinline
func RangeString(b []byte) int {
	n := 0
	for range string(b) {
		n++
	}
	return n
}

// AppendStringToBytes 把 string 追加进 []byte，编译器特例（不为 s 建副本）。
//
//go:noinline
func AppendStringToBytes(dst []byte, s string) []byte { return append(dst, s...) }

// LookupTableLong 的 key 超过 32 字节，用来追查 LookupViaVar 为什么没分配。
var LookupTableLong = map[string]int{}

// LookupViaVarLong 和 LookupViaVar 写法相同，只是 key 更长。
//
//go:noinline
func LookupViaVarLong(b []byte) int {
	s := string(b)
	return LookupTableLong[s]
}

// LookupDirectLong 长 key 的直接查表版本，作为对照。
//
//go:noinline
func LookupDirectLong(b []byte) int { return LookupTableLong[string(b)] }

// ---------------------------------------------------------------------------
// 上半场之三：unsafe 的零拷贝，以及它的代价
//
// unsafe.String 直接让 string 指向 []byte 的底层数组，零拷贝。
// 代价是这个 string 不再是不可变的 —— 改动原 []byte，string 的内容跟着变。
// ---------------------------------------------------------------------------

// UnsafeString 零拷贝转换。调用方必须保证之后不再修改 b。
//
//go:noinline
func UnsafeString(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// UnsafeBytes 零拷贝反向转换。返回的 []byte 绝对不能写 —— string 数据可能在只读段。
//
//go:noinline
func UnsafeBytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// ---------------------------------------------------------------------------
// 下半场：四种驻留实现
//
// 签名统一为 Intern(string) string，唯一的变量是底层容器。
// ---------------------------------------------------------------------------

// Interner 是四种实现的共同接口，只在测试里用来遍历，不进热路径。
type Interner interface {
	Intern(s string) string
	Len() int
}

// --- 实现 1：map + RWMutex（最朴素的并发版本）---

type MutexInterner struct {
	mu sync.RWMutex
	m  map[string]string
}

func NewMutexInterner() *MutexInterner {
	return &MutexInterner{m: make(map[string]string)}
}

//go:noinline
func (i *MutexInterner) Intern(s string) string {
	i.mu.RLock()
	v, ok := i.m[s]
	i.mu.RUnlock()
	if ok {
		return v
	}
	i.mu.Lock()
	if v, ok := i.m[s]; ok { // 双检，避免并发下重复写
		i.mu.Unlock()
		return v
	}
	i.m[s] = s
	i.mu.Unlock()
	return s
}

func (i *MutexInterner) Len() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.m)
}

// --- 实现 2：sync.Map（社区最常见的并发驻留写法）---

type SyncMapInterner struct {
	m sync.Map
}

func NewSyncMapInterner() *SyncMapInterner { return &SyncMapInterner{} }

//go:noinline
func (i *SyncMapInterner) Intern(s string) string {
	interned, _ := i.m.LoadOrStore(s, s)
	return interned.(string)
}

func (i *SyncMapInterner) Len() int {
	n := 0
	i.m.Range(func(_, _ any) bool { n++; return true })
	return n
}

// --- 实现 3：分片 map（单锁和 sync.Map 之间的中间选项）---

const shardCount = 16

type ShardedInterner struct {
	seed   maphash.Seed
	shards [shardCount]struct {
		mu sync.RWMutex
		m  map[string]string
		_  [40]byte // 填充到 cache line，避免相邻分片伪共享
	}
}

func NewShardedInterner() *ShardedInterner {
	i := &ShardedInterner{seed: maphash.MakeSeed()}
	for k := range i.shards {
		i.shards[k].m = make(map[string]string)
	}
	return i
}

//go:noinline
func (i *ShardedInterner) Intern(s string) string {
	sh := &i.shards[maphash.String(i.seed, s)%shardCount]
	sh.mu.RLock()
	v, ok := sh.m[s]
	sh.mu.RUnlock()
	if ok {
		return v
	}
	sh.mu.Lock()
	if v, ok := sh.m[s]; ok {
		sh.mu.Unlock()
		return v
	}
	sh.m[s] = s
	sh.mu.Unlock()
	return s
}

func (i *ShardedInterner) Len() int {
	n := 0
	for k := range i.shards {
		i.shards[k].mu.RLock()
		n += len(i.shards[k].m)
		i.shards[k].mu.RUnlock()
	}
	return n
}

// --- 实现 5：VictoriaMetrics 的双 map 方案 ---
//
// 移植自 VictoriaMetrics lib/bytesutil/internstring.go，去掉了 flag 和内部
// fasttime/timeutil 依赖，核心结构原样保留；后台清理改成手动调用 Cleanup，
// 真实实现里是一个带 jitter 的 ticker goroutine。
//
// 结构可以拆成两半看：
//
// 读侧是 RCU（Read-Copy-Update）：readonly 一旦发布就永不修改，更新者构造新的
// 一份、原子换指针，旧读者手里那份继续有效。传统 RCU 要显式等待所有读者退出
// 临界区才能回收旧版本，这里由 GC 代劳 —— 读者解引用得到的 map 值还在栈上，
// 旧 map 就不会被回收。
// 快路径因此只有一次原子加载 + 一次 map 查找，多核并行读不产生任何写共享，
// 没有 cache line 弹跳 —— 这是它比 RWMutex 和分片方案快的根本原因。
//
// 写侧是攒批合并：新 key 先进有锁的 mutable，攒够 len(readonly) 次慢路径才整体
// 合并进 readonly。这一步是整个设计的关键 —— 若按字面的 copy-on-write 每写一次
// 就复制一次，插入 N 个 key 就是 O(N²)；攒批把 O(N) 的拷贝摊薄成 O(1)。
// 这个结构更接近 LSM 的 memtable → immutable table。

type VMInterner struct {
	mutableLock  sync.Mutex
	mutable      map[string]string
	mutableReads uint64

	readonly atomic.Pointer[map[string]vmEntry]

	expire time.Duration
	maxLen int
}

type vmEntry struct {
	deadline int64
	s        string
}

func NewVMInterner() *VMInterner {
	m := &VMInterner{
		mutable: make(map[string]string),
		expire:  6 * time.Minute,
		maxLen:  500, // 超长串跳过缓存：它们通常唯一性强，存进去纯占地方
	}
	ro := make(map[string]vmEntry)
	m.readonly.Store(&ro)
	return m
}

func (m *VMInterner) getReadonly() map[string]vmEntry { return *m.readonly.Load() }

//go:noinline
func (m *VMInterner) Intern(s string) string {
	if len(s) > m.maxLen {
		return strings.Clone(s)
	}

	readonly := m.getReadonly()
	if e, ok := readonly[s]; ok {
		return e.s // 快路径：一次 atomic.Load + 一次 map 查找，无锁无分配
	}

	m.mutableLock.Lock()
	sInterned, ok := m.mutable[s]
	if !ok {
		// 复查：并发的 goroutine 可能已经迁移过了
		readonly = m.getReadonly()
		e, ok2 := readonly[s]
		if !ok2 {
			// strings.Clone 不是可有可无的：s 可能是从一个大缓冲区里切出来的，
			// 直接存进表会把整个缓冲区永久钉在内存里 —— 驻留表反而成了泄漏源。
			// 这和专题 02 里 slices.Clip 那个坑是同一类问题。
			sInterned = strings.Clone(s)
			m.mutable[sInterned] = sInterned
		} else {
			sInterned = e.s
		}
	}
	m.mutableReads++
	if m.mutableReads > uint64(len(readonly)) {
		// 阈值触发，不是写触发：readonly 有 N 个条目时要攒够 N 次慢路径才合并一次，
		// 于是 O(N) 的全量拷贝摊还成 O(1)。
		m.migrateLocked()
		m.mutableReads = 0
	}
	m.mutableLock.Unlock()
	return sInterned
}

func (m *VMInterner) migrateLocked() {
	readonly := m.getReadonly()
	cp := make(map[string]vmEntry, len(readonly)+len(m.mutable))
	maps.Copy(cp, readonly)
	deadline := time.Now().Unix() + int64(m.expire.Seconds())
	for k, s := range m.mutable {
		cp[k] = vmEntry{s: s, deadline: deadline}
	}
	m.mutable = make(map[string]string)
	m.readonly.Store(&cp)
}

// Cleanup 清掉过期条目。真实实现里由后台 ticker 每 expire/2 调一次。
// 注意它先只读遍历判断有没有过期的，有才重建 —— 避免无谓的全量拷贝。
func (m *VMInterner) Cleanup() {
	readonly := m.getReadonly()
	now := time.Now().Unix()
	need := false
	for _, e := range readonly {
		if e.deadline <= now {
			need = true
			break
		}
	}
	if !need {
		return
	}
	cp := make(map[string]vmEntry, len(readonly))
	for k, e := range readonly {
		if e.deadline > now {
			cp[k] = e
		}
	}
	m.readonly.Store(&cp)
}

func (m *VMInterner) Len() int { return len(m.getReadonly()) }

// --- 实现 4：标准库 unique（Go 1.23+，弱指针 + GC 联动自动回收）---

type UniqueInterner struct{}

func NewUniqueInterner() UniqueInterner { return UniqueInterner{} }

//go:noinline
func (UniqueInterner) Intern(s string) string { return unique.Make(s).Value() }

// Len 无法查询 —— unique 不暴露大小。返回 -1 表示不适用。
func (UniqueInterner) Len() int { return -1 }

// --- 噪声地板对照组（METHODOLOGY 原则 6）---

// MutexInternerNoise 的 Intern 函数体和 MutexInterner.Intern 逐字相同。
type MutexInternerNoise struct {
	mu sync.RWMutex
	m  map[string]string
}

func NewMutexInternerNoise() *MutexInternerNoise {
	return &MutexInternerNoise{m: make(map[string]string)}
}

//go:noinline
func (i *MutexInternerNoise) Intern(s string) string {
	i.mu.RLock()
	v, ok := i.m[s]
	i.mu.RUnlock()
	if ok {
		return v
	}
	i.mu.Lock()
	if v, ok := i.m[s]; ok {
		i.mu.Unlock()
		return v
	}
	i.m[s] = s
	i.mu.Unlock()
	return s
}

func (i *MutexInternerNoise) Len() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.m)
}

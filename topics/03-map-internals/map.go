// Package mapinternals 研究 Go 1.24 起换用的 Swiss Table map 实现。
//
// 术语（和 internal/runtime/maps 保持一致）：
//   - slot  一个 key/elem 存储位
//   - group 8 个 slot + 一个 8 字节控制字，控制字里每个字节存该槽的状态和哈希低 7 位（H2）
//   - table 一张完整的 Swiss 表，容量上限 maxTableCapacity = 1024 个槽
//   - map   顶层结构，持有一个 table 目录（directory），用哈希高位选表（可扩展哈希）
//
// 从源码抄下来的四个常量，本专题反复用到：
//
//	abi.SwissMapGroupSlots   = 8     internal/abi/map_swiss.go:18
//	maxAvgGroupLoad          = 7     internal/runtime/maps/group.go:20   （载荷 7/8）
//	maxTableCapacity         = 1024  internal/runtime/maps/table.go:20
//	SwissMapMaxKeyBytes/Elem = 128   internal/abi/map_swiss.go:22        （超过就改存指针）
//
// 所有被测函数都带 //go:noinline：map 操作本身是 runtime 调用删不掉，
// 但内联之后逃逸分析的结论会变（见专题 01），而本专题第一个场景测的正是逃逸。
package mapinternals

import (
	"math/bits"
	"math/rand/v2"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// 场景 1：小 map 的特权
//
// hint <= 8 且 map 不逃逸时，编译器把 Map 头和那唯一一个 group 都放在栈上
//（cmd/compile/internal/walk/builtin.go:walkMakeSwissMap 里两次 stackTempAddr）。
// hint 变成常量 9 就走不了这条路：makemap 会在堆上开 groups 数组。
//
// 三个函数只差一处：make 的第二个参数，和 map 是否逃逸。
// ---------------------------------------------------------------------------

// SinkStrMap 用来把 map 强制送上堆，作为 SmallMapHint8 的逃逸对照。
var SinkStrMap map[string]int

//go:noinline
func SmallMapNoHint(ks []string) int {
	m := make(map[string]int)
	for i, k := range ks {
		m[k] = i
	}
	n := 0
	for _, k := range ks {
		n += m[k]
	}
	return n
}

//go:noinline
func SmallMapHint8(ks []string) int {
	m := make(map[string]int, 8)
	for i, k := range ks {
		m[k] = i
	}
	n := 0
	for _, k := range ks {
		n += m[k]
	}
	return n
}

//go:noinline
func SmallMapHint9(ks []string) int {
	m := make(map[string]int, 9)
	for i, k := range ks {
		m[k] = i
	}
	n := 0
	for _, k := range ks {
		n += m[k]
	}
	return n
}

// SmallMapEscape 和 SmallMapHint8 逐字相同，只多了最后一行赋值给包级变量。
// 唯一的变量是「逃逸与否」。
//
//go:noinline
func SmallMapEscape(ks []string) int {
	m := make(map[string]int, 8)
	for i, k := range ks {
		m[k] = i
	}
	n := 0
	for _, k := range ks {
		n += m[k]
	}
	SinkStrMap = m
	return n
}

// ---------------------------------------------------------------------------
// 场景 2：make(map, hint) 里的 hint 到底变成多少容量
//
// NewMap 的换算是三步，每步都会向上取整，所以 hint → 容量不是线性的：
//
//	target  = hint * 8 / 7                       载荷 7/8 的反函数
//	dirSize = alignUpPow2(ceil(target / 1024))   一张表最多 1024 槽，超了就切多张
//	每表容量 = alignUpPow2(target / dirSize)      表的槽数必须是 2 的幂（探测序列要求）
//
// PredictShape 把这段算法照抄一遍，用来和实测内存对账（第三条证据链）。
// ---------------------------------------------------------------------------

const (
	groupSlots       = 8
	maxAvgGroupLoad  = 7
	maxTableCapacity = 1024
)

// Shape 是 make(map, hint) 预期得到的结构。
type Shape struct {
	DirSize  int // 目录里有几张表
	TableCap int // 每张表的槽数
	Slots    int // 总槽数 = DirSize * TableCap
	Capacity int // 扩容前最多能装多少条
	Bytes    int // groups 数组占的字节数（不含 table 结构体和目录切片）
}

// alignUpPow2 与 internal/runtime/maps/group.go:267 同构。
func alignUpPow2(n int) int {
	if n == 0 {
		return 0
	}
	return 1 << bits.Len(uint(n-1))
}

// PredictShape 推算 make(map[K]V, hint) 的结构。groupBytes 是单个 group 的字节数：
// 8 字节控制字 + 8 个槽，例如 map[int64]int64 是 8 + 8*(8+8) = 136。
func PredictShape(hint, groupBytes int) Shape {
	if hint <= groupSlots {
		// 小 map：没有目录，就一个 group，8 个槽能填满。
		return Shape{DirSize: 0, TableCap: groupSlots, Slots: groupSlots, Capacity: groupSlots, Bytes: groupBytes}
	}
	target := hint * groupSlots / maxAvgGroupLoad
	dirSize := alignUpPow2((target + maxTableCapacity - 1) / maxTableCapacity)
	tableCap := target / dirSize
	if tableCap < groupSlots {
		tableCap = groupSlots
	}
	tableCap = alignUpPow2(tableCap)

	perTable := tableCap * maxAvgGroupLoad / groupSlots
	if tableCap <= groupSlots {
		perTable = tableCap - 1 // 单 group 的表要留一个空槽终止探测
	}
	return Shape{
		DirSize:  dirSize,
		TableCap: tableCap,
		Slots:    dirSize * tableCap,
		Capacity: dirSize * perTable,
		Bytes:    dirSize * (tableCap / groupSlots) * groupBytes,
	}
}

// BuildIntMap 用给定的 hint 建表。hint < 0 表示不给 hint。
// 返回 map 让它逃逸到堆上 —— 这里量的是建表的分配总量，不是逃逸。
//
//go:noinline
func BuildIntMap(keys []int64, hint int) map[int64]int64 {
	var m map[int64]int64
	if hint < 0 {
		m = make(map[int64]int64)
	} else {
		m = make(map[int64]int64, hint)
	}
	for _, k := range keys {
		m[k] = k
	}
	return m
}

// IntKeys 生成 0..n-1 这 n 个 key。
//
// 注意：这是基准里最顺手的写法，也是一个陷阱。Go 的 memhash64 在 arm64 上用 AES 指令，
// 对「低位取遍所有值的一整块连续整数」，哈希高位的分布是**恰好均衡**的 ——
// 0..n-1（n 为 256 的倍数）分成 2 份、4 份都不差一个。真实世界的 key 没这个待遇。
// 目录选表用的正是哈希最高的若干位，所以顺序 key 会让预分配看起来比实际好。
// 凡是结论依赖哈希分布的实验，都要拿 RandKeys 再跑一遍。见 README 的坑二。
func IntKeys(n int) []int64 {
	ks := make([]int64, n)
	for i := range ks {
		ks[i] = int64(i)
	}
	return ks
}

// RandKeys 生成 n 个互不相同的伪随机 int64 key。
// 用固定种子的 PCG，保证可复现（METHODOLOGY 原则 5）。
func RandKeys(n int) []int64 {
	r := rand.New(rand.NewPCG(0x9E3779B97F4A7C15, 0xBF58476D1CE4E5B9))
	seen := make(map[int64]struct{}, n)
	ks := make([]int64, 0, n)
	for len(ks) < n {
		v := int64(r.Uint64())
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		ks = append(ks, v)
	}
	return ks
}

// ---------------------------------------------------------------------------
// 场景 3：128 字节的分水岭
//
// cmd/compile/internal/reflectdata/map_swiss.go:42,45 是严格大于：
// key 或 elem 的大小超过 128 字节，槽里存的就不是数据本身而是一个指针，
// 每插入一条新 key 都要额外 newobject 一次。
// ---------------------------------------------------------------------------

type (
	Elem128 [128]byte
	Elem129 [129]byte
	Key128  [128]byte
	Key129  [129]byte
)

//go:noinline
func BuildElem128(keys []int64) map[int64]Elem128 {
	m := make(map[int64]Elem128)
	var v Elem128
	for _, k := range keys {
		m[k] = v
	}
	return m
}

//go:noinline
func BuildElem129(keys []int64) map[int64]Elem129 {
	m := make(map[int64]Elem129)
	var v Elem129
	for _, k := range keys {
		m[k] = v
	}
	return m
}

//go:noinline
func BuildKey128(keys []Key128) map[Key128]int {
	m := make(map[Key128]int)
	for i, k := range keys {
		m[k] = i
	}
	return m
}

//go:noinline
func BuildKey129(keys []Key129) map[Key129]int {
	m := make(map[Key129]int)
	for i, k := range keys {
		m[k] = i
	}
	return m
}

// BigKeys128/BigKeys129 生成互不相同的大 key（只有前 8 字节不同）。
func BigKeys128(n int) []Key128 {
	ks := make([]Key128, n)
	for i := range ks {
		putIndex(ks[i][:8], i)
	}
	return ks
}

func BigKeys129(n int) []Key129 {
	ks := make([]Key129, n)
	for i := range ks {
		putIndex(ks[i][:8], i)
	}
	return ks
}

func putIndex(b []byte, i int) {
	for j := 7; j >= 0; j-- {
		b[j] = byte(i)
		i >>= 8
	}
}

// ---------------------------------------------------------------------------
// 场景 4：长字符串 key 的小 map 会跳过哈希
//
// internal/runtime/maps/runtime_faststr_swiss.go:17 getWithoutKeySmallFastStr：
// 当 m.dirLen <= 0（map 从来没超过 8 条）且 len(key) > 64 时，
// 先用 longStringQuickEqualityTest 比长度 + 首 8 字节 + 尾 8 字节做快速排除；
// 只有恰好一个槽通过快速测试时才做一次完整比较，全程不算哈希。
//
// 若有两个槽都通过快速测试（key 的首尾都一样、只有中间不同），
// 代码 goto dohash 退回算哈希 —— 这条快路径就白搭了。
// ---------------------------------------------------------------------------

//go:noinline
func LookupStrMap(m map[string]int, k string) int {
	return m[k]
}

// DistinctKeys 生成 count 个长度为 length 的 key，索引编码在**前 8 字节**，
// 所以首 8 字节各不相同，快速测试能一次排除掉其他槽。
func DistinctKeys(count, length int) []string {
	if length < 16 {
		panic("length must be >= 16")
	}
	ks := make([]string, count)
	for i := range ks {
		var sb strings.Builder
		sb.Grow(length)
		sb.WriteString(pad8(i))
		sb.WriteString(strings.Repeat("x", length-8))
		ks[i] = sb.String()
	}
	return ks
}

// MiddleKeys 生成 count 个长度为 length 的 key，首 8 字节和尾 8 字节全部相同，
// 索引藏在中间 —— 快速测试无法区分，会退回算哈希。
func MiddleKeys(count, length int) []string {
	if length < 32 {
		panic("length must be >= 32")
	}
	ks := make([]string, count)
	for i := range ks {
		var sb strings.Builder
		sb.Grow(length)
		sb.WriteString("PREFIX__")
		sb.WriteString(pad8(i))
		sb.WriteString(strings.Repeat("x", length-24))
		sb.WriteString("__SUFFIX")
		ks[i] = sb.String()
	}
	return ks
}

func pad8(i int) string {
	s := strconv.Itoa(i)
	return strings.Repeat("0", 8-len(s)) + s
}

// StrMapOf 把 ks 装进一个不给 hint 的 map。
// 不给 hint 很关键：只有从没超过 8 条的 map 才有 dirLen == 0，才走得到小 map 快路径。
func StrMapOf(ks []string) map[string]int {
	m := make(map[string]int)
	for i, k := range ks {
		m[k] = i
	}
	return m
}

// ---------------------------------------------------------------------------
// 场景 5：删除不缩容 / 墓碑
//
// table.rehash（internal/runtime/maps/table.go:1118）只有两条出路：
// 容量翻倍，或者切成两张表。没有任何一条路径会把表变小。
// Map.Clear 也只是把槽清空、控制字置 empty，groups 数组原样留着。
//
// 删除时若所在 group 还有空槽，直接置 empty；若该 group 已满，
// 只能立墓碑（ctrlDeleted），否则探测序列会提前终止。
// 墓碑占着容量，Go 1.25 在 growthLeft 用尽时会先试 pruneTombstones 回收
//（table.go:508，能回收 >=10% 容量才动手），回收不动才扩容。
// ---------------------------------------------------------------------------

// BuildChurned 建一个稳定持有 n 条、但经历了 rounds 轮「删一条插一条」的 map。
// len(m) 全程等于 n，变的只有墓碑数量和底层表的大小。
// 返回值里的 keys 是最终留在表里的那批 key。
//
//go:noinline
func BuildChurned(n, rounds int) (map[int64]int64, []int64) {
	m := make(map[int64]int64)
	for i := 0; i < n; i++ {
		m[int64(i)] = int64(i)
	}
	next := int64(n)
	for r := 0; r < rounds; r++ {
		delete(m, next-int64(n))
		m[next] = next
		next++
	}
	keys := make([]int64, n)
	for i := range keys {
		keys[i] = next - int64(n) + int64(i)
	}
	return m, keys
}

// BuildFresh 用同一批 key 建一个全新的、没有任何墓碑的 map。
// 同样不给 hint，让它按自然扩容路径长到该有的大小。
//
//go:noinline
func BuildFresh(keys []int64) map[int64]int64 {
	m := make(map[int64]int64)
	for _, k := range keys {
		m[k] = k
	}
	return m
}

//go:noinline
func LookupIntMap(m map[int64]int64, k int64) int64 {
	return m[k]
}

// ---------------------------------------------------------------------------
// 场景 6：map 里有没有指针，决定 GC 要不要扫它
//
// group 的类型是 struct{ ctrl uint64; slots [8]struct{key K; elem V} }。
// K 和 V 都不含指针时，整个 groups 数组是 noscan，GC 标记阶段直接跳过；
// 只要有一个含指针，这 N 条就全都要扫。
//
// 三个构造函数把两个变量拆开：
//   - NoPtr      → groups 数组 noscan，堆对象数 = O(表数)
//   - SharedPtr  → groups 数组要扫（N 个指针），但只多 1 个堆对象
//   - DistinctPtr→ groups 数组要扫，且多 N 个堆对象
//
// NoPtr↔SharedPtr 隔离出「扫描指针」的成本，SharedPtr↔DistinctPtr 隔离出「对象数」的成本。
// ---------------------------------------------------------------------------

//go:noinline
func BuildNoPtr(n int) map[int64]int64 {
	m := make(map[int64]int64, n)
	for i := 0; i < n; i++ {
		m[int64(i)] = int64(i)
	}
	return m
}

//go:noinline
func BuildSharedPtr(n int) map[int64]*int64 {
	shared := new(int64)
	m := make(map[int64]*int64, n)
	for i := 0; i < n; i++ {
		m[int64(i)] = shared
	}
	return m
}

//go:noinline
func BuildDistinctPtr(n int) map[int64]*int64 {
	m := make(map[int64]*int64, n)
	for i := 0; i < n; i++ {
		v := int64(i)
		m[int64(i)] = &v
	}
	return m
}

// ---------------------------------------------------------------------------
// 噪声地板（METHODOLOGY 原则 6）
//
// 下面两个函数逐字相同。它们之间的差值就是本机在这组基准上的分辨率下限，
// 任何小于它的差异都不能拿来下结论。
// ---------------------------------------------------------------------------

//go:noinline
func NoiseFloorA(m map[int64]int64, k int64) int64 {
	return m[k]
}

//go:noinline
func NoiseFloorB(m map[int64]int64, k int64) int64 {
	return m[k]
}

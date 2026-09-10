# 专题 03：map 的实现与预分配

Go 1.24 把 map 换成了 Swiss Table。这不是一次内部重构就完事的改动 —— 它改变了
"`make(map[K]V, n)` 里的 `n` 意味着什么"、"小 map 要不要分配"、"删掉的条目什么时候还内存"
这些每天都要用到的东西。

先看一个能立刻感受到落差的例子。同样是往一张预分配好的 `map[int64]int64` 里插入 n 条：

```go
m := make(map[int64]int64, n)   // 我知道要装 n 条，就报 n
for _, k := range keys { m[k] = k }
```

| n | 建表分配 | 分配次数 |
|---:|---:|---:|
| 896 | 18.09 KiB | 4 |
| **897** | **36.66 KiB** | **8** |
| 898 | 36.12 KiB | 6 |

**多插一个 key，内存翻倍。** 而且 898 又降回去了。这条曲线不是单调的，
也不是连续的 —— 它是三次向上取整叠出来的台阶。本文讲清楚这些台阶从哪来，
以及另外六件 Swiss Table 之后需要重新学一遍的事。

> 环境：Apple M4（10 核）/ darwin/arm64 / Go 1.25.4。
> 每批数据都自带一对函数体逐字相同的基准当**噪声地板**：主批（`-count=8`，机器上还开着浏览器）
> 是 **9.8%**，纳秒级那批（`-count=15`，机器较闲）是 **1.2%**。
> 小于对应噪声地板的差异，本文一律不解释。详见[附录 B](#附录-b实验设计)。

---

## 目录

- [1. 背景：一张 Swiss 表长什么样](#1-背景一张-swiss-表长什么样)
- [2. 工具](#2-工具)
- [3. 场景一：8 条以内的 map，可能连堆都不上](#3-场景一8-条以内的-map可能连堆都不上)
- [4. 场景二：make(map, n) 里的 n 不是容量](#4-场景二makemap-n-里的-n-不是容量)
- [5. 场景三：128 字节的分水岭](#5-场景三128-字节的分水岭)
- [6. 场景四：小 map 的字符串快路径](#6-场景四小-map-的字符串快路径)
- [7. 场景五：map 只涨不落](#7-场景五map-只涨不落)
- [8. 场景六：删一条插一条，表会长到两倍](#8-场景六删一条插一条表会长到两倍)
- [9. 场景七：有没有指针，决定 GC 扫不扫这一百万条](#9-场景七有没有指针决定-gc-扫不扫这一百万条)
- [10. 场景八：和老实现的正面对比](#10-场景八和老实现的正面对比)
- [一页速查](#一页速查)
- [坑](#坑)
- [附录 A：完整数据与环境](#附录-a完整数据与环境)
- [附录 B：实验设计](#附录-b实验设计)
- [附录 C：延伸阅读](#附录-c延伸阅读)

---

## 1. 背景：一张 Swiss 表长什么样

四个名词，后面全篇都要用（术语和 `internal/runtime/maps` 保持一致）：

```
slot    一个 key/elem 存储位
group   8 个 slot + 一个 8 字节控制字
table   一张完整的 Swiss 表，最多 1024 个槽
map     顶层结构，持有一个 table 目录（directory）
```

**控制字是整个设计的核心。** 它有 8 个字节，一个字节管一个槽：

```
最高位 = 1  →  这个槽是空的（或者是墓碑）
最高位 = 0  →  这个槽有数据，剩下 7 位存该 key 哈希的低 7 位（H2）
```

查找时把要找的 key 的 H2 复制成 8 份，和整个控制字做一次位运算，
**一条指令就比完了 8 个槽**（`ctrlGroup.matchH2`，`group.go:125` 起）。7 位有约
1/128 的假阳性率，所以命中的槽还要再做一次真正的 key 比较 —— 但假阳性很少，
绝大多数情况下一个 group 只需要比一次。

哈希的高 57 位（H1）决定从哪个 group 开始探测，探测序列是二次的
（`p(i) = (i²+i)/2 + hash`，`table.go:1232`）。**探测在遇到"有空槽的 group"时终止** ——
记住这条，场景五和场景六都是它的直接后果。

再往上两层：

- 一张 table 最多 **1024 个槽**（`maxTableCapacity`），载荷上限 **7/8**，也就是最多装 896 条；
- 超过一张表，map 就用**可扩展哈希**：哈希的最高若干位当目录下标选表，
  某张表满了只切它自己，不用重排整个 map。这是 Swiss Table 在 Go 里最重要的改造 ——
  原版 Abseil 的表满了要整体重排，Go 需要把这个 O(N) 摊开。

最后一个特例：**8 条以内的 map 没有目录也没有 table**，就是光秃秃一个 group
（`Map.dirLen == 0`，`dirPtr` 直接指向那个 group）。它甚至没有探测序列，
所以也**不会有墓碑**。场景一和场景四都建立在这个特例上。

---

## 2. 工具

```bash
make escape TOPIC=03-map-internals    # 逃逸分析怎么判的
make frames TOPIC=03-map-internals    # 按栈帧大小列出所有函数
make bench  TOPIC=03-map-internals    # 基准 + benchstat

# 本专题新增：在两种 map 实现之间做 A/B
make ab     TOPIC=03-map-internals BENCH='AB_'
```

`make ab` 是这次给 Makefile 加的：Go 1.25 仍然保留了老的 bucket 实现，
`GOEXPERIMENT=noswissmap` 就能切回去。**同一份源码、同一台机器、同一次运行，
只有 map 实现不同** —— 这是"换成 Swiss Table 到底改变了什么"能拿到的最干净的对照。

验证它确实切换了：

```go
runtime.Version()   // "go1.25.4"  或  "go1.25.4 X:noswissmap"
```

---

## 3. 场景一：8 条以内的 map，可能连堆都不上

### 常见写法

```go
func parseHeaders(lines []string) int {
	m := make(map[string]int)      // 局部小 map，用完就扔
	for i, l := range lines { m[l] = i }
	return m[lines[0]]
}
```

### 实测

四个函数只差 `make` 的第二个参数、和 map 是否被存到包级变量里：

| | `allocs/op` | `B/op` | 栈帧 |
|---|---:|---:|---:|
| `make(map[string]int)`，不逃逸 | **0** | **0** | 328 B |
| `make(map[string]int, 8)`，不逃逸 | **0** | **0** | 328 B |
| `make(map[string]int, 9)`，不逃逸 | 3 | 456 | 136 B |
| `make(map[string]int, 8)`，逃逸 | 2 | 256 | 88 B |

### 为什么

编译器在 `walkMakeSwissMap`（`cmd/compile/internal/walk/builtin.go:322`）里做了两件事，
条件不一样，这是全场景最容易搞混的地方：

```go
if n.Esc() == ir.EscNone {
    m = stackTempAddr(init, mapType)          // ← 条件 1：不逃逸 → Map 头上栈
    if hint 是常量且 <= 8 {
        g := stackTempAddr(&nif.Body, groupType)   // ← 条件 2：还要 hint <= 8 → group 也上栈
        m.dirPtr = g
    }
}
```

`Map` 头上栈只要求"不逃逸"；**那个 group 上栈还要额外满足 `hint <= 8`**。
所以 `hint=9` 那一行虽然逃逸分析照样说 "does not escape"，group 还是去了堆上。

456 字节能逐项对上：

```
groups 数组   2 个 group × 200 B = 400  → size class 416
table 结构体  used2+capacity2+growthLeft2+localDepth1+pad1+index8+groups16 = 32
目录切片      []*table 长度 1 = 8
                                        合计 456 ✓
```

（`map[string]int` 的 group = 8 字节控制字 + 8 × (16 字节 string + 8 字节 int) = 200。）

逃逸那一行的 256 也一样：`Map` 头 48 → size class 48，group 200 → size class 208。

### 怎么用

- 函数内部临时用的小 map，**不要写 `make(map[K]V, 16)` 这种"随手给个余量"** ——
  它把一个零分配的操作变成了三次堆分配。宁可不给 hint。
- 判断标准是**常量** `hint <= 8`。变量 hint 编译器会插一个运行时判断，
  仍然可能走栈上那条路，但你在源码里看不出来。
- 想确认有没有生效，看栈帧比看 `-m` 靠谱：`make frames` 里 328 和 136 的差
  就是那 8 个槽。

<details>
<summary>证据链对账</summary>

`make escape` 对前三个函数都说 `does not escape` —— 单看这个会得出错误结论。

```
map.go:41:11: make(map[string]int) does not escape
map.go:54:11: make(map[string]int, 8) does not escape
map.go:67:11: make(map[string]int, 9) does not escape     ← 但它有 3 次分配
map.go:83:11: make(map[string]int, 8) escapes to heap
```

`make frames` 才把区别露出来：

```
328  SmallMapNoHint     ← Map 头 + group 都在栈上
328  SmallMapHint8
136  SmallMapHint9      ← 只有 Map 头在栈上
 88  SmallMapEscape     ← 都在堆上
```

三条链吻合：基准的 `allocs/op`（0 / 3 / 2）、栈帧（328 / 136 / 88）、
源码里那两层嵌套的 `if`。
</details>

---

## 4. 场景二：make(map, n) 里的 n 不是容量

### 常见写法

```go
m := make(map[int64]int64, len(rows))   // 我知道要装这么多，预分配一下
```

这是对的，但 `len(rows)` 到底换成多大的表，中间隔着三次向上取整。

### 实测

每个 op = 往一张全新的 map 里插入 n 条（`keys` 是 `0..n-1`）：

| n | `make(map, n)` | 不预分配 | `make(map, n+n/7)` |
|---:|---:|---:|---:|
| 896 | **18.09 KiB / 6.77 µs** | 36.63 KiB / 17.87 µs | 36.12 KiB / 8.45 µs |
| 897 | 36.66 KiB / 13.06 µs | 72.71 KiB / 29.34 µs | **36.12 KiB / 7.23 µs** |
| 898 | 36.12 KiB / 6.71 µs | 72.71 KiB / 29.27 µs | 36.12 KiB / 7.51 µs |
| 1792 | 36.12 KiB / 14.77 µs | 72.71 KiB / 38.13 µs | 72.20 KiB / 19.80 µs |
| 1793 | 55.23 KiB / 21.32 µs | 108.8 KiB / 49.17 µs | **72.20 KiB / 17.30 µs** |

预分配相对不预分配稳定省一半内存、快 2～3 倍，这没有悬念。有悬念的是 896 → 897 那一跳。

> 这张表用的是顺序 key。**顺序整数 key 在 arm64 上会得到一个恰好均衡的哈希分布**，
> 多表时比真实 key 更容易"刚好装下" —— 换成随机 key，n=1792 那行是
> 71.55 KiB / 27.19 µs，而不是 36.12 KiB / 14.77 µs。原因和完整数据见[坑二](#坑二顺序整数-key-的哈希分布是完美的基准会骗你)。

### 为什么

`NewMap`（`internal/runtime/maps/map.go:255`）把 hint 换算成结构，一共三步，**每步都向上取整**：

```go
target  := hint * 8 / 7                            // 载荷 7/8 的反函数
dirSize := alignUpPow2(ceil(target / 1024))        // 一张表最多 1024 槽
每表槽数 := alignUpPow2(target / dirSize)           // 槽数必须是 2 的幂（探测序列要求）
```

代进去：

| hint | target | 表数 | 每表槽数 | 每表容量 | 总容量 |
|---:|---:|---:|---:|---:|---:|
| 896 | 1024 | 1 | 1024 | 896 | **896** |
| 897 | 1025 | 2 | **512** | 448 | **896** |
| 898 | 1026 | 2 | **1024** | 896 | **1792** |

`897 × 8 / 7 = 1025`，刚好越过 1024，于是目录从 1 张表变成 2 张。
`1025 / 2 = 512` 正好是 2 的幂，**没有向上取整的余量**，两张表各 512 槽、
各装 448 条 —— 加起来还是 896，比 hint 少一条。必然扩容。

而 `898 × 8 / 7 = 1026`，`1026 / 2 = 513`，向上取整到 **1024**，
于是两张表各 1024 槽，总容量 1792 —— 一下子超配到 hint 的两倍。

**同样是 2 的幂这件事，在 897 上让你少一条，在 898 上让你多一倍。**

这个模式每翻一倍重演一次。仓库里的 `PredictShape()` 把 `NewMap` 的算法照抄了一遍，
可以直接算：

| hint | 表数 | 每表槽数 | 容量 | groups 内存 | 容量/hint |
|---:|---:|---:|---:|---:|---:|
| 896 | 1 | 1024 | 896 | 17 KB | 1.00× |
| 897 | 2 | **512** | 896 | 17 KB | 1.00×（但必然扩容） |
| 898 | 2 | 1024 | 1792 | 34 KB | **2.00×** |
| 1792 | 2 | 1024 | 1792 | 34 KB | 1.00× |
| 14336 | 16 | 1024 | 14336 | 272 KB | 1.00× |
| 100000 | 128 | 1024 | 114688 | 2.1 MB | 1.15× |
| 917504 | 1024 | 1024 | 917504 | 17.0 MB | 1.00× |
| **1000000** | 2048 | 1024 | 1835008 | **34.0 MB** | **1.84×** |

`896 × 2ᵏ`（896、1792、3584、…、917504）是甜点：容量恰好等于 hint，一点不浪费。
往上一个的位置（`+2` 起）表数翻倍、每表仍是 1024 槽，**内存直接翻倍**，
然后一直保持到下一个甜点。所以 `make(map[int64]int64, 1_000_000)` 会申请
34 MB —— 装 100 万条只需要 19 MB，多出来的是 `alignUpPow2(1117) = 2048` 那一步。

`TestH2_HintSteps` 把推算值和实测占用摆在一起对账：没有扩容时实测/预测稳定在
**1.06 倍**（size class 取整 + `table` 结构体 + 目录切片），明显超过 1.06 就说明扩容了。

### 怎么用

- **n ≤ 896（单张表）：`make(map, n)` 精确管用**，容量恰好是 n，一次都不扩容，
  也不浪费。这是最舒服的区间。
- **n > 896：先想清楚 n 落在哪一段**，因为内存只由表数决定，和 n 的精确值无关：

  ```
  表数 = alignUpPow2(ceil(n × 8/7 / 1024))     每张表恒占 1024 槽
  ```

  n 落在一段的**中间**（比如 100000），已经自带 15% 余量，`make(map, n)` 就够了，
  多给也不会多花内存。n 正好压在 `896 × 2ᵏ` 上（896、1792、…、917504），
  没有任何余量，随便一点哈希不均就会扩容 —— 这时要么接受那一次扩容，
  要么多给一点把表数推到下一个 2 的幂，代价是**内存翻倍**。上表 917504 → 1000000
  就是这个代价。
- 例外是 `n = 896 × 2ᵏ + 1` 这个退化点（897、1793、…）：表数翻倍但每表只有 512 槽，
  内存没变，容量却比 hint 少一条，**纯亏**。加到 `+2` 就正常了。
- 反过来，**n ≤ 896 时不要多给**：上表第一行，`n + n/7` 把 18 KiB 变成 36 KiB，
  换来的只是慢 25%。
- 拿不准就用仓库里的 `PredictShape(hint, groupBytes)` 直接算，别猜。

---

## 5. 场景三：128 字节的分水岭

### 常见写法

```go
type Session struct { /* ... */ }        // 结构体一天天变大
var sessions = map[int64]Session{}       // 直接按值存
```

### 实测

1000 条，只改 value 的大小（key 侧的阈值完全一样，数据也一样）：

| | `allocs/op` | 建表 `B/op` |
|---|---:|---:|
| `map[int64][128]byte` | **22** | 579.7 KiB |
| `map[int64][129]byte` | **1022** | 213.3 KiB |

分配次数差 **46 倍**，方向是"越界之后变多"；而字节数差 **2.7 倍**，方向反过来。

### 为什么

`cmd/compile/internal/reflectdata/map_swiss.go:42` 和 `:45` 是两行严格大于：

```go
if keytype.Size() > abi.SwissMapMaxKeyBytes { keytype = types.NewPtr(keytype) }
if elemtype.Size() > abi.SwissMapMaxElemBytes { elemtype = types.NewPtr(elemtype) }
```

`SwissMapMaxKeyBytes = SwissMapMaxElemBytes = 128`。超过 128 字节，
槽里存的就不是数据本身而是一个指针，每插入一条新 key 都要 `newobject` 一次
（`table.go:336` 附近）—— 于是 `allocs/op` 从 O(表数) 变成 O(条目数)。

字节数反过来是因为**扩容要搬运整个槽**。128 字节内联时，一个 group 是
`8 + 8×(8+128) = 1096` 字节；表从 8 槽一路长到 1024 槽，中间每一代都要把
128 字节的净荷完整复制一遍，扔掉的旧表加起来就是那 580 KiB。
存指针时一个 group 只有 `8 + 8×(8+8) = 136` 字节，搬运成本降到 1/8。

### 怎么用

- 结构体接近 128 字节时**心里要有这条线**，它不在语言规范里，只在编译器里。
  跨过去之后每次插入多一次分配、多一层间接寻址、GC 多一个对象要管
  （下面场景七会看到这有多贵）。
- 真的很大就**主动存指针**（`map[K]*V`），至少这样是显式的，
  而不是"改了个字段结构体胖到 130 字节，线上分配次数悄悄涨了 46 倍"。
- 反过来，如果 value 就在 128 附近而且**建表是热点**，
  把它压到 128 以内不一定更省内存 —— 上表第二列就是反例。

<details>
<summary>稳态占用（扣掉扩容垃圾）</summary>

建表 `B/op` 里混着扩容产生的垃圾。`TestH3_Footprint` 只看最终留在堆上的部分：

```
[128]byte（内联）   298 KB，每条 306 字节（净荷 128）
[129]byte（存指针） 176 KB，每条 181 字节（净荷 129）
```

内联版每条 306 字节、净荷才 128，是因为表最多只能装到 7/8，
而且刚扩容完的表是半空的 —— 1000 条落在两张 1024 槽的表里，实际载荷不到 50%，
**空槽也要占 136 字节**。存指针时空槽只占 16 字节，所以浪费小得多。
</details>

---

## 6. 场景四：小 map 的字符串快路径

### 常见写法

```go
var httpMethods = map[string]int{"GET": 0, "POST": 1, /* ... 共 8 个 */}
_ = httpMethods[method]
```

小的、只读的 `map[string]T` 查表 —— 路由、枚举、feature flag，到处都是。

### 实测

同一张 **8 条目**的 `map[string]int`，只改 key 的形状。
「首尾可区分」的 key 走得到快路径；「只有中间不同」的走不到，退回算哈希。
第三列是 9 条目的对照（一超过 8 条就没有快路径了）：

| key 长度 | 8 条 / 首尾可区分 | 8 条 / 只有中间不同 | 9 条 / 首尾可区分 |
|---:|---:|---:|---:|
| 32 | 6.457 ns | 6.409 ns | 4.858 ns |
| 64 | 6.708 ns | 6.705 ns | 5.243 ns |
| **65** | **11.50 ns** | 10.30 ns | 6.956 ns |
| 128 | 11.97 ns | 10.43 ns | 6.740 ns |
| 512 | 12.05 ns | 14.51 ns | 10.72 ns |
| 1024 | **11.99 ns** | 19.42 ns | 15.89 ns |
| 4096 | **11.49 ns** | 49.20 ns | 47.73 ns |

第一列从 65 字节一路平到 4096（**11.50 → 11.49 ns**）。另外两列都是 O(len)，
4096 时分别是第一列的 **4.3 倍**和 **4.2 倍**。

但注意 64 → 65 那一跳：**+71%**。快路径不是免费的。

### 为什么

`internal/runtime/maps/runtime_faststr_swiss.go:17` 有一条只给小 map 的快路径：

```go
func (m *Map) getWithoutKeySmallFastStr(typ *abi.SwissMapType, key string) unsafe.Pointer {
	if len(key) > 64 {
		// 不算哈希。逐槽做 longStringQuickEqualityTest：
		//   比长度 → 比首 8 字节 → 比尾 8 字节
		// 恰好一个槽通过 → 对它做一次完整比较，返回
		// 两个以上通过   → goto dohash，退回算哈希
	}
dohash:
	hash := typ.Hasher(...)
	// ...
}
```

进入条件是 `m.dirLen <= 0` —— **这张 map 从来没超过 8 条**（超过就有目录了，第三列）。

于是三条曲线各自成立：

- **第一列**走完快路径。8 次定长比较（每次 16 字节）+ 一次完整比较，
  两者都不随 key 变长而显著变慢（`memequal` 是 SIMD 的），所以是平的。
- **第二列**的 8 个 key 首尾全一样，快速测试一个都排除不掉，
  代码 `goto dohash` 退回算哈希 —— 既付了 8 次定长比较，又付了 O(len) 的哈希。
- **第三列**有目录，压根进不来这个函数体，直接算哈希。

**64 → 65 的 +71% 是这条路的固定入场费。** 短字符串算一次哈希本来就便宜，
8 次定长比较反而更贵。快路径卖的不是"更快"，是"**不随长度变长**"。

把第一列和第二列相除（同一张表、同样长度、唯一变量是快速测试能不能区分），
盈亏平衡点落在 **128 到 512 字节之间**：

| key 长度 | 65 | 128 | 512 | 1024 | 4096 |
|---|---:|---:|---:|---:|---:|
| 快路径 / 退回哈希 | 1.12× | 1.15× | **0.83×** | **0.62×** | **0.23×** |

也就是说，**Go 源码里那个 64 字节的阈值，对 65～500 字节的 key 是负优化**
（慢 12%～15%），要到 512 字节以上才开始赚。

### 怎么用

- **key 很长（≥512 字节）且表能压到 8 条以内，收益极大** —— 4096 字节时快 4.3 倍。
  典型场景：用整段 SQL、整个请求体、序列化后的配置当 key 的小缓存。
- **key 在 65～500 字节之间，这条路是白亏 10%～15%。** 没有开关可以关掉它，
  唯一的规避手段是让 key 短于 65 字节（比如先做一次自己的短哈希再当 key）。
- **key 的区分度必须在首 8 字节或尾 8 字节。** 像
  `"tenant/prod/region/us-east/service/api/instance/{id}"` 这种前缀极长、
  后缀又一致的 key，快路径完全失效（第二列）。把区分度挪到开头就能拿回来。
- 别指望在超过 8 条的 map 上有这条路。第 9 条的代价不是 1/8，是整条快路径消失。

<details>
<summary>查中和查不中的差别（一个没预料到的结果）</summary>

源码注释说这个阈值是按"8 个槽全都 quick-match 失败"的场景调的，
也就是**查不中**。所以我预期 miss 才是快路径的主场。实测两者几乎一样：

| key 长度 | 8 条查中 | 8 条查不中 | 9 条查中 | 9 条查不中 |
|---:|---:|---:|---:|---:|
| 65 | 11.50 ns | 10.78 ns | 6.956 ns | 6.210 ns |
| 512 | 12.05 ns | 11.67 ns | 10.72 ns | 10.19 ns |
| 4096 | 11.49 ns | 9.927 ns | 47.73 ns | 46.69 ns |

查不中只快 3%～14%，形状完全一致。原因是查中那次"完整比较"用的是 `memequal`，
SIMD 一次比 32 字节以上，4096 字节也就几个纳秒 —— 远比算一次哈希便宜。
**所以快路径对查中同样有效，不只是 miss 优化。**

这一维是跑完第一轮之后补的（见[附录 B](#附录-b实验设计)），不是事前预注册的。
</details>

---

## 7. 场景五：map 只涨不落

### 常见写法

```go
cache := map[int64]int64{}
// ... 高峰期涨到 100 万条 ...
for k := range cache { delete(cache, k) }   // 清空，把内存还回去
```

### 实测

100 万条的 `map[int64]int64`，满表占 36 MB：

| 操作 | 之后占用 |
|---|---:|
| `delete` 到只剩 1 条 | **100.0%** |
| `clear(m)` | **100.0%** |
| 重建一张新 map | **0.0%** |

不是"下降得少"，是**一个字节都不还**。

### 为什么

看 `table.rehash`（`table.go:1118`）的全部出路：

```go
newCapacity := 2 * t.capacity
if newCapacity <= maxTableCapacity {
	t.grow(typ, m, newCapacity)   // 容量翻倍
	return
}
t.split(typ, m)                   // 切成两张 1024 槽的表
```

**没有第三条路。** 没有任何分支会把表变小。`Map.Clear`（`map.go:731`）也只是
把槽清零、控制字置 empty，`groups` 数组原样留在那儿（源码里那句
`// TODO: shrink directory?` 就挂在旁边）。

对 `map[int64]int64` 来说槽里没有指针，所以删除连一个字节都释放不了。
如果是 `map[string]string`，删除能让那些字符串变成垃圾被回收，
但**表本身**还是那么大。

### 怎么用

- **map 的内存占用是历史最高水位，不是当前条目数。** 做容量规划时按峰值算。
- 需要真正收回内存，只有重建一条路：

  ```go
  fresh := make(map[K]V, len(old))
  for k, v := range old { fresh[k] = v }
  old = fresh
  ```

- 长期存活、条目数大起大落的 map（连接表、会话表、按天分区的索引），
  要么定期重建，要么一开始就按分片/按代设计，让整片一起丢掉。
- `clear(m)` 是"清空内容"，不是"释放内存"。它比逐个 delete 快，但省的是时间不是空间。

---

## 8. 场景六：删一条插一条，表会长到两倍

### 常见写法

滑动窗口、LRU、TTL 缓存 —— 条目数稳定，但一直在删一条插一条。

```go
delete(m, oldest)
m[newest] = v      // len(m) 始终是 N
```

### 实测

10 万条稳定驻留，搅动 100 万轮：

| | 占用 | 每条字节 | 查找 |
|---|---:|---:|---:|
| 搅动过的表 | 4626 KB | 47.4 | **3.909 ns** |
| 同一批 key 新建 | 2309 KB | 23.6 | 4.437 ns |
| | **2.00×** | | 搅动的**快 12%** |

内存正好翻倍，而查找**不但没变慢，还快了 12%**（这批数据的噪声地板是 1.2%，
所以 12% 是真的）。

### 为什么

内存翻倍来自墓碑。回到背景那条规则：**探测在遇到"有空槽的 group"时终止**。
所以从一个已经满了的 group 里删东西，不能把槽置空 —— 那会让后面的探测提前停下，
找不到本该找到的 key。只能立一块墓碑（`ctrlDeleted`，`table.go:470`）：

```go
if g.ctrls().matchEmpty() != 0 {
	g.ctrls().set(i, ctrlEmpty)   // 这个 group 还有空槽，直接置空
	t.growthLeft++                // 容量还回来了
} else {
	g.ctrls().set(i, ctrlDeleted) // 满 group：只能立墓碑
	// 注意：growthLeft 不加回来
}
```

墓碑占着容量。删一条插一条，`growthLeft` 只减不增，最终耗尽 → 扩容。
Go 1.25 在扩容前会先试 `pruneTombstones`（`table.go:508`）回收一批，
但它有两道门槛（墓碑要占到容量的 10%、且要能回收 10% 的容量），
兜不住的时候还是得扩。扩完容量翻倍，然后重新开始攒墓碑 —— 稳态就停在两倍。

**查找变快**则是这件事的副作用：表大了一倍，同样的条目数意味着载荷从 ~87% 掉到 ~44%。
载荷低 → 几乎每个 group 都有空槽 → 探测一步就终止。
省下的探测长度不仅抵消了表变大带来的缓存损失，还多赚了 12%。

### 怎么用

- **搅动型 map 按两倍内存做预算。** 这是墓碑机制的固定开销，不是泄漏，也调不掉。
- 但**不用担心它变慢**，甚至会略快一点。我原本预期查找会退化 30% 以上，
  假说被推翻，详见[坑三](#坑三我以为墓碑会拖慢查找)。
- 如果两倍内存不能接受，唯一的手段还是场景五那句：定期重建。

---

## 9. 场景七：有没有指针，决定 GC 扫不扫这一百万条

### 常见写法

```go
var index = map[int64]*Record{}    // 值存指针，省得复制
```

### 实测

每个 op 是一次 `runtime.GC()`，堆里有一张 100 万条的 map：

| | 一次 GC | 相对空堆 |
|---|---:|---:|
| 空堆基线 | 133.8 µs | 1.0× |
| `map[int64]int64` | 244.9 µs | 1.8× |
| `map[int64]*int64`（全部指向同一个对象） | **2.325 ms** | **17.4×** |
| `map[int64]*int64`（各自独立的对象） | **6.271 ms** | **46.9×** |

第三行和第二行只差"槽里放的是 int64 还是指针"，对象数只多了 1 个 —— 但 GC 慢了 **9.5 倍**。

### 为什么

group 的类型是编译期生成的：

```go
struct {
	ctrl  uint64
	slots [8]struct{ key K; elem V }
}
```

`K` 和 `V` 都不含指针时，这个类型是 **noscan**，GC 标记阶段整个 `groups` 数组直接跳过 ——
100 万条 `map[int64]int64` 只有几百个 `groups` 数组要标记，扫描量约等于零。
只要 `K` 或 `V` 含一个指针，整个数组变成 scannable，
GC 每一轮都要遍历 **100 万个槽**去找指针。

第四行比第三行再慢 2.7 倍，是另一个变量：100 万个独立的小对象本身也要标记和清扫。
把它们拆成两行，就能看出**"要不要扫这张表"和"表里指向多少对象"是两笔独立的账**。

### 怎么用

- **大 map 优先用不含指针的 key/value。** `map[int64]int64`、`map[int32]struct{...无指针}`
  这类的 GC 成本几乎为零；换成指针就是几十倍。
- 特别注意 `string` 也是指针类型。`map[string]string` 一百万条，
  GC 每轮要扫 200 万个指针。真的大且长寿，考虑把字符串换成索引 + 一块大 backing array，
  或者干脆做成 `map[uint64]uint32` 的偏移表。
- 这条和场景三是一对：value 超过 128 字节会被**强制**改成指针存储，
  于是同时踩中"每次插入多一次分配"和"整张表变成 scannable"。
- 想确认，`GODEBUG=gctrace=1` 看每轮的标记时间，或者像这里一样直接把
  `runtime.GC()` 当基准跑。

---

## 10. 场景八：和老实现的正面对比

### 常见写法

没有写法 —— 你换个 Go 版本，map 就换了实现。所以问题是：**到底哪些代码因为这次
更换变快了，哪些变慢了？**

Go 1.25 仍然保留着老的 bucket 实现，`GOEXPERIMENT=noswissmap` 一开关就能切回去。
同一份源码、同一台机器、同一次 `make ab` 里跑完两边：

```bash
make ab TOPIC=03-map-internals BENCH='AB_'
```

### 实测

两边在同一次 `make ab` 里跑完，`-count=8`，benchstat 全部给出 p=0.000：

| | Swiss | 老 bucket | 差异 |
|---|---:|---:|---|
| 查中（1000 条） | 4.311 ns | 3.740 ns | **老的快 13%** |
| 查中（100 万条） | 20.98 ns | 28.72 ns | Swiss 快 **37%** |
| 查不中（1000 条） | 4.329 ns | 5.306 ns | Swiss 快 23% |
| 查不中（100 万条） | 16.09 ns | 24.14 ns | Swiss 快 **50%** |
| 建表不预分配（100 万） | 50.37 ms | 62.78 ms | Swiss 快 25% |
| 建表**预分配**（100 万） | 43.49 ms | 32.50 ms | **老的快 25%** |
| 遍历（100 万） | 6.064 ms | 6.613 ms | Swiss 快 9%（贴着噪声地板） |

内存和分配次数：

| | Swiss | 老 bucket |
|---|---:|---:|
| 建表 `B/op`（100 万，预分配） | 36.08 MiB | 38.40 MiB |
| 稳态每条目字节（10 万条） | **23.65** | 27.66 |
| 建表 `allocs/op`（100 万，不预分配） | **8210** | 38192 |
| 建表 `allocs/op`（100 万，**预分配**） | **4098** | **20** |

### 为什么

**查不中的收益最大（50%）**，这正是控制字设计的直接结果。老实现查不中要沿着
bucket 链走，每个 bucket 顺序比 8 个 tophash；Swiss 一条位运算比完 8 个槽，
而且探测遇到"有空槽的 group"立刻终止 —— 查不中通常一个 group 就结束了。

**小 map 查中反而慢 13%** 是另一回事。1000 条的表完全落在 L1/L2 里，
比较根本不是瓶颈，此时 Swiss 多出来的一层间接（目录 → table → groups）
就显出来了。不过这一行 Swiss 侧的波动是 ±13%（老实现侧只有 ±2%），
和差值本身同量级 —— 只能说"老实现在这个点上不吃亏"，不宜再往下解读。

**预分配建表老实现快 25%、分配次数只有 1/205**，是这次对比里最反直觉的一条。
原因在结构：Swiss 的 100 万条要切成 2048 张表，每张表一次 `table` 结构体
加一次 `groups` 数组，光分配就是 4098 次；老实现的 `makemap` 直接
`newarray` 一块连续的 bucket 数组，**20 次分配搞定**。
mallocgc 调 4098 次的开销，就是那 11 ms。

不预分配时局面倒过来：老实现要一路翻倍 + 拉 overflow bucket 链，38192 次分配；
Swiss 每张表独立扩容，8210 次。

**内存 Swiss 稳定省 6%～15%**，来自两处：group 是 `8 + 8×(K+V)`，
老 bucket 是 `8 + 8×K + 8×V + 8`（多一个 overflow 指针）；载荷从 6.5/8 提到 7/8。
理论差 `144/6.5` 对 `136/7` = 22.2 对 19.4 字节每条，实测 27.66 对 23.65 —— 方向和量级都对得上。

### 怎么用

- **升到 1.24+ 之后，收益最大的是"大 map + 查不中多"的场景** —— 布隆过滤器前置的
  缓存、去重集合、路由表 miss 分支。这类代码可以把原来为了绕开 map 而加的
  优化（额外的 bitmap、分片）重新评估一遍。
- **小 map 高频查中的热路径，换实现后可能略慢**（13%，且这一行噪声偏大）。
  这类地方本来就该实测，不要因为"1.24 换了更快的 map"就默认变快了。
- **一次性建一张几百万条的预分配大表**，Swiss 反而更慢。这类批处理代码
  （加载全量索引、构建离线字典）如果卡在建表上，值得实测一下。
- **内存 Swiss 稳定省 6%～15%**，这是唯一一条无条件成立的收益 ——
  堆压力大的服务光升版本就能拿到。
- 遍历的差异在 10% 以内，不构成决策依据。


---

## 一页速查

| 你写的 | 实际发生 | 更好的写法 |
|---|---|---|
| `make(map[K]V, 16)` 局部小 map | 3 次堆分配 | 不给 hint 或给 `<= 8`，零分配 |
| `make(map, n)`，n ≤ 896 | 容量恰好 n，完美 | 保持原样，别加余量 |
| `make(map, 897)`（即 `896×2ᵏ+1`） | 容量 896，必然扩容，纯亏 | 加到 `898` 就正常了 |
| `make(map, n)`，n 正好是 `896×2ᵏ` | 零余量，真实 key 会扩容一次 | 接受这次扩容，或加余量换内存翻倍 |
| `make(map, n)`，n 在两个 `896×2ᵏ` 中间 | 自带 15%～84% 余量 | 保持原样，多给也不省 |
| value 129 字节 | 每插入一条多一次分配 | 压到 128 以内，或显式用 `*V` |
| 9 条的 `map[string]T`，key ≥512 字节 | 每次查都算 O(len) 哈希 | 压到 8 条以内，走快路径，快 4 倍 |
| 8 条的 `map[string]T`，key 65～500 字节 | 快路径反而亏 10%～15% | 把 key 压到 64 字节以内 |
| key 只有中间不同 | 小 map 快路径失效 | 把区分度挪到首 8 字节或尾 8 字节 |
| `clear(m)` 想省内存 | 一个字节都不还 | 重建一张新 map |
| 删一条插一条的缓存 | 稳态占两倍内存（查找反而快 12%） | 按两倍做预算，或定期重建 |
| 百万条 `map[K]*V` | 每轮 GC 多扫百万个指针 | 换成无指针的 key/value |

---

## 坑

### 坑一：`does not escape` 不等于没有分配

`make escape` 对下面三行都说 `does not escape`，但第三行有 3 次堆分配：

```
map.go:41:11: make(map[string]int) does not escape
map.go:54:11: make(map[string]int, 8) does not escape
map.go:67:11: make(map[string]int, 9) does not escape     ← 3 allocs/op
```

因为逃逸分析判的是 `Map` 头，而**那 8 个槽（group）上不上栈是另一个条件**：
`hint` 必须是常量且 ≤ 8。两个条件写在 `walkMakeSwissMap` 的两层嵌套 `if` 里。

**教训：`-m` 的输出是"这个变量会不会逃逸"，不是"这行代码会不会分配"。**
判断有没有真的省下分配，要么看 `allocs/op`，要么看栈帧（`make frames`）。

### 坑二：顺序整数 key 的哈希分布是完美的，基准会骗你

写这个专题时我推理：`hint=1792` 会得到两张表、各装 896 条，
1792 个 key 按哈希最高位二分，两边不可能都不超过 896，所以必然扩容。

跑出来 200 次全部零扩容。用 `//go:linkname` 把 `runtime.memhash64` 拉出来直接看分布：

```
顺序 key 0..3583   分成 2 份: [1792 1792]   最大超出 +0
顺序 key 0..3583   分成 4 份: [896 896 896 896]  最大超出 +0
顺序 key 从 12345 起  分成 4 份: [892 894 897 901]  最大超出 +5
随机 key           分成 4 份: [854 955 911 864]  最大超出 +59
```

**arm64 上 Go 用 AES 指令做整数哈希，对"低位取遍所有值的一整块连续整数"，
哈希高位的分布是恰好均衡的** —— 换任何 seed 都一样，换成从 12345 起算就不均衡了。
而目录选表用的正是最高的那几位。

于是 `keys[i] = int64(i)` —— 基准里最顺手的那行 —— 给了一个真实世界拿不到的完美哈希。
影响有多大：

| | 顺序 key | 随机 key |
|---|---:|---:|
| `hint=1792` 建表 `B/op` | 36.12 KiB | 71.55 KiB |
| `hint=1792` 真实容量 | 1792（= 名义值） | 1743（名义的 97.3%） |
| `hint=3584` 真实容量 | 3584（= 名义值） | 3429（名义的 95.7%） |

**教训：key 生成器是实验的一部分。** 凡是结论依赖哈希分布的实验
（预分配、扩容、分片、负载均衡），都必须拿随机 key 再跑一遍。
本专题所有预分配基准现在都有 `seq` 和 `rand` 两条线。

这也直接改了结论：原本想写"给了 hint 就不会扩容"，实际是
"**n ≤ 896 给 hint 就不会扩容；n > 896 之后，真实容量只有名义值的 93～97%**"。

### 坑三：我以为墓碑会拖慢查找

事前假说 H6 写的是："反复删一条插一条到稳态后，查找耗时比同尺寸新建 map 高 ≥30%"。

实测：搅动过的表查找 **3.909 ns**，新建的 **4.437 ns** —— 搅动的那张**快 12%**。
这批数据的噪声地板是 1.2%，所以不是噪声，是真的反过来了。**假说被推翻。**

推翻的原因很有意思：我只想到了墓碑的坏处（占容量、让探测走更远），
没想到它的连锁反应 —— 墓碑逼着表扩容，扩完容量翻倍，**载荷从 87% 掉到 44%**。
载荷低意味着几乎每个 group 都有空槽，探测一步就终止。
省下来的探测长度不只抵消了表变大带来的缓存损失，还净赚 12%。

所以正确的说法是：**墓碑的代价体现在内存上（2 倍），查找延迟反而受益。**
这两件事我原本是当成一件事想的。

### 坑四：我以为 Swiss Table 是全面变快

事前假说 H8 写的是："查找类 Swiss 快 ≥15%，建表两者差异 <10%"。

前半对了一部分，后半整个错了：

- 查找：大 map 快 37%～50% ✅，但**小 map 查中老实现快 13%** ❌
- 建表：差异不是 <10%，而是 25% —— **而且方向取决于有没有预分配**。
  预分配 100 万条，老实现快 25%、分配次数只有 Swiss 的 1/205。

错在哪：我把"Swiss Table 更快"当成了一个整体判断，
没想到 Go 为了支持增量扩容把一张大表切成了 2048 张小表，
**每张表都要单独分配 groups 数组**。这个结构上的选择在查找侧几乎没有代价
（多一次目录索引），在"一次性建大表"这一侧却要付 4098 次 mallocgc。

**教训：一次实现更换从来不是单调的改进。** 提出"X 比 Y 快"这种假说时，
要先想清楚"在哪个维度、多大规模、什么访问模式下"，
否则它只是不可证伪的口号（原则 1、2）。

### 坑五：用 `ReadMemStats` 量小对象，要先量自己的分辨率

`TestH2_HintSteps` 第一版直接量单张 map 的 `HeapAlloc` 前后差，
`hint=8` 那行跑出 208 / 288 / 472 三个互相矛盾的数 —— 一张小 map 才 192 字节，
低于 `ReadMemStats` 在这个环境下的分辨率。

改成一次建 `1MB / 预期大小` 张再平摊之后，`hint=8` 稳定在 192 字节，
和 `Map` 头 48 + group 144（size class）逐字节对上。

**教训：原则 6 不只管 `ns/op`。** 任何测量手段都要先问一句
"它能分辨的最小量是多少"，再决定要不要攒批。

---

## 附录 A：完整数据与环境

```
goos: darwin      goarch: arm64      cpu: Apple M4（10 核）
go version go1.25.4 darwin/arm64
原始输出：.bench/03-map-internals.txt          主批，-count=8 -benchtime=1s
        .bench/03-map-internals-strkey.txt   纳秒批，-count=15 -benchtime=300ms
        .bench/03-map-internals.{default,noswissmap}.txt   场景八的 A/B

复现：make bench TOPIC=03-map-internals COUNT=8
     make bench TOPIC=03-map-internals BENCH='StrKey_|Tombstone_|NoiseFloor_' COUNT=15 BENCHTIME=300ms
     make ab   TOPIC=03-map-internals BENCH='AB_'
     go test -run 'TestH|TestAB|TestSetup' -v ./topics/03-map-internals/
     GOEXPERIMENT=noswissmap go test -run TestAB_Footprint -v ./topics/03-map-internals/
```

跑基准时机器上有 Chrome 和其他桌面进程，load average ≈ 4（10 核）。
这解释了主批的噪声地板为什么有 9.8%。纳秒批是等机器闲下来之后重跑的，地板降到 1.2%。

<details>
<summary>完整 benchstat 表格</summary>

**主批**（`-count=8 -benchtime=1s`）的 `sec/op`。里面那组 `StrKey_*`（没有 `_Hit`/`_Miss` 后缀的）
就是第一轮跑出来、噪声大到下不了结论的版本，原样留着 —— 和纳秒批对照能看出重跑的必要性：

```
                                  │           sec/op            │
Small_NoHint-10                                    116.8n ±  4%
Small_Hint8-10                                     118.8n ±  1%
Small_Hint9-10                                     178.2n ±  7%
Small_Escape-10                                    156.9n ±  1%
Presize_Exact/seq/n=8-10                           75.02n ±  1%
Presize_Exact/seq/n=9-10                           120.8n ±  1%
Presize_Exact/seq/n=896-10                         6.765µ ± 13%
Presize_Exact/seq/n=897-10                         13.06µ ±  4%
Presize_Exact/seq/n=898-10                         6.708µ ±  2%
Presize_Exact/seq/n=1000-10                        7.287µ ±  2%
Presize_Exact/seq/n=1792-10                        14.77µ ±  2%
Presize_Exact/seq/n=1793-10                        21.32µ ±  2%
Presize_Exact/rand/n=8-10                          76.92n ±  9%
Presize_Exact/rand/n=9-10                          123.3n ±  2%
Presize_Exact/rand/n=896-10                        8.016µ ±  2%
Presize_Exact/rand/n=897-10                        13.43µ ±  2%
Presize_Exact/rand/n=898-10                        6.778µ ±  1%
Presize_Exact/rand/n=1000-10                       7.394µ ±  2%
Presize_Exact/rand/n=1792-10                       27.19µ ±  2%
Presize_Exact/rand/n=1793-10                       26.65µ ±  2%
Presize_NoHint/seq/n=8-10                          73.48n ±  2%
Presize_NoHint/seq/n=9-10                          210.6n ±  5%
Presize_NoHint/seq/n=896-10                        17.87µ ±  2%
Presize_NoHint/seq/n=897-10                        29.34µ ±  7%
Presize_NoHint/seq/n=898-10                        29.27µ ±  2%
Presize_NoHint/seq/n=1000-10                       29.97µ ±  1%
Presize_NoHint/seq/n=1792-10                       38.13µ ± 20%
Presize_NoHint/seq/n=1793-10                       49.17µ ±  2%
Presize_NoHint/rand/n=8-10                         76.04n ±  1%
Presize_NoHint/rand/n=9-10                         222.5n ± 25%
Presize_NoHint/rand/n=896-10                       24.50µ ± 17%
Presize_NoHint/rand/n=897-10                       69.82µ ± 58%
Presize_NoHint/rand/n=898-10                       88.68µ ± 29%
Presize_NoHint/rand/n=1000-10                      40.51µ ± 65%
Presize_NoHint/rand/n=1792-10                      57.15µ ± 18%
Presize_NoHint/rand/n=1793-10                      64.88µ ± 15%
Presize_Headroom/seq/n=8-10                        148.7n ± 11%
Presize_Headroom/seq/n=9-10                        180.0n ± 26%
Presize_Headroom/seq/n=896-10                      8.450µ ± 21%
Presize_Headroom/seq/n=897-10                      7.229µ ±  4%
Presize_Headroom/seq/n=898-10                      7.509µ ± 25%
Presize_Headroom/seq/n=1000-10                     7.834µ ± 15%
Presize_Headroom/seq/n=1792-10                     19.80µ ± 21%
Presize_Headroom/seq/n=1793-10                     17.30µ ± 10%
Presize_Headroom/rand/n=8-10                       135.6n ±  8%
Presize_Headroom/rand/n=9-10                       136.2n ±  5%
Presize_Headroom/rand/n=896-10                     7.202µ ±  2%
Presize_Headroom/rand/n=897-10                     7.129µ ±  1%
Presize_Headroom/rand/n=898-10                     7.287µ ±  3%
Presize_Headroom/rand/n=1000-10                    7.906µ ± 12%
Presize_Headroom/rand/n=1792-10                    14.73µ ±  2%
Presize_Headroom/rand/n=1793-10                    14.66µ ±  9%
BigElem_128-10                                     63.73µ ± 23%
BigElem_129-10                                     65.20µ ±  9%
BigKey_128-10                                      102.0µ ±  4%
BigKey_129-10                                      92.35µ ±  9%
StrKey_Small8_Distinct/len=32-10                   8.795n ± 22%
StrKey_Small8_Distinct/len=64-10                   8.989n ± 10%
StrKey_Small8_Distinct/len=65-10                   14.50n ± 14%
StrKey_Small8_Distinct/len=128-10                  14.46n ±  8%
StrKey_Small8_Distinct/len=512-10                  13.82n ±  9%
StrKey_Large9_Distinct/len=32-10                   6.605n ± 54%
StrKey_Large9_Distinct/len=64-10                   8.347n ± 26%
StrKey_Large9_Distinct/len=65-10                   8.855n ±  5%
StrKey_Large9_Distinct/len=128-10                  9.290n ± 27%
StrKey_Large9_Distinct/len=512-10                  21.08n ± 39%
StrKey_Small8_Middle/len=32-10                     11.76n ± 10%
StrKey_Small8_Middle/len=64-10                     8.239n ±  6%
StrKey_Small8_Middle/len=65-10                     13.62n ±  7%
StrKey_Small8_Middle/len=128-10                    12.88n ±  4%
StrKey_Small8_Middle/len=512-10                    17.48n ±  7%
Tombstone_Fresh-10                                 6.054n ± 34%
Tombstone_Churned-10                               5.352n ± 11%
GC_Baseline-10                                     133.8µ ±  2%
GC_NoPtr-10                                        244.9µ ± 11%
GC_SharedPtr-10                                    2.325m ± 23%
GC_DistinctPtr-10                                  6.271m ± 11%
AB_LookupHit/n=1000-10                             5.344n ±  9%
AB_LookupHit/n=1000000-10                          25.25n ± 12%
AB_LookupMiss/n=1000-10                            5.673n ±  8%
AB_LookupMiss/n=1000000-10                         20.70n ±  9%
AB_Build/n=1000-10                                 40.47µ ±  7%
AB_Build/n=1000000-10                              63.62m ±  2%
AB_BuildPresized/n=1000-10                         10.59µ ±  7%
AB_BuildPresized/n=1000000-10                      53.14m ±  9%
AB_Iterate/n=1000-10                               7.537µ ± 19%
AB_Iterate/n=1000000-10                            7.875m ±  6%
NoiseFloor_A-10                                    4.137n ±  4%
NoiseFloor_B-10                                    4.542n ±  9%
geomean                                            1.658µ
```

**纳秒批**（`-count=15 -benchtime=300ms`，场景四和场景六重跑）：

```
                                        │           sec/op            │
StrKey_Small8_Distinct_Hit/len=32-10                     6.457n ±  2%
StrKey_Small8_Distinct_Hit/len=64-10                     6.708n ±  4%
StrKey_Small8_Distinct_Hit/len=65-10                     11.50n ±  7%
StrKey_Small8_Distinct_Hit/len=128-10                    11.97n ±  7%
StrKey_Small8_Distinct_Hit/len=512-10                    12.05n ±  5%
StrKey_Small8_Distinct_Hit/len=1024-10                   11.99n ±  8%
StrKey_Small8_Distinct_Hit/len=4096-10                   11.49n ±  7%
StrKey_Small8_Distinct_Miss/len=32-10                    6.712n ±  1%
StrKey_Small8_Distinct_Miss/len=64-10                    6.488n ±  1%
StrKey_Small8_Distinct_Miss/len=65-10                    10.78n ±  3%
StrKey_Small8_Distinct_Miss/len=128-10                   9.487n ±  3%
StrKey_Small8_Distinct_Miss/len=512-10                   11.67n ± 19%
StrKey_Small8_Distinct_Miss/len=1024-10                  11.52n ± 16%
StrKey_Small8_Distinct_Miss/len=4096-10                  9.927n ± 13%
StrKey_Large9_Distinct_Hit/len=32-10                     4.858n ±  1%
StrKey_Large9_Distinct_Hit/len=64-10                     5.243n ±  0%
StrKey_Large9_Distinct_Hit/len=65-10                     6.956n ±  2%
StrKey_Large9_Distinct_Hit/len=128-10                    6.740n ±  3%
StrKey_Large9_Distinct_Hit/len=512-10                    10.72n ±  2%
StrKey_Large9_Distinct_Hit/len=1024-10                   15.89n ±  1%
StrKey_Large9_Distinct_Hit/len=4096-10                   47.73n ±  2%
StrKey_Large9_Distinct_Miss/len=32-10                    4.225n ±  0%
StrKey_Large9_Distinct_Miss/len=64-10                    4.721n ±  0%
StrKey_Large9_Distinct_Miss/len=65-10                    6.210n ±  0%
StrKey_Large9_Distinct_Miss/len=128-10                   6.213n ±  2%
StrKey_Large9_Distinct_Miss/len=512-10                   10.19n ±  0%
StrKey_Large9_Distinct_Miss/len=1024-10                  15.16n ±  0%
StrKey_Large9_Distinct_Miss/len=4096-10                  46.69n ±  1%
StrKey_Small8_Middle_Hit/len=32-10                       6.409n ±  5%
StrKey_Small8_Middle_Hit/len=64-10                       6.705n ±  1%
StrKey_Small8_Middle_Hit/len=65-10                       10.30n ±  2%
StrKey_Small8_Middle_Hit/len=128-10                      10.43n ±  4%
StrKey_Small8_Middle_Hit/len=512-10                      14.51n ±  2%
StrKey_Small8_Middle_Hit/len=1024-10                     19.42n ±  1%
StrKey_Small8_Middle_Hit/len=4096-10                     49.20n ±  1%
StrKey_Small8_Middle_Miss/len=32-10                      6.708n ±  4%
StrKey_Small8_Middle_Miss/len=64-10                      6.471n ±  1%
StrKey_Small8_Middle_Miss/len=65-10                      10.67n ±  2%
StrKey_Small8_Middle_Miss/len=128-10                     10.64n ±  2%
StrKey_Small8_Middle_Miss/len=512-10                     14.34n ±  1%
StrKey_Small8_Middle_Miss/len=1024-10                    19.93n ±  1%
StrKey_Small8_Middle_Miss/len=4096-10                    49.99n ±  0%
Tombstone_Fresh-10                                       4.437n ±  3%
Tombstone_Churned-10                                     3.909n ±  2%
NoiseFloor_A-10                                          3.913n ±  1%
NoiseFloor_B-10                                          3.961n ±  2%
geomean                                                  9.794n
```

`B/op` 和 `allocs/op` 两张表在原始输出里，命令见上。

</details>

---

## 附录 B：实验设计

### 噪声地板

`NoiseFloorA` 和 `NoiseFloorB` 的函数体逐字相同：

```go
//go:noinline
func NoiseFloorA(m map[int64]int64, k int64) int64 { return m[k] }

//go:noinline
func NoiseFloorB(m map[int64]int64, k int64) int64 { return m[k] }
```

它跟着每一批数据跑一次，因为**噪声地板不是机器的常数**：

| 批次 | 参数 | 机器状态 | A | B | 地板 |
|---|---|---|---:|---:|---:|
| 主批（场景一/二/三/五/七） | `-count=8 -benchtime=1s` | 开着浏览器，load ≈ 4 | 4.137 ns ±4% | 4.542 ns ±9% | **9.8%** |
| 纳秒批（场景四/六） | `-count=15 -benchtime=300ms` | 较闲 | 3.913 ns ±1% | 3.961 ns ±2% | **1.2%** |

同一台机器、同一对函数，两次相差 8 倍。**所以"这个差异算不算数"必须按它所在那批的地板判**，
不能拿一个数走天下。

第一轮场景四和场景六用的是主批参数，`Tombstone_Fresh` 跑出 ±34%、
`Large9/len=32` 跑出 ±54% —— 结论完全下不了。把这两组挪到纳秒批重跑之后，
最差的一行也只有 ±19%，才敢往下写。这一步花掉的时间比写文档还多，
但没有它，场景六会得出"墓碑不影响查找"这个只对了一半的结论
（正确答案是"反而快 12%"）。

### 控制的变量

| 对照组 | 唯一的差异 | 刻意保持相同的 |
|---|---|---|
| `SmallMapHint8` / `SmallMapHint9` | `make` 的第二个参数 | 函数体逐字相同，同样 8 条 key |
| `SmallMapHint8` / `SmallMapEscape` | 最后一行是否赋给包级变量 | 其余逐字相同 |
| `Presize_Exact` / `_NoHint` / `_Headroom` | hint 的取值 | 同一份 key、同样的插入循环 |
| `seq` / `rand` | key 的哈希分布 | 条目数、hint、插入顺序 |
| `BuildElem128` / `BuildElem129` | value 类型多 1 字节 | 同一份 key，同样的插入循环 |
| `Small8_Distinct` / `Small8_Middle` | 快速测试能不能区分（key 区分度在首尾还是中间） | 条目数、key 长度、map 构造方式 |
| `_Hit` / `_Miss` | 探针在不在表里 | 探针由同一个生成器产出，形状完全一致 |
| `Tombstone_Fresh` / `_Churned` | 表是否经历过搅动 | 完全相同的 10 万个 key |
| `GC_NoPtr` / `_SharedPtr` | 槽里是 int64 还是指针 | 条目数；对象数只差 1 |
| `GC_SharedPtr` / `_DistinctPtr` | 指向 1 个对象还是 100 万个 | 表结构完全相同 |

### 假说命中情况

八条假说全部写在 `map_test.go` 顶部，**跑任何基准之前定稿**，原文保留。

| | 假说 | 结果 |
|---|---|---|
| H1 | 小 map 不上堆，hint=9 掉到堆上（≥3 次分配） | ✅ 命中，且分配次数正好是 3 |
| H2 | hint 换算有台阶，896→897 内存不降反升 | ✅ 命中（18.09 → 36.66 KiB） |
| H3 | 128 字节是硬边界，越界后 `allocs/op ≈ N` | ✅ 命中（22 → 1022） |
| H4a | 8 条目 map 的查找耗时从 65B 到 512B 基本持平 | ✅ 命中（65B→4096B：11.50 → 11.49 ns） |
| H4b | 9 条目 map 随 key 长度单调上升 | ✅ 命中（65B→4096B 涨 6.9 倍） |
| H4c | 首尾相同的 key 会退回算哈希 | ✅ 命中（4096B 时贵 4.3 倍） |
| H4 的口头说法「越过 64 字节反而变快」 | —— | ❌ **推翻**：64→65 是 **+71% 变慢**，且 65～500 字节区间一直是负优化 |
| H5 | 删除和 `clear` 释放的内存 < 10% | ✅ 命中，且是极端形式：0% |
| H6 | 搅动后查找退化 ≥30%，但不超过 2 倍 | ❌ **推翻**：完全没有退化。见坑三 |
| H7 | 含指针的 map GC 慢 ≥5 倍 | ✅ 命中（9.5 倍） |
| H8 | Swiss 的收益在查找和内存，不在插入 | ⚠️ **部分推翻**：查找收益只在大 map 和查不中时成立（小 map 查中老的快 13%）；建表差异远超 10%，且预分配时老的快 25% |

另外，H2 虽然命中，但**推导过程里有一处错**（我以为 `hint=1792` 也会扩容），
是靠原则 7 的事前验证抓出来的，见坑二。

### 关于顺序：哪些是事前的，哪些不是

原则 2 要求假说先于实验。这个专题里有三种情况，分开标注：

**1. 严格事前**：H1–H8 全部写在 `map_test.go` 顶部，跑任何基准之前定稿，原文一字未改。

**2. 事前但晚一步**：H4c 是在 H1–H8 定稿之后、跑任何基准之前补的 ——
读 `runtime_faststr_swiss.go` 时发现 `goto dohash` 那条分支，
意识到 H4 需要一个反例组才完整。仍然满足"假说先于实验"，但不是同一时刻写下的。

**3. 事后补的探索性实验**（不算预注册，只能用来提新假说）：

- **场景四的 `_Miss` 维度和 1024/4096 两个长度。** 第一轮只测了查中、
  且最长只到 512 字节，得到的结论是"快路径在 512 字节时比算哈希还慢 12%"。
  回头再读源码那段注释才发现两件事：阈值是按**查不中**的场景调的，
  而且 512 字节根本不够长。补测之后曲线才完整 —— 盈亏平衡点在 128～512 之间，
  4096 字节时快 4.3 倍。
  顺带一提，我以为 miss 会是快路径的主场，实测 hit 和 miss 差别只有 10%，
  这个预期也错了。
- **`Presize_*` 的 `rand` 这一维**，起因见[坑二](#坑二顺序整数-key-的哈希分布是完美的基准会骗你)。
- **`TestH3_Footprint`**（稳态占用），因为建表 `B/op` 里混着扩容垃圾，
  回答不了"哪种更省内存"。

第 3 类的共同点是：**都不是为了救一个失败的假说，而是发现原实验测的东西不完整。**
它们的结论在文档里照写，但不计入上面的命中表。

---

## 附录 C：延伸阅读

### Go 源码

`internal/runtime/maps/map.go` 开头那 180 行注释是整个实现最值得读的文档，
把 group / table / directory / 可扩展哈希 / 迭代语义讲了一遍。

| 位置 | 内容 |
|---|---|
| `internal/runtime/maps/map.go:16-180` | 设计总述（先读这个） |
| `internal/runtime/maps/map.go:210-243` | `Map` 结构 + 小 map 特例说明 |
| `internal/runtime/maps/map.go:255` | `NewMap` —— hint 怎么换算成容量 |
| `internal/runtime/maps/map.go:731` | `Clear` —— 以及那句 `TODO: shrink directory?` |
| `internal/runtime/maps/group.go:14-30` | 载荷 7/8、控制字编码 |
| `internal/runtime/maps/group.go:125-235` | `matchH2` / `matchEmpty` 的位运算 |
| `internal/runtime/maps/table.go:20` | `maxTableCapacity = 1024` |
| `internal/runtime/maps/table.go:116` | `maxGrowthLeft` |
| `internal/runtime/maps/table.go:266` | `PutSlot` —— 插入与扩容触发 |
| `internal/runtime/maps/table.go:421` | `Delete` —— 墓碑的判断 |
| `internal/runtime/maps/table.go:508` | `pruneTombstones`（Go 1.25 新增） |
| `internal/runtime/maps/table.go:1118` | `rehash` / `split` / `grow` |
| `internal/runtime/maps/table.go:1232` | `probeSeq` 二次探测 |
| `internal/runtime/maps/runtime_faststr_swiss.go:17` | 小 map 的长字符串快路径 |
| `internal/abi/map_swiss.go:13-30` | 全部常量 |
| `cmd/compile/internal/walk/builtin.go:322` | `walkMakeSwissMap` —— 栈上分配 |
| `cmd/compile/internal/reflectdata/map_swiss.go:42` | 128 字节阈值 |

老实现留在 `runtime/map_noswiss.go` 和 `internal/abi/map_noswiss.go`，
`GOEXPERIMENT=noswissmap` 可以切回去做对比。

### 外部资料

- [Abseil 的 Swiss Table 设计说明](https://abseil.io/about/design/swisstables) —— Go 的实现以此为基础
- [Go 1.24 release notes 里的 map 部分](https://go.dev/doc/go1.24#performance-improvements)
- [proposal: Swiss Tables (go.dev/issue/54766)](https://go.dev/issue/54766) —— 包括"单 group 表能不能填满 8 槽"这类还没做的优化

### 本仓库相关专题

- [专题 01：逃逸分析](../01-escape-analysis/) —— 场景一里"什么叫不逃逸"的完整版
- [专题 02：slice 扩容与预分配](../02-slice-growth/) —— 同一个问题在 slice 上的答案，
  对比着看很有意思：slice 的扩容曲线是连续的，map 的是台阶
- [专题 15：读多写少的并发缓存](../15-rcu-cache/) —— 那里的 `sync.Map` 对照组，
  以及为什么老版 `sync.Map` 的 read/dirty 双 map 和本文的 directory 是两回事

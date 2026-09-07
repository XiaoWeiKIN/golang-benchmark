# 01 · 逃逸分析：一个对象凭什么留在栈上

Go 里最容易拿到的性能提升，常常不是换算法，而是让一个对象别跑到堆上去。

先看两个逻辑完全一样的函数：

```go
func NewPointByPointer() *Point { p := &Point{X: 1, Y: 2}; return p }
func NewPointByValue() Point    { return Point{X: 1, Y: 2} }
```

```
Pointer_Returned    7.87 ns/op    16 B/op    1 allocs/op
Pointer_Value       1.77 ns/op     0 B/op    0 allocs/op
```

**4.5 倍，差别只在返回类型。**

这个专题讲清楚两件事：编译器凭什么做这个决定，以及哪些写法会让它翻转。

```bash
make bench  TOPIC=01-escape-analysis   # 跑基准
make escape TOPIC=01-escape-analysis   # 问编译器：这个对象逃逸了吗
make frames TOPIC=01-escape-analysis   # 每个函数的栈帧多大
```

**目录**

- [栈和堆到底差在哪](#栈和堆到底差在哪)
- [怎么问编译器](#怎么问编译器)
- 六个场景
  - [场景 1：返回指针还是返回值](#场景-1返回指针还是返回值)
  - [场景 2：为什么同样的 `&T{}` 有时零分配](#场景-2为什么同样的-t-有时零分配)
  - [场景 3：热路径上别用 fmt](#场景-3热路径上别用-fmt)
  - [场景 4：`make` 的长度写成常量](#场景-4make-的长度写成常量)
  - [场景 5：栈上能放多大](#场景-5栈上能放多大)
  - [场景 6：闭包会把变量拖上堆](#场景-6闭包会把变量拖上堆)
- [一页速查](#一页速查)
- [三个坑](#三个坑)
- [附录](#附录-a完整数据与环境)

---

## 栈和堆到底差在哪

**栈分配**就是把栈指针往下挪几个字节。函数返回时整个栈帧一次性废弃，**GC 完全不参与**。

**堆分配**要走 `runtime.mallocgc`：算 size class、去 mcache 找对应的 span、
运气不好还要加锁向 mcentral 甚至 mheap 要内存、写 GC 标记位、更新 heap goal。
而且事情没完 —— 这块内存之后还要被 GC 扫描、标记、清扫。

所以逃逸分析真正的收益不是省下那点分配时间，而是**把对象从 GC 的工作集里彻底摘出去**。
在高频路径上，"每次调用少一次分配"往往比"算法快 10%"更能压低 P99。

那编译器凭什么判断？只有两条不变量，写在 `cmd/compile/internal/escape/escape.go` 开头：

> 1. 指向栈对象的指针，**不能被存进堆**；
> 2. 指向栈对象的指针，**生命周期不能超过该对象所在的栈帧**。

**证不出这两条，就往堆上放。** 逃逸分析是保守的 —— 它宁可多分配，也不会分配错。

后面六个场景，本质上都是这两条不变量的不同触发方式。

---

## 怎么问编译器

别猜，直接问。这是学逃逸分析最重要的习惯：

| 想知道 | 命令 | 本仓库封装 |
|---|---|---|
| 谁逃逸了 | `go build -gcflags='-m' ./pkg` | `make escape-inline` |
| 排除内联干扰的"纯"逃逸结论 | `go build -gcflags='-m -l' ./pkg` | `make escape` |
| 为什么逃逸（推导链路） | `go build -gcflags='-m -m' ./pkg` | — |
| 栈帧到底多大 | `go build -gcflags='-S' ./pkg` | `make frames` / `make asm` |

`make frames` 特别顺手，它按栈帧大小排序列出包里所有函数：

```
   65576  SumBufAtLimit      ← 64KB 缓冲区确实在栈上
      72  SumConstLenBuf
      40  SumBufOverLimit    ← 只剩一个 slice header，数据搬到堆上了
       0  SumInlinedPointer
```

---

## 场景 1：返回指针还是返回值

**常见写法**：写构造函数时习惯性返回指针。

```go
//go:noinline
func NewPointByPointer() *Point { p := &Point{X: 1, Y: 2}; return p }

//go:noinline
func NewPointByValue() Point { return Point{X: 1, Y: 2} }
```

```
Pointer_Returned    7.87 ns/op    16 B/op    1 allocs/op
Pointer_Value       1.77 ns/op     0 B/op    0 allocs/op
```

**为什么。** 逃逸分析是**自底向上**的：先分析被调函数，得出"参数和返回值会不会泄漏"，
再把结论用到调用方。`NewPointByPointer` 返回的指针指向自己的栈帧 —— 函数一返回栈帧就没了，
撞上不变量 2。所以它是**无条件**堆分配的，不管调用方拿它干什么。

`Point` 只有 16 字节，值返回直接走寄存器，一次拷贝比一次 `mallocgc` 便宜得多。

> **一个反直觉的点**：很多人以为"传指针比传值快，因为省了拷贝"。在 Go 里这个直觉经常是反的 ——
> 传指针可能把对象推上堆，代价是一次 `mallocgc` 加一个 GC 要扫描的对象。

**怎么用。** 小结构体（几十字节以内）默认用值传递、值返回。需要指针的理由应该是
"我要改它" 或 "它真的很大"，而不是"指针更快"。

<details>
<summary>证据链</summary>

| | |
|---|---|
| 基准 | `1 allocs/op` vs `0 allocs/op` |
| 编译器 | `escape.go:33:7: &Point{...} escapes to heap`；`NewPointByValue` 处无输出 |
| 源码 | `escape/escape.go` 开头的不变量 2 |

</details>

---

## 场景 2：为什么同样的 `&T{}` 有时零分配

上面两个函数带了 `//go:noinline`，是为了强行隔开栈帧。**去掉它，结论就变了**：

```go
func newPointInlinable() *Point { return &Point{X: 1, Y: 2} }  // 够小，会被内联

func SumInlinedPointer() int {          // 1.75 ns/op, 0 allocs ← 零分配！
	p := newPointInlinable()
	return p.X + p.Y
}

var escapedPoint *Point

func SumLeakedPointer() int {           // 8.18 ns/op, 1 alloc
	p := newPointInlinable()
	escapedPoint = p                    // ← 只多了这一行
	return p.X + p.Y
}
```

同一个 `newPointInlinable`，一个零分配，一个要分配。`make escape-inline` 把整件事说透了：

```
escape.go:60:24: inlining call to newPointInlinable
escape.go:69:24: inlining call to newPointInlinable
escape.go:55:9:  &Point{...} escapes to heap      ← 独立存在的那份函数体
escape.go:60:24: &Point{...} does not escape      ← 内联进 SumInlinedPointer 后，留在栈上
escape.go:69:24: &Point{...} escapes to heap      ← 内联进 SumLeakedPointer 后，仍然逃逸
```

**同一个字面量，三个不同结论。**

**为什么。** 关键在于**内联发生在逃逸分析之前**。内联把调用展开后，"返回局部变量的指针"
这件事在调用方视角下就不存在了 —— 分析范围收敛到单个栈帧，指针没再往外泄漏，就能留在栈上。

而 `SumLeakedPointer` 那行 `escapedPoint = p` 把指针存进了包级变量（在堆上），
撞的是不变量 1，内联也救不回来。

**怎么用。** 这解释了一个高频困惑：*为什么我的 `&T{}` 有时零分配、有时不是？*
答案通常是**内联预算**。函数写长一点、加个 `defer`、加个循环，超了预算就不内联，
逃逸结论跟着翻转。所以：

- 别记"`&T{}` 一定零分配"，记"去查 `-m`"；
- 热路径上的小构造函数保持简短，让它能被内联；
- `-gcflags='-m'` 里搜 `cannot inline`，会告诉你为什么没内联（`function too complex` 之类）。

---

## 场景 3：热路径上别用 fmt

**常见写法**：随手 `fmt.Sprintf("%d", n)` 拼个字符串。

```go
fmt.Sprintf("%d", n)                  // 36.9 ns    16 B    2 allocs
strconv.Itoa(n)                       // 13.2 ns     8 B    1 alloc
strconv.AppendInt(buf[:0], n, 10)     //  8.6 ns     0 B    0 allocs
```

**为什么。** `fmt.Sprintf` 的签名是 `Sprintf(format string, a ...any)`。把 `int` 装进 `any`
需要**装箱**：接口值是 `(类型指针, 数据指针)` 两个字，数据那一格必须是指针，
所以那个 `int` 得在堆上有个副本。更要命的是 fmt 内部要把参数交给反射，
编译器只能判定"泄漏"，装箱没法优化掉。

所以 2 次分配 = **装箱的 int（8 字节）+ 结果字符串（8 字节）**。

`strconv.Itoa` 没有接口，只剩结果字符串这一次。
`strconv.AppendInt` 连结果都写进你自己复用的缓冲区，稳态下零分配。

**怎么用。**

- 热路径上优先找 `AppendXxx` —— 这是 Go 标准库反复出现的模式：
  `strconv.AppendInt`、`strconv.AppendQuote`、`time.Time.AppendFormat`、`hash.Hash.Sum`…
- 其次用 `strconv.Xxx`；
- `fmt.Sprintf` 留给日志、错误信息这类不在热路径上的地方。

<details>
<summary>怎么确认"2 次分配 = 装箱 + 字符串"这个拆解是对的</summary>

光凭 16 B / 2 allocs 凑得上不算证明。所以先写下一条能被推翻的预测，再跑：

> 把结果字符串从 7 字节（size class 8）换成 12 字节（size class 16），
> Sprintf 的 `B/op` 应该 16→24、Itoa 应该 8→16，而两者 `allocs/op` 都不变。
> 如果 `B/op` 没动，这个拆解就是错的。

```
make bench BENCH='Verify_' TOPIC=01-escape-analysis

Verify_SprintfShort   16 B/op   2 allocs/op       Verify_ItoaShort    8 B/op   1 allocs/op
Verify_SprintfLong    24 B/op   2 allocs/op       Verify_ItoaLong    16 B/op   1 allocs/op
```

四个数字全中，拆解成立。预测的原始措辞保留在 `escape_test.go` 的验证组注释里。

**顺带一个坑**：这个验证的第一版用 `const` 当实参，Sprintf 测出来只有 **1 alloc / 8 B** ——
常量转 `any` 时编译器能静态装箱，装箱那次分配根本不发生。换成变量才测到真实情况。

同类问题：拿 `strconv.Itoa(1)` 测整数格式化会得到"零分配"，因为 0–99 走静态字符串表。
所以本专题统一用 `1234567`。**基准的输入要像线上的输入。**

</details>

---

## 场景 4：`make` 的长度写成常量

```go
buf := make([]byte, 64)   // 常量 → 栈上，0 allocs
buf := make([]byte, n)    // 变量 → 堆上，1 alloc（哪怕 n 恒等于 64）
```

```
Make_ConstLen    39.2 ns    0 B    0 allocs
Make_VarLen      56.0 ns   64 B    1 alloc     ← n 恒为 64，工作量完全一样
```

**为什么。** 编译器要在栈上放一个数组，必须在**编译期**知道它占多少字节，才能规划栈帧布局。
长度是运行期变量时它做不到，只能退化成 `runtime.makeslice` 调用。

`make asm FUNC=SumVarLenBuf` 能看到那条 `CALL runtime.makeslice(SB)`，常量版本里没有。

**怎么用。** 热路径上如果 buffer 尺寸有上界，写成常量数组再切片：

```go
var buf [64]byte                          // 栈上
s := strconv.AppendInt(buf[:0], v, 10)    // 和场景 3 组合起来就是零分配的整数格式化
```

---

## 场景 5：栈上能放多大

常量长度、也不逃逸，是不是就一定在栈上？不是。超过阈值照样被赶到堆上 ——
否则一个深递归就能把栈撑爆。

```
Make_64KB          1.45 µs        0 B    0 allocs    ← make([]byte, 65536)
Make_64KBPlus1     3.90 µs    73728 B    1 alloc     ← make([]byte, 65537)
```

**多一个字节，2.7 倍。** 阈值写死在 `cmd/compile/internal/ir/cfg.go`：

```go
// 显式声明：var x T / x := ...
MaxStackVarSize = int64(128 * 1024)

// 隐式分配：new(T) / &T{} / make([]T, n) / []byte("...")
MaxImplicitStackVarSize = int64(64 * 1024)
```

两个阈值都实测过，`+1` 字节就翻转：

| 写法 | 恰好等于上限 | 上限 +1 字节 |
|---|---|---|
| `make([]byte, N)` | 栈（`make frames` 显示 `locals=65576`） | 堆（`make([]byte, 65537) escapes to heap`） |
| `var a [N]byte` | 栈 | 堆（`moved to heap: a`） |

注意"隐式"比"显式"严格一倍 —— `p := new(T)` 和 `var t T; p := &t` 的阈值不一样。

还有个细节：堆版本的 `B/op` 是 **73728 而不是 65537**。`mallocgc` 向上取整到 size class，
你要 64KB+1，它给你 72KB。持续调用会显著抬高 GC 频率。

**怎么用。** 别把业务逻辑压在阈值边界上 —— 这两个常量是**编译器实现细节**，
不是语言规范，还受 `-gcflags=-smallframes` 影响，可能随版本变。
知道有这么回事，遇到大 buffer 时用 `make frames` 确认一下就行。

---

## 场景 6：闭包会把变量拖上堆

```go
//go:noinline
func SumWithLocalClosure(n int) int {   // 0 allocs
	total := 0
	add := func(v int) { total += v }
	for i := range n { add(i) }
	return total
}

//go:noinline
func MakeAccumulator() func(int) int {  // 2 allocs, 24 B
	total := 0
	return func(v int) int { total += v; return total }
}
```

```
Closure_Local      31.5 ns     0 B    0 allocs    ← 100 次累加
Closure_Returned   99.2 ns    24 B    2 allocs    ← 构造 + 100 次累加
```

**为什么。** 闭包在运行期是一个 **funcval**：函数指针 + 捕获的变量。
捕获是**按引用**的，所以被捕获变量的地址会存进 funcval。

只要 funcval 本身不逃逸，编译器就能把它和捕获变量一起留在栈上，甚至把调用完全内联掉 ——
`make escape-inline` 里那行 `inlining call to SumWithLocalClosure.func1` 就是证据，
局部闭包被内联后循环体退化成一条 `total += i`，所以才有 0.3 ns/次。

一旦闭包被返回或存进外部结构，funcval 逃逸，被它引用的变量也跟着走：

```
escape.go:211:2: moved to heap: total
escape.go:212:9: func literal escapes to heap
```

两次分配精确对应这两行 —— `total` 一次，funcval 一次。

**怎么用。** `sync.Once`、`errgroup.Go`、`http.HandlerFunc`、各种回调注册，
只要闭包被存起来，捕获的变量就上了堆。热路径上频繁构造这类闭包时，
考虑提到循环外只构造一次，或者换成显式的结构体 + 方法值。

<details>
<summary>那 24 字节具体是怎么分的（附一个被推翻的猜测）</summary>

猜测是 `24 B = 堆上的 total(8B) + funcval{fn, *total}(16B)`。写成可推翻的预测：

> 多捕获一个 `int`，funcval 涨 8 字节变成 24 B，捕获的变量各分配一次，
> 所以 `B/op` 应该是 `8 + 8 + 24 = 40`，`allocs/op` 应该是 **3**。

```
Verify_Closure1Var    24 B/op   2 allocs/op
Verify_Closure2Vars   40 B/op   2 allocs/op     ← 40 中了，但 allocs 还是 2！
```

`B/op` 命中，**分配次数猜错了**。`make asm FUNC=MakeAccumulator` 给出答案：

```asm
escape.MakeAccumulator:
  L211  CALL  runtime.newobject                                      ← total (8B)
  L212  MOVD  $type:noalg.struct { F uintptr; X0 *int }, R0
  L212  CALL  runtime.newobject                                      ← funcval (16B)

escape.MakeAccumulator2:
  L223  CALL  runtime.mallocgc                                       ← a 和 b 合并成一次 16B 分配
  L224  MOVD  $type:noalg.struct { F uintptr; X0 *int; X1 *int }, R0
  L224  CALL  runtime.newobject                                      ← funcval (24B)
```

编译器把 funcval 的类型名直接印出来了：`struct { F uintptr; X0 *int }` ——
一个函数指针加一个捕获指针，正好 16 字节。猜测的这一半是对的。

错的是"每个逃逸变量各分配一次"：`a` 和 `b` 走的是**单次 `mallocgc`**（注意不是两次 `newobject`）。
修正后的说法是：**funcval 单独一次分配，同一函数里所有逃逸的局部变量合并成一次分配。**

如果不做这个预测，光看 `Closure_Returned` 的 2 allocs，很容易外推成
"捕获 N 个变量 = N+1 次分配"，错得很隐蔽。

</details>

---

## 一页速查

| 你写的 | 结果 | 更好的写法 |
|---|---|---|
| `func New() *T { return &T{} }`（不可内联） | 必然堆分配 | 小结构体返回 `T` |
| 小结构体传指针 | 可能推上堆 | 传值 |
| `&T{}` 存进全局/外部结构 | 必然堆分配 | 无解，这是语义要求 |
| `fmt.Sprintf("%d", n)` | 2 allocs | `strconv.AppendInt(buf[:0], …)` |
| `make([]byte, n)`，n 是变量 | 必然堆分配 | `var buf [N]byte` + `buf[:0]` |
| 常量 `make`，> 64KB | 堆分配 | 考虑复用（后续 sync.Pool 专题） |
| 闭包被返回/存起来 | 捕获变量 + funcval 上堆 | 提到循环外，或用结构体方法 |
| 拿不准 | | `make escape` 问编译器 |

---

## 三个坑

这三个都是写这个专题时真踩到的，比结论本身更值钱。

### 坑一：你以为在测 64KB 栈分配，其实什么都没测

场景 5 的辅助函数 `touch` 最初没加 `//go:noinline`：

```go
func touch(buf []byte) int {
	total := 0
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = byte(i)
		total += int(buf[i])   // ← 刚写完立刻读同一个下标
	}
	return total
}
```

内联后编译器做 store-to-load forwarding，发现每个读都能直接用刚写的值，
于是**整个 64KB 后备数组都成了死代码**。当时的汇编：

```
SumBufAtLimit STEXT size=48 args=0x0 locals=0x0 ... LEAF|NOFRAME
```

`locals=0x0`、`NOFRAME` —— 栈帧是空的，什么都没分配，基准跑出 **5.3 ns/op**。
直接写进文档就成了"64KB 栈分配只要 5 纳秒"，纯属胡说。

加上 `//go:noinline` 后 `locals=0x10028`（65576 字节），真实数字 1.45 µs，**差 273 倍**。

> **规矩**：任何"快得离谱"的数字，先 `make frames` / `make asm` 确认代码还在，再谈性能。
> `b.Loop()` 只保活函数调用的参数和返回值，管不到被内联展开成纯算术的代码。

### 坑二：跑一次的数字不能信

同一份代码，两次全量运行：

```
BenchmarkPointer_Returned-10    45567601    29.89 ns/op    ← 机器上跑着别的东西
BenchmarkPointer_Returned-10   137322172     8.34 ns/op    ← 干净环境
```

3.8 倍差距，纯粹是后台负载。写这份文档期间又撞上一次：load average 冲到 154 时，
`Format_Sprintf` 从 36.9 ns 变成 59.9–70.1 ns，**而 `B/op` / `allocs/op` 一字未变**。

> **规矩**：`-count` ≥ 6（benchstat 给置信区间的最低样本数），跑之前关掉后台任务，
> 笔记本插电避免降频。**优先看 `allocs/op`** —— 它是编译期定死的，
> `ns/op` 是环境的函数。

### 坑三：基准的输入不像线上的输入

见场景 3 折叠块里的两个例子：`const` 实参让装箱消失、`Itoa(1)` 走静态表。
构造测试数据时问一句：**线上真的长这样吗？**

---

## 附录 A：完整数据与环境

**环境**：Apple M4 / darwin-arm64 / go1.25.4，`-count=8`，benchstat 汇总，采集时机器无显著后台负载。
原始输出在 `.bench/01-escape-analysis.txt`，`make bench` 重新生成。

| 基准 | ns/op | B/op | allocs/op | |
|---|---:|---:|---:|---|
| `Pointer_Returned` | 7.87 ±1% | 16 | 1 | 返回 `*Point` |
| `Pointer_Value` | **1.77 ±3%** | **0** | **0** | 返回 `Point` |
| `Inline_NotEscaping` | **1.75 ±1%** | **0** | **0** | `&Point{}` 内联后留栈上 |
| `Inline_Leaked` | 8.18 ±1% | 16 | 1 | 同样的 `&Point{}`，泄漏给全局 |
| `Format_Sprintf` | 36.9 ±1% | 16 | 2 | `fmt.Sprintf("%d", n)` |
| `Format_Itoa` | 13.2 ±3% | 8 | 1 | `strconv.Itoa(n)` |
| `Format_AppendReuseBuf` | **8.55 ±27%** | **0** | **0** | `strconv.AppendInt(buf[:0], …)` |
| `Make_ConstLen` | 39.2 ±16% | **0** | **0** | `make([]byte, 64)` |
| `Make_VarLen` | 56.0 ±10% | 64 | 1 | `make([]byte, n)`，n 恒为 64 |
| `Make_64KB` | 1.45 µs ±8% | **0** | **0** | `make([]byte, 65536)` |
| `Make_64KBPlus1` | 3.90 µs ±18% | 73728 | 1 | `make([]byte, 65537)` |
| `Closure_Local` | 31.5 ±16% | **0** | **0** | 闭包只在本函数内调用 |
| `Closure_Returned` | 99.2 ±5% | 24 | 2 | 闭包被返回 |

**读这张表的三个注意事项：**

1. `ns/op` 只在这台机器、这一次运行内可比。跨机器、跨运行请只比 `B/op` 和 `allocs/op`。
2. **跨组不可比**。`Make_ConstLen`（64 字节逐字节读写）和 `Make_64KB`（按 4096 步长访问 64KB）
   工作量完全不同，虽然都是 0 分配。
3. `±27%`、`±18%` 这类波动率本身就是警告：**这几个数别用来做精细比较。**

**验证组**（`make bench BENCH='Verify_'`）不在上表里，它只用来检验分配构成的拆解，
所以只看分配指标：

| 基准 | B/op | allocs/op | 验证什么 |
|---|---:|---:|---|
| `Verify_SprintfShort` → `Verify_SprintfLong` | 16 → 24 | 2 → 2 | 场景 3 的拆解 ✅ |
| `Verify_ItoaShort` → `Verify_ItoaLong` | 8 → 16 | 1 → 1 | 场景 3 的拆解 ✅ |
| `Verify_Closure1Var` → `Verify_Closure2Vars` | 24 → 40 | 2 → **2** | 场景 6 的拆解 ⚠️ 部分猜错 |

## 附录 B：实验是怎么设计的

按 [METHODOLOGY.md](../../METHODOLOGY.md) 的要求，六组对照每组只改一个变量，工作量等价：

| 场景 | 变的是什么 | 控制住的是什么 |
|---|---|---|
| 1 | 返回类型 `*Point` vs `Point` | 两个函数都 `//go:noinline`，隔离栈帧，排除内联干扰 |
| 2 | 是否把指针存进包级变量 | 两个函数都可内联，字面量完全相同，**只差一行赋值** |
| 3 | 格式化 API | 同一个输入值，同样产出十进制字符串 |
| 4 | `make` 长度是常量还是变量 | 实参恒为 64，循环体逐字节读写完全一致 |
| 5 | 常量长度 64KB vs 64KB+1 | 同一个 `touch`，按 4096 步长访问，多出的 1 字节不改变访问次数 |
| 6 | 闭包是否被返回 | 都做 100 次累加 |

基准统一用 Go 1.24 的 `testing.B.Loop()`：循环体内**函数调用**的参数和返回值自动保活，
不需要 `var sink T` 样板；循环外的准备代码不计时。
边界见坑一 —— 保活只覆盖函数调用边界。

**关于假说**：本专题是仓库第一个专题，写作顺序是先做实验后补解释。
只有两条拆解（场景 3 和场景 6 的折叠块）是先写下预测再跑数据的，
预测的原始措辞保留在 `escape_test.go` 的验证组注释里 —— 其中一条被推翻了一半。
从专题 02 起，假说和预测会在跑 `make bench` 之前定稿。

## 附录 C：延伸阅读

- `$(go env GOROOT)/src/cmd/compile/internal/escape/escape.go` —— 开头 60 行注释是逃逸分析最好的入门文档
- `$(go env GOROOT)/src/cmd/compile/internal/ir/cfg.go` —— 栈变量大小阈值
- `$(go env GOROOT)/src/runtime/malloc.go` —— `mallocgc` 的完整路径、tiny allocator、size class
- `go doc testing.B.Loop`

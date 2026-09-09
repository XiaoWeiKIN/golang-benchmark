# golang-benchmark

用**可复现的基准 + 编译器/运行时证据**来学 Go 的实现原理和性能优化。

每个专题是一个独立的包，包含三样东西：

- **一组对照基准** —— 同一件事的几种写法，差异必须能在数字上看见；
- **一份原理文档** —— 讲清楚 runtime / 编译器层面为什么会有这个差异，附实测数据；
- **可验证的证据** —— 逃逸分析输出、汇编栈帧、pprof 热点，而不是"据说"。

做法上照搬科学方法：**先写下可证伪的假说和具体预测，再跑实验，然后如实记录哪些被推翻。**
文档里的每个数字都来自这个仓库里真实跑出来的基准，任何"这样写更快"的说法
都要能用 `make bench` 复现，或者用 `make escape` / `make asm` 指出编译器到底做了什么。
完整约束见 [METHODOLOGY.md](METHODOLOGY.md)。

---

## 快速开始

```bash
make tools                              # 安装 benchstat
make list                               # 列出所有专题
make bench   TOPIC=01-escape-analysis   # 跑基准 + benchstat 汇总
make escape  TOPIC=01-escape-analysis   # 看逃逸分析决策
make frames  TOPIC=01-escape-analysis   # 按栈帧大小列出所有函数
make asm     TOPIC=01-escape-analysis FUNC=SumBufAtLimit
make cpu     TOPIC=01-escape-analysis   # CPU profile 热点
make help                               # 全部目标
```

改代码前后做对比：

```bash
make base TOPIC=01-escape-analysis      # 存基线
# ... 改代码 ...
make cmp  TOPIC=01-escape-analysis      # benchstat 给出变化幅度和 p 值
```

环境要求：Go 1.24+（基准统一使用 `testing.B.Loop`）。

---

## 目录约定

```
METHODOLOGY.md     方法论，所有专题都按它执行
topics/<NN>-<slug>/
├── README.md      教程正文（场景 × N）+ 一页速查 + 坑 + 附录（数据/实验设计/延伸阅读）
├── xxx.go         被测代码，每组对照写成可单独调用的函数
└── xxx_test.go    基准，命名为 Benchmark<组名>_<变体>
```

基准命名用 `_` 分组（`BenchmarkFormat_Sprintf` / `BenchmarkFormat_Itoa`），
这样 `make bench BENCH='Format_'` 就能只跑一组。

`Verify_` 前缀留给**验证组** —— 用来检验文档里对 `B/op` 构成的拆解是否成立的基准。
事前写下的预测就放在这些基准的注释里，和结果一起保留（见 01 专题）。

---

## 专题路线图

### 一、内存与分配

| | 专题 | 核心问题 | 状态 |
|---|---|---|---|
| 01 | [逃逸分析](topics/01-escape-analysis/) | 一个对象凭什么留在栈上 | ✅ |
| 02 | [slice 扩容与预分配](topics/02-slice-growth/) | `append` 的增长曲线，`make` 该给多少 cap | ✅ |
| 03 | map 的实现与预分配 | Go 1.24 换成 Swiss Table 之后变了什么 | 待写 |
| 04 | [字符串：拷贝、零拷贝与驻留](topics/04-strings/) | 转换何时拷贝，驻留省下的到底是什么 | ✅ |
| 05 | sync.Pool | 复用怎么和 GC、P 本地缓存配合，什么时候反而更慢 | 待写 |
| 06 | GC 原理与调优 | 三色标记、写屏障、GOGC / GOMEMLIMIT 怎么选 | 待写 |

### 二、语言机制的运行期成本

| | 专题 | 核心问题 | 状态 |
|---|---|---|---|
| 07 | 接口与动态派发 | itab、类型断言、去虚拟化 | 待写 |
| 08 | defer / panic | 开放编码 defer 之后还剩多少开销 | 待写 |
| 09 | 反射 | 代价在哪，什么时候该换代码生成 | 待写 |
| 10 | 泛型 | GC shape stenciling，泛型 vs 接口 vs 代码生成 | 待写 |

### 三、并发

| | 专题 | 核心问题 | 状态 |
|---|---|---|---|
| 11 | GMP 调度 | goroutine 的创建/切换成本，抢占 | 待写 |
| 12 | mutex / atomic / channel | 三种同步手段在不同竞争度下的表现 | 待写 |
| 13 | false sharing | cache line 对齐能带来多大差别 | 待写 |
| 14 | context | 取消传播链路的实际开销 | 待写 |
| 15 | [读多写少的并发缓存](topics/15-rcu-cache/) | RCU + 攒批合并，以及它什么时候会输 | ✅ |

### 四、工程方法

| | 专题 | 核心问题 | 状态 |
|---|---|---|---|
| 16 | pprof 与 trace 实战 | 从一个慢服务定位到具体那行代码 | 待写 |
| 17 | 编译器优化 | 内联预算、边界检查消除、PGO | 待写 |

---

## 方法论

**这是仓库最重要的一份文档：[METHODOLOGY.md](METHODOLOGY.md)。**

它把[科学方法](https://zh.wikipedia.org/zh-cn/%E7%A7%91%E5%AD%A6%E6%96%B9%E6%B3%95)
的原则落到基准测试上 —— 提出可证伪的假说、推导出具体预测、控制变量做实验、
结果必须可重复、数据和程序充分公开。十一条原则，概括起来：

1. 问题指向**机制**，不指向排名
2. **假说必须可证伪，且写在跑基准之前**
3. 预测落到 `allocs/op` 和编译器输出上，`ns/op` 不作判据
4. 对照实验一次只动一个变量
5. `-count` ≥ 8 + benchstat，单次结果不是数据
6. **先量出自己的分辨率再谈差异** —— 留一对代码完全相同的基准当噪声地板
7. **先确认实验测到的真是你以为的东西** —— 编译器会把代码删掉
8. 基准数字 / 编译器证据 / 源码机制，三条链必须吻合
9. 环境、原始数据、复现命令全部公开
10. 如实记录被推翻的假说和翻车过程
11. 结论标注版本、架构、失效条件

但这十条约束的是**怎么做实验**，不是怎么写文档。专题 README 的主线是教程：
**现象 → 机制 → 怎么用**，每个场景四拍走完；完整数据、证据链对账、假说记录
收进折叠块和附录，可查但不挡路。

---

## License

Apache 2.0，见 [LICENSE](LICENSE)。

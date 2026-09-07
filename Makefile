# golang-benchmark —— 跑基准、看编译器决策、抓 profile 的统一入口
#
# 常用：
#   make bench TOPIC=01-escape-analysis          跑基准并用 benchstat 汇总
#   make base  TOPIC=01-escape-analysis          把当前结果存成对比基线
#   make cmp   TOPIC=01-escape-analysis          改代码后和基线对比
#   make escape TOPIC=01-escape-analysis         看逃逸分析决策
#   make asm   TOPIC=01-escape-analysis FUNC=Sum 看某个函数的汇编
#   make cpu   TOPIC=01-escape-analysis          抓 CPU profile 并打印热点

TOPIC     ?= 01-escape-analysis
BENCH     ?= .
COUNT     ?= 8
BENCHTIME ?= 1s
FUNC      ?=

PKG      := ./topics/$(TOPIC)
OUT      := .bench
NEW      := $(OUT)/$(TOPIC).txt
BASE     := $(OUT)/$(TOPIC).base.txt
BENCHSTAT := $(shell go env GOPATH)/bin/benchstat

# -benchmem 给出每次操作的分配字节数和分配次数，是本仓库最关心的两列。
GOTEST := go test -run '^$$' -bench '$(BENCH)' -benchmem -count=$(COUNT) -benchtime=$(BENCHTIME)

.DEFAULT_GOAL := help

## bench: 跑基准，结果写入 bench/<TOPIC>.txt 并用 benchstat 汇总
.PHONY: bench
bench: | $(OUT) $(BENCHSTAT)
	$(GOTEST) $(PKG) | tee $(NEW)
	@echo
	@$(BENCHSTAT) $(NEW)

## base: 把当前基准结果保存为基线（改代码前先跑一次）
.PHONY: base
base: | $(OUT) $(BENCHSTAT)
	$(GOTEST) $(PKG) | tee $(BASE)

## cmp: 用 benchstat 对比基线和当前结果，给出变化幅度和 p 值
.PHONY: cmp
cmp: | $(OUT) $(BENCHSTAT)
	@test -f $(BASE) || { echo "缺少基线，先跑：make base TOPIC=$(TOPIC)"; exit 1; }
	$(GOTEST) $(PKG) | tee $(NEW)
	@echo
	@$(BENCHSTAT) $(BASE) $(NEW)

## escape: 打印逃逸分析决策（-l 关内联，看"纯"逃逸结论）
.PHONY: escape
escape:
	go build -gcflags='-m -l' $(PKG) 2>&1 | grep -v '^#'

## escape-inline: 打印允许内联时的逃逸决策，对比 make escape 可看出内联的影响
.PHONY: escape-inline
escape-inline:
	go build -gcflags='-m' $(PKG) 2>&1 | grep -v '^#'

## asm: 打印函数汇编，FUNC 为函数名前缀。看 locals=0x... 可知栈帧大小
.PHONY: asm
asm:
	@test -n "$(FUNC)" || { echo "用法：make asm TOPIC=$(TOPIC) FUNC=<函数名>"; exit 1; }
	@go build -gcflags='-S' $(PKG) 2>&1 \
	  | awk '/\.$(FUNC)[A-Za-z0-9_]* STEXT/,/^\t0x0000 [0-9a-f]{2} /' \
	  | grep -v -e FUNCDATA -e PCDATA

## frames: 按栈帧大小列出包内所有函数，快速定位谁在栈上开了大块
.PHONY: frames
frames:
	@go build -gcflags='-S' $(PKG) 2>&1 | grep STEXT \
	  | sed -E 's/.*\.([A-Za-z0-9_]+) STEXT.*locals=0x([0-9a-f]+).*/\2 \1/' \
	  | while read -r hex name; do printf '%8d  %s\n' "$$((16#$$hex))" "$$name"; done \
	  | sort -rn

## cpu: 抓 CPU profile 并打印热点
.PHONY: cpu
cpu: | $(OUT)
	$(GOTEST) -cpuprofile=$(OUT)/$(TOPIC).cpu.pprof $(PKG) > /dev/null
	go tool pprof -top -nodecount=20 $(OUT)/$(TOPIC).cpu.pprof

## mem: 抓内存 profile 并按分配次数打印热点
.PHONY: mem
mem: | $(OUT)
	$(GOTEST) -memprofile=$(OUT)/$(TOPIC).mem.pprof $(PKG) > /dev/null
	go tool pprof -top -sample_index=alloc_objects -nodecount=20 $(OUT)/$(TOPIC).mem.pprof

## list: 列出所有专题
.PHONY: list
list:
	@ls -1 topics

## check: gofmt + go vet + 跑一遍测试
.PHONY: check
check:
	@test -z "$$(gofmt -l topics)" || { echo "以下文件未格式化："; gofmt -l topics; exit 1; }
	go vet ./...
	go test ./...

## tools: 安装 benchstat
.PHONY: tools
tools: $(BENCHSTAT)

$(BENCHSTAT):
	go install golang.org/x/perf/cmd/benchstat@latest

$(OUT):
	@mkdir -p $(OUT)

## help: 显示所有可用目标
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
	@echo
	@echo "  变量：TOPIC(=$(TOPIC)) BENCH(=$(BENCH)) COUNT(=$(COUNT)) BENCHTIME(=$(BENCHTIME))"

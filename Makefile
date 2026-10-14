# Bootstrap (bs) —— 开发与发布任务
#
# 所有 Go 命令都通过 scripts/devenv.sh 包裹：它把 GOPATH/GOCACHE 指到仓库内，
# 便于在受限环境（只读 $HOME、CI 容器）里构建，同时对本机开发无副作用。

SHELL := /bin/bash
ROOT  := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
BIN   := $(ROOT)/bin/bs
GO    := go
GOENV := . "$(ROOT)/scripts/devenv.sh" 2>/dev/null || true;

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# lint/vuln 用的二进制版本（`make tools` 安装）。刻意固定版本：
# 不同版本的 golangci-lint 结论不同，不固定会让"本地绿、CI 红"变成常态。
GOLANGCI_VERSION    ?= v2.11.4
GOVULNCHECK_VERSION ?= v1.2.0
# 发布打包用（`make snapshot` / `make release-check`）
GORELEASER_VERSION  ?= v2.18.3
# lint/vuln 的二进制装在 $(GOPATH)/bin（受限环境里 devenv.sh 会把它指到仓库内），
# 它未必在 PATH 上，所以这里显式补进搜索路径。
GOTOOLBIN := $(shell . "$(ROOT)/scripts/devenv.sh" 2>/dev/null; go env GOPATH)/bin

PKG      := github.com/kingfs/Bootstrap/internal/buildinfo
LDFLAGS  := -s -w \
	-X $(PKG).Version=$(VERSION) \
	-X $(PKG).Commit=$(COMMIT) \
	-X $(PKG).Date=$(DATE)

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: help
help:
	@echo "常用目标："
	@echo "  make build      构建 $(BIN)"
	@echo "  make test       单元测试"
	@echo "  make race       单元测试（-race）"
	@echo "  make smoke      无副作用端到端冒烟测试（沙箱目录）"
	@echo "  make pty        在真 PTY 里验证交互式界面（空格勾选/取消/退出码）"
	@echo "  make check      fmt + vet + test + race"
	@echo "  make lint       golangci-lint 静态检查（需先 make tools-lint；不进 check，但进 CI）"
	@echo "  make vuln       依赖已知漏洞扫描（需网络访问 vuln.go.dev；进 CI）"
	@echo "  make coverage-check  单元测试+覆盖率门禁（阈值 COVER_MIN，进 CI）"
	@echo "  make tools      安装 lint/vuln/release 需要的全部二进制（需网络）"
	@echo "  make tools-lint / tools-vuln / tools-release  只装其中一组"
	@echo "  make cross      交叉编译到 6 个平台组合（dist/）"
	@echo "  make docker-matrix  在 ubuntu/debian 裸镜像里跑新机器流程（需 Docker，不进 check）"
	@echo "  make docker-integration  在干净镜像里跑真网络集成测试（含代理全链路；需 Docker+外网）"
	@echo "  make snapshot   goreleaser 本地打包快照"
	@echo "  make release-check  发布流程彩排：打包后用 install.sh 从本地站点真装一遍"
	@echo "  make clean      清理 bin/ dist/ .cache/"

.PHONY: build
build:
	$(GOENV) mkdir -p "$(ROOT)/bin" && $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o "$(BIN)" ./cmd/bs
	@echo "构建完成：$(BIN)"

.PHONY: install
install: build
	$(GOENV) $(GO) install -trimpath -ldflags '$(LDFLAGS)' ./cmd/bs

.PHONY: fmt
fmt:
	$(GOENV) gofmt -l -w ./cmd ./internal ./pkg ./tools

.PHONY: fmt-check
fmt-check:
	@out=$$($(GOENV) gofmt -l ./cmd ./internal ./pkg ./tools); \
	if [ -n "$$out" ]; then echo "以下文件未格式化："; echo "$$out"; exit 1; fi
	@echo "gofmt 检查通过"

.PHONY: vet
vet:
	$(GOENV) $(GO) vet ./...

.PHONY: test
test:
	$(GOENV) $(GO) test ./...

.PHONY: race
race:
	$(GOENV) $(GO) test -race ./...

# lint 与 vuln 刻意**不进 make check**：
#   - lint 比 go vet 慢一个量级，而且需要额外安装二进制（协作者不装就该能跑 check）；
#   - vuln 要联网查 vuln.go.dev，无网络环境下会失败——那不该拦住本地提交。
# 但发版前与改动依赖时应当手动跑一遍（见 AGENTS §3）。
.PHONY: lint
lint:
	@PATH="$(GOTOOLBIN):$$PATH" command -v golangci-lint >/dev/null 2>&1 || { \
		echo "未找到 golangci-lint；先运行 make tools（或把 $(GOLANGCI_VERSION) 装进 PATH）"; exit 1; }
	$(GOENV) PATH="$(GOTOOLBIN):$$PATH" golangci-lint run ./...

.PHONY: vuln
vuln:
	@PATH="$(GOTOOLBIN):$$PATH" command -v govulncheck >/dev/null 2>&1 || { \
		echo "未找到 govulncheck；先运行 make tools（或把 $(GOVULNCHECK_VERSION) 装进 PATH）"; exit 1; }
	$(GOENV) PATH="$(GOTOOLBIN):$$PATH" govulncheck ./...

.PHONY: tools
tools: tools-lint tools-vuln tools-release

# 拆成三组：CI 的 lint / vuln / release 门禁各装自己那份，
# 免得"只想跑 lint"却要顺带编译 goreleaser（几分钟）。
.PHONY: tools-lint
tools-lint:
	$(GOENV) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	@echo "已安装 golangci-lint $(GOLANGCI_VERSION)"

.PHONY: tools-vuln
tools-vuln:
	$(GOENV) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	@echo "已安装 govulncheck $(GOVULNCHECK_VERSION)"

.PHONY: tools-release
tools-release:
	$(GOENV) $(GO) install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)
	@echo "已安装 goreleaser $(GORELEASER_VERSION)"

.PHONY: cover
cover:
	$(GOENV) $(GO) test -coverprofile=.cache/coverage.out ./... && $(GO) tool cover -func=.cache/coverage.out | tail -20

# 覆盖率门禁：CI 用它拦住"悄悄掉下去"，本地也可以随时跑。
#
# 阈值刻意留了几点余量（写这份时的总覆盖率见 docs/design.md §4 的说明）：只许往上抬，
# 掉下来要么补齐测试，要么在提交信息里说清为什么这次例外。
COVER_MIN ?= 70
.PHONY: coverage-check
coverage-check:
	@mkdir -p "$(ROOT)/.cache"
	$(GOENV) $(GO) test -coverprofile="$(ROOT)/.cache/coverage.out" -covermode=atomic ./...
	@total="$$($(GOENV) $(GO) tool cover -func="$(ROOT)/.cache/coverage.out" | sed -n 's/^total:[[:space:]]*.*[[:space:]]\([0-9.]*\)%$$/\1/p')"; \
	case "$$total" in ''|*[!0-9.]*) echo "覆盖率解析失败"; exit 1 ;; esac; \
	echo "总覆盖率：$${total}%（门禁 $(COVER_MIN)%）"; \
	awk -v t="$$total" -v m="$(COVER_MIN)" 'BEGIN { exit (t + 0 >= m + 0) ? 0 : 1 }' || { \
		echo "总覆盖率低于门禁 $(COVER_MIN)%：补测试，或说明为什么这次例外"; exit 1; }

.PHONY: smoke
smoke: build
	$(ROOT)/scripts/smoke.sh

# pty-check 需要 /dev/ptmx：用真 PTY 驱动交互式界面（详见 scripts/pty-check.py 头部说明）。
# 命令本身都是 --dry-run，读写只发生在 .cache/pty 里。
.PHONY: pty
pty: build
	@# 干跑下只打计划表（结果表是同一份内容的翻版，已不再重复），所以断言"计划算出来了"。
	@python3 "$(ROOT)/scripts/pty-check.py" --quiet \
		--expect-code 0 --expect-text "（dry-run：以下操作都不会被执行）" \
		--expect-text "个需要处理" \
		--keys n DOWN ENTER -- init --dry-run
	@python3 "$(ROOT)/scripts/pty-check.py" --quiet \
		--expect-code 0 --expect-text "已取消，未做任何修改" \
		--keys DOWN DOWN ENTER WAIT2 CTRLC -- init --dry-run
	@# 破坏性操作的确认框：只按回车必须走"默认拒绝"，绝不能悄悄开装
	@# （duf 只是"多半没装"的普通 CLI，用来触发确认框；按 n/回车都不会真的安装）
	@python3 "$(ROOT)/scripts/pty-check.py" --quiet \
		--expect-code 0 --expect-text "已取消" \
		--keys ENTER -- tool install duf
	@# 勾选式安装：-i 出复选框界面，回车＝用默认（缺失/可升级的已勾选），再 preview 干跑
	@python3 "$(ROOT)/scripts/pty-check.py" --quiet \
		--expect-code 0 --expect-text "x toggle" \
		--expect-text "（dry-run：以下操作都不会被执行）" \
		--keys ENTER -- tool install -i --dry-run
	@echo "交互式界面 PTY 校验通过"

# e2e 需要网络：真从 GitHub Release 下载并校验，产物只落在 .cache/e2e
.PHONY: e2e
e2e: build
	$(ROOT)/scripts/e2e-install.sh

# Docker 集成矩阵：把 `make cross` 的**静态**二进制丢进干净镜像里跑新机器流程。
# 依赖 cross 而不是 build：本地 build 是动态链接的，容器里可能因 libc 版本而失败，
# 而发布产物正是 cross 这一份（CGO_ENABLED=0）。
# 需要 Docker 与网络（仅用于拉镜像）——刻意不进 check。
.PHONY: docker-matrix
docker-matrix: cross
	$(ROOT)/scripts/docker-matrix.sh

.PHONY: docker-integration
docker-integration: cross
	$(ROOT)/scripts/docker-integration.sh

.PHONY: check
check: fmt-check vet test race

.PHONY: cross
cross:
	@for p in $(PLATFORMS); do \
		os=$${p%%/*}; arch=$${p##*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		out="$(ROOT)/dist/$$os-$$arch/bs$$ext"; \
		mkdir -p "$$(dirname "$$out")"; \
		echo "==> $$os/$$arch"; \
		( $(GOENV) CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath \
			-ldflags '$(LDFLAGS)' -o "$$out" ./cmd/bs ) || exit 1; \
	done
	@echo "交叉编译完成：$(ROOT)/dist"

.PHONY: snapshot
snapshot:
	$(GOENV) PATH="$(GOTOOLBIN):$$PATH" goreleaser release --snapshot --clean

# 发布流程彩排：先打快照，再把 dist/ 摆成本地"下载站"，让 scripts/install.sh 真装一遍
# （校验和、失败路径、版本号都验）。不碰 GitHub，产物只在 .cache/release-check。
# 想反复调试不用重打包时，直接跑 scripts/release-check.sh（可配 DIST_DIR/WORK_DIR）。
.PHONY: release-check
release-check: snapshot
	$(ROOT)/scripts/release-check.sh

.PHONY: clean
clean:
	rm -rf "$(ROOT)/bin" "$(ROOT)/dist" "$(ROOT)/.cache" "$(ROOT)/coverage.out"

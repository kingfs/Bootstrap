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
	@echo "  make check      fmt + vet + test + race"
	@echo "  make cross      交叉编译到 6 个平台组合（dist/）"
	@echo "  make snapshot   goreleaser 本地打包快照"
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

.PHONY: cover
cover:
	$(GOENV) $(GO) test -coverprofile=coverage.out ./... && $(GO) tool cover -func=coverage.out | tail -20

.PHONY: smoke
smoke: build
	$(ROOT)/scripts/smoke.sh

# e2e 需要网络：真从 GitHub Release 下载并校验，产物只落在 .cache/e2e
.PHONY: e2e
e2e: build
	$(ROOT)/scripts/e2e-install.sh

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
	$(GOENV) goreleaser release --snapshot --clean

.PHONY: clean
clean:
	rm -rf "$(ROOT)/bin" "$(ROOT)/dist" "$(ROOT)/.cache" "$(ROOT)/coverage.out"

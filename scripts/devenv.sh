#!/usr/bin/env bash
# Bootstrap 开发环境变量脚本。
#
# 作用：把 Go 的模块缓存与构建缓存放到仓库内的 .cache/ 目录（该目录已 gitignore）。
# 为什么需要：在某些受限环境（容器沙箱、只读 HOME、CI）中 $HOME/go 与 $HOME/.cache/go-build
# 不可写，Go 会以 "read-only file system" 失败。显式指定缓存位置可让构建在任何环境下工作，
# 同时不污染仓库（.cache/ 已在 .gitignore 中）。
#
# 用法：
#   . scripts/devenv.sh && go build ./...
#   . scripts/devenv.sh && go test ./...
#
# 若你的环境默认缓存可用，也可以完全不使用本脚本。

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)"

export GOPATH="${BOOTSTRAP_DEV_GOPATH:-$ROOT/.cache/gopath}"
export GOMODCACHE="$GOPATH/pkg/mod"
export GOCACHE="${BOOTSTRAP_DEV_GOCACHE:-$ROOT/.cache/gobuild}"
export GOFLAGS="${GOFLAGS:--mod=mod}"

mkdir -p "$GOMODCACHE" "$GOCACHE" 2>/dev/null || true

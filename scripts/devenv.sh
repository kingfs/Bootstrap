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

# 仓库根目录。两点务必保留（这里踩过坑）：
#  1. 变量叫 BOOTSTRAP_ROOT 而不是 ROOT：ROOT 这个名字太常见，source 本文件会把调用方的
#     ROOT 覆盖掉（scripts/release-check.sh 的 ROOT 就这样变成了 `/`，导致后面
#     `diff "$ROOT/scripts/install.sh"` 之类的判断全部失灵）。
#  2. `BASH_SOURCE[0]` 是 bash 专有：用 POSIX sh（dash）source 本文件时算出来的是 `/`。
#     所以算完必须校验"这里确实有 go.mod"，对不上就退回当前目录——宁可不生效，
#     也不能把 Go 的缓存写到 `/` 底下。
if [ -z "${BOOTSTRAP_ROOT:-}" ]; then
  _self="${BASH_SOURCE[0]:-$0}"
  _candidate="$(cd "$(dirname "$_self")/.." 2>/dev/null && pwd || pwd)"
  if [ -f "$_candidate/go.mod" ]; then
    BOOTSTRAP_ROOT="$_candidate"
  else
    BOOTSTRAP_ROOT="$PWD"
  fi
  unset _self _candidate
fi
export BOOTSTRAP_ROOT

export GOPATH="${BOOTSTRAP_DEV_GOPATH:-$BOOTSTRAP_ROOT/.cache/gopath}"
export GOMODCACHE="$GOPATH/pkg/mod"
export GOCACHE="${BOOTSTRAP_DEV_GOCACHE:-$BOOTSTRAP_ROOT/.cache/gobuild}"
export GOFLAGS="${GOFLAGS:--mod=mod}"
# golangci-lint 默认把分析缓存写到 $HOME/.cache/golangci-lint，受限环境下会刷一屏
# "read-only file system" 警告（不影响结论，但很吵，且每次都重新分析）。
export GOLANGCI_LINT_CACHE="${BOOTSTRAP_DEV_LINTCACHE:-$BOOTSTRAP_ROOT/.cache/golangci-lint}"

mkdir -p "$GOMODCACHE" "$GOCACHE" "$GOLANGCI_LINT_CACHE" 2>/dev/null || true

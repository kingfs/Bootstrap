#!/bin/sh
# 在干净容器里跑"新机器"流程（Docker 集成矩阵）。
#
# 为什么需要它：scripts/smoke.sh 跑在开发机上——那里 git/curl/go/uv 一应俱全，
# 于是"缺命令时会发生什么"永远测不到。这个脚本把 `make cross` 产出的**静态二进制**
# 丢进 ubuntu/debian 的最小镜像里跑一遍（`--network none`），专门验证：
#   · 缺 git/curl 时不崩、给出可行动的结论；
#   · 不偷偷依赖宿主环境（PATH 收敛、环境变量全清）；
#   · 断网时所有只读/预览命令都能正常返回；
#   · 受管块、配置、dry-run 的行为与开发机上一致。
#
# 需要 Docker（能拉镜像）与网络（只用于拉镜像）。不进 `make check`：
# 它慢、要 Docker，而且与本机工具链无关。用法：make docker-matrix
#
# 用法：
#   scripts/docker-matrix.sh [镜像...]
# 环境变量：
#   BS_IMAGES   镜像列表（默认 "ubuntu:24.04 debian:12"）
#   BS_BIN      要测试的二进制（默认按本机架构取 dist/linux-<arch>/bs）

set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

arch="$(go env GOARCH 2>/dev/null || uname -m)"
case "$arch" in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
esac

BIN="${BS_BIN:-$ROOT/dist/linux-$arch/bs}"
if [ ! -x "$BIN" ]; then
  echo "找不到可执行文件：$BIN" >&2
  echo "先跑 make cross（或设置 BS_BIN 指向某个静态构建）" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "没有 docker：这个矩阵需要 Docker 才能跑" >&2
  exit 1
fi

if [ "$#" -gt 0 ]; then
  images="$*"
else
  images="${BS_IMAGES:-ubuntu:24.04 debian:12}"
fi

WORK="$ROOT/.cache/docker-matrix"
rm -rf "$WORK"
mkdir -p "$WORK"

# 静态二进制才谈得上"丢进任何镜像都能跑"。动态链接的本地构建在这里会被立刻发现。
if command -v file >/dev/null 2>&1; then
  if file "$BIN" | grep -q "statically linked"; then
    echo "==> 二进制：$BIN（静态）"
  else
    echo "!! 警告：$BIN 不是静态链接的；容器里可能因缺 libc 版本而失败" >&2
    echo "   （用 make cross 产物，或 CGO_ENABLED=0 go build）" >&2
  fi
fi

total_fail=0
for image in $images; do
  name="$(printf '%s' "$image" | tr '/:' '__')"
  dir="$WORK/$name"
  mkdir -p "$dir"
  echo
  echo "==> 镜像 $image（--network none）"
  if ! docker pull -q "$image" >/dev/null 2>&1; then
    echo "   拉取失败（离线或镜像名写错），跳过" >&2
    total_fail=$((total_fail + 1))
    continue
  fi
  if docker run --rm --network none \
    -v "$BIN:/usr/local/bin/bs:ro" \
    -v "$ROOT/scripts/docker-check.sh:/check.sh:ro" \
    -v "$dir:/work" \
    -e WORK_DIR=/work \
    "$image" sh /check.sh; then
    echo "   $image：通过"
  else
    echo "   $image：失败" >&2
    total_fail=$((total_fail + 1))
  fi
done

echo
if [ "$total_fail" -eq 0 ]; then
  echo "Docker 集成矩阵全部通过（沙箱目录 $WORK，可安全删除）"
  exit 0
fi
echo "Docker 集成矩阵：$total_fail 个镜像失败" >&2
exit 1

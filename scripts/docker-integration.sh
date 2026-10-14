#!/bin/sh
# 在**有网络的干净容器**里跑 bs 的真实集成测试。
#
# 与已有两条测试轴的分工：
#   make check / smoke       —— 开发机上，无副作用、不联网；
#   make docker-matrix       —— 干净镜像 + `--network none`（断网行为）；
#   make docker-integration  —— 干净镜像 + **真网络**（本脚本）：apt 路径、
#                               GitHub Release 下载 + sha256、直链路径、代理全链路。
#
# 为什么单独一条：`make e2e` 虽然也真下载，但它跑在开发机上——那里 git/curl/CA 证书
# 一应俱全，因此永远看不到"全新最小镜像第一公里"的问题（本脚本第一次跑就抓到：
# ubuntu:24.04 的 docker 镜像没有系统 CA 证书，于是所有 HTTPS 下载都失败在 x509 上，
# 而错误里没提该装什么）。
#
# 需要 Docker 与网络。不进 `make check`：它慢、要 Docker、要外网，还可能被镜像源限速。
#
# 用法：
#   scripts/docker-integration.sh [镜像...]
# 环境变量：
#   BS_IMAGES      镜像列表（默认 "ubuntu:24.04"）
#   BS_BIN         被测二进制（默认 dist/linux-<arch>/bs，即 make cross 的产物）
#   BS_PROXY_URL   代理地址。**必须自己给**（例如 http://127.0.0.1:7890 或公司代理）：
#                  这里是公开仓库，不内置任何内网地址；给空串则跳过代理阶段。

set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

arch="$(go env GOARCH 2>/dev/null || uname -m)"
case "$arch" in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
esac

BIN="${BS_BIN:-$ROOT/dist/linux-$arch/bs}"
if [ ! -f "$BIN" ]; then
  echo "找不到可执行文件：$BIN" >&2
  echo "先跑 make cross（dist/ 会被 make snapshot --clean 清掉）" >&2
  exit 1
fi
if [ ! -x "$BIN" ]; then
  echo "$BIN 不可执行" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "没有 docker：这个集成测试需要 Docker" >&2
  exit 1
fi

if [ "$#" -gt 0 ]; then
  images="$*"
else
  images="${BS_IMAGES:-ubuntu:24.04}"
fi

PROXY_URL="${BS_PROXY_URL:-}"
# 没给地址就跳过代理阶段（容器里 github.com 直连不通，所以走代理的下载都排在代理配置之后）。
if [ -z "$PROXY_URL" ]; then
  echo "提示：未设置 BS_PROXY_URL，本次跳过代理阶段（要用代理就显式给地址，例如 http://127.0.0.1:7890）" >&2
fi

WORK="$ROOT/.cache/docker-integration"
rm -rf "$WORK"
mkdir -p "$WORK"

# 静态二进制才谈得上"丢进任何镜像都能跑"。
if command -v file >/dev/null 2>&1; then
  if file "$BIN" | grep -q "statically linked"; then
    echo "==> 二进制：${BIN}（静态）"
  else
    echo "!! 警告：$BIN 不是静态链接的；容器里可能因缺 libc 版本而失败" >&2
    echo "   （用 make cross 产物，或 CGO_ENABLED=0 go build）" >&2
    exit 1
  fi
fi
if [ -n "$PROXY_URL" ]; then
  echo "==> 代理：$PROXY_URL"
else
  echo "==> 代理：未提供（跳过代理阶段）"
fi

failed=0
for image in $images; do
  # 每个镜像一个独立工作目录：镜像名里的 `/` 与 `:` 换成 `-`，避免建出奇怪路径。
  dir="$(printf '%s' "$image" | tr '/:' '--')"
  img_work="$WORK/$dir"
  mkdir -p "$img_work/home"
  # 把二进制与内层脚本都放进同一个目录，然后**只挂这一个目录**。
  # 不要 `-v <不存在的文件>:/target`：Docker 会在宿主机上把源路径建成**目录**，
  # 之后 make cross 就再也写不进去（这个坑踩过一次）。
  cp "$BIN" "$img_work/bs"
  cp "$ROOT/scripts/docker-integration-check.sh" "$img_work/integration.sh"
  chmod +x "$img_work/bs" "$img_work/integration.sh"

  echo
  echo "================ $image ================"
  if docker run --rm \
    -v "$img_work:/work" \
    -e BS_BIN=/work/bs \
    -e WORK_DIR=/work \
    -e BS_IMAGE="$image" \
    -e BS_PROXY_URL="$PROXY_URL" \
    "$image" sh /work/integration.sh; then
    echo "---- ${image}：通过"
  else
    echo "---- ${image}：失败" >&2
    failed=1
  fi
done

echo
if [ "$failed" -eq 0 ]; then
  echo "Docker 集成测试全部通过（沙箱目录 ${WORK}，可安全删除）"
  exit 0
fi
echo "Docker 集成测试有失败项（沙箱目录 ${WORK}）" >&2
exit 1

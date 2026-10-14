#!/usr/bin/env sh
#
# Bootstrap (bs) 安装脚本。
#
# 从 GitHub Release 下载对应平台的归档，用 checksums.txt 校验 sha256 后安装。
# 只依赖 sh + curl（或 wget）+ sha256sum（或 shasum）。
#
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/kingfs/Bootstrap/main/scripts/install.sh | sh
#   sh scripts/install.sh --version v0.1.0 --dir ~/.local/bin
#
set -eu

REPO="${BS_REPO:-kingfs/Bootstrap}"
VERSION="${BS_VERSION:-latest}"
INSTALL_DIR="${BS_INSTALL_DIR:-}"
# BS_BASE_URL 覆盖下载前缀：公司镜像、`ghproxy` 之类的加速站，以及本地发布彩排
# （`scripts/release-check.sh` 就是这么用的）。留空时走 GitHub Release 的公开地址。
BASE_URL="${BS_BASE_URL:-}"
ASSUME_YES=0

usage() {
  cat <<'EOF'
用法：install.sh [选项]

  --version <vX.Y.Z>   安装指定版本（默认：latest）
  --dir <目录>         安装目录（默认：~/.local/bin，失败则试 /usr/local/bin）
  --repo <owner/repo>  覆盖仓库（默认 kingfs/Bootstrap）
  --base-url <URL>     覆盖下载前缀（镜像/加速站/本地彩排）
  -y, --yes            不做任何确认
  -h, --help           显示帮助

环境变量：BS_VERSION / BS_INSTALL_DIR / BS_REPO / BS_BASE_URL / HTTPS_PROXY（下载时自动使用）
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:?missing value}"; shift 2 ;;
    --dir) INSTALL_DIR="${2:?missing value}"; shift 2 ;;
    --repo) REPO="${2:?missing value}"; shift 2 ;;
    --base-url) BASE_URL="${2:?missing value}"; shift 2 ;;
    -y|--yes) ASSUME_YES=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数：$1" >&2; usage >&2; exit 2 ;;
  esac
done

say() { printf '%s\n' "$*"; }
err() { printf 'install: %s\n' "$*" >&2; exit 1; }

# ---------- 平台识别 ----------
detect_os() {
  case "$(uname -s)" in
    Linux) echo linux ;;
    Darwin) echo darwin ;;
    *) err "不支持的操作系统：$(uname -s)" ;;
  esac
}
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) err "不支持的架构：$(uname -m)" ;;
  esac
}

OS="$(detect_os)"
ARCH="$(detect_arch)"

# ---------- 下载工具 ----------
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
  fetch_stdout() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
  fetch_stdout() { wget -q -O - "$1"; }
else
  err "需要 curl 或 wget"
fi

# sha256 校验：优先 sha256sum（GNU），退化到 shasum（macOS / busybox）
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    err "需要 sha256sum 或 shasum 来校验下载内容"
  fi
}

# ---------- 解析版本 ----------
if [ "$VERSION" = "latest" ] && [ -n "$BASE_URL" ]; then
  err "使用 --base-url/BS_BASE_URL 时必须显式指定 --version（自定义前缀没有 releases/latest）"
fi
if [ "$VERSION" = "latest" ]; then
  # 不依赖 GitHub API 的鉴权：直接跟随 releases/latest 的重定向取 tag。
  if command -v curl >/dev/null 2>&1; then
    VERSION="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's#.*/tag/##')"
  else
    err "自动探测最新版本需要 curl；请用 --version 指定版本"
  fi
  [ -n "$VERSION" ] || err "无法确定最新版本，请用 --version 指定"
fi
# 统一成 vX.Y.Z 形式
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac

PLAIN_VERSION="${VERSION#v}"
ASSET="bootstrap_${PLAIN_VERSION}_${OS}_${ARCH}.tar.gz"
# 显式给的前缀原样用（末尾斜杠去掉）；否则拼 GitHub Release 的公开地址。
#
# 自定义前缀时**不再有 GitHub 兜底**：`--version` 是必须的（没有公开的
# releases/latest 可跟），这样"镜像拿不到"会明确报错，而不是悄悄去连 GitHub。
if [ -n "$BASE_URL" ]; then
  BASE_URL="${BASE_URL%/}/$VERSION"
else
  BASE_URL="https://github.com/$REPO/releases/download/$VERSION"
fi

say "仓库    : $REPO"
say "版本    : $VERSION"
say "下载前缀: $BASE_URL"
say "平台    : $OS/$ARCH"
say "归档    : $ASSET"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/bs-install.XXXXXX")"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT INT TERM

say "==> 下载"
fetch "$BASE_URL/$ASSET" "$TMP/$ASSET" || err "下载失败：$BASE_URL/$ASSET"

say "==> 校验 sha256"
if fetch "$BASE_URL/checksums.txt" "$TMP/checksums.txt" 2>/dev/null; then
  EXPECTED="$(awk -v a="$ASSET" '$2 == a || $2 == "*"a {print $1}' "$TMP/checksums.txt" | head -1)"
  [ -n "$EXPECTED" ] || err "checksums.txt 中没有 $ASSET 的记录"
  ACTUAL="$(sha256_of "$TMP/$ASSET")"
  [ "$EXPECTED" = "$ACTUAL" ] || err "校验失败：期望 $EXPECTED，实际 $ACTUAL"
  say "    sha256 匹配"
else
  err "无法下载 checksums.txt，出于安全考虑中止安装"
fi

say "==> 解压"
tar -xzf "$TMP/$ASSET" -C "$TMP" || err "解压失败"
[ -f "$TMP/bs" ] || err "归档中找不到 bs 可执行文件"

# ---------- 选择安装目录 ----------
# 默认装到 ~/.local/bin（不新建系统目录）；不可写时下面的 install_to 会去试 /usr/local/bin
[ -n "$INSTALL_DIR" ] || INSTALL_DIR="$HOME/.local/bin"
mkdir -p "$INSTALL_DIR" 2>/dev/null || true

install_to() {
  target="$1/bs"
  if [ -w "$1" ] || { [ ! -e "$target" ] && [ -w "$1" ]; }; then
    chmod +x "$TMP/bs"
    mv -f "$TMP/bs" "$target"
    say "已安装：$target"
    return 0
  fi
  return 1
}

if ! install_to "$INSTALL_DIR"; then
  if [ "$ASSUME_YES" -eq 1 ] || [ -t 0 ]; then
    if command -v sudo >/dev/null 2>&1; then
      say "==> $INSTALL_DIR 不可写，尝试 sudo 安装到 /usr/local/bin"
      sudo mkdir -p /usr/local/bin
      chmod +x "$TMP/bs"
      sudo mv -f "$TMP/bs" /usr/local/bin/bs
      INSTALL_DIR="/usr/local/bin"
      say "已安装：/usr/local/bin/bs"
    else
      err "$INSTALL_DIR 不可写，且没有 sudo"
    fi
  else
    err "$INSTALL_DIR 不可写；请用 --dir 指定其他目录"
  fi
fi

# ---------- 后续提示 ----------
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    say ""
    say "注意：$INSTALL_DIR 不在 PATH 中。请把下面这行加入你的 shell 配置："
    say "  export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac

say ""
say "运行 \`bs version\` 验证；接着可以："
say "  bs config init          # 生成默认配置（含代理 profile 示例）"
say "  bs shell install        # 安装 shell 受管块（代理立即生效 + PATH 装配）"
say "  bs doctor               # 体检"

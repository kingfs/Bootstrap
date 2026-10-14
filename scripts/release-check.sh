#!/bin/sh
# 发布流程彩排：把 `make snapshot` 产出的发布物当作"真的发布过"来走一遍安装。
#
# 这个脚本回答的问题只有一个：**发版那天会不会翻车**。
# 它不碰 GitHub，也不推任何东西：把 dist/ 摆成一个"发布目录"（`<base>/<tag>/…`），
# 用本地 HTTP 服务冒充下载站，然后让 scripts/install.sh 真的去装一次。
#
#   make snapshot          # 先产出 dist/（goreleaser 快照）
#   make release-check     # 再跑这个脚本
#
# 覆盖的路径：
#   1) 6 个平台的产物齐全、命名符合约定，checksums.txt 里每条都能对上，归档内容完整；
#   2) install.sh 用 --base-url 指向本地站点时，能下载 → 校验 sha256 → 解开 → 装好；
#   3) 装出来的二进制能跑，`bs version` 里带着**快照版本号**（说明 ldflags 生效）；
#   4) 校验和对不上 / 缺 checksums.txt / 版本不存在时必须中止，且不留下半个可执行文件；
#   5) 给了 --base-url 却没给 --version 时必须明确报错（自定义前缀没有 releases/latest）。
#
# 只依赖 sh + curl + tar + sha256sum + python3（起本地 HTTP 服务）。
# 所有产物只落在 .cache/release-check 里，跑完可以随时删。

set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="${DIST_DIR:-$ROOT/dist}"
WORK="${WORK_DIR:-$ROOT/.cache/release-check}"
INSTALLER="$ROOT/scripts/install.sh"

pass=0
fail=0

ok() { printf '  \033[32mok\033[0m   %s\n' "$1"; pass=$((pass + 1)); }
# warn 不计入断言：它提示"这次彩排的结论可能不可信"，但不该让脚本红掉
# （直接跑本脚本、拿一份旧 dist/ 来对照时会出现这种情况）。
warn() { printf '  \033[33mwarn\033[0m %s\n' "$1"; }
bad() {
  printf '  \033[31mFAIL\033[0m %s\n' "$1"
  fail=$((fail + 1))
}
assert_contains() { # 名称 期望 实际
  case "$3" in
  *"$2"*) ok "$1" ;;
  *) bad "$1"; printf '       期望包含: %s\n       实际输出: %s\n' "$2" "$3" ;;
  esac
}
assert_eq() { # 名称 期望 实际
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1"; printf '       期望: %s\n       实际: %s\n' "$2" "$3"; fi
}
assert_exists() { # 名称 路径
  if [ -e "$2" ]; then ok "$1"; else bad "$1（$2 不存在）"; fi
}
assert_missing() { # 名称 路径
  if [ -e "$2" ]; then bad "$1（$2 不该存在）"; else ok "$1"; fi
}

need() { # 命令名（缺了就退出：这是脚本自身的依赖，不是被测对象的问题）
  command -v "$1" >/dev/null 2>&1 || {
    printf 'release-check: 需要 %s\n' "$1" >&2
    exit 4
  }
}

need curl
need tar
need python3

# sha256_of_file：脚本内部比较"二进制有没有被换掉"用（外部命令存在性在上面检查过）
sha256_of_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || {
  printf 'release-check: 需要 sha256sum 或 shasum\n' >&2
  exit 4
}

if [ ! -d "$DIST" ] || [ -z "$(ls -A "$DIST" 2>/dev/null || true)" ]; then
  printf 'release-check: %s 里没有发布物；先跑 `make snapshot`\n' "$DIST" >&2
  exit 4
fi
if [ ! -f "$INSTALLER" ]; then
  printf 'release-check: 找不到 %s\n' "$INSTALLER" >&2
  exit 4
fi

# 陈旧产物会给出假结论（这条是踩出来的：改了 install.sh 忘了重打包，脚本却"验过了"）
if [ -n "$(find "$ROOT/cmd" "$ROOT/internal" "$ROOT/pkg" "$ROOT/tools" "$ROOT/docs" \
  "$ROOT/scripts/install.sh" "$ROOT/scripts/install.ps1" "$ROOT/README.md" \
  -newer "$DIST/checksums.txt" -type f 2>/dev/null | head -1)" ]; then
  warn "dist/ 比源码旧：先跑 \`make snapshot\`（\`make release-check\` 会先重新打包），否则下面的结论只对新代码之外的东西有效"
fi

# 本机平台（只有本机那一份能真正跑起来；其它平台只检查"在不在、名字对不对"）
case "$(uname -s)" in
  Linux) OS=linux ;;
  Darwin) OS=darwin ;;
  *) printf 'release-check: 不支持的操作系统 %s\n' "$(uname -s)" >&2; exit 4 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) printf 'release-check: 不支持的架构 %s\n' "$(uname -m)" >&2; exit 4 ;;
esac

rm -rf "$WORK"
mkdir -p "$WORK"

SITE_PID=""
stop_site() {
  [ -n "$SITE_PID" ] && kill "$SITE_PID" 2>/dev/null || true
  SITE_PID=""
}
trap 'stop_site' EXIT INT TERM

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

# start_site <目录> <日志名>：起一个本地 HTTP 站点并把端口写进 $SITE_PORT
start_site() {
  SITE_PORT="$(free_port)"
  python3 -m http.server "$SITE_PORT" --bind 127.0.0.1 --directory "$1" >"$WORK/http-$2.log" 2>&1 &
  SITE_PID=$!
  i=0
  while [ "$i" -lt 50 ]; do
    if curl -fsS "http://127.0.0.1:$SITE_PORT/" -o /dev/null 2>/dev/null; then
      return 0
    fi
    i=$((i + 1))
    sleep 0.1
  done
  return 1
}

archive_for() { # os arch → dist 里该平台的归档路径（找不到时输出空）
  for ext in tar.gz zip; do
    candidate="$DIST/bootstrap_${VERSION}_$1_$2.$ext"
    if [ -f "$candidate" ]; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  printf ''
}

# ---------- 版本号：从产物名里认出来（快照形如 0.0.1-next）----------
VERSION=""
for f in "$DIST"/bootstrap_*_"$OS"_"$ARCH".tar.gz; do
  [ -f "$f" ] || continue
  base="$(basename "$f")"
  VERSION="${base#bootstrap_}"
  VERSION="${VERSION%_"$OS"_"$ARCH".tar.gz}"
  break
done
if [ -z "$VERSION" ]; then
  printf 'release-check: dist/ 里没有 %s/%s 的归档，无法推断版本号\n' "$OS" "$ARCH" >&2
  exit 4
fi
TAG="v$VERSION"

# ---------- 1. 产物齐全 + 校验和 ----------
printf '\033[1m== 发布物完整性（版本 %s）\033[0m\n' "$VERSION"
for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os="${p%%/*}"
  arch="${p##*/}"
  assert_exists "有 $os/$arch 归档" "$(archive_for "$os" "$arch")"
done

assert_exists "有 checksums.txt" "$DIST/checksums.txt"
lines="$(wc -l <"$DIST/checksums.txt" | tr -d ' ')"
assert_eq "checksums.txt 覆盖 6 个产物" "6" "$lines"

if command -v sha256sum >/dev/null 2>&1; then
  if (cd "$DIST" && sha256sum -c checksums.txt >/dev/null 2>&1); then
    ok "checksums.txt 里 6 条 sha256 全部对得上"
  else
    bad "checksums.txt 与产物对不上"
    (cd "$DIST" && sha256sum -c checksums.txt 2>&1 | sed 's/^/       /' | head -5) || true
  fi
else
  ok "（跳过 sha256sum -c：本机只有 shasum）"
fi

# ---------- 归档内容 ----------
printf '\n\033[1m== 归档内容\033[0m\n'
LOCAL_ARCHIVE="$(archive_for "$OS" "$ARCH")"
listing="$(tar -tzf "$LOCAL_ARCHIVE")"
assert_contains "归档里有 bs" "bs" "$listing"
assert_contains "归档里有 README.md" "README.md" "$listing"
assert_contains "归档里有 docs/design.md" "docs/design.md" "$listing"
assert_contains "归档里有 scripts/install.sh" "scripts/install.sh" "$listing"
assert_contains "归档里有 scripts/install.ps1" "scripts/install.ps1" "$listing"

WIN_ARCHIVE="$(archive_for windows "$ARCH")"
if [ -n "$WIN_ARCHIVE" ] && command -v unzip >/dev/null 2>&1; then
  win_listing="$(unzip -l "$WIN_ARCHIVE" 2>/dev/null || true)"
  assert_contains "Windows zip 里有 bs.exe" "bs.exe" "$win_listing"
  assert_contains "Windows zip 里有 README.md" "README.md" "$win_listing"
else
  ok "（跳过 Windows zip 内容检查：本机没有 unzip）"
fi

# ---------- 二进制元信息 ----------
printf '\n\033[1m== 二进制元信息\033[0m\n'
mkdir -p "$WORK/unpack"
tar -xzf "$LOCAL_ARCHIVE" -C "$WORK/unpack"
assert_exists "解出来的 bs" "$WORK/unpack/bs"
if command -v file >/dev/null 2>&1; then
  info="$(file "$WORK/unpack/bs")"
  assert_contains "产物是静态链接（CGO_ENABLED=0）" "statically linked" "$info"
else
  ok "（跳过静态链接检查：本机没有 file）"
fi
out="$("$WORK/unpack/bs" version 2>&1)"
assert_contains "bs version 带上快照版本号" "$VERSION" "$out"
assert_contains "产物里带着这次提交（ldflags 或 VCS 戳）" \
  "$(git -C "$ROOT" rev-parse --short=12 HEAD)" "$out"
# 构建时间只在本仓库有 git remote 时才是真的：goreleaser 读不到 remote 就退回假 git 信息
# （Commit=none、CommitDate=0），Date 会渲染成 0001-01-01T00:00:00Z。
# 真发版在 GitHub Actions 里跑，那里一定有 remote，所以这里"有 remote 才断言"。
if git -C "$ROOT" remote get-url origin >/dev/null 2>&1; then
  assert_contains "构建时间是真实时间戳（不是零值）" "built 20" "$out"
else
  ok "（跳过构建时间断言：本仓库没有 git remote，goreleaser 会退回假 git 信息）"
fi

# 归档里带的安装脚本必须与仓库里的一致：用户解包后照着它装，
# 版本不一致会让"发布物"与"仓库"说两套话（这次彩排就发现过脚本改完忘了重打包）。
for f in scripts/install.sh scripts/install.ps1; do
  if tar -xzOf "$LOCAL_ARCHIVE" "$f" 2>/dev/null | diff -q - "$ROOT/$f" >/dev/null 2>&1; then
    ok "归档里的 $f 与仓库一致"
  else
    bad "归档里的 $f 与仓库不一致（重新打包）"
  fi
done

# 内置清单/片段是 go:embed 进二进制的：发布物里必须还在，否则装上也用不了
mkdir -p "$WORK/home"
out="$(env -i PATH=/usr/bin:/bin HOME="$WORK/home" "$WORK/unpack/bs" tool list 2>&1)"
assert_contains "发布物里的内置清单可用（embed 没漏）" "golang" "$out"

# ---------- 2. 摆一个"发布目录"，用本地 HTTP 服务冒充下载站 ----------
printf '\n\033[1m== 用 install.sh 真装一遍（本地站点）\033[0m\n'
SITE="$WORK/site"
mkdir -p "$SITE/$TAG"
cp "$DIST"/*.tar.gz "$DIST"/*.zip "$DIST/checksums.txt" "$SITE/$TAG/" 2>/dev/null || true
if ! start_site "$SITE" main; then
  printf 'release-check: 本地 HTTP 服务没起来\n' >&2
  exit 4
fi
BASE="http://127.0.0.1:$SITE_PORT"

BINDIR="$WORK/prefix/bin"
code=0
out="$(sh "$INSTALLER" --base-url "$BASE" --version "$VERSION" --dir "$BINDIR" --yes 2>&1)" || code=$?
assert_eq "install.sh 退出码 0" "0" "$code"
assert_contains "走了自定义下载前缀" "$BASE/$TAG" "$out"
assert_contains "sha256 校验通过" "sha256 匹配" "$out"
assert_exists "装出来的 bs" "$BINDIR/bs"
if [ -x "$BINDIR/bs" ]; then ok "装出来的文件可执行"; else bad "装出来的文件不可执行"; fi
out="$("$BINDIR/bs" version 2>&1)"
assert_contains "装好的 bs 能跑且版本正确" "$VERSION" "$out"

# 覆盖安装（同一个目录再来一次）不该失败：用户重复跑安装脚本是常态
code=0
out="$(sh "$INSTALLER" --base-url "$BASE" --version "$VERSION" --dir "$BINDIR" --yes 2>&1)" || code=$?
assert_eq "重复安装退出码 0" "0" "$code"
out="$("$BINDIR/bs" version 2>&1)"
assert_contains "重复安装后版本仍然正确" "$VERSION" "$out"

# ---------- 3. 自更新（走本地镜像前缀）----------
# 自更新是整条发布链里最危险的一步：它会替换正在运行的二进制。所以这里用
# BS_GITHUB_API / BS_GITHUB_ASSET_BASE 把 bs 指向本地站点，真跑一遍"更新成功"，
# 以及两条必须失败的路（校验和对不上、站点没有校验和文件）。
#
# 站点摆成 GitHub 的路径形状：<assets>/<repo>/releases/download/<tag>/<asset>，
# API 是 <api>/repos/<repo>/releases/latest（返回 {"tag_name": ...}）。
selfupdate_section() {
  newver="0.0.2-next"
  newtag="v$newver"
  pkg="github.com/kingfs/Bootstrap/internal/buildinfo"
  gh="$WORK/gh"
  release_dir="$gh/github/kingfs/Bootstrap/releases/download"
  api_dir="$gh/api/repos/kingfs/Bootstrap/releases"
  mkdir -p "$api_dir" "$release_dir/$newtag" "$WORK/newrel"

  commit="$(git -C "$ROOT" rev-parse --short HEAD)"
  ldflags="-s -w -X $pkg.Version=$newver -X $pkg.Commit=$commit -X $pkg.Date=2026-01-01T00:00:00Z"
  # 走 bash 起一层：scripts/devenv.sh 是 bash 专有（受限环境里 $HOME 只读，必须靠它把
  # GOPATH/GOCACHE 指到仓库内），而本脚本是 POSIX sh，直接 source 会算错根目录。
  if ! (cd "$ROOT" && bash -c '
      [ -f scripts/devenv.sh ] && . scripts/devenv.sh >/dev/null 2>&1
      exec go build -trimpath -ldflags "$1" -o "$2" ./cmd/bs
    ' bash "$ldflags" "$WORK/newrel/bs"); then
    bad "编译出用于自更新的新版本（${newver}）"
    return
  fi
  ok "编译出用于自更新的新版本（${newver}）"

  printf '{"tag_name":"%s"}' "$newtag" >"$api_dir/latest"
  printf '{"tag_name":"%s"}' "$newtag" >"$gh/github/kingfs/Bootstrap/releases/latest"
  new_asset="bootstrap_${newver}_${OS}_${ARCH}.tar.gz"
  (cd "$WORK/newrel" && tar -czf "$release_dir/$newtag/$new_asset" bs)
  (cd "$release_dir/$newtag" && sha256sum "$new_asset" >checksums.txt)

  # 额外的两个 tag：一个"产物被篡改但校验和是旧的"，一个"根本没有 checksums.txt"
  for extra in 0.0.3-next 0.0.4-next; do
    mkdir -p "$release_dir/v$extra"
    cp "$release_dir/$newtag/$new_asset" "$release_dir/v$extra/bootstrap_${extra}_${OS}_${ARCH}.tar.gz"
  done
  (cd "$release_dir/v0.0.3-next" && sha256sum bootstrap_0.0.3-next_"$OS"_"$ARCH".tar.gz >checksums.txt \
    && printf 'tampered' >>bootstrap_0.0.3-next_"$OS"_"$ARCH".tar.gz)

  if ! start_site "$gh" gh; then
    bad "自更新站点没起来"
    return
  fi
  gh_base="http://127.0.0.1:$SITE_PORT"
  export BS_GITHUB_API="$gh_base/api"
  export BS_GITHUB_ASSET_BASE="$gh_base/github"
  # 万一镜像变量没生效，别真去连 api.github.com：把代理指到一个必然连不上的本地端口
  export HTTPS_PROXY="http://127.0.0.1:1"
  export HTTP_PROXY="$HTTPS_PROXY"

  before_sum="$(sha256_of_file "$BINDIR/bs")"
  code=0
  out="$("$BINDIR/bs" self-update --check 2>&1)" || code=$?
  assert_eq "--check 退出码 0（走本地镜像解析最新版本）" "0" "$code"
  assert_contains "--check 认出镜像上的新版本" "$newver" "$out"
  assert_eq "--check 之后当前二进制没被换掉" "$before_sum" "$(sha256_of_file "$BINDIR/bs")"
  log="$(cat "$WORK/http-gh.log" 2>/dev/null || true)"
  assert_contains "--check 问了 API（latest）" "releases/latest" "$log"
  if printf '%s' "$log" | grep -q "bootstrap_"; then
    bad "--check 不该下载归档（日志里有 bootstrap_ 请求）"
  else
    ok "--check 不下载归档"
  fi

  # 真更新：0.0.1-next → 0.0.2-next
  code=0
  out="$("$BINDIR/bs" self-update --yes 2>&1)" || code=$?
  assert_eq "自更新退出码 0" "0" "$code"
  assert_contains "自更新下载了归档" "$new_asset" "$(cat "$WORK/http-gh.log")"
  out="$("$BINDIR/bs" version 2>&1)"
  assert_contains "替换后的二进制是新版本" "$newver" "$out"
  backup="$(ls "$BINDIR"/bs.bak-* 2>/dev/null | head -1 || true)"
  assert_exists "旧二进制留了备份" "$backup"
  if [ -n "$backup" ]; then
    assert_contains "备份里是旧版本（能手工回滚）" "$VERSION" "$("$backup" version 2>&1)"
  fi

  # 失败路径 1：产物被篡改（校验和还是旧的）→ 必须中止，且当前二进制保持不动
  before_sum="$(sha256_of_file "$BINDIR/bs")"
  code=0
  out="$("$BINDIR/bs" self-update --version v0.0.3-next --yes 2>&1)" || code=$?
  if [ "$code" -ne 0 ]; then ok "被篡改的产物必须失败（退出码 ${code}）"; else bad "被篡改的产物居然装上了"; fi
  # 错误消息是英文短句（AGENTS §5），面向用户的提示才是中文
  assert_contains "失败原因指向校验" "checksum mismatch" "$out"
  assert_eq "失败后当前二进制没被动过" "$before_sum" "$(sha256_of_file "$BINDIR/bs")"
  assert_eq "失败的更新不会多留备份" "1" "$(ls "$BINDIR"/bs.bak-* 2>/dev/null | wc -l | tr -d ' ')"

  # 失败路径 2：站点没有 checksums.txt → 按红线中止，绝不"跳过校验"
  code=0
  out="$("$BINDIR/bs" self-update --version v0.0.4-next --yes 2>&1)" || code=$?
  if [ "$code" -ne 0 ]; then ok "缺 checksums.txt 时必须失败（退出码 ${code}）"; else bad "缺校验和居然装上了"; fi
  assert_contains "失败原因指向缺校验和（绝不降级为不校验）" "no checksum available" "$out"
  assert_eq "缺校验和时当前二进制不变" "$before_sum" "$(sha256_of_file "$BINDIR/bs")"

  unset BS_GITHUB_API BS_GITHUB_ASSET_BASE HTTPS_PROXY HTTP_PROXY
  stop_site
}

printf '\n\033[1m== 自更新（本地镜像前缀）\033[0m\n'
selfupdate_section

# ---------- 4. 失败路径 ----------
printf '\n\033[1m== 失败路径（必须中止，且不留残留）\033[0m\n'
stop_site

# 3a. 篡改归档：校验和必须拦住
TAMPER="$WORK/tamper"
mkdir -p "$TAMPER/$TAG"
cp "$SITE/$TAG"/* "$TAMPER/$TAG/"
printf 'x' >>"$TAMPER/$TAG/$(basename "$LOCAL_ARCHIVE")"
if ! start_site "$TAMPER" tamper; then
  printf 'release-check: 篡改站点没起来\n' >&2
  exit 4
fi
code=0
out="$(sh "$INSTALLER" --base-url "http://127.0.0.1:$SITE_PORT" --version "$VERSION" \
  --dir "$WORK/tamper-bin" --yes 2>&1)" || code=$?
if [ "$code" -ne 0 ]; then ok "被篡改的归档必须失败（退出码 ${code}）"; else bad "被篡改的归档居然装成功了"; fi
assert_contains "失败原因指向校验" "校验失败" "$out"
assert_missing "校验失败后不留可执行文件" "$WORK/tamper-bin/bs"
stop_site

# 3b. 站点上没有 checksums.txt：按红线必须中止
NOSUM="$WORK/nosum"
mkdir -p "$NOSUM/$TAG"
cp "$SITE/$TAG/$(basename "$LOCAL_ARCHIVE")" "$NOSUM/$TAG/"
if ! start_site "$NOSUM" nosum; then
  printf 'release-check: 无校验和站点没起来\n' >&2
  exit 4
fi
code=0
out="$(sh "$INSTALLER" --base-url "http://127.0.0.1:$SITE_PORT" --version "$VERSION" \
  --dir "$WORK/nosum-bin" --yes 2>&1)" || code=$?
if [ "$code" -ne 0 ]; then ok "缺 checksums.txt 时必须失败（退出码 ${code}）"; else bad "缺 checksums.txt 居然装成功了"; fi
assert_contains "失败原因指向校验和文件" "checksums.txt" "$out"
assert_missing "缺校验和时不留下 bs" "$WORK/nosum-bin/bs"
stop_site

# 3c. 自定义前缀 + 不给版本：必须明确报错（没有 releases/latest 可跟）
code=0
out="$(sh "$INSTALLER" --base-url "$BASE" --dir "$WORK/nolatest-bin" --yes 2>&1)" || code=$?
if [ "$code" -ne 0 ]; then ok "自定义前缀下 latest 必须报错（退出码 ${code}）"; else bad "自定义前缀下居然按 latest 装成功了"; fi
assert_contains "报错说清要显式指定版本" "--version" "$out"

# 3d. 站点上没有这个版本：下载失败也要明确
code=0
out="$(sh "$INSTALLER" --base-url "$BASE" --version "v9.9.9" --dir "$WORK/nover-bin" --yes 2>&1)" || code=$?
if [ "$code" -ne 0 ]; then ok "版本不存在时必须失败（退出码 ${code}）"; else bad "不存在的版本居然装成功了"; fi
assert_contains "提示下载失败" "下载失败" "$out"
assert_missing "版本不存在时不留下 bs" "$WORK/nover-bin/bs"

echo
if [ "$fail" -eq 0 ]; then
  printf '\033[1m结果：%d 通过，0 失败\033[0m\n' "$pass"
  printf '发布彩排通过（沙箱目录 %s，可安全删除）\n' "$WORK"
  exit 0
fi
printf '\033[1m结果：%d 通过，%d 失败\033[0m\n' "$pass" "$fail"
exit 1

#!/bin/sh
# 在**有网络的干净容器**里跑 bs 的真实集成测试（这个脚本在容器内执行）。
#
# 与 scripts/docker-check.sh 的分工：
#   · docker-check.sh  —— `--network none`，验证断网/裸机上不崩、提示清楚；
#   · 本脚本          —— 有网络，验证真的能把东西装上来：apt 系统包路径、
#                        GitHub Release 下载 + sha256 校验、直链（params.url）路径，
#                        以及**代理全链路**（配置 → 注入 → 反向对照）。
#
# 它专门在干净系统上找"第一公里"的问题（开发机上永远看不到）：
#   · 镜像里没有 CA 证书时，所有 HTTPS 都失败在 x509 上——错误里必须说清该装什么；
#   · 没有 git/curl 时 apt 路径还走不走得通；
#   · 装完/卸完之后 `bs tool list` 是否立刻反映现状（探测缓存）；
#   · 配了代理之后，bs 自己发起的下载是不是真的走了代理——用"把代理指向黑洞必须失败"
#     这条反向对照来证明，而不是拿"下载成功"当证据。
#
# 注意顺序：容器里 **github.com 直连不通**（真实现象，也正因如此测试必须靠代理），
# 所以"走代理的下载"都排在代理配置之后；关闭代理之后只验证"不再注入变量"这类语义，
# 不再验证能否下载（那是网络环境的事，不是 bs 的行为）。
#
# 调用方（scripts/docker-integration.sh）负责把二进制与本站脚本放进 /work。
# 环境变量：BS_BIN（默认 /work/bs）、WORK_DIR（默认 /work）、BS_PROXY_URL、BS_IMAGE。

set -u

BIN="${BS_BIN:-/work/bs}"
WORK="${WORK_DIR:-/work}"
PROXY_URL="${BS_PROXY_URL:-}"
IMAGE="${BS_IMAGE:-unknown}"

HOME_DIR="$WORK/home"
CONFIG_HOME="$HOME_DIR/.config/bootstrap"
STATE_HOME="$HOME_DIR/.local/state/bootstrap"
CACHE_HOME="$HOME_DIR/.cache/bootstrap"

pass=0
fail=0
fail_names=""

ok() {
  printf '  \033[32mok\033[0m   %s\n' "$1"
  pass=$((pass + 1))
}
bad() {
  printf '  \033[31mFAIL\033[0m %s\n' "$1"
  fail=$((fail + 1))
  fail_names="$fail_names
    - $1"
}
skip() { printf '  \033[33mskip\033[0m %s\n' "$1"; }
note() { printf '       %s\n' "$1"; }

assert_contains() { # 名称 期望 实际
  case "$3" in
  *"$2"*) ok "$1" ;;
  *)
    bad "$1"
    printf '       期望包含: %s\n       实际输出: %s\n' "$2" "$(printf '%s' "$3" | head -c 1200)"
    ;;
  esac
}
assert_contains_any() { # 名称 实际 候选...
  name="$1"
  actual="$2"
  shift 2
  for want in "$@"; do
    case "$actual" in
    *"$want"*)
      ok "$name"
      return
      ;;
    esac
  done
  bad "$name"
  printf '       期望包含其中之一: %s\n       实际输出: %s\n' "$*" "$(printf '%s' "$actual" | head -c 1200)"
}
assert_not_contains() { # 名称 不该出现 实际
  case "$3" in
  *"$2"*)
    bad "$1"
    printf '       不该出现: %s\n       实际输出: %s\n' "$2" "$(printf '%s' "$3" | head -c 1200)"
    ;;
  *) ok "$1" ;;
  esac
}
assert_eq() { # 名称 期望 实际
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1"; printf '       期望: %s\n       实际: %s\n' "$2" "$3"; fi
}
assert_exists() { if [ -e "$2" ]; then ok "$1"; else bad "$1（$2 不存在）"; fi; }
assert_missing() { if [ -e "$2" ]; then bad "$1（$2 不该存在）"; else ok "$1"; fi; }
assert_glob() { # 名称 模式
  # shellcheck disable=SC2086
  if ls $2 >/dev/null 2>&1; then ok "$1"; else bad "$1（没有匹配 $2 的文件）"; fi
}
# 取某个工具在 `bs tool list` 里的那一行（状态列是 installed/missing/...）
tool_line() { printf '%s\n' "$1" | grep -E "^$2[[:space:]]" | head -1; }

# bsrun：环境变量全清、PATH 收敛到系统目录。代理**不**从环境变量来（那正是被测对象），
# 只能由 bs 自己的配置注入。
bsrun() {
  env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin \
    HOME="$HOME_DIR" \
    BS_CONFIG_HOME="$CONFIG_HOME" \
    BS_STATE_HOME="$STATE_HOME" \
    BS_CACHE_HOME="$CACHE_HOME" \
    NO_COLOR=1 \
    "$BIN" "$@"
}

# bsr：跑一条 bs 命令，把合并输出放进 OUT、退出码放进 RC。
#
# 为什么不直接 `out="$(bsrun ...)"`：`set -e` 下"预期失败"的命令会直接终止脚本
# （这条踩过：一次失败的安装把整个集成测试腰斩，后面的阶段全没跑）。
RC=0
OUT=""
bsr() {
  RC=0
  OUT="$(bsrun "$@" 2>&1)" || RC=$?
}

# 代理相关阶段需要地址；没给就显式跳过（不算失败）。
have_proxy() {
  if [ -n "$PROXY_URL" ]; then return 0; fi
  skip "$1（未提供 BS_PROXY_URL）"
  return 1
}

echo "== 环境（$IMAGE）"
echo "  $(uname -srm)，$(. /etc/os-release && echo "$PRETTY_NAME")"
echo "  沙箱：HOME=$HOME_DIR"
for probe in git curl unzip gcc; do
  if command -v "$probe" >/dev/null 2>&1; then echo "  已存在：$probe（干净系统前提被打破）"; fi
done
if [ -e /etc/ssl/certs/ca-certificates.crt ]; then
  echo "  已存在：系统 CA 证书"
else
  echo "  确认：镜像里没有系统 CA 证书（这正是要测的第一公里）"
fi
rm -rf "$HOME_DIR"
mkdir -p "$HOME_DIR"
# 受管块里是 bs shell init——按名字调用 bs。真实用户装的 bs 在 PATH 上，
# 这里用软链模拟（不复制，避免"测的不是同一个文件"）。
ln -sf "$BIN" /usr/local/bin/bs 2>/dev/null || true

echo
echo "== 裸机上必须能给出结论（不崩、不卡）"
bsr version
assert_contains "version 能跑" "bs " "$OUT"
bsr doctor
case "$RC" in
0 | 4) ok "doctor 给出结论（退出码 $RC）" ;;
*) bad "doctor 退出码异常：$RC" ;;
esac
assert_contains "doctor 报出平台" "platform" "$OUT"
assert_contains "doctor 检查系统 CA 证书" "ca certs" "$OUT"
assert_contains "缺 CA 证书时给出可执行命令" "ca-certificates" "$OUT"

echo
echo "== 第一公里：没有 CA 证书时，HTTPS 失败必须说清原因"
bsr tool install lazygit --yes
if [ "$RC" -ne 0 ]; then ok "缺 CA 证书时下载失败（退出码 $RC）"; else bad "缺 CA 证书却装成功了"; fi
assert_contains "错误里给出 x509 提示" "no usable system CA certificates" "$OUT"
assert_contains "错误里保留原始原因" "certificate" "$OUT"
assert_missing "失败后没有留下二进制" "$HOME_DIR/.local/bin/lazygit"

echo
echo "== 配置与 shell 集成（离线）"
bsr config init
assert_contains "生成默认配置" "已生成" "$OUT"
assert_exists "配置文件存在" "$CONFIG_HOME/config.yaml"
bsr config validate
assert_contains "默认配置合法" "配置合法" "$OUT"

printf '# 用户自己的 rc\nexport EDITOR=vim\n' >"$HOME_DIR/.bashrc"
bsr shell install --shell bash --rc "$HOME_DIR/.bashrc"
assert_contains_any "写入受管块" "$OUT" "已写入" "已更新"
assert_contains "用户内容保留" "export EDITOR=vim" "$(cat "$HOME_DIR/.bashrc")"
# 真在 bash 里 source 一遍：受管块里的同名函数包装必须真的可用
OUT="$(env -i PATH=/usr/local/bin:/usr/bin:/bin HOME="$HOME_DIR" \
  BS_CONFIG_HOME="$CONFIG_HOME" BS_STATE_HOME="$STATE_HOME" BS_CACHE_HOME="$CACHE_HOME" \
  bash -c '. "$HOME/.bashrc" >/dev/null 2>&1 || true
    printf "type=%s\n" "$(type -t bs 2>/dev/null || echo none)"
    bs version 2>&1 | head -1' 2>&1)"
assert_contains "rc 里装了 bs 包装函数" "type=function" "$OUT"
assert_contains "包装函数里 bs 能跑" "bs " "$OUT"

echo
echo "== 清单与计划（离线）"
for os in linux darwin windows; do
  bsr tool manifest validate --os "$os"
  assert_contains "清单校验通过（$os）" "清单校验通过" "$OUT"
done
bsr tool list --no-probe
assert_contains "列表含 git" "git" "$OUT"
assert_contains "列表含 lazygit" "lazygit" "$OUT"
bsr tool install git
assert_eq "非交互下缺 --yes 必须拒绝（退出码 5）" "5" "$RC"
assert_contains "拒绝时提示加 --yes" "--yes" "$OUT"

echo
echo "== 装系统 CA 证书（系统包管理器，不由 bs 代劳）"
if apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq --no-install-recommends ca-certificates >/dev/null 2>&1; then
  ok "apt 装好 ca-certificates"
else
  bad "apt 装 ca-certificates 失败（后面依赖 HTTPS 的断言会连带失败）"
fi
bsr doctor
assert_not_contains "doctor 不再报缺 CA 证书" "未找到系统 CA 证书" "$OUT"

echo
echo "== apt 路径：用 bs 装一个系统包（git）"
bsr tool install git --yes
assert_contains "apt 装 git 成功" "git" "$OUT"
assert_contains_any "确实走了系统包管理器" "$OUT" "apt-get install" "apt install"
if command -v git >/dev/null 2>&1; then ok "git 在 PATH 里可用"; else bad "git 装完却找不到"; fi
bsr tool list
assert_contains "装完立刻显示 installed（探测缓存）" "installed" "$(tool_line "$OUT" git)"
bsr tool install git --yes
assert_contains_any "重复安装是幂等的" "$OUT" "0 个需要处理" "已是最新"

# ---------------------------------------------------------------------------
# 从这里开始都要走代理：容器里 github.com 直连不通（`bs proxy test` 的"直连"那行会失败），
# 这恰好是本脚本最有价值的一段——它证明"下载成功"确实是代理的功劳。
# ---------------------------------------------------------------------------
if have_proxy "代理全链路"; then
  echo
  echo "== 代理：配置与连通性"
  bsr proxy add chaitin --http "$PROXY_URL" --https "$PROXY_URL" --no-proxy "localhost,127.0.0.1" --write
  assert_contains "代理 profile 写入配置" "已写入" "$OUT"
  bsr proxy list
  assert_contains "list 能看到它" "chaitin" "$OUT"
  bsr proxy use chaitin
  assert_contains "激活成功" "chaitin" "$OUT"
  bsr proxy status
  assert_contains "status 报告当前生效" "chaitin" "$OUT"

  bsr proxy test
  assert_contains "proxy test 给出直连结论" "直连" "$OUT"
  assert_contains "proxy test 给出代理结论" "代理(" "$OUT"
  assert_contains "proxy test 认为代理可用" "ok" "$OUT"
  note "proxy test：$(printf '%s' "$OUT" | tr '\n' ' ' | cut -c1-200)"

  # 子进程环境注入：用 git（已装、走系统证书）直接连 GitHub。
  # 这样验证的是"环境变量真的被注入了"，与 bs 自己的 HTTP 客户端无关。
  bsr proxy exec -- env
  assert_contains "proxy exec 注入了 HTTPS_PROXY" "HTTPS_PROXY=$PROXY_URL" "$OUT"
  # 用一个确定公开的仓库：本仓库还没推到 GitHub，连它会得到 401 要求输入用户名。
  bsr proxy exec -- git ls-remote https://github.com/octocat/Hello-World HEAD
  if [ "$RC" -eq 0 ]; then ok "带代理的 git 能连上 GitHub"; else bad "带代理的 git 连不上（退出码 $RC）：$(printf '%s' "$OUT" | head -c 300)"; fi
  bsr proxy env --apply
  assert_contains "proxy env 输出可 eval 的语句" "$PROXY_URL" "$OUT"

  echo
  echo "== 走代理的真实下载：GitHub Release + sha256（lazygit）"
  bsr tool install lazygit --yes
  if [ "$RC" -eq 0 ]; then ok "走代理装 lazygit 成功"; else bad "走代理装 lazygit 失败（退出码 $RC）"; note "$(printf '%s' "$OUT" | head -c 400)"; fi
  assert_exists "二进制落在 ~/.local/bin" "$HOME_DIR/.local/bin/lazygit"
  if [ -x "$HOME_DIR/.local/bin/lazygit" ]; then
    # lazygit --version 输出形如 "commit=…, version=0.66.1, os=linux, …"（不含名字）
    ver="$("$HOME_DIR/.local/bin/lazygit" --version 2>&1 | head -1)"
    assert_contains "装出来的二进制能跑" "version=" "$ver"
  else
    bad "lazygit 二进制不存在或不可执行"
  fi
  bsr tool list
  assert_contains "装完立刻显示 installed" "installed" "$(tool_line "$OUT" lazygit)"
  assert_glob "下载缓存里留了归档（断点续传的前提）" "$CACHE_HOME/downloads/lazygit*"
  bsr tool install lazygit --yes
  assert_contains_any "重复安装不再下载" "$OUT" "0 个需要处理" "已是最新"
  bsr tool uninstall lazygit --yes
  assert_contains "卸载有结论" "lazygit" "$OUT"
  assert_missing "卸载后二进制消失" "$HOME_DIR/.local/bin/lazygit"
  bsr tool list
  assert_not_contains "卸完立刻显示 missing" "installed" "$(tool_line "$OUT" lazygit)"

  echo
  echo "== 走代理的直链下载（params.url）：kubectl"
  bsr tool install kubectl --yes
  if [ "$RC" -eq 0 ]; then ok "走代理装 kubectl 成功"; else bad "走代理装 kubectl 失败（退出码 $RC）"; note "$(printf '%s' "$OUT" | head -c 400)"; fi
  assert_exists "kubectl 落在 ~/.local/bin" "$HOME_DIR/.local/bin/kubectl"
  if [ -x "$HOME_DIR/.local/bin/kubectl" ]; then
    assert_contains "kubectl 能跑" "Client Version" "$("$HOME_DIR/.local/bin/kubectl" version --client 2>&1 | head -2)"
  fi
  bsr tool uninstall kubectl --yes
  assert_missing "kubectl 卸载干净" "$HOME_DIR/.local/bin/kubectl"

  echo
  echo "== 反向对照：代理指向黑洞时必须失败（证明注入真的生效）"
  bsr proxy add blackhole --http http://127.0.0.1:9 --https http://127.0.0.1:9 --write
  assert_contains "写入黑洞代理" "已写入" "$OUT"
  bsr proxy use blackhole
  assert_contains "切到黑洞代理" "blackhole" "$OUT"
  rm -rf "$CACHE_HOME/downloads" # 逼它重新下载，否则会用缓存假成功
  bsr tool install lazygit --yes
  if [ "$RC" -ne 0 ]; then ok "黑洞代理下下载必须失败（退出码 $RC）"; else bad "黑洞代理下居然装成功了"; fi
  assert_contains "失败原因指向被注入的代理" "127.0.0.1:9" "$OUT"
  assert_missing "失败后没有留下二进制" "$HOME_DIR/.local/bin/lazygit"

  echo
  echo "== 正面对照：换回真代理，同一件事必须成功"
  bsr proxy use chaitin
  assert_contains "切回真代理" "chaitin" "$OUT"
  bsr tool install lazygit --yes
  if [ "$RC" -eq 0 ]; then ok "再次走真代理装 lazygit 成功"; else bad "走真代理仍然失败（退出码 $RC）"; note "$(printf '%s' "$OUT" | head -c 400)"; fi
  assert_exists "走代理装出来的二进制存在" "$HOME_DIR/.local/bin/lazygit"

  echo
  echo "== 显式关闭代理后不再注入变量"
  bsr proxy off
  assert_contains "关闭有回执" "已停用" "$OUT"
  bsr proxy status
  assert_contains_any "status 显示已关闭" "$OUT" "显式关闭" "未启用代理"
  bsr proxy exec -- env
  assert_not_contains "关闭后不再注入代理变量" "HTTPS_PROXY=" "$OUT"
fi

echo
if [ "$fail" -eq 0 ]; then
  printf '\033[1m结果：%d 通过，0 失败（%s）\033[0m\n' "$pass" "$IMAGE"
  exit 0
fi
printf '\033[1m结果：%d 通过，%d 失败（%s）\033[0m\n' "$pass" "$fail" "$IMAGE"
printf '失败项：%s\n' "$fail_names"
exit 1

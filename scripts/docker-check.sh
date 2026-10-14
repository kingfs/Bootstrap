#!/bin/sh
# 在**裸容器**里验证 bs 的"新机器"路径。
#
# 这个脚本是给 `scripts/docker-matrix.sh` 在容器内执行的，也可以手动跑：
#
#   docker run --rm --network none \
#     -v "$PWD/dist/linux-arm64/bs:/usr/local/bin/bs:ro" \
#     -v "$PWD/scripts/docker-check.sh:/check.sh:ro" \
#     -v "$PWD/.cache/docker-matrix/work:/work" \
#     ubuntu:24.04 sh /check.sh
#
# 与 scripts/smoke.sh 的分工：smoke 跑在**开发机**上（bar 里什么都有），
# 这里跑在 ubuntu/debian 的最小镜像里（没有 git、没有 curl、没有 go），
# 专门抓"只在干净系统上才暴露"的问题：缺命令时崩不崩、提示不清不楚、
# 有没有偷偷依赖宿主环境、有没有在网络不通时卡住。
#
# 全程 `--network none`，因此这里的任何"成功"都不可能靠联网蒙混过关。

set -eu

BIN="${BS_BIN:-/usr/local/bin/bs}"
WORK="${WORK_DIR:-/work}"
HOME_DIR="$WORK/home"
CONFIG_HOME="$HOME_DIR/.config/bootstrap"
STATE_HOME="$HOME_DIR/.local/state/bootstrap"
CACHE_HOME="$HOME_DIR/.cache/bootstrap"

pass=0
fail=0

ok() { printf '  \033[32mok\033[0m   %s\n' "$1"; pass=$((pass + 1)); }
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
assert_contains_any() { # 名称 实际 候选1 [候选2...]
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
  printf '       期望包含其中之一: %s\n       实际输出: %s\n' "$*" "$actual"
}
assert_exists() { # 名称 路径
  if [ -e "$2" ]; then ok "$1"; else bad "$1（$2 不存在）"; fi
}
assert_missing() { # 名称 路径
  if [ -e "$2" ]; then bad "$1（$2 不该存在）"; else ok "$1"; fi
}

# 统一的调用方式：环境全部指向沙箱，PATH 收敛到系统目录（刻意不含 /usr/local/bin，
# 让"宿主 PATH 里恰好有这个工具"无法影响结论）。
bsrun() {
  env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin \
    HOME="$HOME_DIR" \
    BS_CONFIG_HOME="$CONFIG_HOME" \
    BS_STATE_HOME="$STATE_HOME" \
    BS_CACHE_HOME="$CACHE_HOME" \
    NO_COLOR=1 \
    "$BIN" "$@"
}

echo "== 环境"
echo "  $(uname -srm)，shell=$0"
if command -v git >/dev/null 2>&1; then echo "  注意：镜像里有 git（这台不算裸机）"; else echo "  确认：镜像里没有 git"; fi
echo "  确认：镜像里没有 curl：$(command -v curl >/dev/null 2>&1 && echo 有 || echo 没有)"

# $WORK 是挂载点，删不掉（Device or resource busy）；只清里面的沙箱目录。
rm -rf "$HOME_DIR"
mkdir -p "$HOME_DIR"

echo
echo "== 版本与只读命令（缺 git/curl 也必须能跑）"
out="$(bsrun version 2>&1)"
assert_contains "version 能跑" "bs " "$out"

out="$(bsrun doctor 2>&1)" || code=$?
code=${code:-0}
# doctor 在裸机上把缺 git/curl 报成问题（退出码 4）是**正确**行为；崩掉（>4 或 2）才是 bug。
case "$code" in
0 | 4) ok "doctor 在裸机上给出结论（退出码 $code）" ;;
*) bad "doctor 退出码异常：$code" ;;
esac
unset code
assert_contains "doctor 报出平台" "platform" "$out"
assert_contains "doctor 检查 git" "git" "$out"

out="$(bsrun config path)"
assert_contains "config path 指向沙箱" "$CONFIG_HOME" "$out"

echo
echo "== 配置：生成与校验"
out="$(bsrun config init)"
assert_contains "生成默认配置" "已生成" "$out"
assert_exists "配置文件存在" "$CONFIG_HOME/config.yaml"
out="$(bsrun config validate)"
assert_contains "默认配置合法" "配置合法" "$out"
out="$(bsrun config get repo.root)"
assert_contains "config get 的派生值可用" "$HOME_DIR/src" "$out"
out="$(bsrun config set log.level debug)"
assert_contains "config set 生效" "已写入 log.level = debug" "$out"
assert_eq "写回后可读" "debug" "$(bsrun config get log.level)"

echo
echo "== shell 集成：只动受管块"
printf '# 用户自己的 rc\nexport EDITOR=vim\n' >"$HOME_DIR/.bashrc"
before="$(cat "$HOME_DIR/.bashrc")"
out="$(bsrun shell install --shell bash --rc "$HOME_DIR/.bashrc")"
# "已写入"（首次加块）与"已更新"（块已存在但内容变了）都是成功
assert_contains_any "写入受管块" "$out" "已写入" "已更新"
assert_contains "用户内容保留" "export EDITOR=vim" "$(cat "$HOME_DIR/.bashrc")"
assert_eq "受管块只有一份" "1" "$(grep -c '>>> bootstrap' "$HOME_DIR/.bashrc")"
out="$(bsrun shell install --shell bash --rc "$HOME_DIR/.bashrc")"
assert_contains "重复安装幂等" "已是最新" "$out"
out="$(bsrun shell status --shell bash --rc "$HOME_DIR/.bashrc")"
assert_contains "status 报告已安装" "已安装" "$out"
out="$(bsrun shell uninstall --shell bash --rc "$HOME_DIR/.bashrc")"
assert_eq "卸载后 rc 与原始内容一致" "$before" "$(cat "$HOME_DIR/.bashrc")"

echo
echo "== 清单与计划（离线）"
out="$(bsrun tool manifest validate --os linux)"
assert_contains "linux 清单通过校验" "清单校验通过" "$out"
out="$(bsrun tool manifest validate --os darwin)"
assert_contains "darwin 清单通过校验" "清单校验通过" "$out"
out="$(bsrun tool manifest validate --os windows)"
assert_contains "windows 清单通过校验" "清单校验通过" "$out"
out="$(bsrun tool list --no-probe)"
assert_contains "列表含 git" "git" "$out"
assert_contains "列表含 uv" "uv" "$out"

# 裸机上首选方式经常不可用（没有 go/brew），plan 必须仍然给出一个可读的结论：
# 要么选中 apt（镜像里有），要么明确说明为什么都不行。
out="$(bsrun tool plan --profile base 2>&1)"
assert_contains "plan 打印计划头" "工具" "$out"
assert_contains "plan 说明不会执行" "只展示计划" "$out"

echo
echo "== 仓库计划（dry-run，不写盘）"
out="$(bsrun repo get kingfs/Bootstrap --dry-run 2>&1)"
assert_contains "dry-run 打印将执行的命令" "git clone" "$out"
assert_missing "dry-run 不创建仓库根目录" "$HOME_DIR/src"

echo
echo "== 破坏性操作在非交互下必须拒绝（退出码 5）"
code=0
out="$(bsrun tool install git 2>&1)" || code=$?
assert_eq "install 缺 --yes 退出码 5" "5" "$code"
assert_contains "install 提示加 --yes" "--yes" "$out"

echo
echo "== 退出码约定"
code=0
bsrun nosuchcommand >/dev/null 2>&1 || code=$?
assert_eq "未知命令退出码 2" "2" "$code"
code=0
bsrun --config "$WORK/nope.yaml" config validate >/dev/null 2>&1 || code=$?
assert_eq "配置缺失退出码 4" "4" "$code"

echo
if [ "$fail" -eq 0 ]; then
  printf '\033[1m结果：%d 通过，0 失败\033[0m\n' "$pass"
  exit 0
fi
printf '\033[1m结果：%d 通过，%d 失败\033[0m\n' "$pass" "$fail"
exit 1

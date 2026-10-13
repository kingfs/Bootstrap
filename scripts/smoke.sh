#!/usr/bin/env bash
#
# 无副作用冒烟测试。
#
# 原则：所有读写都发生在临时沙箱目录里，绝不触碰开发者真实的 $HOME、rc 文件或系统目录；
# 不发起任何网络请求（网络相关路径一律用 --dry-run 覆盖）。
#
# 用法：
#   scripts/smoke.sh              # 自动构建并测试
#   BS_BIN=./bin/bs scripts/smoke.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${BS_BIN:-$ROOT/bin/bs}"
VERBOSE="${VERBOSE:-0}"

WORK="${BS_SMOKE_DIR:-$ROOT/.cache/smoke}"
rm -rf "$WORK"
mkdir -p "$WORK/home" "$WORK/config" "$WORK/state" "$WORK/cache"

# 记录真实环境，结束后校验没有被改动。
REAL_HOME="${HOME:-}"
REAL_RC_STATE=""
for f in "$REAL_HOME/.bashrc" "$REAL_HOME/.zshrc" "$REAL_HOME/.bash_profile"; do
  [ -f "$f" ] && REAL_RC_STATE="$REAL_RC_STATE$(shasum -a 256 "$f" 2>/dev/null || sha256sum "$f" 2>/dev/null)$(printf '\n')"
done

if [ ! -x "$BIN" ]; then
  echo "==> 构建 $BIN"
  ( cd "$ROOT" && . scripts/devenv.sh && mkdir -p bin && go build -o bin/bs ./cmd/bs )
fi

# 沙箱环境：把 HOME 与所有 bs 目录都指向临时目录。
export HOME="$WORK/home"
export BS_CONFIG_HOME="$WORK/config"
export BS_STATE_HOME="$WORK/state"
export BS_CACHE_HOME="$WORK/cache"
export NO_COLOR=1
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy no_proxy NO_PROXY 2>/dev/null || true

pass=0
fail=0
step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
ok()   { pass=$((pass+1)); printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad()  { fail=$((fail+1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; }

# assert_contains <描述> <期望子串> <实际文本>
assert_contains() {
  if printf '%s' "$3" | grep -qF -- "$2"; then ok "$1"; else
    bad "$1"; printf '       期望包含: %s\n       实际: %s\n' "$2" "$(printf '%s' "$3" | head -5)"
  fi
}
# assert_not_contains <描述> <禁止子串> <实际文本>
assert_not_contains() {
  if printf '%s' "$3" | grep -qF -- "$2"; then
    bad "$1"; printf '       不应包含: %s\n' "$2"
  else ok "$1"; fi
}
assert_eq() {
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1"; printf '       期望: %s\n       实际: %s\n' "$2" "$3"; fi
}
assert_missing() {
  if [ -e "$2" ]; then bad "$1（不应存在的路径: $2）"; else ok "$1"; fi
}
assert_exists() {
  if [ -e "$2" ]; then ok "$1"; else bad "$1（缺少路径: $2）"; fi
}

step "版本与路径"
out="$("$BIN" version)"
assert_contains "bs version 输出" "bs " "$out"
out="$("$BIN" version --json)"
assert_contains "bs version --json 机器可读" '"go_version"' "$out"
out="$("$BIN" config path)"
assert_contains "config path 使用沙箱目录" "$WORK/config" "$out"

step "配置初始化与校验"
out="$("$BIN" config init)"
assert_contains "生成默认配置" "已生成" "$out"
assert_exists "配置文件存在" "$WORK/config/config.yaml"
before="$(stat -c %Y "$WORK/config/config.yaml" 2>/dev/null || stat -f %m "$WORK/config/config.yaml")"
sleep 1
out="$("$BIN" config init)"
assert_contains "重复 init 不覆盖" "已存在" "$out"
after="$(stat -c %Y "$WORK/config/config.yaml" 2>/dev/null || stat -f %m "$WORK/config/config.yaml")"
assert_eq "重复 init 未修改文件" "$before" "$after"
out="$("$BIN" config validate)"
assert_contains "默认配置合法" "配置合法" "$out"

step "代理 profile"
cat > "$WORK/config/config.yaml" <<'EOF'
version: 1
log:
  level: info
  color: never
repo:
  root: ~/src
proxy:
  default_profile: local
  auto_apply: true
  profiles:
    local:
      http_proxy: http://127.0.0.1:7890
      https_proxy: http://127.0.0.1:7890
      no_proxy: 127.0.0.1,localhost,10.2.0.0/16
    chaitin:
      http_proxy: http://proxy.in.chaitin.net:8123
      https_proxy: http://proxy.in.chaitin.net:8123
      no_proxy: 127.0.0.1,localhost,.chaitin.net,.chaitin.cn
    secret:
      http_proxy: http://user:pw@proxy.example:8080
      https_proxy: http://user:pw@proxy.example:8080
tool:
  extra_path:
    - ~/.local/bin
EOF
out="$("$BIN" proxy list)"
assert_contains "proxy list 列出 local" "local" "$out"
assert_contains "proxy list 列出 chaitin" "chaitin" "$out"
out="$("$BIN" proxy status)"
assert_contains "默认 profile 生效" "config profile local" "$out"
out="$("$BIN" proxy use chaitin)"
assert_contains "切换 profile" "chaitin" "$out"
out="$("$BIN" proxy env --apply)"
assert_contains "env --apply 输出 export" "export HTTP_PROXY='http://proxy.in.chaitin.net:8123'" "$out"
out="$("$BIN" proxy env --apply)"
assert_contains "env --apply 同时输出小写变量" "export http_proxy=" "$out"
out="$("$BIN" proxy status)"
"$BIN" proxy use secret >/dev/null 2>&1
assert_contains "凭据被脱敏" "***@proxy.example:8080" "$("$BIN" proxy status)"
assert_not_contains "凭据未泄漏" "user:pw" "$out"
"$BIN" proxy off >/dev/null
out="$("$BIN" proxy env --apply)"
assert_contains "off 后输出清除语句" "unset HTTP_PROXY" "$out"
out="$("$BIN" proxy status)"
assert_contains "off 状态可见" "已被显式关闭" "$out"
out="$("$BIN" proxy env --shell fish --profile local)"
assert_contains "fish 渲染" "set -gx HTTP_PROXY" "$out"
out="$("$BIN" proxy env --shell json --profile local)"
assert_contains "json 渲染" '"HTTPS_PROXY"' "$out"
out="$("$BIN" proxy add smoke --http http://127.0.0.1:7890 --https http://127.0.0.1:7890)"
assert_contains "proxy add 默认只打印片段" "http_proxy: http://127.0.0.1:7890" "$out"

step "shell 片段与受管块"
printf '# 用户自己的配置\nexport EDITOR=vim\n' > "$WORK/home/.bashrc"
REAL_RC_BEFORE="$(cat "$WORK/home/.bashrc")"
out="$("$BIN" shell init bash)"
assert_contains "shell init 含包装函数" "command bs \"\$@\"" "$out"
assert_contains "shell init 含 startproxy" "startproxy()" "$out"
out="$("$BIN" shell init bash)"
assert_contains "shell init 含 chaitinproxy" "chaitinproxy()" "$out"
"$BIN" shell install --shell bash --rc "$WORK/home/.bashrc" >/dev/null
out="$(cat "$WORK/home/.bashrc")"
assert_contains "受管块起始标记" "# >>> bootstrap (managed by 'bs shell install') >>>" "$out"
assert_contains "受管块 eval 行" 'eval "$(bs shell init bash)"' "$out"
assert_contains "用户原有内容保留" "export EDITOR=vim" "$out"
assert_eq "受管块只有一份" "1" "$(grep -c '>>> bootstrap' "$WORK/home/.bashrc")"
before="$(stat -c %Y "$WORK/home/.bashrc" 2>/dev/null || stat -f %m "$WORK/home/.bashrc")"
sleep 1
out="$("$BIN" shell install --shell bash --rc "$WORK/home/.bashrc")"
assert_contains "重复 install 幂等" "已是最新" "$out"
after="$(stat -c %Y "$WORK/home/.bashrc" 2>/dev/null || stat -f %m "$WORK/home/.bashrc")"
assert_eq "重复 install 未修改文件" "$before" "$after"
out="$("$BIN" shell status --shell bash --rc "$WORK/home/.bashrc")"
assert_contains "status 报告已安装" "已安装且为最新" "$out"
out="$("$BIN" shell uninstall --shell bash --rc "$WORK/home/.bashrc")"
assert_contains "uninstall 移除受管块" "已从" "$out"
out="$(cat "$WORK/home/.bashrc")"
assert_eq "uninstall 后内容与原始一致" "$REAL_RC_BEFORE" "$out"
assert_not_contains "uninstall 后无标记" ">>> bootstrap" "$out"

step "PATH 装配"
out="$("$BIN" env path)"
assert_contains "env path 输出 PATH export" "export PATH=" "$out"
assert_contains "env path 包含 extra_path" ".local/bin" "$out"
out="$("$BIN" env path --shell fish)"
assert_contains "fish PATH 渲染" "set -gx PATH" "$out"
out="$("$BIN" env show)"
assert_contains "env show 列出条目" "tool.extra_path" "$out"
out="$("$BIN" env export)"
assert_contains "env export 同时含 PATH 与代理" "export PATH=" "$out"

step "仓库计划（dry-run，无网络无写入）"
out="$("$BIN" repo root)"
assert_contains "repo root 默认 ~/src" "$WORK/home/src" "$out"
# 根目录不存在时，sync 应报告"前置条件"（退出码 4）并指向下一步，而不是一般失败
set +e
out="$("$BIN" repo sync 2>&1)"
code=$?
set -e
assert_eq "repo sync 缺根目录退出码为 4" "4" "$code"
assert_contains "repo sync 提示下一步" "bs repo get" "$out"

out="$("$BIN" repo get kingfs/Bootstrap --dry-run 2>&1)"
assert_contains "dry-run 显示 clone" "github.com/kingfs/Bootstrap" "$out"
assert_contains "dry-run 打印将执行的命令" "git clone" "$out"
assert_contains "dry-run 状态为 planned 而非 cloned" "planned" "$out"
assert_missing "dry-run 不创建目录" "$WORK/home/src"
out="$("$BIN" repo get kingfs/Bootstrap --dry-run --ref develop --depth 1 2>&1)"
assert_contains "dry-run 支持 --ref" "--branch develop" "$out"
out="$("$BIN" repo list)"
assert_contains "空根目录不报错" "暂未发现仓库" "$out"

step "工具清单与安装计划"
out="$("$BIN" tool manifest validate 2>&1)"
assert_contains "内置清单通过校验" "清单校验通过" "$out"
out="$("$BIN" tool manifest validate --os darwin 2>&1)"
assert_contains "macOS 清单通过校验" "清单校验通过" "$out"
out="$("$BIN" tool manifest validate --os windows 2>&1)"
assert_contains "Windows 清单通过校验" "清单校验通过" "$out"
out="$("$BIN" tool manifest path)"
assert_contains "清单来源含内置 common.yaml" "common.yaml" "$out"
out="$("$BIN" tool list --no-probe)"
assert_contains "列表含 git" "git" "$out"
assert_contains "列表含 uv" "uv" "$out"
assert_contains "列表含 profile 里的 rustup" "rustup" "$out"
# nvm 用 verify.file 探测：这条只做 LookPath/文件判断，不会执行任何命令
out="$("$BIN" tool info nvm --json)"
assert_contains "tool info 输出单个对象" '"id": "nvm"' "$out"
out="$("$BIN" tool search ripgrep)"
assert_contains "搜索命中 ripgrep" "ripgrep" "$out"
assert_not_contains "搜索结果排除无关条目" "tmux" "$out"

# 计划与干跑必须是只读的：看得到步骤，且磁盘上不留任何东西。
out="$("$BIN" tool plan --profile golang --dry-run 2>&1)"
assert_contains "计划包含 go 版本管理器 g" "g" "$out"
assert_contains "计划说明不会执行" "只展示计划" "$out"

# 选择条件写错必须是用法错误（退出码 2），而不是含含糊糊的 1
# （set -e 下必须先吞掉非 0 状态，否则脚本自己就退出了）
code=0; out="$("$BIN" tool plan --profile nosuch 2>&1)" || code=$?
assert_eq "未知 profile 退出码 2" "2" "$code"
assert_contains "未知 profile 提示可用值" "not found" "$out"

# manifest show 不带 --json 也要有输出（曾静默返回 0）
out="$("$BIN" tool manifest show git 2>&1)"
assert_contains "manifest show 默认输出 YAML" "id: git" "$out"

# 方法链全不可用时，install 不能返回 0（fnm 只有 brew/cargo/release，沙箱里都没装）
code=0; out="$(env PATH=/usr/bin:/bin "$BIN" tool install fnm --yes 2>&1)" || code=$?
assert_eq "方法链全不可用退出码 4" "4" "$code"
assert_contains "方法链全不可用给出原因" "no usable install method" "$out"
assert_missing "计划不创建安装目标" "$WORK/home/.local/bin/g"
assert_missing "计划不创建 go install 目录" "$WORK/home/go"
out="$("$BIN" tool install --profile golang --dry-run 2>&1)"
assert_not_contains "干跑不执行安装命令" "正在安装" "$out"

step "体检"
set +e
out="$("$BIN" doctor)"
code=$?
set -e
assert_contains "doctor 输出平台信息" "platform" "$out"
assert_contains "doctor 检查 git" "git" "$out"
if [ "$code" -eq 0 ] || [ "$code" -eq 4 ]; then ok "doctor 退出码合理（$code）"; else bad "doctor 退出码异常：$code"; fi

step "退出码约定"
set +e
"$BIN" definitely-not-a-command >/dev/null 2>&1
code=$?
set -e
assert_eq "未知命令退出码为 2" "2" "$code"
set +e
"$BIN" --config "$WORK/missing.yaml" doctor >/dev/null 2>&1
code=$?
set -e
assert_eq "配置缺失退出码为 4" "4" "$code"
set +e
"$BIN" proxy env --shell nonsense >/dev/null 2>&1
code=$?
set -e
assert_eq "非法 --shell 退出码为 2" "2" "$code"

step "无副作用校验"
REAL_RC_AFTER=""
for f in "$REAL_HOME/.bashrc" "$REAL_HOME/.zshrc" "$REAL_HOME/.bash_profile"; do
  [ -f "$f" ] && REAL_RC_AFTER="$REAL_RC_AFTER$(shasum -a 256 "$f" 2>/dev/null || sha256sum "$f" 2>/dev/null)$(printf '\n')"
done
assert_eq "真实 rc 文件未被修改" "$REAL_RC_STATE" "$REAL_RC_AFTER"

printf '\n\033[1m结果：%d 通过，%d 失败\033[0m\n' "$pass" "$fail"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
echo "沙箱目录：$WORK（可安全删除）"

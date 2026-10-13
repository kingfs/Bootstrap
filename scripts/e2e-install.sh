#!/usr/bin/env bash
#
# 真实安装验证（可选，需要网络）。
#
# 与 `scripts/smoke.sh` 的区别：冒烟测试完全不碰网络，这里**真的**会
# 从 GitHub Release 下载归档并按清单安装，用来验证那些"只有联网才暴露"的
# 环节：最新 tag 解析、asset 模板（{{os}}/{{arch|archmap:...}}）、校验和文件
# 的候选链、解压与软链、以及装完之后的复核。
#
# 安全性：所有产物都落在 <repo>/.cache/e2e 下，PATH/配置/状态目录全部指向沙箱，
# 不写 $HOME、不碰系统包管理器、不修改任何 rc 文件。可反复运行。
#
# 用法：
#   scripts/e2e-install.sh            # 默认装 k9s + lazygit
#   TOOLS="k9s" scripts/e2e-install.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${BS_BIN:-$ROOT/bin/bs}"
TOOLS="${TOOLS:-k9s lazygit}"
WORK="${BS_E2E_DIR:-$ROOT/.cache/e2e}"
PROXY="${BS_E2E_PROXY:-http://127.0.0.1:7890}"

if [ ! -x "$BIN" ]; then
  echo "==> 构建 $BIN"
  ( cd "$ROOT" && . scripts/devenv.sh && mkdir -p bin && go build -o bin/bs ./cmd/bs )
fi

rm -rf "$WORK"
mkdir -p "$WORK/bin" "$WORK/config/tools" "$WORK/state" "$WORK/cache" "$WORK/home"

cat > "$WORK/config/config.yaml" <<EOF
version: 1
log:
  level: warn
  color: never
tool:
  extra_path:
    - $WORK/bin
  manifest:
    builtin: true
    user_dirs:
      - $WORK/config/tools
proxy:
  default_profile: e2e
  auto_apply: true
  profiles:
    e2e:
      http_proxy: $PROXY
      https_proxy: $PROXY
      no_proxy: 127.0.0.1,localhost
EOF

# 把两个工具的落点改到沙箱目录（其余字段继承内置清单）。
cat > "$WORK/config/tools/e2e.yaml" <<EOF
version: 1
tools:
  - id: k9s
    install:
      - via: release
        when: {os: [linux, darwin]}
        params:
          repo: derailed/k9s
          asset: "k9s_{{os|osmap:linux=Linux,darwin=Darwin}}_{{arch}}.tar.gz"
          strip_components: 1
          binary: k9s
          dest: "$WORK/bin"
          checksum_file: checksums.sha256
  - id: lazygit
    install:
      - via: release
        when: {os: [linux, darwin]}
        params:
          repo: jesseduffield/lazygit
          asset: "lazygit_{{version}}_{{os}}_{{arch|archmap:amd64=x86_64,arm64=arm64}}.tar.gz"
          strip_components: 1
          binary: lazygit
          dest: "$WORK/bin"
EOF

export HOME="$WORK/home"
export BS_CONFIG_HOME="$WORK/config"
export BS_STATE_HOME="$WORK/state"
export BS_CACHE_HOME="$WORK/cache"
export PATH="$WORK/bin:$PATH"
export NO_COLOR=1

echo "==> 计划（离线预览，应为 release 方式）"
"$BIN" tool plan $TOOLS
echo
echo "==> 真实安装（下载 + 校验 sha256 + 解压）"
"$BIN" tool install $TOOLS --yes

echo
echo "==> 结果核对"
fail=0
for t in $TOOLS; do
  if [ ! -x "$WORK/bin/$t" ]; then
    echo "  FAIL $t 没有安装到 $WORK/bin"
    fail=$((fail+1))
    continue
  fi
  ver="$("$WORK/bin/$t" --version 2>&1 | head -1)"
  echo "  ok   $t -> $WORK/bin/$t ($ver)"
done

echo
echo "==> 幂等性：再装一次应当全部跳过"
out="$("$BIN" tool install $TOOLS --yes)"
printf '%s\n' "$out" | tail -3
if printf '%s' "$out" | grep -q "需要处理"; then
  echo "  ok   第二次没有重新下载"
else
  echo "  FAIL 第二次仍然要处理"
  fail=$((fail+1))
fi

echo
if [ "$fail" -gt 0 ]; then
  echo "结果：$fail 项失败"
  exit 1
fi
echo "结果：全部通过（沙箱目录 $WORK，可安全删除）"

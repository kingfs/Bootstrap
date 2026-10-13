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
#   scripts/e2e-install.sh              # 默认装 g（4MB，校验文件是 sha256sum.txt）
#   TOOLS="k9s lazygit" scripts/e2e-install.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${BS_BIN:-$ROOT/bin/bs}"
TOOLS="${TOOLS:-g}"
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

# 把工具的落点改到沙箱目录。这里必须与内置清单的字段保持一致：
# 这三个仓库的归档**没有**外层目录（实测：根下直接是 g / lazygit / k9s），
# 写了 strip_components 会把文件本身剥掉。
cat > "$WORK/config/tools/e2e.yaml" <<EOF
version: 1
tools:
  - id: g
    install:
      - via: release
        when: {os: [linux, darwin]}
        params:
          repo: voidint/g
          asset: "g{{version}}.{{os}}-{{arch}}.tar.gz"
          binary: g
          dest: "$WORK/bin"
          checksum_file: sha256sum.txt
  - id: k9s
    install:
      - via: release
        when: {os: [linux, darwin]}
        params:
          repo: derailed/k9s
          asset: "k9s_{{os|osmap:linux=Linux,darwin=Darwin}}_{{arch}}.tar.gz"
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
          binary: lazygit
          dest: "$WORK/bin"
EOF

export HOME="$WORK/home"
export BS_CONFIG_HOME="$WORK/config"
export BS_STATE_HOME="$WORK/state"
export BS_CACHE_HOME="$WORK/cache"
# PATH 只保留沙箱 bin + 系统目录：宿主自己装过的同名工具（比如 ~/.g/bin/g）
# 若可见，安装就会被判成"已安装、跳过"，这条 e2e 也就什么都验证不了了。
export PATH="$WORK/bin:/usr/bin:/bin"
export NO_COLOR=1

echo "==> 计划（离线预览，应为 release 方式）"
out="$("$BIN" tool plan $TOOLS)"
printf '%s\n' "$out"
# 计划里就是 skip 的话，这条 e2e 什么都没验证到（多半是 PATH 没隔离干净），
# 必须当场失败，而不是让后面的"安装成功"变成假象。
if printf '%s' "$out" | grep -q "skip"; then
  echo "  FAIL 计划里已经跳过（沙箱里为何已安装？检查 PATH 隔离）"
  exit 1
fi
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
  # 不要 `| head -1`：`set -o pipefail` 下 head 提前关管道会让被测程序吃 SIGPIPE，
  # 整个脚本以 141 退出（k9s 的 --version 输出较长，必现）。
  # 有的工具不认 --version（k9s 用 `k9s version`，而且它 --version 失败仍返回 0，
  # 因此不能靠退出码判断）。这里只为打印好看，探测本身在清单里另有声明。
  ver="$("$WORK/bin/$t" --version 2>&1 || true)"
  if [[ "$ver" == *"unknown flag"* || "$ver" == *"unrecognized"* ]]; then
    ver="$("$WORK/bin/$t" version 2>&1 || true)"
  fi
  # 只留一行好看的：优先取形如 x.y.z 的版本号（k9s 的 `version` 会先打一段 ASCII 横幅，
  # lazygit 会把构建信息全打出来），拿不到就退回第一行。
  line="${ver%%$'\n'*}"
  if [[ "$ver" =~ ([0-9]+\.[0-9]+\.[0-9]+) ]]; then
    line="${BASH_REMATCH[1]}"
  fi
  echo "  ok   $t -> $WORK/bin/$t ($line)"
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

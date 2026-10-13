# ─── PATH 装配：由各工具声明的 provides 目录 + 配置的 tool.extra_path 组成 ───
# 每次启动 shell 都重新计算，因此 `bs tool install <x>` 之后无需手动改 PATH。
if command -v bs >/dev/null 2>&1; then
  eval "$(command bs env path)"
fi

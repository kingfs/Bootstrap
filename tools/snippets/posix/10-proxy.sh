# ─── 代理：让 `bs proxy on/off/use` 在当前 shell 立即生效 ────────────────────
#
# 子进程无法修改父 shell 的环境变量（POSIX 硬约束），所以这里用一个与二进制同名的
# shell 函数做中转——nvm / direnv / zoxide / conda 都是同样的做法。
# 只有 proxy on|off|use 会被拦截，其余子命令原样直通真二进制。

bs() {
  case "${1:-}.${2:-}" in
    proxy.on|proxy.off|proxy.use)
      command bs "$@" || return $?
      eval "$(command bs proxy env --apply --shell "${BS_SHELL:-bash}")"
      ;;
    *)
      command bs "$@"
      ;;
  esac
}

# 保留维护者原有的手感（等价于旧的 startproxy / chaitinproxy 函数）。
startproxy()   { command bs proxy use local   >/dev/null 2>&1 && eval "$(command bs proxy env --apply --shell "${BS_SHELL:-bash}")"; }
stopproxy()    { command bs proxy off         >/dev/null 2>&1 && eval "$(command bs proxy env --apply --shell "${BS_SHELL:-bash}")"; }
chaitinproxy() { command bs proxy use chaitin >/dev/null 2>&1 && eval "$(command bs proxy env --apply --shell "${BS_SHELL:-bash}")"; }
bsproxy()      { eval "$(command bs proxy env "$@")"; }

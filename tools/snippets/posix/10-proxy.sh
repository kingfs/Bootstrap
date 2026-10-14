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

# 两个通用包装：`bs proxy on|off` 之后把结果应用进当前 shell。
# startproxy 可以带 profile 名（不带就用配置里的 default_profile）。
# 公司/个人专属的别名（例如把某个内网 profile 包成自己的函数）请放**用户片段**——
# <config>/shell/posix/10-proxy.sh 同名会整体覆盖本文件，不必改仓库里的这份。
startproxy() { command bs proxy on "$@" >/dev/null 2>&1 && eval "$(command bs proxy env --apply --shell "${BS_SHELL:-bash}")"; }
stopproxy()  { command bs proxy off      >/dev/null 2>&1 && eval "$(command bs proxy env --apply --shell "${BS_SHELL:-bash}")"; }
bsproxy()    { eval "$(command bs proxy env "$@")"; }

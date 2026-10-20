# ─── 代理与仓库：让需要改当前 shell 的命令真正生效 ──────────────────────────
#
# 子进程无法修改父 shell 的环境变量，也无法修改父 shell 的当前目录（POSIX 硬约束），
# 所以这里用一个与二进制同名的 shell 函数做中转——nvm / direnv / zoxide / conda
# 都是同样的做法。只拦截下面几条，其余子命令原样直通真二进制：
#
#   proxy on|off|use   执行完真二进制后 eval 它输出的环境变量
#   repo cd <repo>     把真二进制输出的路径 cd 过去
#   repo get ... --cd  执行完 get 后切到它输出的路径（认带专用标记的那一行）
#
# BS_SHELL_HOOK=1 只作用于被拦截的这一次调用：真二进制据此知道"stdout 会被 shell
# 函数消费"，于是 `repo get --cd` 把表格改写到 stderr，stdout 只留"标记+目标路径"
# （见 internal/shellsnip.HookEnv）。

bs() {
  case "${1:-}.${2:-}" in
    proxy.on|proxy.off|proxy.use)
      BS_SHELL_HOOK=1 command bs "$@" || return $?
      eval "$(command bs proxy env --apply --shell "${BS_SHELL:-bash}")"
      ;;
    repo.cd)
      # 只在 `bs repo cd <一个仓库>` 时接管；--help 之类保持真二进制的行为。
      if [ "$#" -eq 3 ] && [ "${3#-}" = "${3}" ]; then
        _bs_repo_dir=$(BS_SHELL_HOOK=1 command bs repo cd "${3}") || return $?
        cd -- "${_bs_repo_dir}"
      else
        command bs "$@"
      fi
      ;;
    repo.get)
      _bs_repo_cd=0
      _bs_repo_help=0
      for _bs_repo_arg in "$@"; do
        case "${_bs_repo_arg}" in
          --cd|--cd=true) _bs_repo_cd=1 ;;
          -h|--help) _bs_repo_help=1 ;;
          --) break ;;
        esac
      done
      if [ "${_bs_repo_cd}" = 1 ] && [ "${_bs_repo_help}" = 0 ]; then
        _bs_repo_out=$(BS_SHELL_HOOK=1 command bs "$@")
        _bs_repo_code=$?
        # 失败时把可能存在的输出原样还给用户，绝不 cd。
        if [ "${_bs_repo_code}" -ne 0 ]; then
          [ -z "${_bs_repo_out}" ] || printf '%s\n' "${_bs_repo_out}"
          return "${_bs_repo_code}"
        fi
        case "${_bs_repo_out}" in
          __bs_cd__:*)
            cd -- "${_bs_repo_out#__bs_cd__:}"
            ;;
          ?*)
            # 没带标记说明这不是"切目录"的输出（例如误判到了 -- 之后的仓库名），
            # 原样打印，别把它丢掉、更别拿它当目录。
            printf '%s\n' "${_bs_repo_out}"
            ;;
        esac
      else
        command bs "$@"
      fi
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

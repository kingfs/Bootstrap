# ─── 代理与仓库：让需要改当前 shell 的命令真正生效（fish 版） ────────────────
set -gx BS_SHELL fish

# BS_SHELL_HOOK 只对被拦截的这次调用生效（set -lx 是函数作用域）：
# 真二进制据此知道"stdout 会被这个函数消费"（见 internal/shellsnip.HookEnv）。
function bs
    if test (count $argv) -ge 2
        switch $argv[1]
            case proxy
                if contains -- $argv[2] on off use
                    set -lx BS_SHELL_HOOK 1
                    command bs $argv
                    or return $status
                    eval (command bs proxy env --apply --shell fish)
                    return
                end
            case repo
                if test "$argv[2]" = cd; and test (count $argv) -eq 3; and not string match -q -- '-*' $argv[3]
                    set -lx BS_SHELL_HOOK 1
                    set -l dir (command bs repo cd $argv[3])
                    or return $status
                    cd -- $dir
                    return
                end
                if test "$argv[2]" = get
                    set -l want_cd 0
                    set -l want_help 0
                    for a in $argv
                        switch $a
                            case --cd --cd=true
                                set want_cd 1
                            case -h --help
                                set want_help 1
                            case --
                                break
                        end
                    end
                    if test $want_cd -eq 1; and test $want_help -eq 0
                        set -lx BS_SHELL_HOOK 1
                        set -l out (command bs $argv)
                        set -l code $status
                        if test $code -ne 0
                            for line in $out
                                printf '%s\n' $line
                            end
                            return $code
                        end
                        if test (count $out) -eq 1; and string match -q '__bs_cd__:*' $out[1]
                            cd -- (string replace -r '^__bs_cd__:' '' -- $out[1])
                        else if test (count $out) -gt 0
                            # 没带标记：不是切目录的输出（例如误判到了 -- 之后的仓库名），原样打印。
                            for line in $out
                                printf '%s\n' $line
                            end
                        end
                        return
                    end
                end
        end
    end
    command bs $argv
end

# startproxy 可以带 profile 名（不带就用配置里的 default_profile）。
function startproxy
    command bs proxy on $argv >/dev/null 2>&1; and eval (command bs proxy env --apply --shell fish)
end

function stopproxy
    command bs proxy off >/dev/null 2>&1; and eval (command bs proxy env --apply --shell fish)
end

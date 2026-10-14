# ─── 代理：让 `bs proxy on/off/use` 在当前 shell 立即生效（fish 版） ─────────
set -gx BS_SHELL fish

# BS_SHELL_HOOK 只对被拦截的这次调用生效（set -lx 是函数作用域）：
# 真二进制据此知道"紧接着就会有人 source 它的输出"，不再打印没装包装函数的提示。
function bs
    if test (count $argv) -ge 2; and test $argv[1] = proxy; and contains -- $argv[2] on off use
        set -lx BS_SHELL_HOOK 1
        command bs $argv
        or return $status
        eval (command bs proxy env --apply --shell fish)
    else
        command bs $argv
    end
end

# startproxy 可以带 profile 名（不带就用配置里的 default_profile）。
function startproxy
    command bs proxy on $argv >/dev/null 2>&1; and eval (command bs proxy env --apply --shell fish)
end

function stopproxy
    command bs proxy off >/dev/null 2>&1; and eval (command bs proxy env --apply --shell fish)
end

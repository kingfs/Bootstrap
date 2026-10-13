# ─── 代理：让 `bs proxy on/off/use` 在当前 shell 立即生效（fish 版） ─────────
set -gx BS_SHELL fish

function bs
    if test (count $argv) -ge 2; and test $argv[1] = proxy; and contains -- $argv[2] on off use
        command bs $argv
        or return $status
        eval (command bs proxy env --apply --shell fish)
    else
        command bs $argv
    end
end

function startproxy
    command bs proxy use local >/dev/null 2>&1; and eval (command bs proxy env --apply --shell fish)
end

function stopproxy
    command bs proxy off >/dev/null 2>&1; and eval (command bs proxy env --apply --shell fish)
end

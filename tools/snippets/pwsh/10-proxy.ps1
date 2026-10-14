# ─── 代理：让 bs proxy on/off/use 在当前 shell 立即生效（PowerShell 版） ─────
$env:BS_SHELL = 'pwsh'

# BS_SHELL_HOOK 只对被拦截的这次调用生效（调用前后还原）：
# 真二进制据此知道"紧接着就会有人 Invoke-Expression 它的输出"，不再打印
# 没装包装函数的提示。
function bs {
    if ($args.Count -ge 2 -and $args[0] -eq 'proxy' -and @('on', 'off', 'use') -contains $args[1]) {
        $exe = (Get-Command bs -CommandType Application -ErrorAction SilentlyContinue).Source
        if (-not $exe) { return }
        $hookWasSet = Test-Path Env:BS_SHELL_HOOK
        $env:BS_SHELL_HOOK = '1'
        & $exe @args
        $code = $LASTEXITCODE
        if (-not $hookWasSet) { Remove-Item Env:BS_SHELL_HOOK -ErrorAction SilentlyContinue }
        if ($code -ne 0) { return }
        & $exe proxy env --apply --shell pwsh | Out-String | Invoke-Expression
    }
    else {
        $exe = (Get-Command bs -CommandType Application -ErrorAction SilentlyContinue).Source
        if ($exe) { & $exe @args } else { Write-Error 'bs not found' }
    }
}

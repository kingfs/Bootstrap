# ─── 代理：让 bs proxy on/off/use 在当前 shell 立即生效（PowerShell 版） ─────
$env:BS_SHELL = 'pwsh'

function bs {
    if ($args.Count -ge 2 -and $args[0] -eq 'proxy' -and @('on', 'off', 'use') -contains $args[1]) {
        $exe = (Get-Command bs -CommandType Application -ErrorAction SilentlyContinue).Source
        if (-not $exe) { return }
        & $exe @args
        if ($LASTEXITCODE -ne 0) { return }
        & $exe proxy env --apply --shell pwsh | Out-String | Invoke-Expression
    }
    else {
        $exe = (Get-Command bs -CommandType Application -ErrorAction SilentlyContinue).Source
        if ($exe) { & $exe @args } else { Write-Error 'bs not found' }
    }
}

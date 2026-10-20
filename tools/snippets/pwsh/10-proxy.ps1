# ─── 代理与仓库：让需要改当前 shell 的命令真正生效（PowerShell 版） ─────────
$env:BS_SHELL = 'pwsh'

# BS_SHELL_HOOK 只对被拦截的这次调用生效（调用前后还原）：
# 真二进制据此知道"stdout 会被这个函数消费"（见 internal/shellsnip.HookEnv）。
function bs {
    $exe = (Get-Command bs -CommandType Application -ErrorAction SilentlyContinue).Source
    if (-not $exe) { Write-Error 'bs not found'; return }

    # repo cd <repo>：把真二进制输出的路径 Set-Location 过去。
    $repoPath = if ($args.Count -ge 3) { "$($args[2])" } else { '' }
    if ($args.Count -eq 3 -and $args[0] -eq 'repo' -and $args[1] -eq 'cd' -and -not $repoPath.StartsWith('-')) {
        $hookWasSet = Test-Path Env:BS_SHELL_HOOK
        $env:BS_SHELL_HOOK = '1'
        $dir = (& $exe repo cd $repoPath | Select-Object -Last 1)
        $code = $LASTEXITCODE
        if (-not $hookWasSet) { Remove-Item Env:BS_SHELL_HOOK -ErrorAction SilentlyContinue }
        if ($code -ne 0) { return }
        if ($dir) { Set-Location -LiteralPath "$dir" }
        return
    }

    # repo get ... --cd：表格走 stderr，stdout 只留目标路径（有输出才切）。
    if ($args.Count -ge 2 -and $args[0] -eq 'repo' -and $args[1] -eq 'get') {
        $wantCd = $false
        $wantHelp = $false
        foreach ($a in $args) {
            if ($a -eq '--') { break }
            if ($a -eq '--cd' -or $a -eq '--cd=true') { $wantCd = $true }
            if ($a -eq '-h' -or $a -eq '--help') { $wantHelp = $true }
        }
        if ($wantCd -and -not $wantHelp) {
            $hookWasSet = Test-Path Env:BS_SHELL_HOOK
            $env:BS_SHELL_HOOK = '1'
            $out = @(& $exe @args)
            $code = $LASTEXITCODE
            if (-not $hookWasSet) { Remove-Item Env:BS_SHELL_HOOK -ErrorAction SilentlyContinue }
            if ($code -ne 0) {
                if ($out.Count -gt 0) { Write-Output $out }
                return
            }
            if ($out.Count -eq 1 -and "$($out[0])".StartsWith('__bs_cd__:')) {
                $target = "$($out[0])".Substring('__bs_cd__:'.Length)
                Set-Location -LiteralPath $target
            }
            elseif ($out.Count -gt 0) {
                # 没带标记：不是切目录的输出（例如误判到了 -- 之后的仓库名），原样打印。
                Write-Output $out
            }
            return
        }
    }

    # proxy on|off|use：执行完真二进制后 eval 它输出的环境变量。
    if ($args.Count -ge 2 -and $args[0] -eq 'proxy' -and @('on', 'off', 'use') -contains $args[1]) {
        $hookWasSet = Test-Path Env:BS_SHELL_HOOK
        $env:BS_SHELL_HOOK = '1'
        & $exe @args
        $code = $LASTEXITCODE
        if (-not $hookWasSet) { Remove-Item Env:BS_SHELL_HOOK -ErrorAction SilentlyContinue }
        if ($code -ne 0) { return }
        & $exe proxy env --apply --shell pwsh | Out-String | Invoke-Expression
        return
    }

    & $exe @args
}

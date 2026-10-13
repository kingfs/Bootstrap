# ─── PATH 装配（PowerShell 版） ──────────────────────────────────────────────
$exe = (Get-Command bs -CommandType Application -ErrorAction SilentlyContinue).Source
if ($exe) {
    (& $exe env path --shell pwsh | Out-String) | Invoke-Expression
}

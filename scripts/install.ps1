<#
.SYNOPSIS
  Windows 版 Bootstrap (bs) 安装脚本。

.DESCRIPTION
  从 GitHub Release 下载对应平台的 zip，用 checksums.txt 校验 sha256 后安装到
  $env:LOCALAPPDATA\Programs\bs，并把该目录加入用户 PATH（不修改系统 PATH）。

.EXAMPLE
  irm https://raw.githubusercontent.com/kingfs/Bootstrap/main/scripts/install.ps1 | iex
  ./scripts/install.ps1 -Version v0.1.0 -Dir "$env:USERPROFILE\bin"
#>
[CmdletBinding()]
param(
    [string]$Version = 'latest',
    [string]$Dir = '',
    [string]$Repo = 'kingfs/Bootstrap'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Say($msg) { Write-Host $msg }
function Fail($msg) { Write-Error "install: $msg"; exit 1 }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { Fail "不支持的架构：$env:PROCESSOR_ARCHITECTURE" }
}

if (-not $Dir) {
    $Dir = Join-Path $env:LOCALAPPDATA 'Programs\bs'
}

if ($Version -eq 'latest') {
    # 不依赖 API 鉴权：跟随 releases/latest 的重定向。
    $release = Invoke-WebRequest -UseBasicParsing -MaximumRedirection 0 -Uri "https://github.com/$Repo/releases/latest" -ErrorAction SilentlyContinue
    $Version = ($release.Headers.Location -split '/tag/')[-1]
    if (-not $Version) { Fail '无法确定最新版本，请用 -Version 指定' }
}
if (-not $Version.StartsWith('v')) { $Version = "v$Version" }
$plain = $Version.TrimStart('v')

$asset = "bootstrap_${plain}_windows_${arch}.zip"
$baseUrl = "https://github.com/$Repo/releases/download/$Version"
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("bs-install-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null

Say "仓库    : $Repo"
Say "版本    : $Version"
Say "平台    : windows/$arch"
Say "安装到  : $Dir"

try {
    Say '==> 下载'
    $zipPath = Join-Path $tmp $asset
    Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/$asset" -OutFile $zipPath

    Say '==> 校验 sha256'
    $sumsPath = Join-Path $tmp 'checksums.txt'
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/checksums.txt" -OutFile $sumsPath
    }
    catch {
        Fail '无法下载 checksums.txt，出于安全考虑中止安装'
    }
    $expected = (Select-String -Path $sumsPath -Pattern ([regex]::Escape($asset)) |
        Select-Object -First 1).Line -split '\s+' | Select-Object -First 1
    if (-not $expected) { Fail "checksums.txt 中没有 $asset 的记录" }
    $actual = (Get-FileHash -Algorithm SHA256 -Path $zipPath).Hash.ToLower()
    if ($actual -ne $expected.ToLower()) { Fail "校验失败：期望 $expected，实际 $actual" }
    Say '    sha256 匹配'

    Say '==> 解压安装'
    Expand-Archive -Path $zipPath -DestinationPath $tmp -Force
    $exe = Join-Path $tmp 'bs.exe'
    if (-not (Test-Path $exe)) { Fail '归档中找不到 bs.exe' }

    New-Item -ItemType Directory -Force -Path $Dir | Out-Null
    Copy-Item -Force $exe (Join-Path $Dir 'bs.exe')
    Say "已安装：$(Join-Path $Dir 'bs.exe')"

    # 加入用户 PATH（只改用户级，不动系统级）。
    $userPath = [Environment]::GetEnvironmentVariable('PATH', 'User')
    if ($userPath -notlike "*$Dir*") {
        [Environment]::SetEnvironmentVariable('PATH', "$userPath;$Dir", 'User')
        Say ""
        Say "已把 $Dir 加入用户 PATH（新开的终端生效）。"
    }
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

Say ''
Say '运行 `bs version` 验证；接着可以：'
Say '  bs config init    # 生成默认配置（含代理 profile 示例）'
Say '  bs shell install  # 安装 PowerShell 受管块'
Say '  bs doctor         # 体检'

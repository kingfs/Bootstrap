<#
.SYNOPSIS
  Windows 版无副作用冒烟测试。

.DESCRIPTION
  与 scripts/smoke.sh 对应：所有读写都在临时沙箱目录内完成，
  不触碰真实的 $HOME / 注册表 / 系统目录，也不发起网络请求。

.EXAMPLE
  ./scripts/smoke.ps1 -Bin ./bin/bs.exe
#>
[CmdletBinding()]
param(
    [string]$Bin = "$PSScriptRoot/../bin/bs.exe",
    [string]$Work = "$PSScriptRoot/../.cache/smoke-win"
)

$ErrorActionPreference = 'Stop'
$script:Pass = 0
$script:Fail = 0

function Ok($msg) { $script:Pass++; Write-Host "  ok   $msg" }
function Bad($msg) { $script:Fail++; Write-Host "  FAIL $msg" -ForegroundColor Red }

function Assert-Contains($desc, $needle, $text) {
    if ("$text" -like "*$needle*") { Ok $desc } else { Bad "$desc（期望包含 '$needle'）" }
}
function Assert-Missing($desc, $path) {
    if (Test-Path $path) { Bad "$desc（不应存在：$path）" } else { Ok $desc }
}
function Assert-Equal($desc, $expected, $actual) {
    if ("$expected" -eq "$actual") { Ok $desc } else { Bad "$desc（期望 '$expected'，实际 '$actual'）" }
}

if (-not (Test-Path $Bin)) { throw "找不到二进制：$Bin（先运行 go build -o bin/bs.exe ./cmd/bs）" }

if (Test-Path $Work) { Remove-Item -Recurse -Force $Work }
$home = Join-Path $Work 'home'
$config = Join-Path $Work 'config'
$state = Join-Path $Work 'state'
$cache = Join-Path $Work 'cache'
New-Item -ItemType Directory -Force -Path $home, $config, $state, $cache | Out-Null

$env:HOME = $home
$env:USERPROFILE = $home
$env:BS_CONFIG_HOME = $config
$env:BS_STATE_HOME = $state
$env:BS_CACHE_HOME = $cache
$env:NO_COLOR = '1'
Remove-Item Env:HTTP_PROXY, Env:HTTPS_PROXY, Env:http_proxy, Env:https_proxy, Env:no_proxy, Env:NO_PROXY -ErrorAction SilentlyContinue

Write-Host "`n== 版本与路径" -ForegroundColor Cyan
Assert-Contains 'bs version' 'bs ' (& $Bin version | Out-String)

$out = & $Bin config path | Out-String
Assert-Contains 'config path 使用沙箱目录' $config $out

Write-Host "`n== 配置" -ForegroundColor Cyan
& $Bin config init | Out-Null
$cfgFile = Join-Path $config 'config.yaml'
if (Test-Path $cfgFile) { Ok '生成默认配置' } else { Bad '生成默认配置' }
$before = (Get-Item $cfgFile).LastWriteTimeUtc
Start-Sleep -Milliseconds 1100
$out = & $Bin config init | Out-String
Assert-Contains '重复 init 不覆盖' '已存在' $out
Assert-Equal '重复 init 未修改文件' $before (Get-Item $cfgFile).LastWriteTimeUtc

@'
version: 1
proxy:
  default_profile: local
  profiles:
    local:
      http_proxy: http://127.0.0.1:7890
      https_proxy: http://127.0.0.1:7890
      no_proxy: 127.0.0.1,localhost
tool:
  extra_path:
    - ~/.local/bin
'@ | Set-Content -Path $cfgFile -Encoding utf8
$out = & $Bin config validate | Out-String
Assert-Contains '配置合法' '配置合法' $out

Write-Host "`n== 代理" -ForegroundColor Cyan
$out = & $Bin proxy list | Out-String
Assert-Contains 'proxy list' 'local' $out
$out = & $Bin proxy env --apply --shell pwsh | Out-String
Assert-Contains 'pwsh 代理渲染' '$env:HTTP_PROXY' $out
$out = & $Bin proxy env --shell json | Out-String
Assert-Contains 'json 代理渲染' 'HTTPS_PROXY' $out
& $Bin proxy off | Out-Null
$out = & $Bin proxy env --apply --shell pwsh | Out-String
Assert-Contains 'off 输出清除语句' 'Remove-Item Env:HTTP_PROXY' $out

Write-Host "`n== shell 片段" -ForegroundColor Cyan
$out = & $Bin shell init pwsh | Out-String
Assert-Contains 'pwsh 片段含包装函数' 'function bs' $out

Write-Host "`n== 仓库计划（dry-run）" -ForegroundColor Cyan
$out = & $Bin repo get kingfs/Bootstrap --dry-run 2>&1 | Out-String
Assert-Contains 'dry-run 显示 clone' 'git clone' $out
Assert-Missing 'dry-run 不创建目录' (Join-Path $home 'src')

Write-Host "`n== 退出码" -ForegroundColor Cyan
& $Bin definitely-not-a-command *> $null
Assert-Equal '未知命令退出码为 2' 2 $LASTEXITCODE

Write-Host "`n结果：$script:Pass 通过，$script:Fail 失败" -ForegroundColor Cyan
if ($script:Fail -gt 0) { exit 1 }
Write-Host "沙箱目录：$Work（可安全删除）"

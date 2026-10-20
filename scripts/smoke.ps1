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
# 被调用程序写 stderr / 返回非 0 都在本脚本的预期内（退出码本身就是要断言的），
# 别让 pwsh 7.4+ 的这条偏好把它们变成终止性错误（变量在旧版本上不存在，赋值无害）。
$PSNativeCommandUseErrorActionPreference = $false
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
# 不能叫 $home：PowerShell 的 $HOME 是只读自动变量（大小写不敏感），
# 赋值会直接抛 "Cannot overwrite variable HOME because it is read-only or constant."
$homeDir = Join-Path $Work 'home'
$config = Join-Path $Work 'config'
$state = Join-Path $Work 'state'
$cache = Join-Path $Work 'cache'
New-Item -ItemType Directory -Force -Path $homeDir, $config, $state, $cache | Out-Null

$env:HOME = $homeDir
$env:USERPROFILE = $homeDir
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

Write-Host "`n== 补全" -ForegroundColor Cyan
$out = & $Bin completion powershell 2>&1 | Out-String
Assert-Contains 'completion powershell 输出脚本' 'powershell completion for bs' $out
$out = & $Bin completion --help 2>&1 | Out-String
Assert-Contains 'completion 帮助含 install 用法' 'bs completion install' $out
$compRc = Join-Path $homeDir '.completion-profile.ps1'
$out = & $Bin completion install pwsh --rc $compRc 2>&1 | Out-String
Assert-Contains 'install 汇报打开开关' 'shell.completion' $out
Assert-Contains 'shell init 渲染补全片段' 'completion powershell' (& $Bin shell init pwsh | Out-String)

Write-Host "`n== 仓库计划（dry-run）" -ForegroundColor Cyan
$out = & $Bin repo get kingfs/Bootstrap --dry-run 2>&1 | Out-String
Assert-Contains 'dry-run 显示 clone' 'git clone' $out
Assert-Missing 'dry-run 不创建目录' (Join-Path $homeDir 'src')

Write-Host "`n== shell 包装函数（repo cd）" -ForegroundColor Cyan
# 包装函数里的 Get-Command bs 依赖 PATH：把真二进制所在目录放进去。
$binDir = Split-Path -Parent (Resolve-Path $Bin)
$env:PATH = "$binDir;$env:PATH"
$wrapRepo = Join-Path $homeDir 'src/wrap.test/team/project'
New-Item -ItemType Directory -Force -Path (Join-Path $wrapRepo '.git') | Out-Null
& $Bin repo scan | Out-Null
$snippet = & $Bin shell init pwsh | Out-String
Invoke-Expression $snippet
$oldLocation = Get-Location
try {
    Set-Location $Work
    bs repo cd wrap.test/team/project
    Assert-Equal '包装函数让 repo cd 真正切换目录' 'project' (Split-Path -Leaf (Get-Location).Path)
}
finally {
    Set-Location $oldLocation
}

Write-Host "`n== init --no-tools（只做准备）" -ForegroundColor Cyan
$out = & $Bin init --no-tools --yes --no-shell --dry-run 2>&1 | Out-String
Assert-Contains 'init --no-tools 说明跳过了工具安装' '已跳过工具安装' $out

Write-Host "`n== 退出码" -ForegroundColor Cyan
& $Bin definitely-not-a-command *> $null
Assert-Equal '未知命令退出码为 2' 2 $LASTEXITCODE

Write-Host "`n结果：$script:Pass 通过，$script:Fail 失败" -ForegroundColor Cyan
if ($script:Fail -gt 0) { exit 1 }
Write-Host "沙箱目录：$Work（可安全删除）"
# 必须显式 exit 0：脚本里最后一条**外部命令**的退出码会留在 $LASTEXITCODE
# （这里正是"未知命令退出码为 2"那条断言），而 CI 的 pwsh 包装会在脚本结束后
# 拿它当进程退出码——于是 14 项全过、步骤仍然判失败。
exit 0

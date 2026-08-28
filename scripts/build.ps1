# 构建 dsh-systray.exe（windowsgui，无控制台窗口）
# 用法：.\scripts\build.ps1 [-Version v0.3.0]（默认 dev，此时跳过自动更新检查）
param(
    [string]$Version = 'dev'
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$dist = Join-Path $root 'dist'
New-Item -ItemType Directory -Force -Path $dist | Out-Null

$goCandidates = @('D:\Program Files\Go\bin\go.exe', 'go')
$go = $null
foreach ($c in $goCandidates) {
    $found = Get-Command $c -ErrorAction SilentlyContinue
    if ($found) { $go = $found.Source; break }
}
if (-not $go) { throw 'Go toolchain not found' }

# 国内网络友好：默认走 goproxy.cn（如需官方源，删除此设置）
if (-not $env:GOPROXY) { $env:GOPROXY = 'https://goproxy.cn,direct' }

Push-Location $root
& $go mod tidy
if ($LASTEXITCODE -ne 0) { Pop-Location; throw 'go mod tidy failed' }
& $go build -trimpath -ldflags "-s -w -H=windowsgui -X main.appVersion=$Version" -o (Join-Path $dist 'dsh-systray.exe') .
if ($LASTEXITCODE -ne 0) { Pop-Location; throw 'go build failed' }
Pop-Location

Write-Host "Built $dist\dsh-systray.exe"

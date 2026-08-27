# 构建 dsh-systray.exe（windowsgui，无控制台窗口）
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
# 版本号注入：优先取最近 git 标签（如 v1.2.0）；无 git / 无标签时为 dev（跳过自更新检查）
$ver = 'dev'
if (Get-Command git -ErrorAction SilentlyContinue) {
    $tag = & git describe --tags --abbrev=0 2>$null
    if ($LASTEXITCODE -eq 0 -and $tag) { $ver = ($tag | Select-Object -First 1).Trim() }
}
Write-Host "version: $ver"
& $go build -trimpath -ldflags "-s -w -H=windowsgui -X main.version=$ver" -o (Join-Path $dist 'dsh-systray.exe') .
if ($LASTEXITCODE -ne 0) { Pop-Location; throw 'go build failed' }
Pop-Location

Write-Host "Built $dist\dsh-systray.exe"

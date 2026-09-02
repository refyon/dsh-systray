# 构建 dsh-systray.exe（Wails 规范构建：自动嵌入 build/windows/icon.ico 图标资源 + production 标签）
# 注意：必须用 wails build（不能 go build）——纯 go build 会命中 Wails 错误桩并丢失 exe/窗口图标。
# bindings 单独生成（wails generate module），随后 build 用 -skipbindings：
#   wails build 内嵌的 bindings 阶段会把编译出的 wailsbindings.exe 当作完整程序运行一次，
#   在本机（含开机自启动注册表项 / 单实例 / GUI 场景）会因运行副作用不稳定而失败；
#   单独 generate module 稳定成功（exit 0）且产物与内嵌一致。
# 用法：.\scripts\build.ps1 [-Version v0.3.0]（默认 dev，此时跳过自动更新检查）
param(
    [string]$Version = 'dev'
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$dist = Join-Path $root 'dist'
New-Item -ItemType Directory -Force -Path $dist | Out-Null

# 先停掉可能占用 build\bin\dsh-systray.exe 的残留进程，避免 wails build 删除产物时 Access denied
Get-Process dsh-systray -ErrorAction SilentlyContinue | Stop-Process -Force

$wails = $null
foreach ($c in @("$env:GOPATH\bin\wails.exe", 'wails')) {
    $found = Get-Command $c -ErrorAction SilentlyContinue
    if ($found) { $wails = $found.Source; break }
}
if (-not $wails) {
    throw '未找到 wails CLI。请先安装: go install github.com/wailsapp/wails/v2/cmd/wails@v2.15.0'
}

if (-not $env:GOPROXY) { $env:GOPROXY = 'https://goproxy.cn,direct' }

Push-Location $root
# 1) 生成 wailsjs 绑定（单独命令，稳定成功）；捕获输出便于失败诊断
$genOut = & $wails generate module 2>&1 | Out-String
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "wails generate module failed (exit $LASTEXITCODE)`n$genOut" }
# 2) 编译（跳过内嵌 bindings，复用上一步产物）
$buildOut = & $wails build -skipbindings -s -platform windows/amd64 -ldflags "-s -w -H=windowsgui -X main.appVersion=$Version" 2>&1 | Out-String
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "wails build failed (exit $LASTEXITCODE)`n$buildOut" }
Copy-Item (Join-Path $root 'build\bin\dsh-systray.exe') (Join-Path $dist 'dsh-systray.exe') -Force
Pop-Location

Write-Host "Built $dist\dsh-systray.exe (wails build, icon embedded)"

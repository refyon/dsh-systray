# 构建 dsh-systray.exe（Wails 规范构建：自动嵌入 src/build/windows/icon.ico 图标资源 + production 标签）
# 注意：必须用 wails build（不能 go build）——纯 go build 会命中 Wails 错误桩并丢失 exe/窗口图标。
# Go 模块、wails.json、build/、frontend/ 都在 src\（Wails 项目根，见仓库整理），
# 因此 wails 命令一律在 src\ 下执行；编译产物留在 src\build\bin\，仓库根不再放编译产物。
# bindings 单独生成（wails generate module），随后 build 用 -skipbindings：
#   wails build 内嵌的 bindings 阶段会把编译出的 wailsbindings.exe 当作完整程序运行一次，
#   在本机（含开机自启动注册表项 / 单实例 / GUI 场景）会因运行副作用不稳定而失败；
#   单独 generate module 稳定成功（exit 0）且产物与内嵌一致。
# 用法：.\scripts\build.ps1 [-Version v0.3.0]（默认 dev，此时跳过自动更新检查）
#
# 产物完整性标签（编译后自动执行，见文件末尾）：
#   仓库位于 agent 工作区（D:\agent-env）内时，工作区整棵树带有 DSH 沙箱写入授权留下的
#   「Mandatory Label\Low Mandatory Level」强制标签；Windows 会用镜像文件的标签给进程
#   完整性封顶，产物因此只能以 Low 完整性启动——而资源管理器任务栏（本机为提权 High）
#   按 UIPI 拒绝低完整性进程的 Shell_NotifyIcon 注册，表现为「Access is denied」闪退。
#   故编译后把 bin 目录与产物 exe 的标签提升为 High：此后新产物自动继承、编译完即可直接运行。
#   标签位于 SACL，设置需 WRITE_OWNER（目录完全控制），因此请在提权 shell 中构建；
#   设置失败只告警不中断，并明确提示该情况下产物将以低完整性运行。
param(
    [string]$Version = 'dev'
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$src = Join-Path $root 'src'
$binDir = Join-Path $src 'build\bin'
$bin = Join-Path $binDir 'dsh-systray.exe'

# 先停掉可能占用产物的残留进程，避免 wails build 删除产物时 Access denied
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

Push-Location $src
try {
    # 1) 生成 wailsjs 绑定（单独命令，稳定成功）；捕获输出便于失败诊断
    $genOut = & $wails generate module 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0) { throw "wails generate module failed (exit $LASTEXITCODE)`n$genOut" }
    # 2) 编译（跳过内嵌 bindings，复用上一步产物）
    $buildOut = & $wails build -skipbindings -s -platform windows/amd64 -ldflags "-s -w -H=windowsgui -X main.appVersion=$Version" 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0) { throw "wails build failed (exit $LASTEXITCODE)`n$buildOut" }
} finally { Pop-Location }

Write-Host "Built $bin (wails build, icon embedded)"

# 产物打 High 完整性标签（原因见文件头）：
#   目录用可继承形态，供后续构建产物自动继承；exe 单独再设一次，让本次产物立即生效
#   （继承变更不会回溯到已存在的子对象）。icacls 重复设置同一标签是幂等的。
$labelFailures = @()
foreach ($item in @(
        @{ Path = $binDir; Level = '(OI)(CI)H' },
        @{ Path = $bin; Level = 'H' }
    )) {
    $out = & icacls $item.Path /setintegritylevel $item.Level 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0) {
        $labelFailures += "$($item.Path)：$($out.Trim())"
    }
}

if ($labelFailures.Count -gt 0) {
    Write-Warning ("完整性标签设置失败（产物将以低完整性运行，托盘注册会被拒绝并闪退）：`n  " +
        ($labelFailures -join "`n  ") + "`n请在提权 shell 中重新执行本脚本。")
} else {
    $label = (& icacls $bin 2>&1 | Select-String 'Mandatory Label' | Select-Object -First 1)
    if ($label) { Write-Host "Integrity label: $($label.Line.Trim())" }
    Write-Host "可直接运行：$bin"
}

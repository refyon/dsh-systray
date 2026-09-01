# dsh-systray 真实 UI 截图脚本（WebView2 CDP 直出，高保真）
# 用法: powershell -ExecutionPolicy Bypass -File capture_shots.ps1
# 原理: 通过 DSH_SYSTRAY_SHOT_PAGE 让前端启动后直接显示指定页；
#       WebView2 开启 CDP 端口（--remote-debugging-port=9333），
#       capture_cdp.py 调 Page.captureScreenshot 直出渲染像素（不经屏幕合成）。
# 前置: 已构建 build/bin/dsh-systray.exe；python + websocket-client

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$exe = Join-Path $root 'build\bin\dsh-systray.exe'
$outDir = Join-Path $root 'docs\shots'
$cdpPy = Join-Path $PSScriptRoot 'capture_cdp.py'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

Get-Process dsh-systray -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Milliseconds 600
$env:DSH_SYSTRAY_SHOW_WINDOW = '1'

function Wait-Cdp([int]$timeoutSec = 25) {
  $deadline = (Get-Date).AddSeconds($timeoutSec)
  while ((Get-Date) -lt $deadline) {
    try {
      $r = Invoke-WebRequest -Uri 'http://127.0.0.1:9333/json' -UseBasicParsing -TimeoutSec 2 -ErrorAction Stop
      if ($r.StatusCode -eq 200) { return $true }
    } catch { Start-Sleep -Milliseconds 500 }
  }
  return $false
}

function SnapCdp([string]$name, [string]$page) {
  Get-Process dsh-systray -ErrorAction SilentlyContinue | Stop-Process -Force
  Start-Sleep -Milliseconds 600
  Remove-Item Env:DSH_SYSTRAY_SHOT_SPLASH -ErrorAction SilentlyContinue
  $env:DSH_SYSTRAY_SHOT_PAGE = $page
  $p = Start-Process -FilePath $exe -PassThru
  if (-not (Wait-Cdp)) { Write-Host "skip $name (CDP not ready)"; return }
  # 等页面渲染稳定
  Start-Sleep -Seconds 2
  $out = Join-Path $outDir "$name.png"
  python $cdpPy $out 9333
  Get-Process dsh-systray -ErrorAction SilentlyContinue | Stop-Process -Force
  Start-Sleep -Milliseconds 600
}

SnapCdp 'general' 'general'
SnapCdp 'about'   'about'
SnapCdp 'logs'    'logs'
SnapCdp 'export'  'export'
SnapCdp 'import'  'import'

Get-Process dsh-systray -ErrorAction SilentlyContinue | Stop-Process -Force

# 转 WebP（网站/README 用），删除 PNG 源
$py = Get-Command python -ErrorAction SilentlyContinue
if ($py) {
  & python "$PSScriptRoot\convert_webp.py" 2>&1 | Out-Null
  Write-Host 'webp converted'
}
Write-Host 'done'

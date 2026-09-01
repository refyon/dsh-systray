# dsh-systray 真实 UI 截图脚本（Windows）
# 用法: powershell -ExecutionPolicy Bypass -File capture_shots.ps1
# 原理: 通过 DSH_SYSTRAY_SHOT_PAGE 让前端启动后直接显示指定页，逐页重启进程截图。
# 裁剪策略: 只捕获 client 内容区（跳过标题栏），四周再内缩 8px（覆盖 Win11 圆角露底），
#           确保输出为纯窗口内容、不含后方背景/阴影/圆角透底。
# 前置: 已构建 build/bin/dsh-systray.exe

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public class Cap {
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool GetClientRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool ClientToScreen(IntPtr h, ref POINT p);
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
  [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L, T, R, B; }
  [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
}
"@

# 圆角内缩量（px）：覆盖 Windows 11 默认窗口圆角半径，避免四角透出背景
$crop = 8

$root = Split-Path -Parent $PSScriptRoot
$exe = Join-Path $root 'build\bin\dsh-systray.exe'
$outDir = Join-Path $root 'docs\shots'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

Get-Process dsh-systray -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Milliseconds 600
$env:DSH_SYSTRAY_SHOW_WINDOW = '1'

function SnapProcess([string]$name, [string]$page, [int]$waitSec) {
  Get-Process dsh-systray -ErrorAction SilentlyContinue | Stop-Process -Force
  Start-Sleep -Milliseconds 600
  Remove-Item Env:DSH_SYSTRAY_SHOT_SPLASH -ErrorAction SilentlyContinue
  $env:DSH_SYSTRAY_SHOT_PAGE = $page
  $p = Start-Process -FilePath $exe -PassThru
  Start-Sleep -Seconds $waitSec
  $p.Refresh()
  if ($p.MainWindowHandle -eq [IntPtr]::Zero) { Write-Host "skip $name (no window)"; return }
  # 置顶 + 前台，等待稳定
  [void][Cap]::SetWindowPos($p.MainWindowHandle, [IntPtr](-1), 0, 0, 0, 0, 0x0001 -bor 0x0002 -bor 0x0040)
  [void][Cap]::SetForegroundWindow($p.MainWindowHandle)
  Start-Sleep -Milliseconds 1200

  # client 内容区（不含标题栏/阴影/圆角边框）
  $cr = [Cap+RECT]::new()
  [void][Cap]::GetClientRect($p.MainWindowHandle, [ref]$cr)
  $pt = [Cap+POINT]::new()
  [void][Cap]::ClientToScreen($p.MainWindowHandle, [ref]$pt)
  $cw = $cr.R - $cr.L; $ch = $cr.B - $cr.T
  if ($cw -le 0 -or $ch -le 0) { Write-Host "skip $name (bad client rect)"; return }

  # 截取 client 区
  $full = New-Object System.Drawing.Bitmap($cw, $ch)
  $g = [System.Drawing.Graphics]::FromImage($full)
  $g.CopyFromScreen($pt.X, $pt.Y, 0, 0, $full.Size)
  $g.Dispose()

  # 四周内缩 $crop px，裁掉圆角透底（输出全尺寸源图，缩放/锐化由 convert_webp.py 用 Pillow 高质量完成）
  $ow = $cw - 2 * $crop; $oh = $ch - 2 * $crop
  $out = New-Object System.Drawing.Bitmap($ow, $oh)
  $g2 = [System.Drawing.Graphics]::FromImage($out)
  $g2.DrawImage($full, (New-Object System.Drawing.Rectangle(0, 0, $ow, $oh)), `
    (New-Object System.Drawing.Rectangle($crop, $crop, $ow, $oh)), [System.Drawing.GraphicsUnit]::Pixel)
  $g2.Dispose()
  $out.Save((Join-Path $outDir "$name.png"), [System.Drawing.Imaging.ImageFormat]::Png)
  $full.Dispose(); $out.Dispose()
  Write-Host ("saved {0}.png ({1}x{2})" -f $name, $ow, $oh)
}

SnapProcess 'general' 'general' 7
SnapProcess 'about'   'about'   5
SnapProcess 'logs'    'logs'    5
SnapProcess 'export'  'export'  5
SnapProcess 'import'  'import'  5

Get-Process dsh-systray -ErrorAction SilentlyContinue | Stop-Process -Force

# 转 WebP（网站/README 用），删除 PNG 源
$py = Get-Command python -ErrorAction SilentlyContinue
if ($py) {
  & python "$PSScriptRoot\convert_webp.py" 2>&1 | Out-Null
  Write-Host 'webp converted'
}
Write-Host 'done'

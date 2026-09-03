$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public class Cap {
  [DllImport("user32.dll")] public static extern bool GetClientRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool ClientToScreen(IntPtr h, ref POINT p);
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);
  [DllImport("user32.dll")] public static extern bool PrintWindow(IntPtr h, IntPtr hdc, uint flags);
  [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int cmd);
  [DllImport("user32.dll")] public static extern void keybd_event(byte vk, byte scan, uint flags, UIntPtr extra);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L, T, R, B; }
  [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
}
"@

$crop = 8
$root = Split-Path -Parent $PSScriptRoot
$exe = Join-Path $root 'build\bin\dsh-systray.exe'
$outDir = Join-Path $root 'docs\shots'
$readyFile = Join-Path $env:TEMP 'dsh-shot-ready.flag'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

# 截图/预览模式：抑制「打开 Web UI」弹窗；阻止启动完成后隐藏设置窗口
$env:DSH_SYSTRAY_SHOW_WINDOW = '1'
$env:DSH_SYSTRAY_SHOT_READY_FILE = $readyFile

function Test-Varied([System.Drawing.Bitmap]$bmp) {
  $seen = @{}
  $w = $bmp.Width; $h = $bmp.Height
  foreach ($fx in 0.12,0.25,0.5,0.75,0.88) {
    foreach ($fy in 0.12,0.25,0.5,0.75,0.88) {
      $c = $bmp.GetPixel([int]($w*$fx),[int]($h*$fy))
      $seen[[string]$c.ToArgb()] = 1
    }
  }
  return ($seen.Count -gt 3)
}

function Save-Shot([System.Drawing.Bitmap]$bmp, [string]$name) {
  $ow = $bmp.Width - 2*$crop; $oh = $bmp.Height - 2*$crop
  if ($ow -le 0 -or $oh -le 0) { return }
  $out = New-Object System.Drawing.Bitmap($ow, $oh)
  $g2 = [System.Drawing.Graphics]::FromImage($out)
  $g2.DrawImage($bmp, (New-Object System.Drawing.Rectangle(0,0,$ow,$oh)), (New-Object System.Drawing.Rectangle($crop,$crop,$ow,$oh)), [System.Drawing.GraphicsUnit]::Pixel)
  $g2.Dispose()
  $out.Save((Join-Path $outDir "$name.png"), [System.Drawing.Imaging.ImageFormat]::Png)
  Write-Host ("saved {0}.png ({1}x{2})" -f $name, $ow, $oh)
  $out.Dispose()
}

function Stop-AllInstances {
  Get-Process dsh-systray -ErrorAction SilentlyContinue | Stop-Process -Force
  $deadline = (Get-Date).AddSeconds(6)
  while ((Get-Date) -lt $deadline) {
    if (-not (Get-Process dsh-systray -ErrorAction SilentlyContinue)) { break }
    Start-Sleep -Milliseconds 300
  }
  Start-Sleep -Milliseconds 700
}

function SnapProcess([string]$name, [string]$page, [string]$scroll = '' ) {
  Stop-AllInstances
  Remove-Item Env:DSH_SYSTRAY_SHOT_SPLASH -ErrorAction SilentlyContinue
  if ($scroll) { $env:DSH_SYSTRAY_SHOT_SCROLL = $scroll } else { Remove-Item Env:DSH_SYSTRAY_SHOT_SCROLL -ErrorAction SilentlyContinue }
  $env:DSH_SYSTRAY_SHOT_PAGE = $page
  Remove-Item $readyFile -Force -ErrorAction SilentlyContinue
  $p = Start-Process -FilePath $exe -PassThru

  # 1) wait until Go emits the "settings view shown" marker (after splash:done)
  $deadline = (Get-Date).AddSeconds(30)
  while ((Get-Date) -lt $deadline) {
    if (Test-Path $readyFile) { break }
    Start-Sleep -Milliseconds 500
  }
  if (-not (Test-Path $readyFile)) { Write-Host "skip $name (ready marker timeout)"; return }
  Start-Sleep -Milliseconds 900   # let the page settle after view switch

  # 2) find the real main window (wide enough)
  $h = [IntPtr]::Zero; $cw = 0; $ch = 0
  $deadline = (Get-Date).AddSeconds(10)
  while ((Get-Date) -lt $deadline) {
    Start-Sleep -Milliseconds 500
    $p.Refresh()
    if ($p.MainWindowHandle -eq [IntPtr]::Zero) { continue }
    $cr = [Cap+RECT]::new()
    [void][Cap]::GetClientRect($p.MainWindowHandle, [ref]$cr)
    $w = $cr.R - $cr.L; $hh = $cr.B - $cr.T
    if ($w -ge 600 -and $hh -ge 200) { $h = $p.MainWindowHandle; $cw = $w; $ch = $hh; break }
  }
  if ($h -eq [IntPtr]::Zero) { Write-Host "skip $name (main window not found)"; return }

  # 3) force topmost + foreground (Alt trick), retry until foreground owned
  [void][Cap]::ShowWindow($h, 9)
  [void][Cap]::SetWindowPos($h, [IntPtr](-1), 0, 0, 0, 0, 0x0001 -bor 0x0002 -bor 0x0040)
  for ($i = 0; $i -lt 3; $i++) {
    [Cap]::keybd_event(0x12, 0, 0, [UIntPtr]::Zero)
    [Cap]::keybd_event(0x12, 0, 2, [UIntPtr]::Zero)
    [void][Cap]::SetForegroundWindow($h)
    Start-Sleep -Milliseconds 500
    if ([Cap]::GetForegroundWindow() -eq $h) { break }
  }
  Start-Sleep -Milliseconds 1000

  # 4) PrintWindow first (z-order independent); fallback to screen copy
  $bmp = New-Object System.Drawing.Bitmap($cw, $ch)
  $g = [System.Drawing.Graphics]::FromImage($bmp)
  $hdc = $g.GetHdc()
  $ok = [Cap]::PrintWindow($h, $hdc, 3)
  $g.ReleaseHdc($hdc)
  $g.Dispose()
  if ($ok -and (Test-Varied $bmp)) {
    Save-Shot $bmp $name
    Write-Host "  method=printwindow"
    $bmp.Dispose()
    return
  }
  $bmp.Dispose()
  Write-Host "  printwindow blank/failed, fallback to screen copy"
  $pt = [Cap+POINT]::new()
  [void][Cap]::ClientToScreen($h, [ref]$pt)
  $full = New-Object System.Drawing.Bitmap($cw, $ch)
  $g2 = [System.Drawing.Graphics]::FromImage($full)
  $g2.CopyFromScreen($pt.X, $pt.Y, 0, 0, $full.Size)
  $g2.Dispose()
  Save-Shot $full $name
  Write-Host "  method=screencopy"
  $full.Dispose()
}

SnapProcess 'general' 'general'
SnapProcess 'about-top'    'about'
SnapProcess 'about-bottom' 'about' 'bottom'
SnapProcess 'logs'    'logs'
SnapProcess 'export'  'export'
SnapProcess 'import'  'import'

Stop-AllInstances
Remove-Item $readyFile -Force -ErrorAction SilentlyContinue
Write-Host 'done (PNG only; compositing/webp run in separate steps)'
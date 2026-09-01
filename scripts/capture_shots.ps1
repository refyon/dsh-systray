# dsh-systray 真实 UI 截图脚本（Windows）
# 用法: powershell -ExecutionPolicy Bypass -File capture_shots.ps1
# 原理: 通过 DSH_SYSTRAY_SHOT_PAGE 让前端启动后直接显示指定页，逐页重启进程截图。
# 前置: 已构建 build/bin/dsh-systray.exe

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public class Cap {
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L, T, R, B; }
}
"@

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
  [void][Cap]::SetForegroundWindow($p.MainWindowHandle)
  Start-Sleep -Milliseconds 900
  $r = [Cap+RECT]::new()
  [void][Cap]::GetWindowRect($p.MainWindowHandle, [ref]$r)
  $w = $r.R - $r.L; $ht = $r.B - $r.T
  if ($w -le 0) { Write-Host "skip $name (bad rect)"; return }
  $bmp = New-Object System.Drawing.Bitmap($w, $ht)
  $g = [System.Drawing.Graphics]::FromImage($bmp)
  $g.CopyFromScreen($r.L, $r.T, 0, 0, $bmp.Size)
  $bmp.Save((Join-Path $outDir "$name.png"), [System.Drawing.Imaging.ImageFormat]::Png)
  $g.Dispose(); $bmp.Dispose()
  Write-Host ("saved {0}.png ({1}x{2})" -f $name, $w, $ht)
}

SnapProcess 'general' 'general' 7
SnapProcess 'about'   'about'   5
SnapProcess 'logs'    'logs'    5
SnapProcess 'export'  'export'  5
SnapProcess 'import'  'import'  5

Get-Process dsh-systray -ErrorAction SilentlyContinue | Stop-Process -Force
Write-Host 'done'

param(
    [ValidateSet('zh','en')]
    [string]$Lang = 'zh'
)

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
$exe = Join-Path $root 'src\build\bin\dsh-systray.exe'
$outDir = if ($Lang -eq 'en') { Join-Path $root 'docs\shots-en' } else { Join-Path $root 'docs\shots' }
$readyFile = Join-Path $env:TEMP 'dsh-shot-ready.flag'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

# 已在运行的托盘实例：agent 发版场景下托盘可能是 3080 会话宿主的父进程（node 的 stdout 管道挂在
# 它身上，强杀有连坐中断用户会话的风险）。设 DSH_SHOT_KEEP_EXISTING=1 时不碰「别人」的实例，
# 只清理本脚本自己启动的截图实例——截图实例启动时若发现服务已在运行会直接沿用（不会 spawn/杀服务），
# 因此二者可并存。默认行为（未设该变量）不变：先杀光全部实例再逐页截图。
$keepExisting = $env:DSH_SHOT_KEEP_EXISTING -eq '1'
$script:shotPids = @()
if ($keepExisting) {
    # 单实例互斥体：截图实例是同一程序的第二个进程，已在运行的托盘会把它挡在门外（弹窗后退出、
    # 拿不到窗口）。保留模式只保证「不杀别人的实例」，若确实有实例在跑，本次截图会全部失败——
    # 这里显式提示，避免误以为是脚本坏了。
    $others = @(Get-Process dsh-systray -ErrorAction SilentlyContinue)
    if ($others.Count -gt 0) {
        Write-Host ("warn: 检测到已在运行的 dsh-systray 实例（pid {0}）——单实例互斥体会让截图实例拿不到窗口，" -f (($others | ForEach-Object Id) -join ','))
        Write-Host "      如确需重拍，请先退出托盘（或去掉 DSH_SHOT_KEEP_EXISTING 让本脚本停掉全部实例）。"
    }
}

# 截图/预览模式：抑制「打开 Web UI」弹窗；阻止启动完成后隐藏设置窗口
$env:DSH_SYSTRAY_SHOW_WINDOW = '1'
$restoreLang = $false
if ($Lang -eq 'en') {
    $cfgPath = Join-Path ([Environment]::GetFolderPath('ApplicationData')) 'dsh-systray\config.json'
    $cfgBak = Join-Path $env:TEMP 'dsh-cfg-backup.json'
    if (Test-Path $cfgPath) { Copy-Item $cfgPath $cfgBak -Force } else { Remove-Item $cfgBak -Force -ErrorAction SilentlyContinue }
    $cfg = @{}
    if (Test-Path $cfgPath) { try { $cfg = Get-Content $cfgPath -Raw | ConvertFrom-Json -AsHashtable } catch { $cfg = @{} } }
    $cfg['language'] = 'en'
    [IO.File]::WriteAllText($cfgPath, ($cfg | ConvertTo-Json -Depth 5), [Text.UTF8Encoding]::new($false))
    $restoreLang = $true
}
$env:DSH_SYSTRAY_SHOT_READY_FILE = $readyFile

function Test-Varied([System.Drawing.Bitmap]$bmp) {
  # 采样密度 15×15：稀疏采样会把「白底卡片页」（关于页）误判成空白，进而走 screen copy 兜底
  # ——无交互桌面的环境（agent 发版）该兜底必然失败。密集采样下空白页仍只有 1~2 色，正常页
  # 因文字/控件抗锯齿必然多色，判断更稳。
  $seen = @{}
  $w = $bmp.Width; $h = $bmp.Height
  for ($i = 1; $i -lt 16; $i++) {
    for ($j = 1; $j -lt 16; $j++) {
      $c = $bmp.GetPixel([int]($w*$i/16),[int]($h*$j/16))
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
  if ($keepExisting) {
    # 只收掉本脚本启动过的截图实例（/T 连子进程：万一某个实例自行拉起过服务一并清理）
    foreach ($procId in $script:shotPids) {
      & taskkill /PID $procId /T /F 2>$null | Out-Null
    }
    $script:shotPids = @()
    Start-Sleep -Milliseconds 500
    return
  }
  Get-Process dsh-systray -ErrorAction SilentlyContinue | Stop-Process -Force
  $deadline = (Get-Date).AddSeconds(6)
  while ((Get-Date) -lt $deadline) {
    if (-not (Get-Process dsh-systray -ErrorAction SilentlyContinue)) { break }
    Start-Sleep -Milliseconds 300
  }
  Start-Sleep -Milliseconds 700
}

function SnapProcessOnce([string]$name, [string]$page, [string]$scroll = '' ) {
  Stop-AllInstances
  Remove-Item Env:DSH_SYSTRAY_SHOT_SPLASH -ErrorAction SilentlyContinue
  if ($scroll) { $env:DSH_SYSTRAY_SHOT_SCROLL = $scroll } else { Remove-Item Env:DSH_SYSTRAY_SHOT_SCROLL -ErrorAction SilentlyContinue }
  $env:DSH_SYSTRAY_SHOT_PAGE = $page
  Remove-Item $readyFile -Force -ErrorAction SilentlyContinue
  $p = Start-Process -FilePath $exe -PassThru
  $script:shotPids += $p.Id

  # 1) wait until Go emits the "settings view shown" marker (after splash:done)
  $deadline = (Get-Date).AddSeconds(30)
  while ((Get-Date) -lt $deadline) {
    if (Test-Path $readyFile) { break }
    Start-Sleep -Milliseconds 500
  }
  if (-not (Test-Path $readyFile)) { Write-Host "  fail: ready marker timeout"; return $false }
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
  if ($h -eq [IntPtr]::Zero) { Write-Host "  fail: main window not found"; return $false }

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
  # 无交互桌面的环境（agent 发版/无人值守）screen copy 必然报 "The handle is invalid"——
  # 兜底失败只判本页失败（SnapProcess 会重试/跳过），不能让整个脚本中断。
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
    return $true
  }
  $bmp.Dispose()
  Write-Host "  printwindow blank/failed, fallback to screen copy"
  try {
    $pt = [Cap+POINT]::new()
    [void][Cap]::ClientToScreen($h, [ref]$pt)
    $full = New-Object System.Drawing.Bitmap($cw, $ch)
    $g2 = [System.Drawing.Graphics]::FromImage($full)
    $g2.CopyFromScreen($pt.X, $pt.Y, 0, 0, $full.Size)
    $g2.Dispose()
    Save-Shot $full $name
    Write-Host "  method=screencopy"
    $full.Dispose()
    return $true
  } catch {
    Write-Host "  fail: screen copy unavailable ($($_.Exception.Message))"
    return $false
  }
}

function SnapProcess([string]$name, [string]$page, [string]$scroll = '' ) {
  for ($attempt = 1; $attempt -le 2; $attempt++) {
    if (SnapProcessOnce $name $page $scroll) { return }
    Write-Host ("  retry {0} ({1}/2)" -f $name, $attempt)
    Start-Sleep -Milliseconds 800
  }
  Write-Host "skip $name (after retries)"
}
SnapProcess 'general' 'general'
SnapProcess 'about-top'    'about'
SnapProcess 'about-bottom' 'about' 'bottom'
SnapProcess 'logs'    'logs'
SnapProcess 'export'  'export'
SnapProcess 'import'  'import'
SnapProcess 'sync'    'sync'

Stop-AllInstances
Remove-Item $readyFile -Force -ErrorAction SilentlyContinue
if ($restoreLang) {
    if (Test-Path $cfgBak) { Copy-Item $cfgBak $cfgPath -Force; Remove-Item $cfgBak -Force }
    elseif (Test-Path $cfgPath) { Remove-Item $cfgPath -Force }
}
Write-Host "done $Lang (PNG only; compositing/webp run in separate steps)"

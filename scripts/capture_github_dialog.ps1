# capture_github_dialog.ps1 —— 截取「GitHub 授权确认」弹窗（网站轮播 / README 用图）
#
# 为什么要脚本化：这个弹窗是 Windows 自绘窗口（类名 DSH_Systray_Dialog），
# 只有在「私有仓库插件检查更新」时才会出现。为了让发版物料能稳定复现这张图，
# 这里用 dialogshot.exe（tmp_shot_dialog_test.go 编出的测试二进制）直接弹出同一个弹窗。
#
# 两种模式：
#   默认（窗口 + 弹窗合成）——先以截图模式启动设置窗口（DSH_SYSTRAY_SHOT_PAGE 指定页面），
#     再把弹窗弹到窗口之上，整屏抓取设置窗口客户区，得到与其它轮播图同尺寸（808×505）的
#     「设置窗口 + 授权弹窗」图。
#   -DialogOnly —— 只截弹窗本身（旧行为：PrintWindow 单窗口抓取，436×191）。
#
# 注意：设置程序是单实例（互斥体），脚本会先结束正在运行的 dsh-systray 托盘实例，
#       跑完后需用户自行重新启动托盘程序。
#
# 用法：
#   powershell -File scripts\capture_github_dialog.ps1                                  # 中文 → docs\shots\github-auth.png
#   powershell -File scripts\capture_github_dialog.ps1 -Lang en                         # 英文 → docs\shots-en\github-auth.png
#   powershell -File scripts\capture_github_dialog.ps1 -Page about -Scroll bottom       # 背景页 = 关于页（已安装插件）
#   powershell -File scripts\capture_github_dialog.ps1 -DialogOnly                      # 仅弹窗（旧图形态）
param(
    [string]$OutFile = '',
    [ValidateSet('zh','en')]
    [string]$Lang = 'zh',
    [string]$Page = 'about',
    [string]$Scroll = 'bottom',
    [switch]$DialogOnly
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
using System.Text;
public class DshCap {
  public delegate bool EnumProc(IntPtr h, IntPtr p);
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr p);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetClassNameW(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] public static extern bool PrintWindow(IntPtr h, IntPtr hdc, uint flags);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool GetClientRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool ClientToScreen(IntPtr h, ref POINT p);
  [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);
  [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int cmd);
  [DllImport("user32.dll")] public static extern void keybd_event(byte vk, byte scan, uint flags, UIntPtr extra);
  [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L, T, R, B; }
  [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }

  // 按「进程 + 窗口类名 + 可见」找窗口：FindWindowW 传 null 标题在本机匹配不到该自绘窗口，
  // 因此改用 EnumWindows 精确筛选（实测能稳定拿到 DSH_Systray_Dialog）。
  public static IntPtr FindByOwner(uint pid, string cls) {
    IntPtr found = IntPtr.Zero;
    EnumWindows(delegate(IntPtr h, IntPtr p) {
      uint wp; GetWindowThreadProcessId(h, out wp);
      if (wp != pid) return true;
      var sb = new StringBuilder(256);
      GetClassNameW(h, sb, 256);
      if (sb.ToString() == cls && IsWindowVisible(h)) { found = h; return false; }
      return true;
    }, IntPtr.Zero);
    return found;
  }

  // z 序名次（EnumWindows 自上而下，1 = 最上层）：确认弹窗确实压在设置窗口之上
  public static int ZIndex(IntPtr target) {
    int idx = 0, i = 0;
    EnumWindows(delegate(IntPtr h, IntPtr p) {
      i++;
      if (h == target) { idx = i; return false; }
      return true;
    }, IntPtr.Zero);
    return idx;
  }
}
"@

# 屏幕坐标一律按物理像素取（PowerShell 默认 DPI 不感知时 GetWindowRect/CopyFromScreen 会被虚拟化缩放）
[void][DshCap]::SetProcessDPIAware()

$crop = 8   # 与其它轮播图一致：裁掉 8px 窗口圆角/边框
$root = Split-Path -Parent $PSScriptRoot
$appExe = Join-Path $root 'build\bin\dsh-systray.exe'
$dialogExe = Join-Path $env:TEMP 'dsh-dialog-shot\dialogshot.exe'
$readyFile = Join-Path $env:TEMP 'dsh-shot-ready.flag'
$HWND_TOPMOST = [IntPtr](-1)
$SWP_NOSIZE = 0x0001; $SWP_SHOWWINDOW = 0x0040

if (-not $OutFile) {
    $sub = 'shots'
    if ($Lang -eq 'en') { $sub = 'shots-en' }
    $OutFile = Join-Path $root "docs\$sub\github-auth.png"
}
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $OutFile) | Out-Null

if (-not (Test-Path $appExe)) { throw "未找到 $appExe（先构建：scripts\build.ps1）" }
if (-not (Test-Path $dialogExe)) {
    Write-Host "building dialogshot.exe ..."
    Push-Location $root
    try {
        & go test -c -o $dialogExe .
        if ($LASTEXITCODE -ne 0) { throw 'go test -c 失败（dialogshot.exe 未生成）' }
    } finally { Pop-Location }
}

if ($Lang -eq 'en') { $env:DSH_SYSTRAY_LANG = 'en' } else { Remove-Item Env:DSH_SYSTRAY_LANG -ErrorAction SilentlyContinue }
$env:DSH_TMP_SHOT_DIALOG = '1'

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

function Save-Cropped([System.Drawing.Bitmap]$bmp, [string]$path) {
    $ow = $bmp.Width - 2*$crop; $oh = $bmp.Height - 2*$crop
    if ($ow -le 0 -or $oh -le 0) { throw "截图尺寸过小：$($bmp.Width)x$($bmp.Height)" }
    $out = New-Object System.Drawing.Bitmap($ow, $oh)
    $g2 = [System.Drawing.Graphics]::FromImage($out)
    $g2.DrawImage($bmp, (New-Object System.Drawing.Rectangle(0,0,$ow,$oh)), (New-Object System.Drawing.Rectangle($crop,$crop,$ow,$oh)), [System.Drawing.GraphicsUnit]::Pixel)
    $g2.Dispose()
    $out.Save($path, [System.Drawing.Imaging.ImageFormat]::Png)
    Write-Host ("saved {0} ({1}x{2})" -f $path, $ow, $oh)
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

# 弹出授权弹窗；返回 @{ Proc; Hwnd }
function Start-AuthDialog {
    $dp = Start-Process -FilePath $dialogExe -ArgumentList '-test.run=TestShotGitHubAuthDialog' -PassThru -WindowStyle Hidden
    $hwnd = [IntPtr]::Zero
    for ($i = 0; $i -lt 100; $i++) {
        $hwnd = [DshCap]::FindByOwner([uint32]$dp.Id, 'DSH_Systray_Dialog')
        if ($hwnd -ne [IntPtr]::Zero) { break }
        Start-Sleep -Milliseconds 150
    }
    if ($hwnd -eq [IntPtr]::Zero) {
        try { $dp.Kill() } catch {}
        throw '未找到授权弹窗（DSH_Systray_Dialog）'
    }
    return @{ Proc = $dp; Hwnd = $hwnd }
}

function Close-AuthDialog($d) {
    try { if (-not $d.Proc.HasExited) { $d.Proc.Kill() } } catch {}
    Start-Sleep -Milliseconds 400
}

function Focus-Window([IntPtr]$h) {
    # Alt 轻敲解除前台锁定后再 SetForegroundWindow（截图脚本惯用手法）
    for ($i = 0; $i -lt 3; $i++) {
        [DshCap]::keybd_event(0x12, 0, 0, [UIntPtr]::Zero)
        [DshCap]::keybd_event(0x12, 0, 2, [UIntPtr]::Zero)
        [void][DshCap]::SetForegroundWindow($h)
        Start-Sleep -Milliseconds 400
        if ([DshCap]::GetForegroundWindow() -eq $h) { break }
    }
}

if ($DialogOnly) {
    # ---- 旧模式：只截弹窗本身（PrintWindow） ----
    $d = Start-AuthDialog
    [void][DshCap]::SetWindowPos($d.Hwnd, $HWND_TOPMOST, 0, 0, 0, 0, $SWP_NOSIZE -bor 0x0002 -bor $SWP_SHOWWINDOW)
    [void][DshCap]::SetForegroundWindow($d.Hwnd)
    Start-Sleep -Milliseconds 900
    $r = [DshCap+RECT]::new()
    [void][DshCap]::GetWindowRect($d.Hwnd, [ref]$r)
    $w = $r.R - $r.L; $h = $r.B - $r.T
    $bmp = New-Object System.Drawing.Bitmap($w, $h)
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $hdc = $g.GetHdc()
    [void][DshCap]::PrintWindow($d.Hwnd, $hdc, 2)  # PW_RENDERFULLCONTENT
    $g.ReleaseHdc($hdc)
    $g.Dispose()
    $bmp.Save($OutFile, [System.Drawing.Imaging.ImageFormat]::Png)
    $bmp.Dispose()
    Write-Host ("saved {0} ({1}x{2})" -f $OutFile, $w, $h)
    Close-AuthDialog $d
    return
}

# ---- 默认模式：完整设置窗口 + 授权弹窗压在窗口之上 ----
Stop-AllInstances
Remove-Item Env:DSH_SYSTRAY_SHOT_SPLASH -ErrorAction SilentlyContinue
$env:DSH_SYSTRAY_SHOW_WINDOW = '1'
$env:DSH_SYSTRAY_SHOT_PAGE = $Page
if ($Scroll) { $env:DSH_SYSTRAY_SHOT_SCROLL = $Scroll } else { Remove-Item Env:DSH_SYSTRAY_SHOT_SCROLL -ErrorAction SilentlyContinue }
$env:DSH_SYSTRAY_SHOT_READY_FILE = $readyFile
Remove-Item $readyFile -Force -ErrorAction SilentlyContinue

$app = Start-Process -FilePath $appExe -PassThru
$deadline = (Get-Date).AddSeconds(30)
while ((Get-Date) -lt $deadline) {
    if (Test-Path $readyFile) { break }
    Start-Sleep -Milliseconds 500
}
if (-not (Test-Path $readyFile)) { Stop-AllInstances; throw '设置窗口就绪标记超时（DSH_SYSTRAY_SHOT_READY_FILE）' }
Start-Sleep -Milliseconds 1000

# 找真正的设置窗口（客户区够宽，排除启动器/隐藏窗）
$h = [IntPtr]::Zero; $cw = 0; $chh = 0
$deadline = (Get-Date).AddSeconds(10)
while ((Get-Date) -lt $deadline) {
    $app.Refresh()
    if ($app.MainWindowHandle -ne [IntPtr]::Zero) {
        $cr = [DshCap+RECT]::new()
        [void][DshCap]::GetClientRect($app.MainWindowHandle, [ref]$cr)
        $w = $cr.R - $cr.L; $hh = $cr.B - $cr.T
        if ($w -ge 600 -and $hh -ge 200) { $h = $app.MainWindowHandle; $cw = $w; $chh = $hh; break }
    }
    Start-Sleep -Milliseconds 500
}
if ($h -eq [IntPtr]::Zero) { Stop-AllInstances; throw '未找到设置窗口' }

# 摆到固定位置并置顶（弹窗要压在它上面，必须保证窗口完整可见、无遮挡）
[void][DshCap]::ShowWindow($h, 9)
[void][DshCap]::SetWindowPos($h, $HWND_TOPMOST, 40, 40, 0, 0, $SWP_NOSIZE -bor $SWP_SHOWWINDOW)
Focus-Window $h
Start-Sleep -Milliseconds 900

$pt = [DshCap+POINT]::new()
[void][DshCap]::ClientToScreen($h, [ref]$pt)

$d = Start-AuthDialog
$dr = [DshCap+RECT]::new()
[void][DshCap]::GetWindowRect($d.Hwnd, [ref]$dr)
$dw = $dr.R - $dr.L; $dh = $dr.B - $dr.T
$dx = $pt.X + [int](($cw - $dw) / 2)
$dy = $pt.Y + [int](($chh - $dh) / 2)

# 摆弹窗并核验：弹窗必须仍可见、矩形落在截图区内、且 z 序高于设置窗口。
# 注意不要在弹窗上敲 Alt（Focus-Window）——那会夺走弹窗的前台/置顶状态，
# 实测会让弹窗在屏幕抓取前消失，抓到的图只剩设置窗口。
function Set-DialogOverWindow {
    param([IntPtr]$Dlg, [IntPtr]$Owner, [int]$X, [int]$Y)
    for ($i = 1; $i -le 6; $i++) {
        [void][DshCap]::SetWindowPos($Dlg, $HWND_TOPMOST, $X, $Y, 0, 0, $SWP_NOSIZE -bor $SWP_SHOWWINDOW)
        [void][DshCap]::SetForegroundWindow($Dlg)
        Start-Sleep -Milliseconds 500
        $r = [DshCap+RECT]::new()
        [void][DshCap]::GetWindowRect($Dlg, [ref]$r)
        $inside = ($r.L -ge $pt.X) -and ($r.T -ge $pt.Y) -and ($r.R -le ($pt.X + $cw)) -and ($r.B -le ($pt.Y + $chh))
        $above = ([DshCap]::ZIndex($Dlg) -gt 0) -and ([DshCap]::ZIndex($Dlg) -lt [DshCap]::ZIndex($Owner))
        if ($inside -and $above -and [DshCap]::IsWindowVisible($Dlg) -and (-not $d.Proc.HasExited)) { return $true }
        Write-Host ("  dialog check {0}/6: inside={1} aboveOwner={2} exited={3}" -f $i, $inside, $above, $d.Proc.HasExited)
    }
    return $false
}

if (-not (Set-DialogOverWindow -Dlg $d.Hwnd -Owner $h -X $dx -Y $dy)) {
    $dump = Join-Path $env:TEMP 'dsh-github-auth-debug.png'
    $full = New-Object System.Drawing.Bitmap(1920, 1080)
    $g0 = [System.Drawing.Graphics]::FromImage($full)
    $g0.CopyFromScreen(0, 0, 0, 0, $full.Size)
    $g0.Dispose()
    $full.Save($dump, [System.Drawing.Imaging.ImageFormat]::Png)
    $full.Dispose()
    Close-AuthDialog $d
    Stop-AllInstances
    throw "授权弹窗未压在设置窗口之上（全屏现场：$dump）"
}

$bmp = New-Object System.Drawing.Bitmap($cw, $chh)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($pt.X, $pt.Y, 0, 0, $bmp.Size)
$g.Dispose()
$varied = Test-Varied $bmp
Save-Cropped $bmp $OutFile
$bmp.Dispose()
if (-not $varied) { Write-Host 'warn: 截图内容疑似空白，请检查窗口是否被其它置顶窗口遮挡' }

Close-AuthDialog $d
Stop-AllInstances
Remove-Item $readyFile -Force -ErrorAction SilentlyContinue
Remove-Item Env:DSH_SYSTRAY_SHOT_PAGE, Env:DSH_SYSTRAY_SHOT_SCROLL, Env:DSH_SYSTRAY_SHOT_READY_FILE, Env:DSH_SYSTRAY_SHOW_WINDOW, Env:DSH_TMP_SHOT_DIALOG -ErrorAction SilentlyContinue
Write-Host "done $Lang (设置窗口 $($cw)x$($chh) + 弹窗 $($dw)x$($dh) → $OutFile)"

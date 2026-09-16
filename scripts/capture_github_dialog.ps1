# capture_github_dialog.ps1 —— 截取「GitHub 授权确认」弹窗（网站轮播 / README 用图）
#
# 为什么要脚本化：这个弹窗是 Windows 自绘窗口（类名 DSH_Systray_Dialog），
# 只有在「私有仓库插件检查更新」时才会出现。为了让发版物料能稳定复现这张图，
# 这里用 dialogshot.exe（tmp_shot_dialog_test.go 编出的测试二进制）直接弹出同一个弹窗。
#
# 两种模式：
#   默认（窗口 + 弹窗合成）——先以截图模式启动设置窗口（DSH_SYSTRAY_SHOT_PAGE 指定页面），
#     再把弹窗弹到窗口客户区中央，抓成与其它轮播图同尺寸（808×505）的「设置窗口 + 授权弹窗」图。
#     抓图优先整屏抓取，桌面不可用（锁屏等）时自动退回 PrintWindow 双窗合成，见下方注释。
#   -DialogOnly —— 只截弹窗本身（旧行为：PrintWindow 单窗口抓取，436×191）。
#
# 弹窗文案是脱敏示例（tmp_shot_dialog_test.go 用 prompt-assistant / example/prompt-assistant，
# 与 plugin_update.go shotPlugins 的虚构示例集一致），截图会进公开站点，不要写真实私有仓库名。
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
# 每次都重编 dialogshot.exe：弹窗文案改动（如脱敏示例名）必须落到截图上，
# 旧二进制残留会让图里继续出现改动前的文案（有构建缓存，重编只需数秒）。
Write-Host 'building dialogshot.exe ...'
Push-Location $root
try {
    & go test -c -o $dialogExe .
    if ($LASTEXITCODE -ne 0) { throw 'go test -c 失败（dialogshot.exe 未生成）' }
} finally { Pop-Location }

if ($Lang -eq 'en') { $env:DSH_SYSTRAY_LANG = 'en' } else { Remove-Item Env:DSH_SYSTRAY_LANG -ErrorAction SilentlyContinue }
$env:DSH_TMP_SHOT_DIALOG = '1'

# 采样判断"窗口是否真的画好了"：9×9 网格取色，真实界面（浅灰底 + 白卡 + 文本/边框）通常
# 有 8 种以上颜色，空白/未绘制窗口只有 1-2 种。阈值取 >5：原来 5×5 网格 + >3 在"关于页底部"
# 这种大片浅色页面上会误判为空白（2026-09-15 实测 4 色，勉强过线，稍有偏差即失败）。
function Test-Varied([System.Drawing.Bitmap]$bmp) {
    $seen = @{}
    $w = $bmp.Width; $h = $bmp.Height
    foreach ($fx in 0.06,0.14,0.25,0.37,0.5,0.63,0.75,0.86,0.94) {
        foreach ($fy in 0.06,0.14,0.25,0.37,0.5,0.63,0.75,0.86,0.94) {
            $c = $bmp.GetPixel([int]($w*$fx),[int]($h*$fy))
            $seen[[string]$c.ToArgb()] = 1
        }
    }
    return ($seen.Count -gt 5)
}

# 抓取并等待界面画好：切换视图/首次绘制瞬间会抓到近空白位图，这里重试若干次。
function Get-VariedWindowBitmap {
    param([IntPtr]$Hwnd, [int]$W, [int]$H, [uint32]$Flags, [int]$Tries = 6)
    $bmp = $null
    for ($i = 1; $i -le $Tries; $i++) {
        if ($bmp) { $bmp.Dispose() }
        $bmp = Get-WindowBitmap -Hwnd $Hwnd -W $W -H $H -Flags $Flags
        if (Test-Varied $bmp) { return $bmp }
        if ($i -lt $Tries) {
            Write-Host ("  capture retry {0}/{1}（窗口尚未绘制完成）" -f $i, $Tries)
            Start-Sleep -Milliseconds 800
        }
    }
    return $bmp
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
# 弹窗客户区（真实弹窗带系统标题栏，截图里只取客户区内容 → 出图是无边框卡片，见下方抓图说明）
$dcr = [DshCap+RECT]::new()
[void][DshCap]::GetClientRect($d.Hwnd, [ref]$dcr)
$dww = $dcr.R - $dcr.L; $dhh = $dcr.B - $dcr.T
$dx = $pt.X + [int](($cw - $dw) / 2)
$dy = $pt.Y + [int](($chh - $dh) / 2)

# 摆弹窗并核验：矩形必须落在截图区内（合成按窗口实际位置贴图）、可见、进程未退出。
# 注意不要在弹窗上敲 Alt（Focus-Window）——那会夺走弹窗的前台/置顶状态。
function Set-DialogOverWindow {
    param([IntPtr]$Dlg, [IntPtr]$Owner, [int]$X, [int]$Y)
    for ($i = 1; $i -le 6; $i++) {
        [void][DshCap]::SetWindowPos($Dlg, $HWND_TOPMOST, $X, $Y, 0, 0, $SWP_NOSIZE -bor $SWP_SHOWWINDOW)
        [void][DshCap]::SetForegroundWindow($Dlg)
        Start-Sleep -Milliseconds 500
        $r = [DshCap+RECT]::new()
        [void][DshCap]::GetWindowRect($Dlg, [ref]$r)
        $inside = ($r.L -ge $pt.X) -and ($r.T -ge $pt.Y) -and ($r.R -le ($pt.X + $cw)) -and ($r.B -le ($pt.Y + $chh))
        if ($inside -and [DshCap]::IsWindowVisible($Dlg) -and (-not $d.Proc.HasExited)) { return $true }
        Write-Host ("  dialog check {0}/6: inside={1} visible={2} exited={3}" -f $i, $inside, [DshCap]::IsWindowVisible($Dlg), $d.Proc.HasExited)
    }
    return $false
}

if (-not (Set-DialogOverWindow -Dlg $d.Hwnd -Owner $h -X $dx -Y $dy)) {
    Close-AuthDialog $d
    Stop-AllInstances
    throw '授权弹窗未摆到设置窗口客户区内'
}

# 抓图说明（方案 C：真实弹窗保留系统标题栏，**只有截图呈现无边框卡片**）：
#   设置窗口取客户区（PW_CLIENTONLY|PW_RENDERFULLCONTENT，同 capture_shots.ps1）；
#   弹窗只取**客户区**（GetClientRect + ClientToScreen + PW_CLIENTONLY）——系统标题栏与
#   边框都不进图；再由 compose_dialog_shot.py 贴成圆角卡片并画四周柔和阴影。
#   因此应用内弹窗保持 Windows 原生外观，网站轮播图/README 是无边框卡片风格。
#   为什么不直接整屏抓取：真实阴影由 DWM 合成、PrintWindow 渲染不到，且整屏抓取要求
#   桌面可访问（锁屏 / 无交互会话时 BitBlt 报 "The handle is invalid"）。合成路径与桌面
#   状态无关，中英两张图的圆角与阴影完全一致；整屏抓取仅在 PrintWindow 失败时兜底。
function Get-WindowBitmap {
    param([IntPtr]$Hwnd, [int]$W, [int]$H, [uint32]$Flags)
    $bmp = New-Object System.Drawing.Bitmap($W, $H)
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $hdc = $g.GetHdc()
    [void][DshCap]::PrintWindow($Hwnd, $hdc, $Flags)
    $g.ReleaseHdc($hdc)
    $g.Dispose()
    return $bmp
}

# 抓取 + 合成都放在 try 里：失败路径也要把弹窗进程与设置窗口收拾干净（否则残留进程会
# 让下一次运行的单实例互斥、窗口查找全部异常——2026-09-15 实测）。
try {
    $iw = $dww; $ih = $dhh            # 弹窗客户区尺寸（标题栏/边框不计入）
    $dpt = [DshCap+POINT]::new()
    [void][DshCap]::ClientToScreen($d.Hwnd, [ref]$dpt)
    $px = $dpt.X - $pt.X; $py = $dpt.Y - $pt.Y   # 弹窗客户区在设置窗口客户区图中的位置

    $appBmp = Get-VariedWindowBitmap -Hwnd $h -W $cw -H $chh -Flags 3
    if (Test-Varied $appBmp) {
        $dlgBmp = Get-VariedWindowBitmap -Hwnd $d.Hwnd -W $iw -H $ih -Flags 3 -Tries 4
        Write-Host ("  method=printwindow (弹窗只取客户区 {0}x{1}，标题栏不入图)" -f $iw, $ih)
    } else {
        $appBmp.Dispose()
        Write-Host '  printwindow blank, fallback to screen copy（从整屏图裁出弹窗客户区）'
        $appBmp = New-Object System.Drawing.Bitmap($cw, $chh)
        $g = [System.Drawing.Graphics]::FromImage($appBmp)
        $g.CopyFromScreen($pt.X, $pt.Y, 0, 0, $appBmp.Size)
        $g.Dispose()
        if (-not (Test-Varied $appBmp)) { $appBmp.Dispose(); throw '整屏抓取失败（桌面不可访问且 PrintWindow 为空）' }
        if ($px -lt 0 -or $py -lt 0 -or ($px + $iw) -gt $cw -or ($py + $ih) -gt $chh) {
            $appBmp.Dispose(); throw "弹窗客户区超出设置窗口截图范围（$px,$py ${iw}x${ih}）"
        }
        $dlgBmp = New-Object System.Drawing.Bitmap($iw, $ih)
        $gd = [System.Drawing.Graphics]::FromImage($dlgBmp)
        $gd.DrawImage($appBmp,
            (New-Object System.Drawing.Rectangle(0,0,$iw,$ih)),
            (New-Object System.Drawing.Rectangle($px,$py,$iw,$ih)),
            [System.Drawing.GraphicsUnit]::Pixel)
        $gd.Dispose()
    }

    $tmpDir = Join-Path $env:TEMP 'dsh-dialog-shot-frames'
    New-Item -ItemType Directory -Force -Path $tmpDir | Out-Null
    $appPng = Join-Path $tmpDir 'app.png'; $dlgPng = Join-Path $tmpDir 'dialog.png'
    $appBmp.Save($appPng, [System.Drawing.Imaging.ImageFormat]::Png)
    $dlgBmp.Save($dlgPng, [System.Drawing.Imaging.ImageFormat]::Png)
    $appBmp.Dispose(); $dlgBmp.Dispose()
    Push-Location $root
    try {
        python scripts\compose_dialog_shot.py $appPng $dlgPng $OutFile `
            --x $px --y $py --w $iw --h $ih --crop $crop --radius 10
        if ($LASTEXITCODE -ne 0) { throw "合成失败（compose_dialog_shot.py exit $LASTEXITCODE）" }
    } finally { Pop-Location }
} catch {
    Write-Host "  capture failed: $($_.Exception.Message)"
    Close-AuthDialog $d
    Stop-AllInstances
    throw
}

Close-AuthDialog $d
Stop-AllInstances
Remove-Item $readyFile -Force -ErrorAction SilentlyContinue
Remove-Item Env:DSH_SYSTRAY_SHOT_PAGE, Env:DSH_SYSTRAY_SHOT_SCROLL, Env:DSH_SYSTRAY_SHOT_READY_FILE, Env:DSH_SYSTRAY_SHOW_WINDOW, Env:DSH_TMP_SHOT_DIALOG -ErrorAction SilentlyContinue
Write-Host "done $Lang (设置窗口 $($cw)x$($chh) + 弹窗 $($dw)x$($dh) → $OutFile)"

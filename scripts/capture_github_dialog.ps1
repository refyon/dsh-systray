# capture_github_dialog.ps1 —— 截取「GitHub 授权确认」弹窗（网站/README 用图）
#
# 为什么要脚本化：这个弹窗是 Windows 自绘窗口（类名 DSH_Systray_Dialog），
# 只有在「私有仓库插件检查更新」时才会出现。为了让发版物料能稳定复现这张图，
# 这里用 dialogshot.exe（tmp_shot_dialog_test.go 编出的测试二进制）直接弹出同一个弹窗，
# 按类名定位窗口、PrintWindow 截图，再发 Esc 关闭。
#
# 用法：powershell -File scripts\capture_github_dialog.ps1
param(
    [string]$OutFile = '',
    [ValidateSet('zh','en')]
    [string]$Lang = 'zh'
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
  [DllImport("user32.dll")] public static extern bool PrintWindow(IntPtr h, IntPtr hdc, uint flags);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);
  [DllImport("user32.dll")] public static extern void keybd_event(byte vk, byte scan, uint flags, UIntPtr extra);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L, T, R, B; }

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
}
"@

$root = Split-Path -Parent $PSScriptRoot
$exe = Join-Path $env:TEMP 'dsh-dialog-shot\dialogshot.exe'
if (-not (Test-Path $exe)) { throw "未找到 $exe（先：go test -c -o <该路径> .）" }
if (-not $OutFile) {
    $sub = 'shots'
    if ($Lang -eq 'en') { $sub = 'shots-en' }
    $OutFile = Join-Path $root "docs\$sub\github-auth.png"
}
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $OutFile) | Out-Null

$env:DSH_TMP_SHOT_DIALOG = '1'
if ($Lang -eq 'en') {
    # 英文截图：用 DSH_SYSTRAY_LANG 覆盖界面语言，不改动用户 config.json
    $env:DSH_SYSTRAY_LANG = 'en'
} else {
    Remove-Item Env:DSH_SYSTRAY_LANG -ErrorAction SilentlyContinue
}
$p = Start-Process -FilePath $exe -ArgumentList '-test.run=TestShotGitHubAuthDialog' -PassThru -WindowStyle Hidden

$hwnd = [IntPtr]::Zero
for ($i = 0; $i -lt 100; $i++) {
    $hwnd = [DshCap]::FindByOwner([uint32]$p.Id, 'DSH_Systray_Dialog')
    if ($hwnd -ne [IntPtr]::Zero) { break }
    Start-Sleep -Milliseconds 150
}
if ($hwnd -eq [IntPtr]::Zero) {
    try { $p.Kill() } catch {}
    throw '未找到授权弹窗（DSH_Systray_Dialog）'
}

[void][DshCap]::SetWindowPos($hwnd, [IntPtr](-1), 0, 0, 0, 0, 0x0001 -bor 0x0002 -bor 0x0040)
[void][DshCap]::SetForegroundWindow($hwnd)
Start-Sleep -Milliseconds 900

$r = [DshCap+RECT]::new()
[void][DshCap]::GetWindowRect($hwnd, [ref]$r)
$w = $r.R - $r.L; $h = $r.B - $r.T
$bmp = New-Object System.Drawing.Bitmap($w, $h)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$hdc = $g.GetHdc()
[void][DshCap]::PrintWindow($hwnd, $hdc, 2)  # PW_RENDERFULLCONTENT
$g.ReleaseHdc($hdc)
$g.Dispose()
$bmp.Save($OutFile, [System.Drawing.Imaging.ImageFormat]::Png)
$bmp.Dispose()
Write-Host ("saved {0} ({1}x{2})" -f $OutFile, $w, $h)

# Esc 关闭弹窗（弹窗按 -1 结果返回），随后结束测试进程
[DshCap]::keybd_event(0x1B, 0, 0, [UIntPtr]::Zero)
[DshCap]::keybd_event(0x1B, 0, 2, [UIntPtr]::Zero)
Start-Sleep -Milliseconds 600
try { if (-not $p.HasExited) { $p.Kill() } } catch {}

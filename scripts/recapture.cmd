@echo off
REM 在真实桌面会话（可见屏幕）上双击本脚本，重新生成 dsh-systray 全部截图与 README 主图。
REM 步骤：capture_shots.ps1（逐页截屏→webp）→ make_hero.py（合成主图）→ convert_webp.py（转 webp 并清理 png）
setlocal
cd /d "%~dp0.."
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\capture_shots.ps1
if errorlevel 1 echo [capture] 失败，请确认当前为已登录的可见桌面会话
python scripts\make_hero.py
python scripts\convert_webp.py
echo.
echo 完成：docs\shots\*.webp 与 docs\screenshot-hero.webp 已更新
pause

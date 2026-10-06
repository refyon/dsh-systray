@echo off
REM 重新生成 dsh-systray 发布物料（在任何有 Edge/Chrome 的机器上都能跑，不需要托盘/桌面会话）。
REM
REM 为什么不再跑 exe 抓窗口：老流程把出图尺寸绑在「窗口客户区像素」上，客户区大小受显示器缩放
REM 影响（笔记本 125%/150% 缩放下抓到的图右侧内容被裁掉）。现在改为无头 Chromium 按固定 CSS
REM 视口（840x560 = 设置窗口逻辑尺寸）渲染真实前端，尺寸恒定、与显示器无关，且不会打扰
REM 正在运行的托盘实例。
REM
REM 说明：网站轮播已是 iframe 实时预览（docs/mock），位图轮播截图（原 docs/shots[-en]）
REM 2026-10-06 起不再产出；这里只维护 README 主图所需的两块素材。
REM
REM 步骤：build_mock.mjs（把真实前端同步成站点实时界面预览 docs/mock/）
REM       -> render_shots.mjs（渲染主图素材：常规页背景 + 真实启动卡 + 画面文字）
REM       -> make_hero.py 中/英主图合成 -> convert_webp.py（主图转 webp）
REM       -> verify_shots.py（主图/素材/裁切/脱敏/预览同步 验收）
setlocal
cd /d "%~dp0.."

where node >nul 2>nul || (echo [错误] 需要 Node.js 18+（内置 fetch/WebSocket）& pause & exit /b 1)
where python >nul 2>nul || (echo [错误] 需要 Python 3 + Pillow& pause & exit /b 1)

node scripts\build_mock.mjs || (echo [失败] 站点预览生成出错 & pause & exit /b 1)
node scripts\render_shots.mjs --lang zh,en || (echo [失败] 渲染阶段出错 & pause & exit /b 1)
python scripts\make_hero.py --lang zh || (echo [失败] 中文主图合成出错 & pause & exit /b 1)
python scripts\make_hero.py --lang en || (echo [失败] 英文主图合成出错 & pause & exit /b 1)
python scripts\convert_webp.py || (echo [失败] 主图 webp 转换出错 & pause & exit /b 1)
python scripts\verify_shots.py --from-render docs\.shots-parts || (echo [失败] 验收未通过 & pause & exit /b 1)

echo.
echo 完成：docs\mock（站点实时预览）、docs\screenshot-hero[-en].webp 已更新
pause

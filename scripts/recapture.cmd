@echo off
REM 重新生成 dsh-systray 全部截图物料（在任何有 Edge/Chrome 的机器上都能跑，不需要托盘/桌面会话）。
REM
REM 为什么不再跑 exe 抓窗口：老流程把出图尺寸绑在「窗口客户区像素」上，客户区大小受显示器缩放
REM 影响（笔记本 125%/150% 缩放下抓到的图右侧内容被裁掉，README 与网站轮播因此出现异常）。
REM 现在改为无头 Chromium 按固定 CSS 视口（840x560 = 设置窗口逻辑尺寸）渲染真实前端，
REM 尺寸恒定、与显示器无关，且不会打扰正在运行的托盘实例。
REM
REM 步骤：render_shots.mjs（渲染 PNG + 主图前景件 + 画面文字）
REM       -> convert_webp.py --only shots（轮播图转 webp）
REM       -> make_hero.py（中英主图合成）-> convert_webp.py --only hero
REM       -> verify_shots.py（尺寸/裁切/一致性/脱敏验收）
setlocal
cd /d "%~dp0.."

where node >nul 2>nul || (echo [错误] 需要 Node.js 18+（内置 fetch/WebSocket）& pause & exit /b 1)
where python >nul 2>nul || (echo [错误] 需要 Python 3 + Pillow& pause & exit /b 1)

node scripts\render_shots.mjs --lang zh,en --keep-png || (echo [失败] 渲染阶段出错 & pause & exit /b 1)
python scripts\convert_webp.py --only shots || (echo [失败] webp 转换出错 & pause & exit /b 1)
python scripts\make_hero.py --lang zh || (echo [失败] 中文主图合成出错 & pause & exit /b 1)
python scripts\make_hero.py --lang en || (echo [失败] 英文主图合成出错 & pause & exit /b 1)
python scripts\convert_webp.py --only hero || (echo [失败] 主图 webp 转换出错 & pause & exit /b 1)
python scripts\verify_shots.py --from-render docs\.shots-tmp || (echo [失败] 验收未通过 & pause & exit /b 1)

echo.
echo 完成：docs\shots、docs\shots-en、docs\screenshot-hero.webp、docs\screenshot-hero-en.webp 已更新
pause

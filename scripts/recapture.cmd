@echo off
REM 重新生成 dsh-systray 发布物料（在任何有 Edge/Chrome 的机器上都能跑，不需要托盘/桌面会话）。
REM
REM 为什么不再跑 exe 抓窗口：老流程把出图尺寸绑在「窗口客户区像素」上，客户区大小受显示器缩放
REM 影响（笔记本 125%/150% 缩放下抓到的图右侧内容被裁掉）。现在改为无头 Chromium 按固定 CSS
REM 视口（840x560 = 设置窗口逻辑尺寸）渲染真实前端，尺寸恒定、与显示器无关，且不会打扰
REM 正在运行的托盘实例。
REM
REM 说明：网站轮播已是 iframe 实时预览（docs/mock），位图轮播截图（原 docs/shots[-en]）
REM 2026-10-06 起不再产出；README 首屏 2026-10-12 改用设置界面演示动图，
REM 2026-10-16 起格式为 APNG（docs/demo/settings-*.apng，深浅双主题），主图及其合成/转换脚本一并移除。
REM
REM 步骤：build_mock.mjs（把真实前端同步成站点实时界面预览 docs/mock/）
REM       -> render_shots.mjs（渲染设置窗口整窗截图 + 画面文字素材）
REM       -> record_demo.mjs + encode_demo.mjs（重录 README 演示动图；编码需要 ffmpeg）
REM       -> verify_shots.py（排版/裁切/脱敏/预览同步/动图 验收）
setlocal
cd /d "%~dp0.."

where node >nul 2>nul || (echo [错误] 需要 Node.js 18+（内置 fetch/WebSocket）& pause & exit /b 1)
where python >nul 2>nul || (echo [错误] 需要 Python 3 + Pillow& pause & exit /b 1)

node scripts\build_mock.mjs || (echo [失败] 站点预览生成出错 & pause & exit /b 1)
node scripts\render_shots.mjs --lang zh,en || (echo [失败] 渲染阶段出错 & pause & exit /b 1)

REM README 演示动图：编码环节需要 ffmpeg（PATH 或 DSH_FFMPEG 指向可执行文件），
REM 没有 ffmpeg 时只跳过动图重录，静态物料照常产出（避免把整条链路卡死）。
set "HAVE_FFMPEG="
where ffmpeg >nul 2>nul && set "HAVE_FFMPEG=1"
if defined DSH_FFMPEG set "HAVE_FFMPEG=1"
if not defined HAVE_FFMPEG (
  echo [跳过] 未找到 ffmpeg（PATH 或 DSH_FFMPEG）：README 演示动图不重录
) else (
  node scripts\record_demo.mjs || (echo [失败] 录制演示动图出错 & pause & exit /b 1)
  node scripts\encode_demo.mjs || (echo [失败] 编码演示动图出错 & pause & exit /b 1)
)

python scripts\verify_shots.py --from-render docs\.shots-parts || (echo [失败] 验收未通过 & pause & exit /b 1)

echo.
echo 完成：docs\mock（站点实时预览）+ docs\.shots-parts（核对素材）+ docs\demo（README 动图）已更新
pause

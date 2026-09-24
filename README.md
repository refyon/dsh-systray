<h1 align="center">
  <img src="docs/icon.svg" width="72" alt="dsh-systray logo" />
  <br />
  dsh-systray
</h1>

<p align="center">
  后台启动
  <a href="https://github.com/deepseek-ai/deepseek-harness">DeepSeek Harness</a>
  Web 本地服务器，并常驻系统托盘。
</p>

<p align="center">
  <a href="https://refyon.github.io/dsh-systray/"><strong>网站</strong></a> ·
  <a href="https://github.com/refyon/dsh-systray/releases/latest">更新日志</a>
</p>

<p align="center">
  <a href="README.md">简体中文</a> · <a href="README.en.md">English</a>
</p>

<p align="center">
  <a href="https://github.com/refyon/dsh-systray/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/refyon/dsh-systray?style=flat-square&color=2563eb" /></a>
  <img alt="Windows x64" src="https://img.shields.io/badge/Windows-x64-2563eb.svg?style=flat-square" />
  <img alt="macOS" src="https://img.shields.io/badge/macOS-Universal-2563eb.svg?style=flat-square" />
  <a href="https://github.com/refyon/dsh-systray/actions/workflows/release.yml"><img alt="Release build" src="https://github.com/refyon/dsh-systray/actions/workflows/release.yml/badge.svg" /></a>
</p>

<img src="docs/screenshot-hero.webp" alt="dsh-systray 设置窗口与自动部署/安装 Harness 依赖" />

dsh-systray 是一个 Windows / macOS 系统托盘应用，围绕三个核心特性设计：**轻量**（单文件免安装、免管理员权限、托盘常驻低占用）、**可靠**（环境自检自愈、启动失败自动回退、更新失败自动回滚）、**可迁移**（会话/插件/目录一键导出导入，换机无缝恢复）。双击即可后台拉起 DeepSeek Harness Web 本地服务，无需记忆端口。界面基于 [Wails v2](https://wails.io)（Go 后端 + WebView2 / WKWebView 前端）重构，设置窗口七页分类管理开机自启与后台服务、版本与更新（dsh-systray / Harness / 插件按模块独立检查）、数据同步、实时日志，整体配色支持浅色 / 深色自动跟随系统，内置自动更新。

> [!IMPORTANT]
> 这是一个社区维护的非官方工具，依赖快速演进的 `@deepseek-ai/dsh`。macOS 构建未经 Apple 公证，Windows 构建未做商业代码签名，首次运行可能需手动放行（Windows SmartScreen「仍要运行」/ macOS「右键 → 打开」）。首次启动约需 2–5 分钟自动部署环境。

## 系统要求

| 平台 | 要求 |
| --- | --- |
| Windows | **Windows 10 1803+ / Windows 11**，需要 [WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/)（随 Microsoft Edge 预装；发行包已内嵌引导安装器兜底，缺失时自动安装） |
| macOS | macOS 11.0+（Big Sur 及更新版本） |

## 下载

| 平台 | 架构 | 包 | 下载 |
| --- | --- | --- | --- |
| Windows | x64 | ZIP | [下载 Windows 版](https://github.com/refyon/dsh-systray/releases/latest/download/dsh-systray-windows-x64.zip) |
| macOS | Intel + Apple Silicon | ZIP (.app) | [下载 macOS 版](https://github.com/refyon/dsh-systray/releases/latest/download/dsh-systray-macos-universal.zip) |

## 功能

围绕三个核心特性设计：**轻量、可靠、可迁移**。

### 轻量 —— 双击即用，常驻无忧
- **双击启动**：无窗口、后台拉起 harness 的 `pnpm dsh web --port <port> --no-open`；启动进度（运行环境检查 → 依赖安装 → 服务就绪）在窗口内可见，就绪后弹窗提示（可一键打开 Web UI）
- **单文件免安装**：Windows 单 exe、macOS 单 .app，免管理员权限；便携 Node.js / pnpm 运行时按需自动就位
- **托盘常驻**：右键菜单直达「打开 Web UI / 设置 / 退出」，服务状态一望即知；深浅色主题随系统自动切换
- **单实例**：已在运行时再次双击会弹窗提示「已在运行中」，不产生第二个托盘图标

### 可靠 —— 自检、自愈、可回退
- **环境自检**：启动时检查 node / pnpm / harness，缺失时运行内置安装脚本（含 `git clone` 拉取 harness 源码）
- **启动失败自动回退**：服务启动失败（进程异常退出 / 加载错误）时，自动回退到上次正常运行的 harness 与插件状态并重启
- **更新双保险**：后台自动检查 GitHub Releases 新版本，窗口内展示下载进度并可取消；dsh-systray / DeepSeek Harness / 插件按模块独立检查更新，更新前自动快照、安装后健康校验，失败自动回退到上一可用版本
- **私有仓库插件**：来自私有仓库的插件在检查更新时引导 GitHub 设备流授权——弹窗确认后自动打开浏览器、**一次性授权码直接显示在进度窗口里**（同时写入剪贴板），授权完成自动重试检查（网络瞬断自动重连，最多 3 次）；首次使用会自动下载 GitHub CLI 便携版并显示下载进度。授权后还会把凭据接给更新链路（`gh auth setup-git` 写 git 凭据助手 + 用户级 `~/.npmrc` 写 `//codeload.github.com/:tokenHelper`），两处配置都只写**命令**、不写 token，也不把 token 发给第三方镜像
- **日志**：「日志」页实时跟踪 app.log / server.log，显示完整路径，自动跟随最新写入，支持一键清空

### 可迁移 —— 数据随身带，换机无缝恢复
- **账号同步（dsh-connect）**：邮箱验证码登录后，**开机自启动**、**最后选用的 Harness 版本（含预发布通道）**与**所有在线插件**随账号在多台机器间同步；本机目录、端口等环境相关配置与本地插件（`file:` / `local`）不上传。拉到的改动**不会自动生效**——设置页「数据同步」里常驻「重启生效」提示，点按钮进入进度视图（逐项文案 + 取消），按「每 key 取最新」合并落地（重复上报幂等、删除是墓碑不会复活）、单项失败可续做；未落地前状态显示「待生效 N 项」而非「已同步」。后台每 20 分钟自动检查一次，左侧「数据同步」项以小字彩色状态显示（同步中 / 待同步 / 待生效 / 同步失败 / 已同步+时间）
- **导出 / 导入**：会话记录、已安装插件、自选文件目录打包为 zip 备份；导入时解析压缩包罗列可恢复项，冲突询问并自动备份，恢复期间自动暂停/重启后台服务
- **配置即数据**：全部配置保存在用户目录（`config.json`），数据在 `~/.dsh`，随导出包完整迁移
- **跨平台一致**：Windows / macOS 同一套界面与数据格式（设计令牌见 [DESIGN.md](DESIGN.md)）

## 配置

`config.json` 位于用户配置目录（可选，缺失时用默认值）：

```json
{
  "port": 3080,
  "harnessDir": "/path/to/deepseek-harness",
  "startupTimeoutSec": 300,
  "updateMirror": "",
  "harnessPrerelease": false,
  "accountApiBase": ""
}
```

- `port`：服务器端口，默认 3080（可被环境变量 `DSH_SYSTRAY_PORT` 覆盖）
- `harnessDir`：harness 源码 / 安装目录，建议显式配置为实际路径（未配置时默认官方惯例位置 `~/deepseek-harness`，与官方 `npx '@deepseek-ai/dsh' web` 部署语义一致；旧版私有目录 `%LOCALAPPDATA%\Programs\dsh-systray-harness` 仅作迁移探测，不再作为新部署目标）
- `startupTimeoutSec`：服务启动等待超时（秒），默认 300（可被 `DSH_SYSTRAY_STARTUP_TIMEOUT` 覆盖）
- `updateMirror`：可选，GitHub 更新下载镜像前缀（国内网络友好，如 `https://ghproxy.net/`）
- `harnessPrerelease`：是否把 alpha/beta/rc 视为 harness 可更新版本（默认关闭，仅更新稳定版）
- `accountApiBase`：可选，账号同步服务（[dsh-connect](https://github.com/refyon/dsh-connect)）地址，默认官方正式域名。登录态与同步游标单独存放在同目录的 `account.json`（权限 0600，只含令牌与游标，不含密码）；在「设置 → 数据同步」退出登录即清除

## 架构

```
┌────────────────────────────── dsh-systray (Wails v2) ──────────────────────────────┐
│  src/frontend/（静态 HTML/CSS/JS，go:embed 内嵌，零构建步骤）                       │
│    ├── 启动/更新进度视图 + 设置七页（常规/关于/日志/导出/导入/数据同步/帮助）       │
│    └── 浅色/深色设计令牌（style.css :root 与 prefers-color-scheme）                 │
├───────────────────────────────────────────────────────────────────────────────────┤
│  Go 后端（src/）                                                                    │
│    ├── main.go       入口：配置/单实例/服务编排/窗口生命周期                        │
│    ├── app.go        Wails Bindings（配置/服务/日志/更新/导出导入）                 │
│    ├── platform_*.go 自启动/运行时/服务器/对话框/托盘图标（Windows/macOS）          │
│    ├── updater.go      自动更新（exe / .app 整包替换，校验+回滚；harness 版本/预发布通道）│
│    ├── plugin_update.go 插件清单与单独检查/更新（npm、GitHub 默认分支、本地来源判定）│
│    └── exportimport.go / ziptool.go  数据打包与恢复                                 │
└───────────────────────────────────────────────────────────────────────────────────┘
```

- **前端**：原生 HTML/CSS/JS，无 Node 构建链；Wails `-s` 直接嵌入 `src/frontend/dist`
- **托盘**：[energye/systray](https://github.com/energye/systray)（与 Wails 事件循环共存的 fork；macOS 经 `RunWithExternalLoop` 集成，不接管 NSApplication）
- **更新**：Windows 替换单文件 exe；macOS 替换整个 `.app` 包（`ditto` 解压保留权限）

## 构建

前置：Go 1.21+、[Wails CLI v2](https://wails.io/docs/gettingstarted/installation)（`go install github.com/wailsapp/wails/v2/cmd/wails@v2.15.0`）、前端静态文件（无需 Node）。

构建命令都在 `src/` 下执行（Wails 项目根 = `wails.json` 所在目录）；Windows 也可直接用 `scripts\build.ps1`（自动 `generate module` + `-skipbindings` 构建）。

| 平台 | 构建命令（在 `src/` 下执行） |
| --- | --- |
| Windows | `wails build -s -clean -platform windows/amd64 -ldflags "-X main.appVersion=v0.10.4"` |
| macOS | `wails build -s -clean -platform darwin/universal -ldflags "-X main.appVersion=v0.10.4"` |

> - 产物：`src/build/bin/dsh-systray.exe`（Windows）/ `src/build/bin/dsh-systray.app`（macOS），仓库根不再输出编译产物
> - `-s`：跳过前端构建（直接内嵌 `src/frontend/dist`）；改动前端后无需其他步骤，直接重新 `wails build`
> - `-X main.appVersion=` 注入当前版本号，供自动更新对比使用（GitHub Actions 打 tag 发布时自动注入；本地开发可省略，此时为 `dev`，跳过更新检查）
> - Windows 如需为未预装 WebView2 的机器兜底，加 `-webview2 download`（内嵌引导安装器，CI 已启用）
> - macOS 产物为 `.app` 包；`src/build/darwin/Info.plist` 已注入 `LSUIElement=true`（纯托盘应用，不显示 Dock 图标）

## 开发

```bash
cd src
wails dev   # 热重载开发模式（需 Node 可选；静态前端下等同于编译并运行）
```

本地调试时可用环境变量：`DSH_SYSTRAY_PORT`、`DSH_SYSTRAY_HARNESS_DIR`、`DSH_SYSTRAY_STARTUP_TIMEOUT`。

## 平台差异

| 能力 | Windows | macOS |
| --- | --- | --- |
| 界面渲染 | WebView2（Chromium） | WKWebView（Safari 内核） |
| 启动服务器 | `cmd /c` | `sh -c` |
| 打开 Web UI | `rundll32 url.dll` | `open` |
| 开机自启动 | 注册表 `HKCU\...\Run` | `~/Library/LaunchAgents/*.plist`（launchd） |
| 退出杀外部服务 | `netstat` + `taskkill` | `lsof` + `SIGTERM` |
| 提示方式 | MessageBox（自绘圆角弹窗） | `osascript` 通知/弹窗 |
| loading 界面 | Wails 窗口内进度视图 | Wails 窗口内进度视图 |
| Dock 图标 | — | 隐藏（LSUIElement，纯托盘） |

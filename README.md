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
  <a href="https://github.com/refyon/dsh-systray/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/refyon/dsh-systray?style=flat-square&color=2563eb" /></a>
  <img alt="Windows x64" src="https://img.shields.io/badge/Windows-x64-2563eb.svg?style=flat-square" />
  <img alt="macOS" src="https://img.shields.io/badge/macOS-Universal-2563eb.svg?style=flat-square" />
  <a href="https://github.com/refyon/dsh-systray/actions/workflows/release.yml"><img alt="Release build" src="https://github.com/refyon/dsh-systray/actions/workflows/release.yml/badge.svg" /></a>
</p>

<img src="docs/screenshot-hero.webp" alt="dsh-systray 设置窗口与依赖更新" />

dsh-systray 是一个 Windows / macOS 系统托盘应用：双击即可后台拉起 DeepSeek Harness Web 本地服务，托盘一望即知，无需记忆端口。界面基于 [Wails v2](https://wails.io)（Go 后端 + WebView2 / WKWebView 前端）重构，设置窗口五页分类管理开机自启与后台服务、版本与更新、实时日志，支持会话/插件/目录的一键导出与恢复；整体配色支持浅色 / 深色自动跟随系统，托盘图标随系统深浅色自适应，内置自动更新。

> [!IMPORTANT]
> 这是一个社区维护的非官方工具，依赖快速演进的 `@deepseek-ai/dsh`。macOS 构建未经 Apple 公证，Windows 构建未做商业代码签名，首次运行可能需手动放行（Windows SmartScreen「仍要运行」/ macOS「右键 → 打开」）。首次启动约需 2–5 分钟自动部署环境。

## 系统要求

| 平台 | 要求 |
| --- | --- |
| Windows | **Windows 10 1803+ / Windows 11**，需要 [WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/)（随 Microsoft Edge 预装；发行包已内嵌引导安装器兜底，缺失时自动安装） |
| macOS | macOS 11.0+（Big Sur 及更新版本） |

## 下载

| 平台 | 架构 | 包 | 大小 | 下载 |
| --- | --- | --- | --- | --- |
| Windows | x64 | ZIP | 4.8 MB | [下载 Windows 版](https://github.com/refyon/dsh-systray/releases/latest/download/dsh-systray-windows-x64.zip) |
| macOS | Intel + Apple Silicon | ZIP (.app) | 8.3 MB | [下载 macOS 版](https://github.com/refyon/dsh-systray/releases/latest/download/dsh-systray-macos-universal.zip) |

## 功能

- **双击启动**：无窗口、后台拉起 harness 的 `pnpm dsh web --port <port> --no-open`
- **启动 loading**：Wails 窗口内展示启动进度（运行环境检查 → 依赖安装 → 服务就绪），就绪后弹窗提示（可一键打开 Web UI）
- **托盘右键菜单**：
  - **打开 Web UI**：用默认浏览器打开 `http://127.0.0.1:<port>/`
  - **设置**：打开设置窗口（五页分类：常规-开机自启与后台服务重启 / 关于-版本与检查更新 / 日志-实时查看与清空 / 导出、导入-数据打包与恢复）
  - **退出**：关闭后台服务器进程树（含外部启动的 dsh web 服务）并退出托盘
- **日志**：「日志」页实时跟踪 app.log，显示完整路径，自动跟随最新写入，支持一键清空
- **导出 / 导入**：会话、插件、文件目录打包为 zip 备份；导入时解析压缩包罗列可恢复项，冲突询问并自动备份回滚，恢复期间自动暂停/重启后台服务
- **单实例**：已在运行时再次双击会弹窗提示「已在运行中」，不产生第二个托盘图标
- **依赖自检**：启动时检查 node / pnpm / harness 源码，缺失时运行内置安装脚本（含 `git clone` 拉取 harness 源码）
- **自动更新**：后台自动检查 GitHub Releases 新版本，发现新版本时在窗口内展示下载进度并可取消，安装完成自动重启（Windows / macOS 一致）
- **深浅色主题**：设置界面跟随系统浅色 / 深色模式自动切换（设计令牌见 [DESIGN.md](DESIGN.md)）

## 配置

`config.json` 位于用户配置目录（可选，缺失时用默认值）：

```json
{
  "port": 3080,
  "harnessDir": "/path/to/deepseek-harness",
  "startupTimeoutSec": 300,
  "updateMirror": "",
  "harnessPrerelease": false
}
```

- `port`：服务器端口，默认 3080（可被环境变量 `DSH_SYSTRAY_PORT` 覆盖）
- `harnessDir`：harness 源码 / 安装目录，建议显式配置为实际路径（未配置时默认官方惯例位置 `~/deepseek-harness`，与官方 `npx '@deepseek-ai/dsh' web` 部署语义一致；旧版私有目录 `%LOCALAPPDATA%\Programs\dsh-systray-harness` 仅作迁移探测，不再作为新部署目标）
- `startupTimeoutSec`：服务启动等待超时（秒），默认 300（可被 `DSH_SYSTRAY_STARTUP_TIMEOUT` 覆盖）
- `updateMirror`：可选，GitHub 更新下载镜像前缀（国内网络友好，如 `https://ghproxy.net/`）
- `harnessPrerelease`：是否把 alpha/beta/rc 视为 harness 可更新版本（默认关闭，仅更新稳定版）

## 安装插件

dsh-systray 后台启动的就是官方 `web` profile（插件与数据都在官方 `~/.dsh`，即 `$DSH_HOME`），
因此安装插件**直接使用 DeepSeek Harness 官方命令即可**，与官方部署完全兼容：

```bash
npx '@deepseek-ai/dsh' plugin --profile web add github:refyon/restrict-discipline
```

前置要求（在任意终端执行）：

- **node / npx**：本机已装 Node.js；或本机由 dsh-systray 内置了便携运行时（首次部署后
  **新开一个终端**，内置 node / pnpm 已自动写入用户 PATH）
- **git**：`github:` 形式的插件依赖 git 拉取（缺失时启动日志会有提示；安装 Git 后新开终端即可）

> 插件安装在 `~/.dsh/profiles/web`，与 dsh-systray 后台服务共用同一数据根；装完后
> **需重启后台服务才生效**：托盘 → 设置 → 常规 → 重启后台服务（或退出托盘重开）。
> 带构建脚本（prepare）的插件若被 pnpm 拦截（`ERR_PNPM_IGNORED_BUILDS`），把提示的
> 包名加入 `%USERPROFILE%\.dsh\profiles\web\pnpm-workspace.yaml` 的 `allowBuilds` 后重新执行安装命令。

## 架构

```
┌────────────────────────────── dsh-systray (Wails v2) ──────────────────────────────┐
│  frontend/（静态 HTML/CSS/JS，go:embed 内嵌，零构建步骤）                           │
│    ├── 启动/更新进度视图 + 设置五页（常规/关于/日志/导出/导入）                     │
│    └── 浅色/深色设计令牌（style.css :root 与 prefers-color-scheme）                │
├───────────────────────────────────────────────────────────────────────────────────┤
│  Go 后端                                                                           │
│    ├── main.go       入口：配置/单实例/服务编排/窗口生命周期                       │
│    ├── app.go        Wails Bindings（配置/服务/日志/更新/导出导入）                │
│    ├── platform_*.go 自启动/运行时/服务器/对话框/托盘图标（Windows/macOS）          │
│    ├── updater.go    自动更新（exe / .app 整包替换，校验+回滚）                    │
│    └── exportimport.go / ziptool.go  数据打包与恢复                                │
└───────────────────────────────────────────────────────────────────────────────────┘
```

- **前端**：原生 HTML/CSS/JS，无 Node 构建链；Wails `-s` 直接嵌入 `frontend/dist`
- **托盘**：[energye/systray](https://github.com/energye/systray)（与 Wails 事件循环共存的 fork；macOS 经 `RunWithExternalLoop` 集成，不接管 NSApplication）
- **更新**：Windows 替换单文件 exe；macOS 替换整个 `.app` 包（`ditto` 解压保留权限）

## 构建

前置：Go 1.21+、[Wails CLI v2](https://wails.io/docs/gettingstarted/installation)（`go install github.com/wailsapp/wails/v2/cmd/wails@v2.11.0`）、前端静态文件（无需 Node）。

| 平台 | 构建命令 |
| --- | --- |
| Windows | `wails build -s -clean -platform windows/amd64 -ldflags "-X main.appVersion=v0.5.0"` |
| macOS | `wails build -s -clean -platform darwin/universal -ldflags "-X main.appVersion=v0.5.0"` |

> - `-s`：跳过前端构建（直接内嵌 `frontend/dist`）；改动前端后无需其他步骤，直接重新 `wails build`
> - `-X main.appVersion=` 注入当前版本号，供自动更新对比使用（GitHub Actions 打 tag 发布时自动注入；本地开发可省略，此时为 `dev`，跳过更新检查）
> - Windows 如需为未预装 WebView2 的机器兜底，加 `-webview2 download`（内嵌引导安装器，CI 已启用）
> - macOS 产物为 `.app` 包；`build/darwin/Info.plist` 已注入 `LSUIElement=true`（纯托盘应用，不显示 Dock 图标）

## 开发

```bash
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

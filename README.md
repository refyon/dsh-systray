<h1 align="center">
  <img src="docs/icon.svg" width="72" alt="dsh-systray logo" />
  <br />
  dsh-systray
</h1>

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

<img src="docs/screenshot-hero.webp" alt="dsh-systray 设置窗口" />

> [!IMPORTANT]
> Windows 构建未做代码签名、macOS 构建未经 Apple 公证，首次运行可能需手动放行（Windows SmartScreen「仍要运行」/ macOS「右键 → 打开」）；首次启动约需 2–5 分钟自动部署环境。

## 系统要求

| 平台 | 要求 |
| --- | --- |
| Windows | **Windows 10 1803+ / Windows 11**，需要 [WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/)（缺失时发行包会自动安装） |
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
- **日志**：「日志」页实时跟踪统一日志文件（完整路径可一键复制），自动跟随最新写入，支持一键清空

### 可迁移 —— 数据随身带，换机无缝恢复
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
  "language": "auto"
}
```

| 字段 | 说明 |
| --- | --- |
| `port` | 服务器端口，默认 3080（可用 `DSH_SYSTRAY_PORT` 覆盖） |
| `harnessDir` | harness 安装目录，默认 `~/deepseek-harness` |
| `startupTimeoutSec` | 服务启动等待超时（秒），默认 300 |
| `updateMirror` | GitHub 下载镜像前缀，如 `https://ghproxy.net/` |
| `harnessPrerelease` | 是否把 alpha/beta/rc 视为可更新版本，默认关闭 |
| `language` | 界面语言：`auto` / `zh` / `en`，默认 `auto` |

## 构建

前置：Go 1.21+、[Wails CLI v2](https://wails.io/docs/gettingstarted/installation)（`go install github.com/wailsapp/wails/v2/cmd/wails@v2.15.0`）。

| 平台 | 构建命令 |
| --- | --- |
| Windows | `wails build -s -clean -platform windows/amd64 -ldflags "-X main.appVersion=v0.8.13"` |
| macOS | `wails build -s -clean -platform darwin/universal -ldflags "-X main.appVersion=v0.8.13"` |

- `-s`：跳过前端构建，直接内嵌 `frontend/dist`（前端改动后重新 `wails build` 即可）
- `-X main.appVersion=`：注入版本号供自动更新对比（CI 打 tag 时自动注入；本地可省略 = `dev`）
- Windows 需为未预装 WebView2 的机器兜底时加 `-webview2 download`

## 平台差异

| 能力 | Windows | macOS |
| --- | --- | --- |
| 界面渲染 | WebView2（Chromium） | WKWebView（Safari 内核） |
| 启动服务器 | `cmd /c` | `sh -c` |
| 打开 Web UI | `rundll32 url.dll` | `open` |
| 开机自启动 | 注册表 `HKCU\...\Run` | `~/Library/LaunchAgents/*.plist`（launchd） |
| 退出杀外部服务 | `netstat` + `taskkill` | `lsof` + `SIGTERM` |
| 提示方式 | MessageBox（自绘圆角弹窗） | `osascript` 通知/弹窗 |
| Dock 图标 | — | 隐藏（LSUIElement，纯托盘） |

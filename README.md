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

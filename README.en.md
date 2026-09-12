<h1 align="center">
  <img src="docs/icon.svg" width="72" alt="dsh-systray logo" />
  <br />
  dsh-systray
</h1>

<p align="center">
  <a href="https://refyon.github.io/dsh-systray/"><strong>Website</strong></a> ·
  <a href="https://github.com/refyon/dsh-systray/releases/latest">Release notes</a>
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

<img src="docs/screenshot-hero.webp" alt="dsh-systray settings window" />

> [!IMPORTANT]
> Windows builds are not code-signed and macOS builds are not notarized, so the first launch may need a manual allow (Windows SmartScreen “Run anyway” / macOS “right-click → Open”); the first start takes about 2–5 minutes to deploy the environment.

## System Requirements

| Platform | Requirement |
| --- | --- |
| Windows | **Windows 10 1803+ / Windows 11** with [WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/) (installed automatically by the release bundle when missing) |
| macOS | macOS 11.0+ (Big Sur or later) |

## Download

| Platform | Architecture | Package | Download |
| --- | --- | --- | --- |
| Windows | x64 | ZIP | [Download for Windows](https://github.com/refyon/dsh-systray/releases/latest/download/dsh-systray-windows-x64.zip) |
| macOS | Intel + Apple Silicon | ZIP (.app) | [Download for macOS](https://github.com/refyon/dsh-systray/releases/latest/download/dsh-systray-macos-universal.zip) |

## Features

Designed around three core traits: **Lightweight, Reliable, Portable**.

### Lightweight — double-click to use, always in the tray
- **Start with a double-click**: windowless — launches harness in the background with `pnpm dsh web --port <port> --no-open`; the startup progress (environment check → dependency install → service ready) is visible in the window, with a prompt when ready (one-click to open the Web UI)
- **Single-file, no install**: one exe on Windows / one .app on macOS, no admin rights; portable Node.js / pnpm runtimes are provisioned on demand
- **Always in the tray**: the right-click menu reaches “Open Web UI / Settings / Quit”; service status at a glance; light/dark theme follows the system
- **Single instance**: double-clicking while running shows a “already running” prompt instead of a second tray icon

### Reliable — self-check, self-heal, rollback
- **Environment self-check**: checks node / pnpm / harness on startup and runs the built-in installer if missing (including `git clone` for a source harness)
- **Auto-rollback on startup failure**: if the service fails to start (process crash / load error), it rolls back to the last known-good harness & plugin state and restarts
- **Twofold update safety**: background checks for new GitHub Releases; download progress is shown in the window and cancellable; dsh-systray / DeepSeek Harness / plugins are checked independently per module, with automatic snapshots before updates, health verification after install and auto-rollback to the last working version on failure
- **Logs**: the Logs page follows the unified log file live (full path, one-click copy), auto-scrolls to the newest writes and supports one-click clearing

### Portable — data travels with you, seamless restore on another machine
- **Export / Import**: sessions, installed plugins and chosen file directories are bundled into a zip backup; import parses the bundle to list restorable items, prompts on conflicts with automatic backups, and pauses/restarts the background service during restore
- **Config is data**: all configuration lives under the user directory (`config.json`), data under `~/.dsh`, and both migrate fully with the export bundle
- **Cross-platform consistency**: same UI and same data format on Windows and macOS (design tokens in [DESIGN.md](DESIGN.md))

## Configuration

`config.json` lives in the user config directory (optional; defaults are used when missing):

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

| Field | Description |
| --- | --- |
| `port` | Server port, default 3080 (overridable via `DSH_SYSTRAY_PORT`) |
| `harnessDir` | Harness install directory, default `~/deepseek-harness` |
| `startupTimeoutSec` | Timeout (seconds) waiting for the service to start, default 300 |
| `updateMirror` | GitHub download mirror prefix, e.g. `https://ghproxy.net/` |
| `harnessPrerelease` | Whether alpha/beta/rc builds count as updateable versions (off by default) |
| `language` | UI language: `auto` / `zh` / `en`, default `auto` |

## Build

Requirements: Go 1.21+ and [Wails CLI v2](https://wails.io/docs/gettingstarted/installation) (`go install github.com/wailsapp/wails/v2/cmd/wails@v2.15.0`).

| Platform | Command |
| --- | --- |
| Windows | `wails build -s -clean -platform windows/amd64 -ldflags "-X main.appVersion=v0.8.13"` |
| macOS | `wails build -s -clean -platform darwin/universal -ldflags "-X main.appVersion=v0.8.13"` |

- `-s`: skip the frontend build and embed `frontend/dist` directly (just re-run `wails build` after frontend changes)
- `-X main.appVersion=`: injects the version used by the updater (CI injects it from the tag; omitting it locally means `dev`)
- Add `-webview2 download` on Windows to bundle the WebView2 bootstrapper

## Platform Differences

| Capability | Windows | macOS |
| --- | --- | --- |
| UI rendering | WebView2 (Chromium) | WKWebView (Safari engine) |
| Start server | `cmd /c` | `sh -c` |
| Open Web UI | `rundll32 url.dll` | `open` |
| Autostart | Registry `HKCU\...\Run` | `~/Library/LaunchAgents/*.plist` (launchd) |
| Kill external service on exit | `netstat` + `taskkill` | `lsof` + `SIGTERM` |
| Prompts | MessageBox (custom rounded dialog) | `osascript` notification/dialog |
| Dock icon | — | Hidden (LSUIElement, tray only) |

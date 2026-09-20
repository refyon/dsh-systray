<h1 align="center">
  <img src="docs/icon.svg" width="72" alt="dsh-systray logo" />
  <br />
  dsh-systray
</h1>

<p align="center">
  Launches the
  <a href="https://github.com/deepseek-ai/deepseek-harness">DeepSeek Harness</a>
  Web server in the background and lives in the system tray.
</p>

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

<img src="docs/screenshot-hero.webp" alt="dsh-systray settings window and automatic deployment / Harness dependency install" />

dsh-systray is a Windows / macOS system-tray application built around three core traits: **Lightweight** (single-file, no install, no admin rights, low-footprint tray residency), **Reliable** (environment self-check & self-healing, auto-rollback on startup failure, auto-rollback on update failure), and **Portable** (one-click export/import of sessions, plugins and folders; seamless restore on another machine). A double-click starts the DeepSeek Harness Web local service in the background — no ports to remember. The UI is rebuilt on [Wails v2](https://wails.io) (Go backend + WebView2 / WKWebView frontend). The settings window organizes seven pages: autostart & the background service, versions & updates (dsh-systray / Harness / plugins checked independently per module), data sync, and live logs; the color scheme follows the system light/dark mode and auto-update is built in.

> [!IMPORTANT]
> This is a community-maintained, unofficial tool that depends on the fast-moving `@deepseek-ai/dsh`. The macOS build is not notarized by Apple and the Windows build has no commercial code signing, so the first run may require a manual allow (Windows SmartScreen “Run anyway” / macOS “right-click → Open”). First launch takes about 2–5 minutes to deploy the environment automatically.

## System Requirements

| Platform | Requirement |
| --- | --- |
| Windows | **Windows 10 1803+ / Windows 11** with [WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/) (preinstalled with Microsoft Edge; the release bundle embeds a bootstrap installer fallback that installs it automatically when missing) |
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
- **Private-repo plugins**: plugins hosted in private repositories guide you through GitHub device-flow authorization on update checks — after confirming, the browser opens automatically and the **one-time code is shown right in the progress window** (and copied to the clipboard); the check retries itself once authorized. The GitHub CLI is downloaded automatically on first use with visible progress. Credentials live in the system credential store — dsh-systray never writes any token to disk and never sends it to third-party mirrors
- **Logs**: the Logs page follows app.log / server.log live, shows full paths, auto-scrolls to the newest writes and supports one-click clearing

### Portable — data travels with you, seamless restore on another machine
- **Account sync (dsh-connect)**: after signing in with an email code, your **start-at-login switch**, the **selected Harness version (including the prerelease channel)** and **every online plugin** follow the account across machines. Machine-specific settings (directories, ports) and local plugins (`file:` / `local`) are never uploaded. Pulled changes **never apply automatically** — the "Data sync" page keeps a persistent "restart to apply" prompt, and the button merges by "latest per key" (duplicate reports are idempotent, removals are tombstones that cannot come back). A background check runs every 20 minutes and the left nav shows a small colour-coded status (syncing / pending / failed / synced + time)
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
  "accountApiBase": ""
}
```

- `port`: server port, default 3080 (overridable via the `DSH_SYSTRAY_PORT` environment variable)
- `harnessDir`: harness source / install directory; we recommend setting it explicitly to the real path (when unset the default official location `~/deepseek-harness` is used, matching the official `npx '@deepseek-ai/dsh' web` deployment semantics; the legacy private directory `%LOCALAPPDATA%\Programs\dsh-systray-harness` is only probed for migration and is no longer a deployment target)
- `startupTimeoutSec`: timeout (seconds) waiting for the service to start, default 300 (overridable via `DSH_SYSTRAY_STARTUP_TIMEOUT`)
- `updateMirror`: optional GitHub update download mirror prefix (handy behind mainland-China networks, e.g. `https://ghproxy.net/`)
- `harnessPrerelease`: whether alpha/beta/rc builds count as updateable harness versions (off by default — only stable versions)
- `accountApiBase`: optional base URL of the account-sync service ([dsh-connect](https://github.com/refyon/dsh-connect)); defaults to the official domain. The sign-in state and sync cursor live in a separate `account.json` in the same directory (mode 0600; it holds only the token and cursor, never a password) and are removed when you sign out on the "Data sync" page

## Architecture

```
┌────────────────────────────── dsh-systray (Wails v2) ──────────────────────────────┐
│  src/frontend/ (static HTML/CSS/JS, embedded via go:embed, zero build steps)        │
│    ├── startup/update progress view + seven settings pages (general/about/logs/export/import/sync/help)
│    └── light/dark design tokens (style.css :root and prefers-color-scheme)          │
├───────────────────────────────────────────────────────────────────────────────────┤
│  Go backend (src/)                                                                  │
│    ├── main.go       entry: config / single instance / service orchestration / window lifecycle
│    ├── app.go        Wails Bindings (config / service / logs / update / export-import)
│    ├── platform_*.go autostart / runtime / server / dialogs / tray icons (Windows/macOS)
│    ├── updater.go      auto-update (whole-package exe / .app replacement, verify+rollback; harness version & prerelease channel)
│    ├── plugin_update.go plugin inventory & per-plugin check/update (npm, GitHub default branch, local sources)
│    └── exportimport.go / ziptool.go  data bundling & restore
└───────────────────────────────────────────────────────────────────────────────────┘
```

- **Frontend**: plain HTML/CSS/JS without a Node build chain; Wails `-s` embeds `src/frontend/dist` directly
- **Tray**: [energye/systray](https://github.com/energye/systray) (a fork coexisting with the Wails event loop; on macOS integrated via `RunWithExternalLoop`, without taking over NSApplication)
- **Update**: Windows replaces the single exe; macOS replaces the whole `.app` bundle (`ditto` extraction preserves permissions)

## Building

Prerequisites: Go 1.21+, [Wails CLI v2](https://wails.io/docs/gettingstarted/installation) (`go install github.com/wailsapp/wails/v2/cmd/wails@v2.15.0`), static frontend files (no Node needed).

Run the build commands inside `src/` (the Wails project root = the directory holding `wails.json`); on Windows you can also use `scripts\build.ps1` (runs `generate module` + a `-skipbindings` build).

| Platform | Build command (run inside `src/`) |
| --- | --- |
| Windows | `wails build -s -clean -platform windows/amd64 -ldflags "-X main.appVersion=v0.9.5"` |
| macOS | `wails build -s -clean -platform darwin/universal -ldflags "-X main.appVersion=v0.9.5"` |

> - Output: `src/build/bin/dsh-systray.exe` (Windows) / `src/build/bin/dsh-systray.app` (macOS); the repository root keeps no build artifacts
> - `-s`: skips the frontend build (embeds `src/frontend/dist` directly); after frontend changes simply re-run `wails build`
> - `-X main.appVersion=` injects the current version used for auto-update comparison (injected automatically when GitHub Actions builds a tagged release; local builds may omit it — the version is then `dev`, which skips update checks)
> - On Windows add `-webview2 download` to embed a bootstrap installer for machines without WebView2 (enabled in CI)
> - macOS output is a `.app` bundle; `src/build/darwin/Info.plist` sets `LSUIElement=true` (pure tray app — no Dock icon)

## Development

```bash
cd src
wails dev   # hot-reload dev mode (Node optional; with a static frontend this equals compile & run)
```

Useful environment variables for local debugging: `DSH_SYSTRAY_PORT`, `DSH_SYSTRAY_HARNESS_DIR`, `DSH_SYSTRAY_STARTUP_TIMEOUT`.

## Platform differences

| Capability | Windows | macOS |
| --- | --- | --- |
| Rendering | WebView2 (Chromium) | WKWebView (Safari engine) |
| Start server | `cmd /c` | `sh -c` |
| Open Web UI | `rundll32 url.dll` | `open` |
| Autostart | registry `HKCU\...\Run` | `~/Library/LaunchAgents/*.plist` (launchd) |
| Kill external service on quit | `netstat` + `taskkill` | `lsof` + `SIGTERM` |
| Prompting | MessageBox (custom rounded dialog) | `osascript` notifications/dialogs |
| Loading UI | in-window progress view (Wails) | in-window progress view (Wails) |
| Dock icon | — | hidden (LSUIElement, pure tray) |

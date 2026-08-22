# dsh-systray

Windows / macOS 托盘应用：双击后后台启动 DeepSeek Harness Web 本地服务器（不显示窗口），并在系统托盘显示白底黑鲸鱼图标。

## 功能

- 双击启动：无窗口、后台拉起 harness 的 `pnpm dsh web --port <port> --no-open`
- 启动 loading：服务启动期间显示加载窗口，就绪后弹窗提示（可一键打开 Web UI）
- 托盘右键菜单：
  - **打开 Web UI**：用默认浏览器打开 `http://127.0.0.1:<port>/`
  - **开机自启动**：可开关，启用时显示打勾（写入系统登录自启动项）
  - **退出**：关闭后台服务器进程树（含外部启动的 dsh web 服务）并退出托盘
- 单实例：已在运行时再次双击会弹窗提示「已在运行中」，不产生第二个托盘图标
- 依赖自检：启动时检查 node / pnpm / harness 源码，缺失时运行内置安装脚本（含 `git clone` 拉取 harness 源码）

## 配置

在可执行文件同目录放置 `config.json`（可选，缺失时用默认值）：

```json
{
  "port": 3080,
  "harnessDir": "/path/to/deepseek-harness"
}
```

- `port`：服务器端口，默认 3080（可用环境变量 `DSH_SYSTRAY_PORT` 覆盖）
- `harnessDir`：harness 源码目录，建议显式配置为实际路径（未配置时按平台取默认值）

## 构建

前置：安装 Go 工具链（1.21+）。

| 平台 | 构建命令 |
| --- | --- |
| Windows | `go build -trimpath -ldflags '-s -w -H=windowsgui' -o dist\dsh-systray.exe .` |
| macOS | `CGO_ENABLED=1 go build -o dist/dsh-systray .` |

> macOS 的托盘依赖 Cocoa（cgo），需在 macOS 上构建，无法从 Windows 交叉编译。
> Windows 也可直接运行 `scripts\build.ps1`。

## 自动构建与发布（GitHub Actions）

仓库已配置 `.github/workflows/release.yml`：推送 `v*` 标签即自动在 GitHub 云端并行编译 **Windows** 与 **macOS** 两个平台的可执行文件，并发布到 GitHub Release。

```bash
git tag v1.0.0
git push origin v1.0.0
```

- **windows-latest**：编译 `dsh-systray.exe`（`-H=windowsgui`，无控制台窗口）
- **macos-latest**：编译 arm64 + amd64 两个架构并用 `lipo` 合并为 **universal** 通用二进制（Intel / Apple Silicon 均可直接运行）
- 产物打包为 zip 上传到 Release 页面，并附带 `SHA256SUMS.txt` 校验和
- 不满足标签条件时，也可在 Actions 页面「Run workflow」手动触发，仅产出可下载的构建产物，不创建 Release
- 未配置代码签名/公证，Windows 首次运行可能出现 SmartScreen 提示、macOS 首次运行需右键「打开」绕过 Gatekeeper

## 平台差异

| 能力 | Windows | macOS |
| --- | --- | --- |
| 启动服务器 | `cmd /c` | `sh -c` |
| 打开 Web UI | `rundll32 url.dll` | `open` |
| 开机自启动 | 注册表 `HKCU\...\Run` | `~/Library/LaunchAgents/*.plist`（launchd） |
| 退出杀外部服务 | `netstat` + `taskkill` | `lsof` + `SIGTERM` |
| 提示方式 | MessageBox | `osascript` 通知/弹窗 |
| loading 界面 | 原生 Win32 窗口 | 系统通知 |

## 运行与日志

- Windows 产物 `dist\dsh-systray.exe` 双击即用；macOS 产物 `dist/dsh-systray` 需先 `chmod +x`
- 日志位于「用户配置目录」下的 `dsh-systray/logs/`（`app.log` 托盘日志、`server.log` 服务器输出）：
  - Windows：`%APPDATA%\dsh-systray\logs\`
  - macOS：`~/Library/Application Support/dsh-systray/logs/`

## 说明与限制

- 服务器启动依赖 **Node.js + pnpm + harness 源码**。缺 node/pnpm 时会尝试自动安装（需授权）；缺 harness 源码时会自动 `git clone https://github.com/deepseek-ai/deepseek-harness.git`（分支 master）到默认目录并执行 `pnpm install`。
- `dsh web` 需要已构建的前端产物；若首次启动报错，先在 harness 目录执行一次 `pnpm install` 和 `pnpm run build`。
- 托盘图标为白底黑鲸鱼，任何主题下都清晰可见；已内嵌在 `icon_gen.go`（由 `scripts\gen-icon.mjs` 生成），无需每次构建重新生成。

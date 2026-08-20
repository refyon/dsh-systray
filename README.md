# dsh-systray

Windows / macOS 托盘应用：双击后后台启动 DeepSeek Harness Web 本地服务器（不显示窗口），并在系统托盘显示白底黑鲸鱼图标。

## 功能

- 双击启动：无窗口、后台拉起 harness 的 `pnpm dsh web --port <port> --no-open`
- 启动 loading 窗口：显示加载窗口（可最小化/关闭，或点「确定」提前关闭），就绪后弹窗提示（可一键打开 Web UI）
- 托盘右键菜单：
  - **打开 Web UI**：用默认浏览器打开 `http://127.0.0.1:<port>/`
  - **开机自启动**：可开关，启用时显示打勾（写入 `HKCU\...\CurrentVersion\Run`）
  - **退出**：关闭后台服务器进程树（含外部启动的 dsh web 服务）并退出托盘
- 单实例：已在运行时再次双击会弹窗提示「已在运行中」，不产生第二个托盘图标
- 依赖自检：启动时检查 node / pnpm / harness 源码，缺失则弹出 UAC 并运行内置安装脚本（含 `git clone` 拉取 harness 源码）

## 配置

在 exe 同目录放置 `config.json`（可选，缺失时用默认值）：

```json
{
  "port": 3080,
  "harnessDir": "I:/deepseek-harness"
}
```

- `port`：服务器端口，默认 3080（可用环境变量 `DSH_SYSTRAY_PORT` 覆盖）
- `harnessDir`：harness 源码目录，默认 `I:\deepseek-harness`

## 构建

前置：本机 Go 1.27（`D:\Program Files\Go`）。

```powershell
powershell -File scripts\build.ps1
```

或手动：

```powershell
cd dsh-systray
go mod tidy
go build -trimpath -ldflags '-s -w -H=windowsgui' -o dist\dsh-systray.exe .
```

## 产物与运行

- 产物：`dist\dsh-systray.exe`，绿色免安装，双击即用（无需 Go / Node 运行时）
- 日志：`%LOCALAPPDATA%\dsh-systray\logs\`（`app.log` 为托盘日志，`server.log` 为服务器输出）

## macOS 支持

- 需在 **macOS 上构建**（托盘依赖 Cocoa，需 cgo，无法从 Windows 交叉编译）：`CGO_ENABLED=1 go build -o dist/dsh-systray .`
- 默认 harness 目录：`~/deepseek-harness`（Windows 为 `I:\deepseek-harness`）
- 开机自启动：写入 `~/Library/LaunchAgents/com.deepseek.dsh-systray.plist`（launchd）
- loading / 就绪提示：用 `osascript` 通知与弹窗（macOS 无 Windows 式 loading 窗口与 MessageBox）
- 退出时用 `lsof` 找监听进程并 `SIGTERM` 终止

## 说明与限制

- 服务器启动依赖 **Node.js + pnpm + harness 源码**。缺 node/pnpm 时应用会尝试自动安装（需 UAC 授权）；缺 harness 源码时会自动 `git clone https://github.com/deepseek-ai/deepseek-harness.git`（分支 master）到 `I:\deepseek-harness`，并执行 `pnpm install`。若在 config.json 改了 `harnessDir`，请同步更新 `scripts\install-prereqs.ps1` 里的目录。
- `dsh web` 需要已构建的前端产物；若首次启动报错，先在 harness 目录执行一次 `pnpm install` 和 `pnpm run build`。
- 托盘图标为白底黑鲸鱼，任何任务栏颜色下都清晰可见。图标已内嵌在 `icon_gen.go`（由 `scripts\gen-icon.mjs` 生成），无需每次构建重新生成。

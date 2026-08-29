# dsh-systray

Windows / macOS 托盘应用：双击后后台启动 DeepSeek Harness Web 本地服务器，并常驻系统托盘。

## 功能

- 双击启动：无窗口、后台拉起 harness 的 `pnpm dsh web --port <port> --no-open`
- 启动 loading：服务启动期间显示加载窗口，就绪后弹窗提示（可一键打开 Web UI）
- 托盘右键菜单：
  - **打开 Web UI**：用默认浏览器打开 `http://127.0.0.1:<port>/`
  - **设置**：打开设置窗口（左侧分类栏：常规-开机自启动开关 / 关于-版本号与检查更新）
  - **退出**：关闭后台服务器进程树（含外部启动的 dsh web 服务）并退出托盘
- 单实例：已在运行时再次双击会弹窗提示「已在运行中」，不产生第二个托盘图标
- 依赖自检：启动时检查 node / pnpm / harness 源码，缺失时运行内置安装脚本（含 `git clone` 拉取 harness 源码）
- 自动更新：启动 30 秒后静默检查 GitHub Releases 新版本，发现新版本时提示「立即更新 / 稍后」（Windows / macOS 一致）

## 配置

`config.json` 位于用户配置目录（可选，缺失时用默认值）：
- Windows：`%APPDATA%\dsh-systray\config.json`
- macOS：`~/Library/Application Support/dsh-systray/config.json`
（旧版本 exe 同目录的 config.json 仍可被兼容读取）

```json
{
  "port": 3080,
  "harnessDir": "/path/to/deepseek-harness",
  "startupTimeoutSec": 300
}
```

- `port`：服务器端口，默认 3080（可用环境变量 `DSH_SYSTRAY_PORT` 覆盖）
- `harnessDir`：harness 源码目录，建议显式配置为实际路径（未配置时按平台取默认值）
- `startupTimeoutSec`：服务启动等待超时（秒），默认 300（可用环境变量 `DSH_SYSTRAY_STARTUP_TIMEOUT` 覆盖）；超时后若服务进程仍在运行会继续后台等待并二次提示

## 构建

前置：安装 Go 工具链（1.21+）。

| 平台 | 构建命令 |
| --- | --- |
| Windows | `go build -trimpath -ldflags "-s -w -H=windowsgui -X main.appVersion=v0.3.0" -o dist\dsh-systray.exe .` |
| macOS | `CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.appVersion=v0.3.0" -o dist/dsh-systray .` |

> `-X main.appVersion=` 注入当前版本号，供自动更新对比使用（GitHub Actions 打 tag 发布时自动注入；本地开发可省略，此时为 `dev`，跳过更新检查）。

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
- 自动更新：从 GitHub Releases（`refyon/dsh-systray`）查询最新版本并下载对应平台的 zip（Windows 替换 `dsh-systray.exe`，macOS 通过辅助脚本替换 `.app` 后重新打开）；更新包经 `SHA256SUMS.txt` 校验。更新重启不会关闭后台 Web 服务，新实例会自动复用。macOS 需程序所在目录有写权限（如 `/Applications` 下更新失败时，请手动下载替换）。

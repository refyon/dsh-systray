# AGENTS.md

仓库级协作约定。改这个仓库的代码前先读；与 `CONTRIBUTING.md` 的分工是：CONTRIBUTING 讲「人怎么贡献」，本文讲「怎么改不会把已有行为弄坏」。

## 不可变约定（改这些等于破坏兼容）

这些值承载用户已有环境，改动会导致老用户升级后状态丢失或行为异常。确需变更时，必须同时提供迁移路径并在 `docs/RELEASE_NOTES.md` 写明。

| 项 | 值 / 位置 |
| --- | --- |
| 用户配置文件名 | `config.json`，位于系统配置目录（Windows `%APPDATA%\dsh-systray`、macOS `~/Library/Application Support/dsh-systray`） |
| 登录态文件 | 同目录 `account.json`，权限 `0600` |
| 日志文件名 | `dsh-systray.log`（含轮转归档，日志页按完整路径读取） |
| Harness 数据目录 | `~/.dsh`（不迁移、不改名） |
| 开机自启 | Windows 注册表 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 的 `DeepSeekHarness`；macOS 启动项（LaunchAgent） |
| 单实例互斥名 | `Local\dsh-systray-single-instance` |
| 导出包格式 | `dsh-systray-export-*.zip`，导入需兼容历史包（含旧版 `link:`/`file:` 依赖写法，见 `reconcileProfileDepsRepair`） |
| 更新包布局 | 压缩包根级 `dsh-systray.exe` / `dsh-systray.app`（带 `dist/` 前缀会报「更新包中缺少 …」） |
| 校验文件 | Release 资产含 `SHA256SUMS.txt`；更新前必须校验 |
| 自启动参数 | `--autostart`（静默启动，不显示窗口） |

## 构建与验证

命令在 `src/` 下执行（`go.mod`、`wails.json`、`build/`、`frontend/` 都在该目录）。

```bash
# 构建（Windows 也可用 scripts\build.ps1；必须走 wails build，纯 go build 会命中 Wails 错误桩并丢失图标）
cd src && wails build -s -clean -platform windows/amd64 -ldflags "-X main.appVersion=v1.3.6"

# 测试
cd src && go test ./...                      # Go 回归（服务守护 / 更新 / 同步 / 导入导出）
node scripts/check-frontend-i18n.mjs         # 文案键齐全
node scripts/check-frontend-files-card.mjs   # 文件列表纯函数
node scripts/check-frontend-lang.mjs         # 真实浏览器 7 页 × 中英基线
```

- `scripts/build.ps1` 是规范构建入口：先生成绑定再 `build -skipbindings`。
- **前端改动后必须跑 `node scripts/build_mock.mjs`**：官网轮播是 `docs/mock/` 的 iframe 实时预览，不同步就会漂移，`python scripts/verify_shots.py`（28 项验收）会失败。
- 发布物料链路：`build_mock.mjs` → `render_shots.mjs` → `verify_shots.py`（见 `scripts/recapture.cmd`）。
- README 首屏用 `docs/demo/settings-*.gif` 演示动图（无 hero 位图，主图合成/转换脚本已移除）；界面改动后动图需重录。

## 改动纪律

- **注释与提交信息用中文**；面向用户的文案一律走 i18n（静态 `data-i18n` + `I18N_EN`，动态 `tr()`/`fmt()` + `I18N_DYN`）。
- **前端无源码**：直接改 `src/frontend/dist/{index.html,main.js,style.css}`。
- 涉及界面时遵循 `docs/DESIGN.md`：颜色/间距走 token、一屏一个主操作、状态齐全（hover/active/focus/disabled/loading/empty/error）、弹窗必须前台、触控目标 ≥44px。
- **新增 Go 导出方法后**：`wails generate module` 在本机可能静默不产出（exit 0 但无 `frontend/wailsjs`）。前端通过 `window.go.main.App.*` 运行时反射调用绑定，因此绑定文件缺失不影响出包；不要为了让生成器跑通而改构建流程。
- **启动方式双路径**（Web UI / 官方桌面端）：仅某一路径成立的条目用声明式标记 `data-only-web` / `data-only-desktop`，不要在 JS 里逐个判 id；具破坏性的操作（切换启动方式、重置）必须先 `confirmDialog`。
- 大改动先开 issue 讨论（[design_discussion](.github/ISSUE_TEMPLATE/design_discussion.yml)）再动手，避免返工。

## 性能与体量红线

- Web 界面仅监听 loopback；新增出网请求必须说明用途，并纳入代理（`proxy` 配置）与镜像（`updateMirror` / `mirrorBase`）路径。
- 日志页可处理大文件：走增量读取与虚拟滚动，禁止一次性全量传字符串到前端。
- 同步有容量上限（每账号 10 MB），超限只提示一次并暂停本轮，不要改成逐文件报错刷屏。
- 新增运行时依赖前先评估：安装包体积、首次启动时长（当前约 2–5 分钟）、是否需要管理员权限。

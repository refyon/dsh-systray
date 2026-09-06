# 评估：dsh-systray macOS 四项修复（图标尺寸 / 日志为空 / 开机自启动 / 阻止关机）

> 状态：评估完成，待用户确认后动工（2026-09-05）。
> 涉及文件：platform_darwin.go、internal/systray/systray_darwin.{go,m}、main.go(onBeforeClose/onReady)、
> build/darwin/Info.plist、icon_gen.go、scripts/gen-icon.mjs、.github/workflows/release.yml。

## 总则

- 四个问题全部集中在 darwin 专属路径，Windows 沙箱无法编译/运行（cgo + AppKit）。
- 改动纪律：改完 darwin 文件先 `GOOS=darwin go vet ./...` 预检（历史教训：Windows 本地编译不覆盖 darwin 文件），
  编译交给 CI macos-latest（universal），最终验收必须在用户真机。
- 版本策略：纯修复 → v0.7.2（补丁版 +1）。

---

## 1. mac 托盘图标尺寸（"还是差了一点点"）

### 现状（internal/systray/systray_darwin.m）

- `setIcon` C 函数：`[image setSize:NSMakeSize(16,16)]`，然后 `paddedStatusImage`：
  22×22pt 画布、`glyphRatio=0.6` → 图形实际 13.2pt、画布顶格 22pt（菜单栏高度）。
- 源图：`extractLargestPNG(iconData)` 从 ICO 取最大条目（可能 256×256），无专门 Retina 资产。
- 模板图 iconDataTemplate 已是纯黑+alpha（符合 template 要求）。

### mac 通用图标尺寸（事实）

- 菜单栏（NSStatusBar）厚度约 22pt；Apple HIG 与社区共识：菜单栏图标**图形 16–18pt 见方**，
  主流做法 18×18pt @1x + 36×36px @2x 模板 PNG；22pt 画布会让按钮占满菜单栏、视觉偏大
  （参考 StackOverflow 12714923 / 33703966）。
- 当前实现「22pt 画布 + 13.2pt 图形」两头都不在标准区：按钮顶格、图形偏小。

### 步骤

1. 与用户确认"差一点点"的具体方向（偏大/偏小/糊）——取一张真机截图对照系统图标。
2. `paddedStatusImage`：canvas 22→18，glyphRatio 0.6→0.95（图形约 17pt，四周留 0.5pt 安全边）；
   `setIcon` 里 `setSize(16,16)` 与 padding 的重复缩放合并为单一入口。
3. 生成专用 mac 菜单栏资产：扩展 scripts/gen-icon.mjs 输出 18×18 与 36×36 两张纯黑模板 PNG，
   在 systray_darwin.go 用 `NSImageRep` 双 representation 注册（`[image addRepresentation:]`），
   保证 Retina 清晰，替代"ICO 取最大条目"。
4. 菜单项内图标（`SetIcon`/16pt）不动；托盘 tooltip/菜单逻辑不动。

### 关键注意点

- template 图标必须纯黑+alpha（深浅色自动适配），勿用彩色图标当 template。
- `paddedStatusImage` 的 `fromRect:NSZeroRect` 语义 = 整图缩放，改 canvas 后无需改绘制逻辑。
- 无法在本机看效果：参数化（canvas/glyphRatio）后请用户真机确认，一次到位。

---

## 2. mac 日志为空

### 现状与链路

- `logDir = os.UserConfigDir()/dsh-systray/logs`（mac = `~/Library/Application Support/dsh-systray/logs`），
  `initUnifiedLog` 开进程级句柄，`log.SetOutput(appLogWriter{})`，子进程输出走 `newModuleLogWriter`，
  前端日志页 `ReadLogTail` 轮询同一文件。整条链路平台无关。

### 根因候选（按概率排序，需真机取证后锁定）

1. **与开机自启动同源的启动方式问题**（见 §3）：LaunchAgent 直接 exec .app 内裸二进制，缺
   NSBundle/LSUIElement 上下文，进程异常或 HOME 环境异常 → 日志没写到预期位置或进程根本没起来。
2. `initUnifiedLog` 失败**完全静默**（`unifiedFile=nil` 时所有日志丢弃）：logDir 创建失败、
   文件打开失败（权限/TCC）都无提示、无回退。
3. `os.UserConfigDir()` 失败回退 `os.TempDir()` → 日志写进临时目录，用户查 `~/Library` 为空。
4. 用户运行版本早于 v0.7.0（统一日志之前）。
5. 日志文件为空时前端页面无任何提示文案，易被误判为 bug。

### 步骤

1. **取证**（先于任何代码改动）：请用户提供：
   - 日志页显示的路径（log-path 行）；
   - `ls -la ~/Library/Application\ Support/dsh-systray/logs/` 输出；
   - 关于页版本号；
   - `log show --last 10m --predicate 'process == "dsh-systray"'`（崩溃/异常记录）。
2. 加固（无论根因均值得做）：
   - `initUnifiedLog` 失败回退链：UserConfigDir 失败 → `~/Library/Logs/dsh-systray/` → `os.TempDir()/dsh-systray/`；
     仍失败则至少写 stderr（Console 可见）；失败不再静默。
   - 启动首行固定写版本/pid/日志完整路径（用户可对照页面路径与实际文件）。
   - mac 上 `log.SetOutput(io.MultiWriter(appLogWriter{}, os.Stderr))`，Console 双保险诊断。
   - 前端日志页：文件不存在/为空时显示明确提示（含完整路径），替换当前空白页。
3. 与 §3 修复联动后真机验证：双击启动 → 日志页出现 [app] 首行 → `cat` 文件内容一致。

---

## 3. mac 开机自启动失效

### 现状（platform_darwin.go enableAutostart）

- LaunchAgent plist：`RunAtLoad` + ProgramArguments 直接指向 **.app/Contents/MacOS/dsh-systray 裸二进制** + `--autostart`；
- 加载用 `launchctl load`（已废弃 API），错误被忽略；`isAutostartEnabled` 只检查 plist 文件是否存在。

### 根因候选（按概率）

1. **裸 exec bundle 内二进制**：Wails 应用依赖 NSBundle 环境（LSUIElement 生效、WebKit 初始化）；
   launchd 直接 exec 时 bundle 上下文缺失 → 启动即异常/崩溃（登录时无托盘出现）= 用户感知"自启动失效"。
2. **quarantine + 未签名**：release.yml 无 codesign/notarization 步骤，zip 解压带 `com.apple.quarantine`，
   Gatekeeper 会拦截 launchd 启动路径（手动右键"打开"能过，launchd 路径不过）。
3. `launchctl load` 已废弃（macOS 13+ 推荐 `bootstrap`/`bootout`），失败无反馈；
   Ventura+ load 后还需 `kickstart` 才立即运行（影响验证方式）。
4. 自愈逻辑（main.go 347-350）每次启动重写 plist，掩盖真实加载失败状态。

### 修复方案（分层）

- **macOS 13+ 首选**：`SMAppService.mainApp.register()`（System Settings → 登录项，用户可见可管理，
  Apple 官方推荐；需 cgo 调 ServiceManagement.framework，或独立小 cgo 桥接函数）。
- **macOS 11/12 回退**：LaunchAgent 修复：
  1. ProgramArguments 改 `/usr/bin/open -a <bundle路径> --args --autostart`（open 经 LaunchServices 正常起
     .app，LSUIElement 生效；--args 之后的参数会传给应用，保持 --autostart 静默逻辑）；
  2. 加 `LimitLoadToSessionType=Aqua`、`ProcessType=Interactive`；
  3. 加载改 `launchctl bootstrap gui/$(id -u) <plist>`（卸载 `bootout`），检查错误并 showMessageBox 反馈；
  4. plist 模板抽成纯函数 + 单测（避免再手写字符串）。
- **签名/公证**：CI 加 `codesign --force --deep --sign -`（ad-hoc），zip 打包后
  `xattr -cr` 清 quarantine（或 README 指引"首次被拦时右键打开"）。
  彻底根除 quarantine 拦截需 Apple Developer ID 签名 + notarization（付费证书，需用户决策，非本次必需）。

### 验证

- 真机：开启开关 → `launchctl print gui/$(id -u)/com.deepseek.dsh-systray` 状态正常（或系统设置登录项可见）→
  注销重登 → 托盘出现且完全静默（无弹窗，--autostart 生效）。
- CI 只能验证编译通过，行为必须真机验收。

---

## 4. mac 版 app 阻止系统关机/重启

### 根因（高置信）

关机/注销时 loginwindow 向 GUI 应用发 `kAEQuitApplication` → Wails `OnBeforeClose`（darwin 分支）→
`askStopServer()` 弹 osascript `display dialog` → 模态阻塞退出：
- 无人点击时系统等待、超时后强制杀（用户看到"正在等待 dsh-systray 退出/阻止关机"）；
- 关机态下 osascript 可能永不返回（-1）→ `onBeforeClose` 返回 true 主动阻止退出，更糟。

### 修复步骤

1. **systray_darwin.m**：在 `applicationDidFinishLaunching` 注册：
   - `NSWorkspaceWillPowerOffNotification`（关机/重启）
   - `NSWorkspaceSessionDidResignActiveNotification`（注销/切换用户）
   回调经新增 export 函数通知 Go 置 `systemShuttingDown` 标志（复用现有 cgo export 模式）。
2. **Go 侧**：`onBeforeClose`（darwin 分支）与 mQuit 处理器开头检查 `systemShuttingDown` →
   跳过 askStopServer，直接 `keepServerRunning=true`（或 false，按产品策略定）+
   `quitRequested=true` + 放行退出。
3. **SIGTERM/SIGINT 处理**：launchd 注销会对 agent 发 SIGTERM，当前无处理（直接死，服务残留）。
   加 `signal.Notify` → 置 quitting → 触发与 onShutdown 相同的清理路径，不弹窗。
4. （可选增强）自定义 `applicationShouldTerminate` 检查 quit Apple event 的 sender 是否为
   loginwindow，区分"用户 ⌘Q"与"系统退出"——通知方案已覆盖主场景，此步按需。

### 验证

- `osascript -e 'tell application "System Events" to log out'` / 实际重启真机验证：不再出现等待弹窗。
- `osascript -e 'tell app "dsh-systray" to quit'` 仍应弹询问框（用户主动退出行为不变，回归点）。

---

## 通用关键问题点

1. **无法本机验证**：全部 darwin 行为（图标渲染、通知、launchd、关机流程）只能 CI 编译 + 真机验收；
   开发循环依赖用户反馈，每项修完给出"真机验收清单"。
2. **cgo 改动风险**：.m 文件改动在 Windows 上不参与编译，vet 只查 Go 侧；
   .m 语法错误要等 CI macos-latest 才知道 → 一次改完、review 后再推。
3. **发布流程**（沿用约定）：改代码 → 版本化 wails build → 截图重拍（本四项不涉及 UI 页变化，约可跳过）→
   RELEASE_NOTES.md 写 `## v0.7.2` 区块（新增/变更/移除）→ push main → tag → CI Release。
4. **自启动修复的向后兼容**：老用户残留旧 plist（裸 exec 版）→ 启动自愈逻辑必须升级为
   重写为新形态（open -a / SMAppService），否则老用户不修。
5. **测试落点**：plist 模板生成、日志回退链可单测（Go）；NSWorkspace 通知与关机行为无单测手段，
   写进真机验收清单（docs/验证指引）。

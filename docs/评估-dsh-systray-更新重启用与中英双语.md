# 评估：更新取消后按钮重启用 + README/网站/应用 中英双语

> 2026-09-06 · 需求来源：用户提出三项改进的可行性评估（步骤 + 关键问题点）
> 现状代码基准：main @ e5058ae（v0.7.3 之后）

## 0. 结论摘要

| 需求 | 结论 | 建议顺序 |
| --- | --- | --- |
| 1 更新取消后重启用更新按钮 | 现有代码存在 3 个缺口（死事件/取消不生效/按钮状态不重置），修复成本低，建议最先做 | ① |
| 2 README + 网站双语（自动检测默认语言） | 网站可做「浏览器语言优先」自动检测；**GitHub README 无法自动检测**（静态渲染），只能双文件互链。建议网站双语 → README 双文件 | ② |
| 3 应用 UI 双语切换 | 面最大：前端 5 页 + Go 托盘/弹窗/splash 全部硬编码中文。需要统一 i18n 层 + config 落盘 + 系统语言检测，建议最后做并复用需求 2 的术语表 | ③ |

---

## 1. 需求 1：检查更新后用户取消更新 → 重新使能更新按钮（win/mac）

### 1.1 现状与根因

- 关于页 `btn-systray-update` / `btn-harness-update` 在用户确认更新后被置 `disabled = true`
  （`frontend/dist/main.js` L394/L406），确认弹窗点「取消」时提前 return，**这一层没问题**。
- 真正的缺口在「更新进行中用户取消」路径：

  1. **`update:done` 是死事件**：前端 L1292 已监听 `update:done` 并写好重启用逻辑
     （L1295-1299），但全仓库 Go 侧**没有任何一处 emit 该事件**（grep 仅前端引用）。
     即：设计上想恢复按钮，事件从未发出。
  2. **dsh-systray 自身更新取消后按钮不恢复**：取消入口有两个——
     ①前端 splash 视图「取消更新」（`wireSplashCancel` L1307-1311，只调 `CancelUpdate()`
     并隐藏取消按钮）；②关闭进度窗口（`main.go` L492 `splashOnCloseFn` → `cancelActiveUpdate`）。
     两路最终都走 `cancelActiveUpdate()`（`updater.go` L122）→ 下载 ctx 取消 →
     `platform_windows.go` L947/L958/L971 静默 `return`。期间没有任何事件通知前端，
     按钮保持 disabled，且 splash 视图停留在 update 模式（窗口被 `hideMainWindow` 隐藏，
     重开窗口后前端 DOM 状态不重置）。
  3. **Harness 更新根本不可取消**：`registerActiveUpdate` 只在
     `platform_windows.go` L920（dsh-systray 自身更新）登记；
     `runHarnessUpdate`（`updater.go` L903）不登记任何取消句柄，其内部的
     git/npm 子进程也没有 context 传递 → 用户在 harness 更新中点「取消更新」实际是 no-op。
  4. mac 侧 `startUpdateApply`（`platform_darwin.go` L718）走共享的
     `downloadAndApplyUpdate`（`updater.go` L1181），需逐一对齐取消点。

### 1.2 实施步骤（每步含验证）

1. **Go：登记 harness 更新的取消句柄**
   `runHarnessUpdate` 内 `ctx, cancel := context.WithCancel(...)` + `registerActiveUpdate(cancel)`
   + `defer clearActiveUpdate()`；git/npm 调用改 `exec.CommandContext(ctx, ...)`；
   在快照（L946）、安装、重启等阶段间加 `ctx.Err()` 检查点，取消时立即走已有回退
   `rollbackUpdate`（快照备份已存在）或直接停在中点（未动状态时）。
   **验证**：更新到「正在安装依赖」阶段点取消 → 观察进程被终止、回退执行、无残留半安装。
2. **Go：emit `update:done`**
   定义事件负载 `{ ok, canceled, error }`；在以下终点统一发出：
   - `runHarnessUpdate` 成功/失败回退/取消后（`updater.go` L904 起各 return 点）；
   - `startUpdateApply` 取消点（win L947/958/971 + darwin 对应点）发出 `{canceled:true}`；
   - 失败/解压/校验等 return 点发出 `{ok:false,error:...}`。
   **验证**：前端加临时日志，逐路径确认事件到达且 payload 正确。
3. **前端：事件处理补全**
   `update:done` 处理器（L1292）补充：`hideSplash()` + `showSettings()` + 重启用两个
   更新按钮 + `refreshVersions()` + 按 `canceled` 显示「已取消更新」提示；
   `wireSplashCancel` 改为：调 `CancelUpdate()` 后隐藏按钮并把状态文案改为「正在取消…」，
   由 `update:done` 统一收尾（避免双路径各自重置造成竞态）。
   **验证**：①确认更新→下载中点取消→回设置页、按钮可再次点击、再次检查更新可重新走到确认；
   ②harness 更新中点取消→同上；③点「检查更新」按钮（非更新）保持原逻辑不受影响。
4. **边界：不可取消窗口**
   自身更新进入「正在更新程序/替换并重启」阶段（`splash.Update(..., 0.9)` 之后，
   `replaceAndRelaunch` 已启动）后取消应为 no-op 或隐藏取消按钮——进程即将重启，
   此时取消会造成半更新状态。在 Go 进入该阶段时发 `splash:progress` 附带
   `{cancellable:false}`，前端据此隐藏「取消更新」。
   **验证**：解压完成→点击取消→不生效且无异常日志；重启后版本正确。
5. **双平台回归**
   本地 Windows 构建走全流程；mac 侧 `downloadAndApplyUpdate` 取消点逐一同源补齐
   （本机为 Windows，darwin 编译由 CI/用户 mac 验证，先 GOOS=darwin go vet 预检）。
   **验证**：`go vet ./...` + 现有 `updater_test.go` 等测试全绿；win/mac 手工取消各阶段。

### 1.3 关键问题点

- **取消语义**：harness 更新中途 kill 子进程可能留下半改 node_modules/git 状态，
  必须复用现有快照/回退（`snapshotHarness`/`rollbackUpdate`），取消时按「已动状态」分档处理。
- **事件竞态**：多个更新入口（手动/自动 30s/托盘菜单 `checkForUpdatesManual`）共用
  `updateFlowBusy`，emit 时机要保证只对应「本次」更新；自动检查弹窗路径是原生对话框，
  不涉及按钮，勿混入。
- **前端无源码构建链**：改动直接写 `frontend/dist/*`（仓库现状），注意同时维护
  `main.js` 事件名拼写与 Go 侧一致（`update:done`）。
- **不可取消窗口** 见步骤 4，不做会在替换阶段产生半安装风险。

---

## 2. 需求 2：README + 网站 中英双语（自动检测默认语言）

### 2.1 现状

- 网站 `docs/index.html`：单文件中文硬编码（hero/sub/features/notes/footer/轮播气泡
  captions 数组/版本徽章文案约 40+ 串），GitHub Pages 由 `docs/` 直接托管。
- `README.md`：中文单文件；`.github/workflows/release.yml` L175 起有 python 步骤
  **正则替换 README 下载表两行大小**（`| Windows | x64 | ZIP | X.X MB |` 与
  `| macOS | Intel + Apple Silicon | ZIP (.app) | X.X MB |`）并 bot 提交回 main。

### 2.2 方案与步骤

#### 2.2.1 网站（可行自动检测）

1. **提取文案**：把 `index.html` 全部用户可见文案 + `shots` captions 抽成
   `const I18N = { zh: {...}, en: {...} }`，DOM 加 `data-i18n` 属性，渲染时填充。
2. **语言决策链**（优先级从高到低）：
   ① localStorage `dshLang`（用户手动切换后永久记住）
   ② URL 参数 `?lang=zh|en`
   ③ `navigator.languages[0]` / `navigator.language`（zh*→zh，其余→en）
   ④ 兜底默认语言（建议 en，或按产品决策 zh）
3. **切换控件**：右上角（hero 上方）「中 / EN」按钮，≥44px 热区、aria-label、
   当前语言高亮；切换即重渲染全部文案 + `document.documentElement.lang` +
   `<title>`/`meta description`。
4. **验证**：本地起静态服务器，分别用 zh/en 浏览器语言首访（自动）、`?lang=`、
   手动切换+刷新（记忆）、无 localStorage 隐私模式（走浏览器语言）。

#### 2.2.2 README（GitHub 无法自动检测，采用双文件互链）

1. 保留 `README.md`（中文），新增 `README.en.md`（英文全量翻译，含下载表/功能/配置
   JSON 注释块中文化为英文）。两文件顶部互链徽章：
   `[English](README.en.md) · [简体中文](README.md)`。
2. **CI 同步改造**：release.yml 的 sizes 正则步骤需同时处理两个文件
   （英文行 `| Windows | x64 | ZIP | X.X MB |` 结构保持一致则正则兼容，注意
   `Download Windows` 链接文案不同；建议把行模式改为按「平台列 + 列尾数字」匹配，
   两个文件各自替换，python 循环 `["README.md","README.en.md"]`）。
3. **验证**：发版后 bot commit 正确更新两个文件的 MB 值；GitHub 页面两个 README
   渲染正常、互链可跳、锚点（#功能 等标题）不因翻译破坏内部引用（如有）。

### 2.3 关键问题点

- **IP 地理方案不推荐作为主判据**：GitHub Pages 无服务端，IP→语言只能调第三方
  geo API（ipapi/ipinfo 等），引入隐私合规、限流、中国大陆可达性差等问题；
  若确需「IP 兜底」仅作为浏览器语言缺省时的可选信号，失败静默降级，
  并加 `rel="dns-prefetch"` 与超时。浏览器 `Accept-Language`/`navigator.language`
  是客户端唯一零依赖可靠来源。
- **截图是中文 UI**：轮播 screenshots（general/about/logs/export/import）内置中文界面，
  英文站显示中文截图属可接受过渡态；彻底解决需 shotmode 重拍英文截图（后续需求）。
- **SEO**：单页运行时切换语言，爬虫只收录默认语言文案；如需英文索引需静态化双页
  （如 `docs/en/` 或 `?lang=en` 的预渲染），现阶段成本高，标记为遗留。
- **保持既有资产稳定**：`docs/icon.svg`、下载直链、`api.github.com` 版本徽章、
  `data-reveal` 动画、轮播 lazy 加载逻辑不动，只做文案层。
- **release.yml 正则不能悄悄失效**：改 README 表格结构会直接破坏发版 CI 的
  sizes 同步（该步骤失败会导致 bot 提交缺失），改造后必须发一版验证。

---

## 3. 需求 3：应用（设置窗口/托盘/弹窗）中英双语

### 3.1 现状面（全部硬编码中文）

| 层面 | 位置 | 量级 |
| --- | --- | --- |
| 设置窗口 | `frontend/dist/index.html`（5 页：常规/关于/日志/导出/导入）+ `main.js` 全部动态文案（弹层/提示/状态/插件行） | 数百条 |
| 托盘菜单 | `main.go` L847-856（服务启动中…/打开 Web UI/设置/退出 + tooltip），`serviceStatusText()` 状态串 | 少量 |
| splash 进度 | Go 全仓 `startSplash("…")` / `splash.Update("…")`（启动/更新/重置/插件/导入导出） | 数十处 |
| 原生弹窗 | `showMessageBox(...)`、`askUpdateDialog`/`askUpdateHarness`/`askCancelUpdate`/`askStopServer`/`askRestartServiceMac` 等 win/mac 双实现 | 数十处 |

### 3.2 方案

- **语言配置**：`config.json` 新增 `"language": "auto|zh|en"`（缺省 auto）；
  常规页加「语言」下拉（跟随系统/中文/English）。切换 → 绑定 `SetLanguage` 写配置 →
  Go `EventsEmit("lang:changed")` → 前端整体重渲染 + Go 重建托盘菜单文案。
- **系统语言检测（auto）**：Windows `GetUserDefaultUILanguage`（syscall，映射 zh*→zh、
  其余→en）；macOS `defaults read -g AppleLanguages` 或 NSLocale 取首选。
- **Go i18n 层**：新建 `i18n.go`：`T(key) string` + 两套 map；所有面向用户的
  Go 字符串（托盘/splash/messageBox/ask 系列/服务状态文案）迁入；
  日志（`log.Printf`/unified log）**保持中文不译**（诊断面，且避免历史日志语言混杂）。
- **前端 i18n 层**：`main.js` 加 `t(key)` + `data-i18n` 批量替换；启动时从
  `GetConfig` 读语言；`lang:changed` 触发全量重渲染（含当前页面、插件列表、导入行）。
- **术语表共享**：需求 2/3 用同一份 zh↔en 词汇表（轻量/可靠/可迁移、检查更新、
  恢复、导出导入等），存 `docs/i18n-glossary.md` 供三端统一。

### 3.3 实施步骤（每步含验证）

1. **配置 + 检测 + 绑定**：ConfigInfo 加 language 字段、GetConfig/SetConfig、
   `SetLanguage` 绑定；启动早段读配置（先于托盘菜单构建）。
   **验证**：改 config.json 重启 → 托盘菜单语言变化；下拉切换 → 配置落盘。
2. **前端提取**：index.html/main.js 字符串全部入 dict（先 zh 兜底 = 现状文案），
   常规页加语言下拉；`lang:changed` 重渲染。
   **验证**：EN 下逐页过一遍无残留中文（grep 中文兜底）；英文文案溢出不破版
   （按钮/菜单自适应已有基础，逐页检查）。
3. **Go 提取**：托盘菜单 + serviceStatusText → T()；startSplash/showMessageBox/ask*
   分批替换（每批一次全量测试）。
   **验证**：`go vet ./...`、现有测试全绿；win 本地走「检查更新→取消→弹窗」双语各一遍。
4. **双平台回归 + 截图**：shotmode 重拍 docs/shots（英文版另存 `docs/shots-en/`，
   网站英文页引用英文截图，回补需求 2 遗留）。
   **验证**：win + mac（用户实机）走启动/更新/重置/导入导出双语言冒烟。

### 3.4 关键问题点

- **体量最大、易漏**：前端数百条 + Go 数十处；先做枚举清单（grep 中文正则）再翻译，
  避免漏翻半中半英。
- **初始化顺序**：托盘菜单在配置读取**之前**构建 → 语言必须更早读取；appCtx 未就绪时
  EventsEmit 失效（启动早期 splash 文案走启动时语言，切换语言不影响已完成的历史串）。
- **英文宽度溢出**：英文普遍更长（"Check for updates"/"Restore session, plugins and
  directories"），需逐页检查 flex/截断；托盘菜单已有宽度自适应，验证英文 label。
- **版本/命令/路径不译**：`pnpm dsh web`、`config.json`、版本号、日志原文保持原样。
- **重启语义**：托盘菜单重建在运行中即可（SetTitle 更新），无需重启；窗口内即时切换。
- **回退**：i18n dict 以 zh 为 key 回退，翻译缺失自动落 zh，避免空白文案。
- **前端直接改 dist**：无源码构建链，与需求 1 同——维护时注意 dist 即源。

---

## 4. 总体实施顺序与测试清单

1. 需求 1（小改，独立收益）→ 2（网站 i18n → README 双文件 + CI 改造）→ 3（app i18n，复用术语表）。
2. 测试清单：需求 1 各取消阶段 × 双平台；需求 2 浏览器语言/`?lang`/localStorage/CI sizes 同步；
   需求 3 双语言全页面走查 + grep 中文残留 + shotmode 重拍 + win/mac 冒烟。
3. 遗留/待用户决策：
   - README 默认语言（zh 保留 vs 改为 en 默认）；
   - 网站是否引入 IP 地理第三方（建议否）；
   - app 语言默认「跟随系统」还是首次启动询问；
   - 英文版截图是否本期制作（影响网站英文页观感）。

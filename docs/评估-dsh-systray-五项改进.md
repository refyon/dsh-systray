# dsh-systray 五项改进 · 评估（步骤与关键问题点）

日期：2026-09-05 · 范围：需求 1-5 · 代码基线：v0.7.0（commit 955f882 之后）

总体判断：1/2/4 为 UI+交互改造（中低风险，可一批交付）；3/5 为版本一致性与恢复健壮性（高风险，5 已从
`D:\Downloads\new_device.log` 拿到可复现根因）。

---

## 需求 1：常规页「重启后台服务」旁加「打开 Web UI」按钮

**现状**：`frontend/dist/index.html` L57 仅有 `btn-restart`；Go 侧 `OpenWebUI()` 绑定已存在
（`app.go` L902，走 `resolveRunningService()` 实际端口，托盘已在用），前端零改动即可调用。

**步骤**
1. `index.html` L57 改为按钮组：
   `<div class="btn-group"><button id="btn-restart" class="btn btn-primary">重启后台服务</button>
   <button id="btn-open-webui" class="btn btn-ghost">打开 Web UI</button></div>`
2. `style.css` 加 `.btn-group { display: flex; gap: var(--sp-2); }`（8pt 网格；两按钮同高，
   `.btn` 的 `min-height: 40px` 已保证）。
3. `main.js`：`btn-open-webui` → `bindings().OpenWebUI()`；在 `refreshService()` 回调里按服务
   运行态同步 disabled（未运行时禁用，与 `svc-sub`「服务就绪后可打开 Web UI」文案联动）。

**关键点**
- 层级：重启=主操作（primary）、打开=次级（ghost）——符合 DESIGN.md「一屏一个主操作」。
- 端口改动未重启的窗口期，`OpenWebUI` 已按实际端口打开，无需新逻辑。
- 服务未运行时禁用按钮而非点击静默无反应（DESIGN.md 要求完整状态）。
- `card-row` 是 flex 布局，两按钮必须包在 `.btn-group` 内，避免挤压左侧文案；窄窗口考虑 `flex-wrap`。

**验证**：构建后手动点测；`DSH_SYSTRAY_SHOT_PAGE` shotmode 截图回归 `docs/shots`（无 UI 变化页不重拍）。

---

## 需求 2：本地插件「更新」= 选路径 → 比较版本 → 差异确认覆盖 / 已是最新

**现状**：`classifyPluginSpec`（`plugin_update.go` L141）把 `file:/link:/workspace:` 判为
`canUpdate=false`（原因「本地路径安装，无远程来源，无法更新」）；前端（`main.js` L439-449）据此把
「更新」按钮渲染为灰色禁用。改造目标就是为这类来源开放一个**本地路径更新流程**。

**步骤**
1. Go 新绑定 `PickLocalPluginPath(id string)`：
   - `wruntime.OpenDirectoryDialog`（owner=主窗口，防重复弹窗用 atomic 模式，同 `PickHarnessDir`）；
   - 校验所选目录 `package.json` 的 `name` 与插件名一致，不一致报「所选目录不是插件 <name>」；
   - 读取其 `version`，与已装版本 `compareVersions` 比较，返回 `{path, version, newer}`。
2. 前端（`renderPluginRow`/事件委托）：`source ∈ {file, link, workspace}` 的行，「更新」按钮变可用；
   点击 → `PickLocalPluginPath` → 比较结果：
   - 相同：`setNote("已经是最新（vX）")`；
   - 更新：`confirmDialog("检测到新版本 vX（当前 vY），是否覆盖更新？")` → 确认后 `ApplyLocalPluginUpdate`。
3. Go `ApplyLocalPluginUpdate(id, newPath)`：复用现有更新管线
   （`killServer → snapshotPluginProfile → 改 package.json 依赖 spec 为 link:<新路径> → pnpm install
   → restartAndVerifyHealing → 失败 restorePluginProfileSnapshot 回退并报错 → 成功 cleanup + plugins:changed 事件`）。
   多 profile 合并行需逐一目录执行（同 `StartPluginUpdate` 的 locs 循环模式）。
4. `wails generate module` 重新生成 `frontend/wailsjs` 绑定。

**关键点**
- 版本相同但内容不同：「有差异才提示覆盖」建议以**版本号为主判据**（简单优先）；同版本强制覆盖
  属少数场景，可作为后续增强（比对 mtime/hash），本轮不实现。
- `link:`/`file:` spec 在 Windows pnpm 下的写法需实测（正斜杠、与已装 spec 形态保持一致）；选的是
  目录而非 tgz。
- 无 `version` 字段的本地包：给「无法判定版本，仍可覆盖安装」提示，不硬报错。
- 更新后服务会重启，健康校验失败自动回退——机制已存在，直接复用。
- 所选目录被后续移动/删除会导致依赖悬空：更新后 spec 持久化到 package.json，属正常 pnpm 语义，
  文案上注明「本地路径依赖，移动目录后需重新选择」。

**验证**：`classifyPluginSpec`/比较逻辑单测 + fixture 本地插件两版本真机手测 + 全量 `go test`。

---

## 需求 3：重置（0.1.2-rc.1）与检查更新（0.1.1-rc.2）版本不一致

**根因**：两条路径用**不同数据源**——
- 重置（npm 形态）走 `fetchNpmResetTarget`（`updater.go` L613）：查 npm registry 全部已发布版本，
  无稳定版回退最新预发布 → **0.1.2-rc.1**（日志 L3255 实证「npm 最新发布（预发布） v0.1.2-rc.1」）；
- 检查更新走 `fetchHarnessLatestTags`（L415）：GitHub Releases API。该列表大概率缺 0.1.2-rc.1
  （0.1.2-rc.1 已发 npm 但未建 GitHub Release/仅 tag），故 newest 落到 **0.1.1-rc.2**；且通道关闭
  时稳定版过滤为空，note 显示「仓库暂无稳定 Release，最新可用为 0.1.1-rc.2（预发布）」。
- 注释自述「GitHub Release 常先于 npm」（updater.go L406），0.1.2-rc.1 是反例——npm 反而领先。

**步骤**
1. 联网验证（本沙箱网络被断）：对比
   `api.github.com/repos/deepseek-ai/deepseek-harness/releases` 与
   `registry.npmjs.org/@deepseek-ai/dsh` 的版本集，确认 0.1.2-rc.1 是否只在 npm 侧。
2. 抽统一解析器 `resolveInstallableHarnessTarget(shape)`：npm 形态 → npm 已发布版本
   （复用 `npmHarnessPublishedVersions`/`fetchNpmResetTarget` 逻辑）；源码形态 → GitHub Releases。
3. `queryHarnessUpdate`（检查更新）在 npm 形态下改走 npm 源，与重置共用同一解析函数——
   两处版本永远一致；安装前保留 `npmHarnessVersionAvailable` 预检兜底。
4. 通道语义统一：无稳定版时两处口径一致（「最新可用 0.1.2-rc.1（预发布）」），
   检查更新的更新按钮在 npm 形态下照常可用（精确版本安装，不走 @latest）。
5. `hint-harness` note 文案与 `ver-harness` 展示同步刷新。

**关键点**
- **不可**把重置简单改回 GitHub 源：npm 形态安装必须用 npm 已发布版本，否则
  `ERR_PNPM_NO_MATCHING_VERSION`（0.1.3-alpha.1 教训，已入项目记忆）。
- npm dist-tag `latest` 不含预发布：安装必须精确版本（现有做法保留）。
- 源码形态保持 GitHub 源不动；两种形态差异用纯函数单测覆盖（注入 tags/versions 列表）。
- `isNpmHarnessReady` 依赖 harnessDir 状态，query 时目录未就绪要有兜底（保持现有 cur 回填行为）。

**验证**：`updater_test.go` 增加「npm 形态、GitHub 缺版本」用例；真机两处按钮对比显示一致。

---

## 需求 4：恢复操作禁用/取消/回退/失败态

**现状**
- `ApplyRestore`（`app.go` L806）立即返回，内部 goroutine 执行；前端 `await` 结束 1.2s 后按钮即恢复
  可点（`main.js` L813），恢复仍在后台跑——存在并行点击风险（后端 pendingRestore 单槽会跳过第二次，
  但 UI 不表达）。
- 失败态已有（`import:done` error → imp-hint）；plugins kind 已有 `.importbak` 快照 + 健康校验回退。
- **无取消机制**；sessions/files kind 无 profile 快照（仅 restoreItem 内顶层改名备份，且
  overwrite=false 时无备份）。

**步骤**
1. Go：新增 restoreJob 状态（running/kind/cancel 原子标志 + 互斥）；`ApplyRestore` 运行中再调用
   直接返回错误「已有恢复进行中」；新绑定 `CancelRestore()`。
2. 取消注入提取循环：`goExtractZip` 每文件循环检查 cancel；7z 外部命令改 `exec.CommandContext`
   （取消时 Windows 需 `taskkill /T` 杀进程树，否则继续写盘）。
3. 取消路径：停止提取 → 删除半成品（记录已提取顶层条目逐个 RemoveAll）→ 恢复 backups →
   plugins kind 走 `rollbackImportProfiles` → `resumeServiceAfterRestore` →
   emit `import:done{canceled:true}`。
4. sessions/files kind 补回退（当前没有）：推荐方案 A——对将被覆盖的顶层目录做 `.dshbak` 改名备份
   （同卷 rename 秒级，与 restoreItem 的 backups 机制合并），overwrite=false 时新建目录在取消时删除；
   方案 B（先解压到临时 staging、全部成功再 rename 提交）更干净但改动大，不选。
5. 前端：全局 `inRestore` 标志——进行中禁用**所有**行「恢复」按钮；`imp-progress` 区加「取消恢复」
   按钮（btn-ghost）；`import:done` 区分 canceled/error/ok 三态展示（取消=「已取消，已回退到恢复前
   状态」，失败=「恢复失败：<原因>」，失败文案用 --danger 色）。
6. 边界：取消落在 PreviewRestore 与 ApplyRestore 之间 → ApplyRestore 检测 cancel 直接 no-op；
   恢复已完成但事件未送达 → 幂等忽略。

**关键点**
- 回退可靠性优先于取消速度：先停写再回滚，回滚期间取消按钮 disabled + 「正在回退…」文案。
- 取消必须成对调用 `resumeServiceAfterRestore`，避免服务被停死。
- sessions 顶层目录可能很大，快照用同卷 rename 而非复制（项目既有模式）。
- `pendingRestore` 单槽与恢复 goroutine 的生命周期要统一管理，避免取消竞态（锁保护）。

**验证**：`import_guard_test.go` 增取消用例（小 zip 模拟中途取消）；真机用大导出包点取消观察回退。

---

## 需求 5：0.1.2-rc.1 恢复旧版插件失败 · 日志根因与提高成功率方案

**根因（`D:\Downloads\new_device.log` 实证，时间线 10:57-11:02）**
1. **决定性错误——本地链接依赖跨机失效**：导出包携带源机绝对路径依赖
   `"dsh-ui-taste": "link:D:/agent-env/qtz/plugins/dsh-ui-taste"`，恢复时
   `registerRestoredPlugins` 把源 profile 的 dependencies 合并写回新机 profile
   （C:\Users\work\.dsh\profiles\web），新机无此路径 → pnpm install 报
   `ERR_PNPM_LINKED_PKG_DIR_NOT_FOUND`（L3242/3323/3489/3670 反复出现）→
   reconcile 失败（L3328/3670）→ heal 再失败（L3494/3836）。
2. **叠加问题——旧插件与新核心 API 不兼容**：恢复的旧版插件在 0.1.2-rc.1 核心下加载失败：
   restrict-discipline/dsh-ui-taste 缺 `@deepseek-ai/dsh-settings` 的 `settingsNamespace` 导出、
   codegraph 插件缺 `@deepseek-ai/dsh-llm` 的 `assertNever`（L3339/3348/3357…）→ 服务启动不健康
   → `finishPluginImport` 判定失败自动回退（L3653/3995）→ 恢复失败（L3658）。
3. **环境性**：网络抖动 `UND_ERR_DESTROYED`（L3245-3247）进一步削弱 reconcile 可靠性。

**提高成功率方案（按优先级）**
1. **导出侧消毒（根治 1）**：导出时扫描 profile package.json，把指向绝对路径的
   `link:/file:/workspace:` 依赖改写为 `npm:<name>@<version>`（version 从被链接包 package.json 读取）；
   取不到版本则从 manifest 剔除并在导出结果警告「已略过本地链接依赖 X」。
2. **恢复侧预检+自修复（兜底 1）**：`registerRestoredPlugins` 合并前做同样改写（兼容旧导出包）；
   `PreviewRestore` 把「将改写 N 个本地链接依赖」计入 preview 文案；reconcile 失败后解析 pnpm 输出
   中的 `ERR_PNPM_LINKED_PKG_DIR_NOT_FOUND`/`ERR_PNPM_NO_MATCHING_VERSION` → 摘除/改写问题依赖 →
   重试 1 次（当前 reconcile 失败只记日志不修复）。
3. **版本兼容修复（缓解 2）**：`finishPluginImport` 健康失败且 `parseBootLogSuspects` 有可疑插件时，
   不直接回退——先尝试一轮目标修复：把可疑插件 `pnpm update` 到最新兼容版（或按 harness 版本对齐
   `@deepseek-ai/*` 家族）→ 再验证 → 仍失败才回退；失败提示列出可疑插件与缺失导出（现文案只说
   「版本/插件不兼容」）。
4. **网络韧性**：reconcile/install 加 `--prefer-offline` + fetch-retries/镜像回退。
5. **失败信息提升**：恢复失败文案带首个失败依赖名与原因（从 pnpm stderr 提取），用户可自助判断。

**关键点**
- 消毒必须双向兼容旧导出包（zip 内 manifest 也是旧格式）。
- 方案 3 的「自动升级可疑插件」改变用户意图（恢复=要旧版本）——建议做成「重试并尝试修复」按钮
  或弹窗询问，而非静默执行。
- link: 依赖在源机确实可用（开发态），消毒只发生在导出/恢复路径，不动运行态环境。
- 本机无 new_device 环境复现，验证靠单测（注入旧 manifest）+ 用户在真机用同一导出包重试。

**验证**：sanitize/rewrite/reconcile-retry 单测；真机重试同一导出包，恢复成功或给出明确失败原因。

---

## 实施落地（2026-09-05，已编译未提交）

五项全部实施并 `go test ./...` 全绿、`wails build -skipbindings` 编译通过（dev 版 exe 已换入 dist）。与上述评估的方案差异：

- **需求2**：新增绑定 `PickLocalPluginPath` / `ApplyLocalPluginUpdate`（plugin_update.go：`packageMeta`/`localPickRelation`/`localLinkSpec`/`setProfileDepSpec`/`runLocalPluginUpdate`），本地来源行的「更新…」按钮走选目录 → 比较（same=newer=older=unknown）→ 确认覆盖；执行复用远程更新的快照/回退/LKG 管线。
- **需求4**：`importJob` 串行槽 + `CancelRestore` 绑定；取消检查点覆盖解压循环（`zipExtractStop`/`goExtractZipStop`，7z 走进程 kill）与 pnpm 对齐前（对齐中取消也回退）；`import:done` 增 canceled/note 字段，前端禁用全部恢复按钮 + 取消按钮 + 三态文案。
- **需求5**：采用「恢复侧存在性改写」而非导出侧消毒（避免同机开发态恢复把 link 误改 npm 声明）：`sanitizeProfileLocalDeps`（缺失路径 → 有已装版本改写 `npm:name@version`，无版本移除依赖+stripBundleEntry）；`reconcileProfileDepsRepair` 失败时解析 pnpm 输出（No matching version / ERR_PNPM_LINKED_PKG_DIR_NOT_FOUND）摘除问题依赖重试一次；失败/成功文案带说明。**未实现**「可疑插件自动升级」（版本错配跨核心升级时恢复仍会回退——保留为后续可选项，当前失败提示会列出可疑插件）。
- **需求3**：`queryHarnessUpdate` npm 形态改走 `fetchNpmResetTarget`（与重置同源），GitHub Release 缺失 0.1.2-rc.1 时检查更新也能看到 0.1.2-rc.1；源码形态维持 GitHub。
- 前端直接编辑 `frontend/dist/*`（无源码）；新增测试 `restore_sanitize_test.go`（sanitize/drop/parse/relation/spec 归一）。

## 共同注意事项（构建 / 测试 / 发布）

- 新增 Go 导出方法后必须 `wails generate module` 重新生成 `frontend/wailsjs` 绑定；
  构建用 `build.ps1`（`wails generate module + build -skipbindings`，项目既有规则）。
- 前端无源码，直接编辑 `frontend/dist/*`（main.js / index.html / style.css）。
- 全部 `go test` 绿；UI 变化用 shotmode 截图回归 `docs/shots`，无变化页不重拍。
- **真机 e2e 警示**：本沙箱 3080 是会话宿主，`killServer` 会杀本会话——恢复/重置/更新联调必须在
  另一台机器或换端口进行（项目记忆既有结论）。
- 发布：功能新增 → 版本 bump v0.8.0；push main + tag → CI 自动构建三平台 + SHA256SUMS + README 同步。
- 建议分两批提交：批次 A（需求 1/2/4，UI+交互）；批次 B（需求 3/5，版本一致性+恢复健壮性，
  改动面大、回归风险高，单独验证后发布）。

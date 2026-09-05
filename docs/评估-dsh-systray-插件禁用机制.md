# dsh-systray · 插件不兼容自动禁用机制 · 评估（步骤与关键问题点）

日期：2026-09-05 · 需求：更新 harness 后若因插件不兼容启动失败 → 禁用该插件；关于页保留记录并显示
禁用状态与原因；允许检查更新；更新后仍不兼容则继续禁用（隐含：更新后兼容则自动重新启用）。

## 1. 需求拆解与语义确认

| 行为 | 语义 | 实现要点 |
| --- | --- | --- |
| 「禁用」 | 保留 package.json 依赖与 node_modules 文件，仅从 `dsh.profile.bundles`（激活清单）移除 | 复用/扩展 `stripBundleEntry`（exportimport.go 已有） |
| 「关于页保留记录」 | 插件行数据源是 profile `dependencies` 枚举（`buildPluginRows`），不删依赖行即自然保留 | `PluginRow` 增 `Disabled`/`DisableReason` 字段 |
| 「显示禁用状态及原因」 | 行内徽标「已禁用」+ 小号原因（启动日志的加载错误摘要） | 原因来自 `parseBootLogSuspects` 命中的错误行 |
| 「允许检查更新」 | 禁用行仍渲染「检查更新/更新」按钮 | `buildPluginRows` 不过滤禁用行 |
| 「更新后依旧不兼容则继续禁用」 | 插件更新成功但启动健康校验仍失败且点名该插件 → 保留新版本、维持禁用、刷新原因 | `runPluginUpdate` 失败分支改造 |
| （隐含）更新后兼容 | 健康通过且该插件此前禁用 → 重新加入 bundles、清除禁用记录 | `runPluginUpdate` 成功分支改造 |

## 2. 现状与可复用件（已核实代码）

- 启动健康校验：`restartAndVerifyServer` → `rotateServerLog`（每次启动独立成档）→ `verifyServerBoot`
  （追加段扫描 `harnessBootErrorMarkers`）。失败后当前日志 = 本次启动现场，
  `parseBootLogSuspects(0)`（service_guard.go）可拿到可疑插件名（load/activate/resolve/patch 四类正则）。
- harness 更新：`runHarnessUpdate`（updater.go）失败即 `rollbackUpdate`（仅回退 harness 目录，
  `restoreHarnessSnapshot`，**不涉及 profile**）→ 禁用动作改的是 profile，回退路径需自行还原。
- 插件更新：`runPluginUpdate`（plugin_update.go）快照（package.json/pnpm-lock + node_modules 改名）
  → 安装 → `restartAndVerifyServer` → 失败 `rollbackPluginUpdate` / 成功 `promoteProfileLkg`。
- LKG 冷启动回退：`tryBootRollback` 同时恢复 harness + 各 profile LKG——**禁用成功后必须
  promote 受影响 profile 的 LKG**，否则下次冷启动失败会用「旧 profile LKG（含插件）+ 新 harness」
  回退，再次不兼容。
- 官方包过滤：`isOfficialHarnessPkg`（harness_reset.go）；bundles 激活清单语义来自
  `registerRestoredPlugins`/`mergePluginConfigIntoProfile`（exportimport.go）。
- `parseBootLogSuspects` 有既有单测（service_guard_test.go）——保持原函数不动，另加带原因版本。

## 3. 方案设计（三个关键决策）

### 决策 A：禁用状态的存储位置 —— 推荐 profile package.json 新字段

在 profile 的 `package.json` 增加 `dsh.profile.disabledPlugins: {"<name>": "<原因>"}`（pnpm/npm 忽略
未知字段，harness 只读自身 schema，风险低）：

- 优点：与 bundles 同文件原子读写；**随导出/恢复流转**（跨机恢复后禁用态仍在）；
  关于页直接从 profile 读取，无第二数据源同步问题。
- 备选（不推荐）：systray 配置目录独立 record 文件——跨 profile 汇总容易，但导出/恢复不携带，
  恢复时 `registerRestoredPlugins` 会把禁用插件重新写回 bundles（需要额外的联动补丁），且
  与 profile 状态两处真相易漂移。
- 联动要求（无论选哪种）：
  - `mergePluginConfigIntoProfile` 合并 bundles 时**跳过 disabled 名单中的插件**（否则恢复会重新激活）；
  - `exportPlugins` 增加 `Disabled map[string]string` 随 manifest 携带；
  - `sanitizeProfileLocalDeps`/`dropProfileDependency` 移除依赖时同步清 `disabledPlugins` 条目；
  - `ResetHarness(clearPlugins)` 与 `RemovePlugin` 成功后清除对应条目（依赖已不存在）。

### 决策 B：禁用时机与流程 —— 只在「有嫌疑用户插件」时介入，单轮禁用，不级联

`runHarnessUpdate` 失败分支改造（插在 `rollbackUpdate` 之前）：
1. `parseBootLogSuspects(0)` → 过滤：非官方包（`isOfficialHarnessPkg`）且存在于 `buildPluginRows`
   （依赖枚举）→ 嫌疑用户插件列表。
2. 列表为空（核心故障/官方包故障）→ 走原回退路径，不做任何禁用。
3. 非空 → 对受影响 profile（嫌疑插件的 `row.Locs`）快照 `package.json`（轻量备份，只需包文件；
   用现有 `.dshbak` 后缀，不搬 node_modules）→ 逐个禁用：strip bundles + 写
   `disabledPlugins[name]=<错误行摘要>` → `restartAndVerifyServer`：
   - 成功：`promoteProfileLkg`（受影响 profile）→ `promoteHarnessLkg(prev)` → 清理 profile 快照
     → 弹窗「已更新到 vX；插件 A、B 与新版不兼容已自动禁用，可在关于页检查更新后重试」；
   - 仍失败：恢复 profile 快照（**重新启用**，禁用只对「新 harness 无法承受的插件」成立）→
     `rollbackUpdate`（原有整体回退，不留禁用记录）。
4. 刻意**不做多轮级联禁用**：第一轮禁完仍失败即回退——防止「禁光插件后成功」掩盖核心故障；
   逐插件二分定位列为后续增强。

### 决策 C：插件更新与禁用态的互操作

`runPluginUpdate` 两处分支：
- 失败分支：`restartAndVerifyServer` 失败且 suspects 含该插件名（或该插件本就禁用）→
  **保留新版本**：清理快照（不回滚）、维持/更新 `disabledPlugins` 记录（原因+新版本号）、
  提示「更新后仍与新版本不兼容，继续保持禁用」；其余失败原因 → 原回退路径。
- 成功分支：该插件在 `disabledPlugins` → 重新加入 bundles、清除记录、提示「已更新并重新启用」。
- 时序注意：禁用态插件的快照包含「禁用态 package.json」——按原路径回退时自动保持禁用，无需特判。
- （推荐可选项）新增 `EnablePlugin(id)` 手动重新启用绑定：用户自行修好环境或误禁用时可恢复；
  不加则用户只能靠「更新成功」或删除重装，恢复路径过窄。

## 4. 分步实施步骤

1. **profile 工具函数**（exportimport.go / plugin_update.go）：`profileDisabledPlugins(dir) map[string]string`、
   `setPluginDisabled(dir, name, reason)`（strip bundles + 写字段）、`setPluginEnabled(dir, name)`
   （追加 bundle 条目 + 清字段）、`appendBundleEntry(root, name)`（与既有 `stripBundleEntry` 对称）。
2. **嫌疑带原因解析**（service_guard.go）：新增 `parseBootSuspectReasons(offset) map[string]string`
   （激活失败行 `name: Error...` 取错误文本；其余类别用兜底文案「启动日志存在加载错误（与当前版本不兼容）」）；
   保持 `parseBootLogSuspects` 原样（既有测试不动）。新增 `suspectUserPlugins()`：
   嫌疑名 ∩ 用户插件行（过滤官方包）。
3. **harness 更新接入**（updater.go `runHarnessUpdate`）：按决策 B 在「重启健康校验失败」处插
   禁用尝试分支；维护 profile package.json 快照的备份/恢复/清理；成功后 LKG 双 promote。
4. **插件更新接入**（plugin_update.go `runPluginUpdate`）：按决策 C 改造失败/成功分支。
5. **行数据与 UI**（plugin_update.go + frontend/dist/*）：
   - `PluginRow` 增 `Disabled bool` / `DisableReason string`（`buildPluginRows` 从 profile 读取）；
   - `main.js renderPluginRow`：禁用行加「已禁用」徽标（危险色系小字）+ 原因行（截断 80 字）；
     「检查更新」按钮照常渲染；`doPluginUpdate` 确认弹窗文案区分禁用态
     （「更新成功后将自动重新启用；仍不兼容则继续保持禁用」）；
   - 成功后 `plugins:changed` 刷新（现有机制），行状态随新数据更新。
6. **导出/恢复/重置/删除联动**（exportimport.go / harness_reset.go / plugin_update.go）：
   `exportPlugins.Disabled` 字段；`mergePluginConfigIntoProfile` 跳过禁用名 bundles；
   `RemovePlugin`/`ResetHarness` 清 `disabledPlugins` 相应条目。
7. **测试**：纯函数单测——禁用/启用往返（bundles + disabled 映射一致）、suspects 过滤官方包、
   原因解析、`exportPlugins` 往返、merge 跳过禁用 bundles、快照恢复后禁用态还原。
8. **构建**：`wails generate module` + `build.ps1`（新增绑定 `EnablePlugin` 时必做）；
   真机 e2e：另机/换端口（本沙箱 3080 是会话宿主，`killServer` 会杀本会话）。

## 5. 关键问题点

- **故障归因可靠性**：`parseBootLogSuspects` 的命中名可能包含官方包（代码注释明言不过滤官方），
  必须过滤，否则会把官方 bundle 当用户插件「禁用」而静默失败；suspect 名与依赖名的拼写需精确匹配
  （load 行逗号分隔、scoped 包名带 `/`）。
- **回退一致性**：禁用改的是 profile；`rollbackUpdate` 只回退 harness 目录——禁用尝试失败时必须
  先恢复 profile 快照再回退，否则「harness 回到旧版但插件仍被禁用」。
- **LKG 一致性**：禁用成功必须 promote 受影响 profile LKG（顺序：先 profile 后 harness），
  否则 `tryBootRollback` 冷启动回退会拼出「新 harness + 旧插件激活清单」再次失败。
- **健康校验窗口**：禁用后重启用 `restartAndVerifyServer`（10s 健康窗口 + 日志轮转），
  不能用 `serverResponding` 简判——加载错误晚于 HTTP 就绪刷出（代码注释已有教训）。
- **禁用≠删除**：`buildPluginRows` 按依赖枚举（保留行）、`collectPluginClosure` 按依赖打包
  （导出仍带文件）——两处都不需要改；但要防止未来「按 bundles 枚举」的改动破坏该前提。
- **恢复导入的激活陷阱**：`mergePluginConfigIntoProfile` 现在无条件合并 bundles，
  不改的话恢复会重新激活禁用插件（决策 A 联动第 1 条）。
- **更新失败分支判定次序**：先判「suspects 含该插件」再走「保留新版本+继续禁用」，
  否则正常失败（网络/构建）也会被误判为兼容性问题而留下禁用的新版本。
- **多个嫌疑插件**：一次禁用全部再验证；不逐一分诊（防禁光插件假成功）。弹窗/日志必须列出
  全部被禁用插件与原因，用户可自助删除/更新。
- **版本回滚后状态清理**：回退 harness 时禁用的插件全部自动恢复（快照恢复），
  不写 `disabledPlugins`；只有「保留新 harness」才落禁用记录——保证记录语义=「对当前 harness 不兼容」。
- **用户体验**：禁用提示用用户语言（不说 cordis/loader）；关于页徽标不喧宾夺主
  （DESIGN.md：一屏一主操作、危险色克制）。

## 6. 验证方案

- 单测：第 4 步第 7 条清单，`go test ./...` 全绿。
- 真机 e2e（独立环境）：准备一个与目标 harness 核心不兼容的旧插件 fixture →
  更新 harness → 观察：启动失败 → 插件自动禁用 → 关于页显示「已禁用+原因」→
  检查更新并更新为兼容版 → 自动重新启用；换一个不可修复的旧版 → 更新后仍禁用。
  反向用例：禁用尝试后仍失败 → harness 整体回退且插件恢复启用；无嫌疑（核心故障）→ 原回退路径。

## 7. 与既有改动的关系

- 本机制建立在上一轮五项改动（未提交）之上：复用 `stripBundleEntry`、`restartAndVerifyServer`、
  `parseBootLogSuspects`、快照/回退/LKG 管线。
- 本需求与需求 5 评估中搁置的「可疑插件自动升级」是同一问题的两种策略（禁用 vs 升级）：
  本需求落地后，恢复导入时若健康失败且点名单一插件，也可考虑同策略降级为「禁用该插件继续恢复」
  （比整体回退成功率更高）——作为后续增强。
- 发布：并入下一批功能版（建议 v0.8.0，含前五项）；关于页插件行新增禁用徽标 →
  需重拍 `docs/shots/about-*.webp`。

## 实施落地（2026-09-05 · 版本号 v0.7.1 · 已编译未提交）

按需求全文实施并扩展：被禁用的插件提供「启用」按钮，启用失败自动重新禁用并重启服务；
自动禁用覆盖 harness 更新、插件更新、插件删除、导入恢复四条操作路径（保证这些操作后服务可启动）。

- 新文件 `plugin_disable.go`：禁用状态核心（profile `dsh.profile.disabledPlugins: {name: reason}`；
  `disablePluginInProfile`/`enablePluginInProfile`/`clearProfileDisabledRecord`/`pluginDisabledAcross`/
  `.disbak` 轻量备份）、启动嫌疑收敛（`userSuspectPlugins` = 日志点名 ∩ 用户插件行，过滤官方包）、
  自动禁用尝试（`disableBootSuspects`：备份→禁用→重启校验；仍失败还原备份交由调用方回退）、
  手动启用（`enablePluginAndVerify`：启用→校验；失败→按日志原因重新禁用→再重启）。
- service_guard.go：`bootSuspectReasons`（嫌疑→原因文案）+ `trimHintLine`。
- updater.go `runHarnessUpdate`：重启校验失败先 `disableBootSuspects`，健康则保留新版本
  （禁用名单列于弹窗；harness LKG 提升照旧），仍失败才 `rollbackUpdate`。
- plugin_update.go：`runPluginUpdate` 校验失败先自动禁用嫌疑（可能含被更新插件本身）——健康则保留
  新版本；此前被禁用的插件更新成功自动尝试重新启用（`enablePluginAndVerify`），仍不兼容则继续禁用；
  `runPluginRemove` 校验失败同样先禁用其它阻碍插件，删除成功清残留禁用记录。
- exportimport.go：`finishPluginImport` 返回 (note, err)，恢复导入健康失败先自动禁用点名插件
  保留导入（禁用清单进 import:done note），仍失败才整体回退；`exportPlugins.Disabled` 随 manifest
  携带；`mergePluginConfigIntoProfile` 跳过禁用名 bundles、合并禁用原因（目标原因优先）；
  `sanitizeProfileLocalDeps` 移除依赖时同步清禁用记录。
- harness_reset.go `cleanProfilePlugins`：重置时清除用户插件禁用记录。
- app.go：新绑定 `EnablePlugin` + `runPluginEnable`。
- PluginRow 增 `Disabled`/`DisabledReason`；`buildPluginRows` 填充；关于页渲染「已禁用」徽标、
  禁用原因行与「启用」按钮；更新弹窗文案区分禁用态（成功自动启用 / 仍不兼容继续禁用）。
- 单测：`plugin_disable_test.go`（禁用/启用往返、幂等、记录清理、跨目录、merge 跳过禁用、
  manifest 携带、sanitize 联动、原因截断），与全量 `go test` 一起全绿。
- 构建：`wails generate module` + `wails build -skipbindings`，`-Version v0.7.1`，exe 已换入 dist。
- 未做：git 提交 / tag / CI 发布；冷启动自愈仍走既有 LKG 回退（本需求聚焦插件增删改/导入恢复路径）。


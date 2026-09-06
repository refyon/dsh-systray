# 评估：mac 重选导入的本地插件后仍未加载进 harness

> 2026-09-07 · 需求：评估「mac 上对导入的本地插件重新选择目录后，harness 仍未加载该插件」的步骤与关键问题点
> 现状代码基准：main @ fe1fd18（v0.8.1）；本机为 Windows 沙箱，无法复现 mac 行为，结论基于代码链路审计

## 0. 结论摘要

- 单凭代码无法定案：重选本地插件涉及「导入裁决（副本/待重指定）→ 重选事务（快照/改 spec/pnpm install/重启校验）→ 激活清单（bundles）→ 禁用自愈」四段，任何一段在 mac 上偏差都会表现为「插件没进 harness」。
- 按概率排序列出 6 个候选根因（R1–R6，见 §2），其中 **R1 禁用自愈循环、R2 bundles 未激活、R3 所选目录自身不可加载** 最可能；mac 特有的差异点集中在 R1 的日志行判定与 R5 的便携 pnpm 环境。
- 建议先按 §3 让用户在 mac 上采集三样证据（插件行状态、profile package.json 三键、统一日志中插件名相关行），即可 1 分钟定案；§4 给出各根因的修复方案，§5 是实施注意点。

## 1. 现状机制（代码定位）

1. **导入裁决**（exportimport.go `sanitizeProfileLocalDepsAll` L690）：跨机恢复时本地 spec（`link:/file:/workspace:` 或盘符路径，`localSpecPath` L645 / `isDriveAbsPath` L671 兼容 Windows 盘符）目标缺失 →
   - 有导入包副本 → 迁到 `<dshHome>/local-plugins/<name>` 改写 `link:<副本>` 继续加载（`adoptRestoredCopy`）；
   - 无副本 → 移出 `dependencies`，记 `dsh.profile.pendingLocalPlugins`（spec + bundled 是否曾激活），并清理 bundle/禁用记录。
2. **重选入口**：插件行「更新…」→ `PickLocalPluginPath`（app.go L523，`OpenDirectoryDialog`）→ 比较版本 → 前端确认 → `ApplyLocalPluginUpdate` → `runLocalPluginUpdate`（plugin_update.go L1484）：
   停服务 → 各 profile 快照 → `relinkPendingLocal`（L1347：**仅当 pending 记录 bundled=true 才把 name 加回 `dsh.profile.bundles`**）→ `setProfileDepSpec` 写 `link:<所选目录>`（`localLinkSpec` L1450，反斜杠归一）→ `pnpm install`（`runProfileCmd` L823，直接 exec 无 shell，6 分钟超时）→ 版本对照 → `restartAndVerifyServer` → wasDisabled 时 `enablePluginAndVerify` 重启用。
3. **harness 加载口径**：harness 只加载 `dsh.profile.bundles` 清单内的插件（从 profile node_modules 解析，pnpm `link:` 为符号链接）。

## 2. 候选根因（按概率）

- **R1 禁用自愈循环（最可能之一）**：重选成功后 `enablePluginAndVerify`（plugin_disable.go L367）会重启并做启动健康校验；若启动日志中该插件名命中错误特征（`bootSuspectReasons`，service_guard.go L175，正则 `error|syntaxerror|typeerror|…|not found|failed to load` 且行内包含插件名）→ 自动重新禁用 → 插件不进 harness。用户侧现象：行显示「已禁用」徽标+原因。**mac 特有疑点**：服务经 `sh -c` 启动，mac 上插件加载器/依赖报错行格式与 Windows 不同，存在把无害行（如插件名出现在 "not found" 的提示行）误判为加载失败的可能——需日志实证。
- **R2 bundles 未激活（最可能之二，跨平台逻辑漏洞）**：若导入时该插件在源机本就未激活（bundled=false），pending 记录 bundled=false；重选时 `relinkPendingLocal` **不会**把 name 补进 bundles（L1363 `if bd`）→ spec 已正确 link、node_modules 有符号链接，但 harness 激活清单里没有它 → **永不加载**，且 UI 无任何报错/禁用提示。这正是「重新选择后依旧没有加载」的典型无症状形态。
- **R3 所选目录自身不可加载**：用户选中的本地目录缺少构建产物 / 自身 node_modules / main 入口缺失 → 服务能正常启动（健康校验通过），但该插件 loader 报错被跳过 → 静默不加载。诊断靠统一日志 [server] 段中插件名相关行。
- **R4 多 profile 错位**：`enumeratePluginProfiles`（L110）更新「声明该插件的 profile」（row.Locs），若 mac 上实际运行用的是另一个 profile（DSH_HOME 指向、默认 profile 与命名 profile 并存），更新的 profile 没生效。诊断：对比 `$DSH_HOME`/`~/.dsh/profiles` 与日志中服务实际使用的 profile。
- **R5 mac 便携 pnpm 环境（mac 特有，低概率）**：`pnpmWrapper() = runtimeDir()/pnpm`（platform_darwin.go L108），若 wrapper 缺失执行权限 / 运行时目录在带空格路径（`~/Library/Application Support/…`）且某处经 shell 包装、或镜像网络失败 → `pnpm install` 失败 → 事务**会回退并弹错**；用户未见报错则基本排除。
- **R6 版本对照/路径小概率**：`installedPluginVersion` 读 `node_modules/<name>/package.json`（L141，跟随符号链接）取不到版本时 `pickedVer==""` 放行、pickedVer 非空但装出版本更低则回退——低概率，回退时有弹窗可感知。

## 3. 诊断步骤（mac 上 3 分钟定案）

1. **插件行状态**：设置页「关于」→ 该插件行是否带「已禁用」徽标及原因、「待重指定」标记、当前版本与「本地路径」字段——直接区分 R1（禁用）/R2（无徽标但无激活）/R3（无徽标、版本已更新）。
2. **profile 三键**（终端执行，把 `<profile>` 换成实际目录，`ls ~/.dsh/profiles/`）：
   ```bash
   P=~/.dsh/profiles/<profile>   # 或旧布局 ~/.dsh/profiles
   python3 -c "import json;r=json.load(open('$P/package.json'));print('deps:',r.get('dependencies',{}).get('<插件名>'));print('bundles:',r.get('dsh',{}).get('profile',{}).get('bundles'));print('disabled:',r.get('dsh',{}).get('profile',{}).get('disabledPlugins'));print('pending:',r.get('dsh',{}).get('profile',{}).get('pendingLocalPlugins'))"
   ls -l "$P/node_modules/<插件名>"   # 应为符号链接且目标=所选目录
   ```
   判读：bundles 缺 name → R2；disabled 有 name → R1；deps spec 仍为旧值 → 事务未生效（查 R5/回退弹窗）；符号链接断 → 目标目录被移动/删除。
3. **统一日志**：设置页「日志」搜索插件名，复制 [server]（加载器报错）与 [profile]（pnpm install 输出）相关行发回。判读：有无 loader 错误 → R3/R1 证据；`profile cmd: pnpm install (dir=…)` 后是否有 ERR → R5。

## 4. 修复方案（按根因）

- **R2（推荐先修，代码即改）**：`relinkPendingLocal` 中无论 `bundled` 真假都补进 bundles（重选目录 = 用户明确期望激活该插件；原 bundled=false 只是源机未激活，重选是新的显式激活意图）。改动点 plugin_update.go L1363：`if bd { appendBundleEntry(root, name) }` → 无条件 `appendBundleEntry(root, name)`；同文件注释同步。**验证**：`restore_sanitize_test.go` 现有 `bundled:false` 用例断言调整/新增「重选后进 bundles」用例；`go test ./...`。
- **R1**：先看证据行——若是真不兼容（插件代码对当前 harness 报错）→ 属预期行为，引导用户修插件后重试；若是误判（mac 日志行误命中）→ 收紧 `bootSuspectReasons`/`parseBootLogSuspects` 的命中条件（如仅取 `[server]` 段、行内须含插件名且排除已知无害模式），需要用户提供的日志行作为回归样本。
- **R3**：非代码缺陷，属提示不足——在重选成功弹窗追加「若所选目录未安装依赖/未构建，插件可能无法加载」提示，并建议在重选事务内对所选目录做 `node_modules`/`package.json main` 存在性预检（可选）。
- **R4**：诊断后若确认 profile 错位 → 检查 `enumeratePluginProfiles` 与 harness 实际 profile 解析口径（`$DSH_HOME`、默认 profile 命名），必要时把 row.Locs 扩展到 harness 实际使用的 profile 集合。
- **R5**：按 §3-3 日志判读；如为 wrapper 权限 → `ensureRuntime` 补 `chmod +x`；镜像失败 → 走既有镜像回退。
- **通用改进（建议一并做）**：重选事务成功后，日志补一行终态快照（`[plugin] <name> spec=… bundles=yes/no link=<目标>`），让此类问题下次直接凭日志定案。

## 6. 【已定案——用户证据 2026-09-07】

用户 mac 实机证据：
1. 插件行：无「已禁用」，有「待重指定」；重选目录后提示「已经是最新版本」。
2. `~/.dsh/profiles/web/package.json`：`dependencies` 无 dsh-ui-taste；`bundles` 无 dsh-ui-taste；`pendingLocalPlugins.dsh-ui-taste = {bundled:true, spec:"file:D:/agent-env/qtz/plugins/dsh-ui-taste"}`（Windows 源路径）→ 待重指定记录仍在。
3. 日志目录只有 `dsh-systray.log.1/.2/.3`，**没有 `dsh-systray.log`**，且 `.1` 仍在增长（最新 mtime）→ 日志页空白。

**根因 A（插件不加载，主因）**：待重指定行重选时，`PickLocalPluginPath` 用 `row.Version`（读 node_modules 残留副本的版本）与所选目录版本比较，`localPickRelation` 返回 `"same"` → 前端 `doLocalPluginUpdate`（main.js L917-919）走「已经是最新」分支**直接 return，不调用 ApplyLocalPluginUpdate** → pending 记录从未清除、spec/bundles 从未修复 → harness 不加载。前端把「版本相同」误判为「无需操作」，忽略了待重指定行真正要做的是**重链接**而非版本升级。

**根因 B（日志空白，mac 独有，附带根因）**：`rotateServerLog`（service_guard.go L77）在每次拉起服务前把 `dsh-systray.log` rename 为 `.1`；POSIX（mac）允许对打开中的文件改名 → 进程持有的 `unifiedFile` 句柄继续写入已改名的 `.1`，基础文件 `dsh-systray.log` 不再存在；日志页只枚举/读取该基础文件 → 空白。Windows 上 rename 打开中的文件会失败（文件锁）→ 走「保留原文件」回退 → 无此问题。**连带影响**：mac 上启动日志扫描基线（`serverLogLines`）读不到基础文件 → 嫌疑插件检测/禁用自愈在 mac 上长期失效（与「无已禁用徽标」一致）。

**修复方案**：
- A：main.js `doLocalPluginUpdate` —— `p.pendingLocal` 为 true 时**跳过 same 短路**，确认文案改为「重新指定该插件的安装来源为所选目录（版本相同，仅重定向链接）」，确认后照常 `ApplyLocalPluginUpdate`（Go 事务内 `relinkPendingLocal` 已支持：bundled=true 时恢复激活）。
- B：`rotateServerLog` 轮转成功后**重开统一日志句柄**（close 旧句柄 + `initUnifiedLog` 重建），使后续写入落到新建的 `dsh-systray.log`（返回基线 0 语义不变；rename 失败路径保持现状）；`service_guard_test.go` 轮转用例同步适配（轮转后基础文件存在且为空）。
- C（可选诊断增强）：`runLocalPluginUpdate` 成功后日志补一行终态快照 `[plugin] <name> spec=… bundles=yes link=<目标>`。

## 5. 关键问题点

- 本机 Windows 沙箱无法复现 mac 行为，改动前必须先拿到 §3 证据确定 R1/R2/R3，否则可能「修了 R2 却发现是 R1」。
- R2 的修复语义是「重选=显式激活」，与「源机未激活则恢复后也不激活」的导入保守策略可能冲突——需用户确认产品语义（建议：重选总是激活）。
- `enablePluginAndVerify` 依赖启动日志判定，若 R1 误判根因在 mac 日志行格式，需一并改判定逻辑并补 mac 样本回归，否则「修好禁用后又被禁用」。
- 前端无源码构建链，若需改弹窗提示直接编辑 `frontend/dist/*`；Go 侧改动后 `go vet`/`go test ./...` 全绿再提交。
- 所有改动走同一「快照→改→install→重启校验→回退」事务语义，勿绕过（避免半安装态）。

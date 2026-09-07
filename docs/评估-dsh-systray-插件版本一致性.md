# 评估：本地插件跨机导入后版本陈旧（dsh-ui-taste 本地 0.2.1 → 新机仍显示 0.2.0）

日期：2026-09-06 · 范围：导出/导入管线中插件版本号一致性 · 代码基线：v0.8.2（commit 91c0b5d 之后）

现象：dsh-ui-taste 在源机（本地）为 0.2.1，导出后在新机导入，新机仍显示 0.2.0；restrict-discipline 存在相同问题。

---

## 一、版本号数据流（现状，均已代码核实）

1. **显示**：插件列表的版本列 = `installedPluginVersion()` 实时读取
   `profiles/<p>/node_modules/<name>/package.json`（`plugin_update.go` L315/331/419）。
   显示层本身无缓存，不是显示缓存问题。
2. **导出**：
   - `manifest.json` 的 `exportManifest.Plugins` 只记录 `Dependencies`（spec 文本）/
     `Bundles` / `Disabled`（`exportimport.go` L53-58）——**不记录任何版本号**。
   - `plugins.zip` 通过 `collectPluginClosure → resolveNodeModules`（L217-272）把
     `node_modules/<name>` 经 `EvalSymlinks` 解析后的**实体目录**打包进 zip。
     对 `file:` spec：实体可能是 pnpm 安装时的快照副本，未必等于 spec 指向目录的当前状态。
3. **导入顺序**（`import_flow.go` L212-319）：
   解压 plugins.zip → node_modules（`restoreItem`，overwrite=true）
   → `registerRestoredPlugins`（manifest 合并进 profile package.json；
   **同名依赖保留目标已有 spec**，`exportimport.go` L548-552）
   → `sanitizeProfileLocalDepsAll`（只处理「本地 spec 且目标路径不存在」的依赖：
   有 zip 恢复副本 → `adoptRestoredCopy` 迁到 `<dshHome>/local-plugins/<name>` 并改写 spec 为
   `link:<副本>`；无副本 → 移出依赖记「待重指定」）
   → preflight 预检 + `pnpm install` 对齐（npm/github spec 会**重新解析**，解压出的恢复副本被覆盖）。

## 二、陈旧根因候选（按概率排序）

1. **重复导入复用旧副本（系统性缺陷，两个插件同病，最符合「依旧显示 0.2.0」）**
   - `adoptRestoredCopy` L760-762：`local-plugins/<name>` 已存在 → **直接复用，不比较版本**。
   - `mergePluginConfigIntoProfile` L548-552：目标 profile 已有该依赖 → **保留旧 spec**。
   - `missingLocalDeps`（L731-753）：只挑「路径不存在」的本地 spec；第一次导入后 spec 已改写为
     `link:C:/Users/<x>/.dsh/local-plugins/<name>`，该路径在新机**存在** → 第二次导入时
     sanitize 完全不处理该插件 → zip 里携带的新版本（0.2.1）被无视，旧 0.2.0 副本永远保留。
2. **导出快照陈旧（file: 语义）**
   `file:` 依赖在 pnpm 下 node_modules/<name> 可能是安装时快照（非 dev 目录直连）；
   dev 目录升到 0.2.1 后若源机未重装，导出打包的是 0.2.0 快照而非 dev 目录现状。
   本机（lenovo）当前实体 = 0.2.1（与 dev 一致），但不排除用户源机导出时仍为 0.2.0。
3. **npm/github spec 导入后重解析漂移**
   restrict-discipline（本机 spec = `github:refyon/restrict-discipline`）导入后 pnpm 重新解析
   仓库 HEAD/registry，天然可能与源机已装版本不一致；zip 恢复副本在 install 阶段被覆盖。
4. 排除项：待重指定（pending）行版本显示为空，不会显示 0.2.0；显示链路实时读取，排除前端缓存。

> 定案方法：看新机三处状态即可锁定根因——
> ① 新机 profile package.json 中该插件 spec 是否已是 `link:…local-plugins…`；
> ② `local-plugins/<name>/package.json` 的 version；
> ③ 导出包 plugins.zip 内该插件 package.json 的 version。
> 若 ①②=0.2.0 且 ③=0.2.1 → 根因 1；若 ③ 本身=0.2.0 → 根因 2；restrict-discipline 走 ③。

## 三、修复步骤（推荐方案，分三阶段）

### 阶段 A：导出侧治本（版本入 manifest + 本地 spec 直取源目录）

1. `exportManifest.Plugins` 新增 `Versions map[string]string`：导出时读
   `installedPluginVersion()` 快照每个插件的已装版本写入 manifest。
2. `collectPluginClosure` 对 `file:/link:/workspace:` spec 且 spec 目标目录存在时，
   **打包源改为 spec 目标目录本身**（而非 node_modules 实体/EvalSymlinks 结果），
   保证 zip 携带 dev 目录当前版本；目标目录不存在才回退 node_modules 实体。
3. 导出时校验：本地 spec 的已装版本 < 目标目录版本 → 导出结果警告
   「本地插件 X：已装 vY，源目录 vZ，请先在源机刷新安装后重新导出」。

### 阶段 B：导入侧治本（版本感知刷新，覆盖重复导入）

4. `adoptRestoredCopy` 加版本比较：canon 已存在时读双方 package.json 版本——
   恢复副本更新 → canon 先改 `.bak` 再迁入新副本；更旧/相同 → 复用旧副本 + note。
5. 新增「已链接副本刷新」步骤（置于 sanitize 之后、pnpm 对齐之前）：
   spec 指向 `local-plugins/<name>` 且 zip 恢复副本版本更新 → 刷新该副本（自带 `.bak` 回退），
   spec 不动。此步骤直接修复根因 1 的重复导入场景。
6. `mergePluginConfigIntoProfile` 保持「保留已有」语义不变（防覆盖设计），
   版本纠偏全部交给步骤 4/5 的副本刷新——spec 永不强行改写。
7. 导入结束用 `logPluginTerminalState`（已有，`plugin_update.go` L1421）打印每个本地插件终态；
   版本仍不一致时给出用户可见 note。

### 阶段 C：验证与交付

8. 前端无 UI 改动（版本列实时读取）；如需提示仅在 note 区加文案（直接改 `frontend/dist/*`）。
9. 单测（沿用 `restore_sanitize_test.go` 模式）：
   - adopt 版本比较（新/旧/相同/缺 version 四分支）；
   - 重复导入刷新：先 0.2.0 导入再 0.2.1 导入 → local-plugins 与 node_modules 均 0.2.1；
   - 导出源选择：file: spec 目标目录 0.2.1 / node_modules 快照 0.2.0 → zip 内为 0.2.1；
   - merge+Versions 交互。
10. 真机验证：用 DSH_HOME 重定向双目录离线复现（不碰本机 3080 宿主）；
    最终由用户在真实新机上用同一导出包重试并回传三处状态（见「定案方法」）。

## 四、关键问题点

1. **local-plugins 副本不在 .importbak 快照保护内**（快照只包 profile 目录）：刷新/替换副本
   必须自带 `.bak` 与回退，失败半成品会致 link 悬空。
2. **pnpm file:/link: 链接形态随 pnpm 版本而异**（junction/快照/实体），导出选源不能依赖
   链接形态判断，必须显式按 spec 目标目录选源（阶段 A 步骤 2 即为此）。
3. **版本缺失**（package.json 无 version）：不刷新、给 note，禁止按 mtime/内容猜测。
4. **状态保持**：刷新副本只换文件，bundles 激活、disabled 记录、pending 记录一律不动；
   刷新后必须重启健康校验，不兼容走既有自愈禁用路径。
5. **merge「保留已有」是防覆盖设计，不能改为无条件覆盖**：只在「zip 版本更新」时刷副本。
6. 回归面：恢复管线既有测试（sanitize 六类场景）+ 五项改进批次 B 的 reconcile 修复链全量过；
   同名多 profile（dirs 集合）时刷新要逐目录一致，防半刷新。
7. 构建发布：`wails generate module`（仅当新增绑定/字段影响前端）、`build.ps1`、`go test ./...`
   全绿；版本 bump v0.8.3（补丁级）；`docs/RELEASE_NOTES.md` 写 v0.8.3 区块（发布约定）；
   CI 三平台自动构建。
8. **e2e 红线**：本沙箱 3080 是会话宿主，`killServer` 会断会话——真机联调必须在用户机器或
   DSH_HOME 隔离目录进行（项目记忆既有结论）。

## 五、实施落地（2026-09-07，已编译未提交未发布）

**用户提供的导出包 `D:\Downloads\dsh-systray-export-20260907-132933.zip` 检查结论**：manifest 由
appVersion 0.8.3 生成，4 个插件；plugins.zip 内 `dsh-ui-taste=0.2.1`、`restrict-discipline=0.8.1`
——与本地一致，导出包本身携带正确版本 → 「新机仍显示 0.2.0」锁定为**导入侧旧状态复用**
（根因 1：adoptRestoredCopy 直接复用旧 local-plugins 副本 / merge 保留旧 spec /
missingLocalDeps 只认「路径缺失」，重复导入时新版本被无视），按三阶段方案实施：

- **阶段 A（导出侧）**：`exportPlugins` 新增 `Versions map[string]string`；`buildExportZip`
  以 `pluginVersionSnapshot`（读 node_modules 实际已装版本）写入 manifest.Plugins.Versions；
  `collectPluginClosure` 改为接收 name→spec 映射——本地 spec（file:/link:/workspace:）且目标
  目录有效时打包源**直取 spec 目标目录**（携带 dev 目录当前版本，而非 pnpm 安装旧快照）。
- **阶段 B（导入侧）**：`adoptRestoredCopy` 版本感知——稳定副本已存在时仅当恢复副本版本更新
  才替换（旧目录 `.dshbak-<ts>` 备份、失败回退），否则复用；新增 `refreshStableLocalCopies`：
  spec 已指向 local-plugins 稳定副本（前次导入 adopt）而本次导入包携带更新副本时升级副本，
  覆盖重复导入主场景；sanitize 全量重构，替换/刷新均输出用户可见说明行。
- **需求 2（检查更新后刷新版本状态）**：`doPluginCheck` 完成后 `await loadPlugins()` 重拉列表
  （行内状态由 plugState 恢复不丢失）；`import:done` kind=plugins 时也 `loadPlugins()`，导入
  改变安装/版本后列表立即刷新。
- **测试**：新增 adopt 替换/复用/重复导入刷新/闭包本地 spec 源选择/版本快照 5 组用例，
  `go test ./...` 全绿；真实 profile 导出验证：manifest.Versions 齐备
  （dsh-ui-taste 0.2.1 / restrict-discipline 0.8.1 / dsh-cost-meter 1.7.13 / dsh-plugin-codegraph 0.1.6），
  zip 内 dsh-ui-taste@0.2.1、restrict-discipline@0.8.1。
- **遗留（真机 e2e）**：重复导入刷新需在用户新机上用同一导出包实测——第一次导入（旧包或本包）
  后再导入本包，观察 dsh-ui-taste 保持/更新为 0.2.1；本沙箱 3080 宿主不可 killServer，验证须在
  用户机器或 DSH_HOME 隔离目录进行。

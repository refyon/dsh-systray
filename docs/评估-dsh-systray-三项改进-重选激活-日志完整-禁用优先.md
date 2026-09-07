# 评估：dsh-systray 三项改进（mac 重选仍未加载续查 + 日志页完整显示 + 禁用优先于回退）

> 2026-09-07 · 需求：①[mac] 重新选择导入的本地插件后依旧没有加载到 harness；②日志页内容框应完整显示所有已写入内容；③导入插件/更新 harness 后尽可能以当前已更新版本启动为目标，尽量不回退版本，用禁用肇事插件换取启动成功
> 代码基准：main @ 3b676d3（v0.8.2 之后）；本机 Windows 沙箱无法复现 mac 行为，结论基于代码链路审计 + 既有用户证据

## 0. 结论摘要

- 问题①是 v0.8.2「待重指定重选不加载」修复的续查：v0.8.2 已修复前端 same 短路与日志轮转句柄（根因 A/B，用户证据定案）。本轮按评估文档《评估-dsh-systray-mac本地插件重选未加载.md》§4 的候选根因继续落地：**R2（重选不激活，代码级确定性漏洞）已修复**——`relinkPendingLocal` 此前仅在历史记录 bundled=true 时把插件加回激活清单；bundled=false 时重选事务「成功」但 harness 激活清单里没有它 → 插件永不加载且无任何提示（无症状形态）。现改为**重选=显式激活**：无条件补进 `dsh.profile.bundles`。另加固三处：mac `killServer` 端口监听进程 SIGTERM→轮询→SIGKILL 兜底（旧进程不退出会让重启校验误判「端口已有可用服务」而跳过重启，新配置永不加载）；重选成功弹窗提示所选目录缺 node_modules（R3 提示不足）；终态日志覆盖全部 profile 并记录 node_modules 链接是否建立（R4 多 profile 错位定位）。若 mac 上仍不加载，凭新终态日志 + profile 三键可一次定案（见 §1.4）。
- 问题②三处「已写入内容」丢失点全部修复：前端 DOM 4000 行裁剪（最早写入的行被静默丢弃）、轮转后 offset 跳到新文件末尾（轮转后新写入的行被整体跳过——mac 每次服务重启都轮转）、轮转归档 .1/.2/.3 不可见（轮转前历史从页面消失）。现日志页 = 归档（旧→新）+ 基础文件完整拼接，清空日志一并删除归档。
- 问题③落地「禁用优先、回退兜底」两级自愈：更新 harness / 导入插件失败时先按启动日志点名禁用（已有），**未奏效或无点名嫌疑时禁用全部已激活用户插件再试**（新增 disableAllUserPlugins）；冷启动失败（tryBootRollback）由「直接恢复 LKG 旧版本」改为**先禁用肇事插件保留当前版本**，两者都失败才回退 LKG。禁用只摘除激活清单并记录原因，依赖与文件保留，可在关于页逐个重新启用。

## 1. 问题①：mac 重选导入的本地插件仍未加载（续 v0.8.2）

### 1.1 现状机制回顾

导入裁决（副本→`local-plugins/<name>` 继续加载；无副本→移出 dependencies 记 `pendingLocalPlugins{spec,bundled}`）→ 重选事务（快照→`relinkPendingLocal`→`setProfileDepSpec` 写 `link:<目录>`→pnpm install→版本对照→重启校验）→ harness 只加载 `dsh.profile.bundles` 清单内的插件。

### 1.2 本轮修复（代码级确定性漏洞）

| 根因 | 修复 | 改动点 |
|---|---|---|
| R2：bundled=false 时重选只清记录不激活 → spec/node_modules 就位但激活清单没有它，插件永不加载且无症状 | 重选=显式激活：`relinkPendingLocal` 无条件 `appendBundleEntry`（bundled 仅记录源机历史状态） | plugin_update.go |
| 旧服务进程未退出 → `restartAndVerifyServer` 误判「端口已有可用服务」跳过重启 → 新配置永不加载 | mac `killServer`：SIGTERM 后轮询端口释放（10×200ms），仍存活 SIGKILL 兜底（与 Windows taskkill /F 语义对齐） | platform_darwin.go |
| R3：所选目录缺 node_modules/构建产物 → 重选成功但插件无法加载，无提示 | 成功弹窗检测所选目录 node_modules 缺失时附排查提示 | plugin_update.go |
| R4：多 profile 错位无法定位 | 终态日志逐 profile 输出，并记录 node_modules 链接是否建立 | plugin_update.go |

### 1.3 测试与验证

- `TestRelinkPendingRestoresBundleAndClearsRecord` 更新：bundled=false 也断言激活进 bundles。
- `go vet ./...` + `go test -count=1 ./...` 全绿；dev exe 已换入 dist。

### 1.4 若 mac 上仍不加载（下一步证据清单）

重选成功后按序采集发回，凭日志一次定案：

1. 统一日志（日志页现在可完整看到）中 `plugin <名> terminal state:` 行——declared/spec/bundles/pendingLocal/disabled/node_modules 五键即为定案依据；
2. `~/.dsh/profiles/<实际 profile>/package.json` 的 `dependencies`、`dsh.profile.bundles`、`disabledPlugins`、`pendingLocalPlugins` 四键；
3. `ls -l ~/.dsh/profiles/<profile>/node_modules/<插件名>` 是否为符号链接且指向所选目录；
4. 日志 `[server]` 段中插件名相关报错行（loader 报错 → 插件自身不可加载，属 R3 而非本进程缺陷）。

## 2. 问题②：日志页完整显示所有已写入内容

### 2.1 三处内容丢失点与修复

| 丢失点 | 根因 | 修复 |
|---|---|---|
| 最早写入的行从页面消失 | 前端 `renderLog` 裁剪 DOM 至 4000 行（轮转/日志量大时静默丢头） | 移除裁剪：视图规模受轮转窗口约束，重置时整体重载 |
| 轮转后新写入的行被整体跳过 | `ReadLogTail` 在 offset 越过文件末尾时跳到当前末尾（mac 每次服务重启都轮转日志，这段现场正是排障最需要的部分） | offset 越界置 `Reset=true` 并从 0 重读；前端清空视图重载 |
| 轮转前的历史从页面消失 | 归档 `.1/.2/.3` 不参与展示，日志页只读基础文件 | 新增 `ReadLogArchives` 绑定（.3→.2→.1 旧到新合并）；前端首次加载与重置时先渲染归档再尾读基础文件；`ClearLog` 一并删除归档（清空后不再「复活」） |

### 2.2 验证

- `TestReadLogTailResetOnTruncate`（轮转后 Reset + 从头重读）、`TestReadLogArchivesMergesOldestFirst`（归档合并顺序）新增于 service_guard_test.go；`node --check frontend/dist/main.js` 通过。

## 3. 问题③：导入/更新后禁用优先于回退版本

### 3.1 现状与缺口

`runHarnessUpdate` / `finishPluginImport` 已有「启动日志点名禁用」自愈；缺口在：

1. 点名禁用未奏效、或无点名嫌疑（如插件崩溃未留名）时直接整体回退——不满足「尽量不回退版本」；
2. **冷启动失败路径**（`tryBootRollback`）直接恢复 LKG 旧版本，从未尝试禁用肇事插件——用户更新 harness 成功后，下次冷启动因插件不兼容失败时整个 harness 被悄悄回退。

### 3.2 修复（两级自愈：点名禁用 → 全部用户插件禁用 → 回退）

- 新增 `disableAllUserPlugins`（plugin_disable.go）：备份全部 profile（.disbak）→ 禁用所有已激活的非官方插件（有依赖行走 disablePluginInProfile，bundle-only 走 disableGhostPluginInProfile，均记录原因）→ 重启健康校验；成功保留禁用态并清理备份，失败整体还原。
- `runHarnessUpdate`：点名禁用失败 → `disableAllUserPlugins` → 仍失败才回退版本；弹窗区分「点名禁用」与「全部禁用」文案。
- `finishPluginImport`：同上，保留导入 + 当前版本优先。
- `tryBootRollback`（冷启动）：先点名禁用、再全部禁用，任一成功即**保持当前版本**（清理 LKG、提示 `reportBootKept`）；都失败才走原有 LKG 回退。签名改为 `(kept, rolled, prev, disabledNames)`，main.go 调用点与提示同步更新。
- `activatedUserPluginNames`：枚举各 profile 激活清单的非官方插件名（name@… 取 base、去重排序），供兜底禁用使用。

### 3.3 语义边界

- 禁用**不删除**依赖与文件，只摘除激活清单并记录原因；关于页每行可「启用」（启用失败自动重新禁用并重启，服务保持可用）。
- 全部禁用仍无法启动（核心故障/环境问题）→ 仍回退到上一可用版本（回退路径不变，安全网保留）。
- 冷启动失败自愈无 LKG 时也能保留当前版本（只要禁用后能启动）；这是相对旧行为的增强（旧行为无 LKG 时只报错）。

## 4. 验证与发布

- `go vet ./...`、`go test -count=1 ./...` 全绿（含新增用例：重选激活、ReadLogTail 重置、归档合并、激活名单枚举）；`node --check main.js` 通过。
- dev exe（wails generate module + build -skipbindings）已换入 dist；版本号建议 v0.8.3（RELEASE_NOTES 已备区块）。

## 5. 关键问题点

- 前端无源码构建链：改动直接编辑 `frontend/dist/main.js`，后续照此。
- 「重选=显式激活」与「源机未激活则恢复后不激活」的导入保守策略并存：导入阶段仍保守（只激活 cfg.Bundles 覆盖名），重选是用户显式动作、总是激活——两者语义不冲突。
- 兜底「禁用全部用户插件」为核弹级操作：仅用于点名定位失败的场景，弹窗明确列出被禁用清单与逐个重新启用路径；若后续用户反馈过度激进，可把兜底范围收窄为「本次导入/更新的插件集合」。
- mac 行为仍需实机验证（本沙箱 Windows）；§1.4 证据清单已备，若问题①仍复现，凭终态日志行即可定案。

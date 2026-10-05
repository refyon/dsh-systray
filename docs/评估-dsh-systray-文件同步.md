# dsh-systray 文件/文件夹同步 · 评估（实现步骤与关键问题点）

日期：2026-10-05 · 范围：需求 1-6 · 代码基线：v0.7.0（含已评估未提交的五项改进与插件禁用机制）

总体判断：这是**首个跨端大功能**——文件内容无法走现有 ops 同步流（value ≤4KiB、64KiB 请求体），
必须新增服务端对象存储端点 + 客户端独立文件同步引擎 + 一整套列表 UI。规模约为前几轮单项改动的
3-5 倍，建议严格按「服务端 → 客户端引擎 → UI」三批推进，且**动工前必须先确认语义问题**（见第 1 节）。

---

## 1. 必须先定的语义决策（阻塞项，默认推荐已给出）

| # | 决策点 | 推荐（默认） | 影响 |
| --- | --- | --- | --- |
| D1 | 单向备份还是多设备**双向**同步 | 双向（需求「跟随账号同步」暗示多设备；10MB 免费容量才有意义） | 双向多出「远端改动应用 + 冲突处理 + 删除墓碑」整条链路，工作量约 +40% |
| D2 | 冲突策略（两设备各改同一文件） | **冲突副本**：保留本机为 `名字 (冲突-设备名).ext` 再取远端，列表标「冲突」，不静默覆盖 | 静默覆盖=数据丢失；按 updatedAt LWW 对二进制文件不成立 |
| D3 | 远端改动是否沿用「重启生效」确认，还是自动应用 | 文件独立走**页面内「应用」按钮**（默认不自动覆盖本机文件） | 与现有 pendingApply 模式精神一致，但集合与按钮分离（文件不改设置） |
| D4 | 登出/换账号时本地文件清单如何处理 | 清单保留本机、同步暂停；换账号后做一次**全量对账**（远端清单不同） | 避免换号后误删本机文件或错传 |
| D5 | 空文件夹是否同步、目录大小排序口径 | 空目录不同步（只同步文件）；目录大小=子树合计（客户端聚合缓存） | 省去 dir 节点墓碑体系 |
| D6 | 多设备路径不一致如何处理 | **云端身份 = entryId + 条目内相对路径**；绝对路径永不上传；源设备原位、接收设备落本机「同步接收目录」 | 见 1.1 节（已定） |

### 1.1 多设备路径不一致处理方案（已定，用户确认）

**原则：云端身份 = `entryId + 条目内相对路径`；任何设备的绝对路径都是本机私有映射，永不上传。**

1. **源设备原位**：添加文件/文件夹只是登记（生成 entryId + displayName），不复制、不移动；
   本地绝对路径只写本机 `filesync.json`（0600），不上传（与 ops 模型「不上报绝对路径」同一原则）。
2. **接收设备落「同步接收目录」**：其他设备拉到该条目时，下载到本机默认接收目录
   （`<用户文档目录>\DeepSeekSync`，Windows 按 KnownFolder 解析、回退 `%USERPROFILE%\Documents`；
   设置项预留，MVP 固定默认），路径 = `接收目录/<条目显示名>/<条目内相对路径>`。
   各机各写各的根，路径差异天然无冲突。
3. **条目内相对路径是唯一合并键**：同一条目同一文件在两台设备上分别表现为源设备原位路径与
   接收设备副本路径，版本比较只用 `(rev, sha256, mtime)`，与绝对路径无关。
4. **重命名语义**：列表「重命名」改的是 displayName（云端标签 + 接收目录文件夹名），
   **不动本机源文件**；重名校验 Windows 非法字符（`\/:*?"<>|`）与保留名（CON/NUL）。
5. **删除两档确认**：「只从同步列表移除」（停止同步、本机文件保留）与「同时删除本机文件」
   （云端下发墓碑、波及所有设备——弹窗文案必须明示）。
6. **同名条目不魔法合并**：两台设备各自添加的条目即使显示名相同也是两个独立条目（两个 entryId），
   接收目录出现 `notes` 与 `notes (2)`，行为可预期；接收目录落盘冲突自动加 ` (2)` 后缀并在列表标出。
7. **条目嵌套防护**：添加时检测「选中路径已在其它条目的目录内 / 包含其它条目目录」→ 提示，
   不允许重叠（避免同一文件双条目上传与墓碑互搏）。

---

## 2. 现状盘点（已核实代码）

**服务端 dsh-connect**（Cloudflare Worker + D1）：
- API v1 契约冻结：改动须同步 `src/types.ts` + `docs/API.md` + 测试（API.md 末节明确）；
- ops 模型：per-key LWW、`MaxOpValueBytes=4KiB`、`MaxJSONBody=64KiB`、每 Worker 调用 ≤50 D1 查询
  （`docs/API.md` 每端点查询预算表是验收项）；
- `wrangler.jsonc` 仅绑定 D1（`DB`），**无 R2/对象存储**；免费档成本约束见 5.3；
- 已有 `janitor` cron（每小时），可复用为过期对象/孤儿清理；`DELETE /v1/me` 已做账号级物理删除。

**客户端 dsh-systray**（Go + Wails，前端无源码直接改 dist）：
- 账号同步引擎完备可借鉴：`account_ops.go`（幂等 opId 队列 + 退避重试 `accountFlushOps`）、
  `account_sync.go`（拉取合并/待生效/`accountReenqueueDriftedApplied` 漂移重判）、
  `account_background.go`（20 分钟 tick + `account:changed` 事件广播）、`account_store.go`（0600 原子写）。
- 文件/目录选择对话框绑定已存在（`app.go` L783/L1160 `OpenDirectoryDialog`、L1160 `OpenFileDialog`）；
  系统默认打开文件有先例（`platform_windows.go` `openDir`/`revealFile`，ShellExecuteW 模式）；
- `go.mod` **无 fsnotify**（新增依赖需评估）；`github.com/bep/debounce` 已在依赖树（间接，可转直接用于变更防抖）；
- UI 基建齐全：`confirmDialog`、`data-i18n` + `I18N_DYN` + `scripts/check-frontend-i18n.mjs`、
  DESIGN.md token（进度条组件已有样式）、shotmode 截图回归。

**结论**：服务器侧从零建文件通道；客户端侧复用「队列/退避/事件/对话框」模式，新写引擎与 UI。

---

## 3. 实现步骤

### 阶段 1：服务端（dsh-connect，先行、可独立验证）

1. **存储与配额数据模型**
   - `wrangler.jsonc` 增加 R2 绑定（`r2_buckets: [{binding:"FILES", bucket_name:"dsh-connect-files"}]`）；
   - 迁移 `0003_files.sql`：
     `files(id INTEGER PK AUTOINCREMENT, user_id TEXT NOT NULL REFERENCES users ON DELETE CASCADE,` 
     `path TEXT NOT NULL, parent_path TEXT, is_dir INTEGER, size INTEGER, sha256 TEXT, mtime INTEGER,` 
     `rev INTEGER, deleted_at INTEGER, r2_key TEXT, created_at/updated_at INTEGER, UNIQUE(user_id, path))`
     + `idx(user_id, id)`；`users` 表加 `tier TEXT DEFAULT 'free'`（**付费扩容只预留字段与
     quota 返回，不实现支付**——需求 1 的「预留接口能力」）。
   - 配额：`GET /v1/files/quota` → `{limit:10485760, used, tier}`；limit 按 tier 查表（free=10MiB）。

2. **新端点**（v1 追加，老客户端不受影响；契约按既有格式登记 API.md/types.ts）
   - `GET /v1/files` — 元数据列表（分页，≤500/页，1 条查询）；
   - `POST /v1/files` — 登记上传（路径/size/sha256/mtime）→ 返回 `{id, uploadUrl(预签名 PUT), rev}`；
   - `POST /v1/files/{id}/complete` — 上传完成确认：**服务端校验 sha256/size + 配额**，通过才落 rev，
     否则删对象返回 `quota_exceeded`（新增错误码）或 `checksum_mismatch`；
   - `GET /v1/files/{id}/content` — 预签名下载 URL（15 分钟有效）；
   - `PATCH /v1/files/{id}` — 重命名（同路径冲突校验）；目录重命名=批量改子项 parent_path；
   - `DELETE /v1/files/{id}` — 删除（软删 deleted_at + 删 R2 对象；目录级联子项；**墓碑必须保留**，
     对账时下发给其它设备，否则别机又上传回来——关键点 5.9）；
   - `POST /v1/files/sync` — 对账（入参本地清单 `{path,mtime,sha256,size}[]`，返回
     `upload/remove/conflict[]`，模式同 `/v1/plugins/sync` 的只读计算，≤2 查询）。

3. **配额权威校验**：complete 用 D1 事务（读 sum(size) + 插入/更新，超限回滚），两设备并发上传
   天然串行；**更新文件先删旧对象再计新大小**（释放空间）；客户端预检只是体验优化，不是防线。

4. **测试与部署**：vitest（内存 store mock，沿用 ops 测试模式）+ 迁移测试；
   staging 域名先行，客户端 `accountApiBase` 覆盖点测；老服务端无新端点 → 客户端 404 优雅降级。

### 阶段 2：客户端同步引擎（新文件 `account_files.go` + `account_files_store.go`）

1. **清单持久化** `filesync.json`（与 account.json 同目录，0600）：`entries[{id, kind(file/dir),
   localPath, relPath, status, lastError, size, mtime, sha256, rev}]` + 自己的 `LastSyncedAt`。
   登出保留清单、暂停同步（D4）。
2. **变更检测**：选中目录用 fsnotify（新增依赖）监听 + 2s 防抖（`bep/debounce`）；
   事件风暴/监听丢失（目录被改名）→ 降级为周期全量扫描（mtime+size 快判，变化才 sha256）。
   排除：`Thumbs.db/desktop.ini/.DS_Store`、符号链接/junction（防环）、超长路径（>250 提示跳过）。
3. **上传管线**：预签名 PUT 直传（绕开 Worker 体积限制）→ complete 确认；
   **陈旧上传防护**：complete 携带 base rev，服务端 rev 已前进 → 拒绝并返回当前 rev（防覆盖他人新版本）；
   失败重试复用 `accountFlushOps` 退避模式；上传前查配额缓存，预估超限直接标失败 + 弹窗（需求 6）。
4. **拉取/应用**：`POST /v1/files/sync` 对账 → 差异进**文件待生效列表**（独立于设置的 pendingApply）→
   页面内「应用」按钮执行下载/删除（覆盖本机文件前弹确认，列出将被覆盖的文件名）；冲突走冲突副本（D2）。
5. **状态与事件**：每项状态机 `synced/syncing/pending-upload/pending-download/conflict/error(+原因)/
   quota-blocked`；新事件 `files:changed`（或并入 `account:changed` 快照），随 20 分钟 tick 与
   「立即同步」一起刷新——满足需求 2「跟随账号同步一同更新同步状态」。
6. **Wails 绑定**：`PickSyncFiles/PickSyncFolder/RemoveSyncEntry/RenameSyncEntry/OpenSyncEntry/
   FilesSyncStatus/FilesApply`，执行后 `wails generate module` 重新生成 wailsjs。

### 阶段 3：前端（直接编辑 frontend/dist/*）

1. `index.html` 数据同步页新增「文件同步」卡片：容量进度条（复用进度条样式 + `--ok/--warn/--danger`
   三档：<80%/80-100%/超限）+「已用 X / 共 10 MB」+「添加文件」「添加文件夹」按钮 + 列表容器 +
   空态提示（需求 4：未添加时显示「还没有同步的文件或文件夹，点上方按钮添加」）。
   **卡片数量约束**：DESIGN.md「一屏最多 3 张主卡片」——登录态下可见卡片 = 账号卡 + 文件卡 +
   待生效卡，恰好 3 张；文件卡仅在已登录时显示。
2. `main.js`：
   - 列表渲染：**文件夹优先**（先全部文件夹、再全部文件），组内按所选排序键（名称/大小/修改时间，
     可点表头切换，升/降序）；文件夹行可展开/折叠显示子项（缩进层级），目录大小=子树合计（Go 侧
     聚合后下发，避免前端递归）；
   - 行内元素：图标+名称（超长截断）、大小（自动换算单位，**数值保持 <1000**，如 `999 B`→`1 KB`）、
     修改时间、状态徽标（成功/失败+原因/同步中/冲突）、操作按钮（文件：打开/重命名/删除；文件夹：
     展开/重命名/删除）；触控目标 ≥44px、disabled/empty/error 态齐全；
   - 打开=系统默认（Go 绑定 ShellExecuteW 先例）；重命名=confirmDialog 输入（重名与非法字符校验）；
     删除=二次确认（「只从同步列表移除」与「同时删除本机文件」两个选项——**必须区分**）；
   - 容量不足弹窗：需求 6「修改文件或添加新文件超出容量」→ Go 侧检测到超限即
     `confirmDialog`/alert 提示「可用容量不足（已用 X / 共 10 MB），请清理或删除部分文件」，
     并在列表项标「容量不足」状态；
   - 文案全部 `tr()` + 登记 `I18N_DYN`（双语），跑 `scripts/check-frontend-i18n.mjs`。
3. `style.css`：列表行、徽标、排序表头、空态、容量进度条（全部走 DESIGN.md token）。

### 阶段 4：集成与发布

- 单测：Go 侧 httptest mock 服务端（复用 `syncTestServer` 模式）覆盖清单/对账/冲突/配额/退避；
  服务端 vitest 覆盖端点/配额事务/墓碑/级联删除。
- 真机 e2e 在**另一台机器或换端口**进行（本沙箱 3080 是会话宿主，`killServer` 会杀会话——既有教训）；
  服务端先用 staging 域名 + 测试账号联调。
- 版本：功能新增 → dsh-systray bump v0.8.0；dsh-connect 独立部署（wrangler deploy）。
- 截图回归：数据同步页重拍 `docs/shots`（新卡片）。

---

## 4. 需求逐条覆盖核对

| 需求 | 落点 |
| --- | --- |
| 1 自选文件/文件夹 + 10MB + 付费预留 | 选择对话框绑定 + 服务端 quota 端点（tier 字段预留，不实现支付） |
| 2 列表显示在数据同步页、跟随账号同步状态 | 文件卡 + `files:changed`/`account:changed` 联动刷新 |
| 3 大小（<1000 换算）/状态+失败原因/展开折叠/删除重命名/打开 | 列表行组件 + 打开走 ShellExecuteW |
| 4 空列表提示 | 空态文案 |
| 5 排序（名称/大小/修改时间，文件夹在前） | 表头排序 + 文件夹优先分组 |
| 6 超容量弹窗 + 进度条 | quota 预检弹窗 + 容量进度条（三档语义色） |

---

## 5. 关键问题点

1. **两条同步通道必须隔离**：文件不进 ops 流（4KiB/64KiB 上限、每小时压实会把文件记录压掉）；
   文件元数据也不进 ops 流（压实 + LWW 语义对文件不对）。文件通道独立端点、独立游标/对账，
   只共享登录态/令牌/退避参数。
2. **配额防线只在服务端**：客户端预检会被绕过（断网改文件、旧客户端），complete 必须权威校验；
   两设备并发用 D1 事务串行；**更新大文件先删旧对象再确认新大小**，否则「10MB 里替换 8MB 文件」
   会假性超限。
3. **成本与限额**：R2 免费档 10GB 存储 + class A（每千次写）10 万次/月 + class B（每千次读）
   1000 万次/月——上传走预签名直传绕开 Worker 请求体限制（免费 100MB）与 CPU 时长；
   下载也走预签名直连。D1 查询预算：`/v1/files` 列表 1 查询 + 分页，`/v1/files/sync` ≤2 查询，
   不得逐文件查询（N+1 会瞬间打爆 50 查询预算）。
4. **文件监听可靠性**：Windows 下监听目录被改名/移动 → watch 失效（重挂 + 对账兜底）；
   文件被其它程序占用（Word/锁）→ 读失败要重试并给「文件被占用」状态而非死循环；
   `Thumbs.db` 等系统文件必须排除，否则永不静默；大目录（数万文件）基线扫描要分批 + 进度反馈。
5. **覆盖安全（数据丢失红线）**：远端改动**默认不自动覆盖**本机文件（应用前确认 + 冲突副本）；
   重命名若实现为「删除+新增」会导致 10MB 重新上传——MVP 可接受（标注后续优化：服务端
   rename-by-id 不重传内容）；Windows 大小写不敏感 vs 云端敏感（`a.txt`/`A.txt` 同目录互撞要预检）。
6. **删除墓碑**：跨设备删除必须靠服务端 `deleted_at` 墓碑 + 对账下发，不能只删 R2 对象
   （否则别机「本机有、远端无」会反向再上传，删除永远不传播）。
7. **状态漂移自愈**：沿用 ops 引擎的教训（AppliedVals/漂移重判/游标不变量）——filesync.json 必须
   保存 `{sha256, mtime, rev}` 三元组并定期对账；本机文件被外部改回旧内容要能重判重传。
8. **登出/换账号**：登出=暂停（清单保留）；换账号=远端清单全变，全量对账前不得自动上传/删除本机
   任何文件（防误删）；账号注销（`DELETE /v1/me`）须级联清 R2 对象（cron 兜底清孤儿）。
9. **UI 约束**：一屏 ≤3 张主卡片（DESIGN.md）——文件卡并入已有页面结构，不新增第 4 张；
   目录树大时默认全折叠 + 渐进披露（不做虚拟滚动也行，10MB 上限天然限制条目数）；
   删除按钮必须区分「移出同步」与「删除本机文件」。
10. **路径安全**：服务端校验路径穿越（`..`/绝对路径/非法字符），R2 key 用 `user_id/hash` 隔离，
    不信任客户端 path；预签名 URL 短时效；所有端点 Bearer 鉴权。
11. **排序细节**：名称排序用 locale 感知（中文拼音）或至少 byte 序稳定；「文件夹在前」是硬分组
    （文件夹组与文件组各自排序后再拼接），不是同一序列里交错；目录大小=子树合计需 Go 侧缓存，
    每次扫描 O(n) 重算在大目录下卡 UI。
12. **重试与断点**：MVP 整文件重试（10MB 上限，重传成本可控）；分块/断点续传（R2 multipart）列为
    后续增强，本轮不做——避免与「简单优先」冲突。
13. **构建纪律**：新增 Go 导出方法后必须 `wails generate module`；构建走 `build.ps1`
    （`wails generate module + build -skipbindings`）；新增 fsnotify 依赖需 `go get` 后核对
    go.sum 与三平台可编译性（darwin 文件本机不编译——平台专属改动要 `GOOS=darwin go vet` 预检，
    既有教训）。
14. **兼容矩阵**：老客户端 × 新服务端 = 无感；新客户端 × 老服务端 = 文件卡按 404 隐藏并提示
    「服务端不支持文件同步」；服务端契约变更同步 API.md/types.ts/测试（冻结契约的红线）。

---

## 6. 建议分批与验证

- **批次 A（服务端）**：R2 + 迁移 + 端点 + 配额事务 + vitest + staging 部署。
  验证：curl/脚本走通 上传→complete→列表→下载→删除→quota 超限 全链路。
- **批次 B（客户端引擎）**：filesync.json + 监听/扫描 + 上传/对账管线 + 配额预检 + Go 单测。
  验证：mock 服务端全场景单测 + 真机（另机）双设备对传。
- **批次 C（UI）**：列表/排序/操作/进度条/弹窗 + i18n + 截图回归。
  验证：shotmode 截图 + 手动点测全交互态（空/失败/冲突/容量不足）。
- 发布：A 先行上线（无 UI 变化），B+C 合并进 dsh-systray v0.8.0。

> 依赖：A 是 B 的前置；B/C 可与 A 并行（B 用 mock 契约开发，联调时切换真实 staging）。
> 待用户确认：第 1 节 D1-D5（尤其 D1 双向 vs 单向、D2 冲突策略、D3 应用确认）。

---

## 批次 A 实施落地（2026-10-05 · dsh-connect 服务端 · 本地全绿、未提交）

**新增**：`migrations/0003_files.sql`（`file_entries` / `files` + `users.tier`）、`src/files/reconcile.ts`
（校验 + 对账纯函数）、`src/files/quota.ts`（档位 → 上限，付费扩容唯一扩展点）、`src/api/files_handlers.ts`
（8 个端点）、`test/files.spec.ts`（14 例）、`test/files-d1.spec.ts`（D1 + 真实 R2 集成）、`test/fake-r2.ts`。

**改动**：`src/types.ts`（`quota_exceeded` / `checksum_mismatch` 两个扩展错误码 + 文件同步类型与常量）、
`src/store/{types,d1,memory}.ts`（文件存储方法 + `purgeExpired` 增 `files`）、`src/api/app.ts`（`FILES` 绑定与路由）、
`src/api/handlers.ts`（注销账号连带清理对象）、`src/index.ts`（janitor 日志）、`src/auth/service.ts`、
`wrangler.jsonc`（`r2_buckets`）、`vitest.config.ts`（miniflare `r2Buckets`）、`worker-configuration.d.ts`
（`wrangler types` 重生成）、`docs/API.md`（端点 13-20 + 错误码 + 查询预算）、`README.md`、
`.github/workflows/deploy.yml`（deploy 前确保 R2 桶存在）。

**与评估方案的差异与关键决策**：
1. 上传经 Worker 转发到 R2（Workers 无 S3 预签名 URL 能力），单文件 ≤10 MiB，sha256 由 R2 `put` 校验和验证；
2. 对账端点**只读**：本机删除由客户端另行调用 `DELETE /v1/files/objects` 传播（服务端无法区分
   「本机没有」与「本机删了」）；
3. **对象 key 每次写入唯一**（`…@随机后缀`）：先写新对象 → 元数据提交成功 → 删旧对象。若按路径固定 key，
   「写入 → 配额拒绝 → 补偿删除」会把线上文件内容一并删掉（本地用例已暴露该数据丢失路径，故改设计）；
4. 配额闸门压成**单条 SQL**（`WHERE 已用(排除本行) + 本文件 <= limit` 同时约束 INSERT 与 UPSERT 分支），
   D1 无交互式事务下并发上传不会双双放行；替换文件排除本行旧大小，避免假性超限；
5. 墓碑保留 30 天（`FileTombstoneRetentionSec`），janitor 清理；条目删除=硬删（跨设备靠「服务端没有」收敛）；
6. 付费扩容只预留 `users.tier` 字段与 `limit` 返回，扩展点只有 `src/files/quota.ts`。

**验证**：`npx tsc --noEmit` 0 错误；Node 环境 110 用例全绿（含文件同步 14 例）；
D1/R2 集成用例本机 workerd 无法启动（miniflare 建目录被拒，既有环境限制）留待 CI；
作为替代用 `node:sqlite` 对关键 SQL 做了 15 项断言全通过
（脚本：`scripts/dsh-systray数据同步功能需求评估/verify-files-sql.mjs`）。

**未做**：git 提交与推送——push main 会触发生产部署（含 D1 迁移与新建 R2 桶），需用户确认后再执行。

---

## 批次 B 实施落地（2026-10-05 · dsh-systray 客户端引擎 · 本地全绿、未提交）

**新增**：
- `src/account_files_store.go`：服务端契约类型（逐字对齐端点 13-20）+ 本机清单 `filesync.json`（0600 原子写，
  与 account.json 同目录）+ 状态视图（`FileSyncStatusInfo` / 条目树 / 文件行）+ 接收目录与路径解析；
- `src/account_files_client.go`：8 个端点的客户端（普通调用走 `c.do`；文件内容走新增的 `c.doRaw`，
  2 分钟超时、二进制体、业务码仍按 `error.code` 分支）；
- `src/account_files.go`：引擎——扫描（排除 Thumbs.db/~$ 临时文件/回收站、跳过符号链接与接收目录）、
  摘要缓存（size+mtime 未变不重算）、增量上传、删除传播、对账动作过滤、应用（下载/冲突副本/删除/改名/移除）、
  并发闸门、`files:changed` 事件与后台循环（60s 本地扫描 + 5 分钟远端对账节流）；
- `src/account_files_bindings.go`：Wails 绑定 **8 个**（`FilesStatus` / `FilesAdd` / `FilesRemoveEntry` /
  `FilesRemovePath`（条目内文件或子目录，云端打墓碑 + 可选删本机）/ `FilesRenameEntry` /
  `FilesOpenEntry` / `FilesApplyPending` / `FilesSyncNow`）+ 嵌套防护 + 名称校验（含 Windows 保留名）+
  危险删除保护（拒卷根/用户主目录/接收目录本身）；
- `src/account_files_test.go`：15 个用例（扫描与排除、删除台账与恢复、单文件条目、动作过滤、
  上传/删除传播端到端（假服务端校验 sha 与配额）、容量拦下、下载与冲突副本、mtime 保持、
  远端删除的源/接收设备差异、条目改名与移除、重叠检测、名称规则、冲突副本命名、删除保护、快照视图、清单往返、启动重判）。

**改动**：`src/account.go`（`fileHTTP` 传输客户端 + 3 个扩展错误码）、`src/account_runtime.go`
（登录后触发一次文件同步；**换账号清空文件同步清单**，避免新账号对账把本机条目误判为「服务端已删除」而删副本）、
`src/main.go`（启动载入清单 + 启动后台循环）、`src/i18n.go`（24 条双语）、
`src/platform_{windows,darwin}.go`（`openFile`：ShellExecuteW / `open`）。

**关键实现决策**：
1. **不加 fsnotify 依赖**：后台每 60 秒扫描本机（只 stat，size+mtime 未变不重算摘要），有变更或距上次
   对账 5 分钟才联网——比文件系统监听简单且跨平台一致；懒轮询的开销在 10 MiB 量级可忽略；
2. **远端删除的源/接收差异**：接收设备删本地副本；**源设备不删用户原文件**，改为记入 `Ignored`
   （文件被改动后自动重新同步），避免远端删除误删用户原件——委托删除一律需要用户在前端二次确认；
3. 下载后 `Chtimes` 保持服务端 mtime，避免下次扫描把刚下载的文件误判为「本机改动」再传回去；
4. 冲突判定：本机有未上传改动且服务端要覆盖 → 先另存 `名字 (冲突-本机).ext` 再写入；
5. 容量预检扣除该文件上次占用（`SyncedSize`），服务端单语句闸门仍是权威。

**验证**：`go build ./...`、`go vet ./...` 干净；`go test ./... -count=1` 全绿（**77.9s**，含新增 15 个文件同步用例）。

**未做**：前端文件卡（批次 C）、`wails generate module` 重新生成前端绑定、发布清单。

---

## 批次 C 计划（下一步执行，UI 结构已定）

**列表口径**：列表行 = 用户添加的**条目**（文件夹/文件）；文件夹条目可展开显示条目内文件树
（子目录行的操作为展开/折叠与「打开」，重命名走条目级——跨设备改子目录相对路径会牵动全部 relPath，
列为后续增强）。

1. `index.html`：`page-sync` 内新增 `sync-files` 卡（仅登录时显示，保持一屏 ≤3 张卡）：
   标题/说明 + 「添加文件」「添加文件夹」「立即同步」+ 容量进度条（复用现有 `progress` 组件）+
   待应用块（计数 + 「应用改动」）+ 排序工具条（名称/大小/修改时间）+ 树容器 + 空态 + hint。
2. `main.js`：`refreshFiles()`（调 `FilesStatus`）/ `renderFilesCard()` / 树渲染
   （文件夹优先 + 三级排序 + 展开态记忆）/ 行内操作（打开、重命名、删除）/ 容量不足弹窗 /
   `EventsOn("files:changed")` 订阅 / `wireFiles()` 绑定按钮。
   删除交互：条目删除用 `confirmDialog3` 三选（同时删除本机文件 / 仅移出同步 / 取消）；
   文件与子目录删除用 `confirmDialog` 二次确认（文案说明会传播到其它设备）。
3. `style.css`：`.files-*` 样式全部走 DESIGN.md token（行、层级缩进、状态徽标、工具条、空态）。
4. i18n：静态键进 `I18N_EN` + `data-i18n`；动态文案进 `I18N_DYN`，跑 `scripts/check-frontend-i18n.mjs`。
5. `wails generate module` 重新生成 `frontend/wailsjs`（新增 8 个绑定）。
6. 验证：i18n 检查脚本 + 构建（`scripts/build.ps1`）+ shotmode 截图（数据同步页，含容量条与列表）。

---

## 批次 C 实施落地（2026-10-05 · 前端文件卡 · 本地校验通过、未提交）

**改动**：
- `src/frontend/dist/index.html`：数据同步页新增 `sync-files` 卡（标题/说明 + 添加文件/添加文件夹/立即同步 +
  容量进度条 + 待应用块 + 排序工具条 + 树容器 + 空态 + hint）；新增「重命名条目」输入弹层
  `files-rename-modal`（原生 prompt 在 WebView 不可靠，用既有 modal 体系自绘）；
- `src/frontend/dist/main.js`：文件卡渲染与交互约 300 行——容量条（80% 转 warn、满转 danger）、
  条目树（`relPath` 聚合出目录层级，目录聚合大小/时间/状态取最差）、排序（文件夹优先 + 名称/大小/修改时间，
  再次点击切换升降序，标签语言安全）、展开态记忆、行内操作（打开/重命名/删除/移除）、
  两档删除确认（`confirmDialog3`：同时删除本机文件 / 仅移出同步 / 取消）、容量不足弹窗（数量变化才弹）、
  `files:changed` 与 `account:changed` 事件刷新、页面切换与启动时加载；I18N_DYN 新增 31 条动态译文、
  I18N_EN 新增 14 条静态译文；
- `src/frontend/dist/style.css`：`.files-*` 全套样式（进度条 8px 胶囊、行 hover、层级缩进 `--depth`、
  状态徽标三态、危险操作色），全部走 DESIGN.md token；
- `src/frontend/wailsjs/**`：手工补齐 8 个 `Files*` 绑定（`App.d.ts` / `App.js` / `models.ts` 三个模型类）。

**环境事实（本机限制，已记录）**：
1. `wails generate module` 在本机**静默失败**（exit 0、无输出、不写文件；`-v 2` 亦然）——前端运行时经
   `window.go.main.App` 调用，不受影响；且 `src/frontend/wailsjs/` 已 gitignore（本地产物，不进仓库），
   故对仓库无影响（本地绑定已按生成器同形手工补齐，便于编辑器识别）；
2. 本会话的托盘应用正在运行（PID 27468，即本会话宿主），**不能跑 `scripts/build.ps1`**（脚本会先杀
   `dsh-systray` 进程），也不能起第二个实例（单实例互斥体先于 shotMode 生效，会弹「已在运行中」退出）；
   **但截图不需要托盘**：`scripts/render_shots.mjs` 用无头 Edge + `scripts/shot-shim.mjs` 演示桩直接渲染
   `src/frontend/dist`（不启动 exe、不占托盘），因此截图回归已在本机完成（见下）。

**验证**：`node --check main.js` 通过；`node scripts/check-frontend-i18n.mjs` 通过
（125 条静态译文 / 247 条动态译文 / 141 个元素 id，0 条告警）；`go build`/`go vet`/`go test ./...` 全绿；
**非破坏性 Wails 构建**成功（`wails build -skipbindings -o dsh-systray-verify.exe`，23.7s，产物已删除，
未触碰运行中的 dsh-systray.exe）。

### 补充验证（第二轮，均在会话宿主机上可做）

1. **前端文件卡纯函数单测**（Node + 最小 DOM 桩加载 main.js，30 项断言全过）：
   容量换算（999 B / 1 KB / 1.2 KB / 999 KB / **999950 → 1 MB 进位** / 10.5 MB / 非法输入）、
   目录树聚合（大小合计、文件数、时间取最新、状态取最差、深层递归）、
   排序（名称升降序、大小降序、时间升序，**目录恒在文件前**）、
   行渲染（名称 HTML 转义、条目/子目录/文件各自的操作按钮、层级缩进 `--depth`、
   条目展开→顶层文件可见、子目录未展开→内容不渲染、再展开→可见更深一层）。
   该测试当场抓出一个真实缺陷：`filesFmtSize` 在 `999950 B` 时四舍五入成 `1000 KB`（应为 `1 MB`），已修（进位保护）。
2. **客户端↔服务端线格式契约单测**（Go）：`fileSyncRequest` 字段（entries/files 与条目/文件字段名）、
   服务端对账响应（6 类动作 + quota，形状取自 docs/API.md 端点 17 示例）、上传响应、对象 query 参数
   （`entryId` + `relPath`，含空格转义）逐项钉住——字段名写错只会在真机联调暴露，这里提前拦住。
3. **macOS 侧预检**：`internal/systray` 的 darwin 实现依赖 cgo（本机无 darwin 交叉编译器，`GOOS=darwin
   CGO_ENABLED=0 go vet` 在该包报 undefined——既有性质，与本次改动无关）；改为人工核对
   `platform_darwin.go` 的依赖符号：`errors` / `strings` / `os` / `os/exec` 均已导入，
   `trackChildProcess` 与 `openDir` 同文件既有用法一致（避免历史上「平台专属文件遗留未用导入」那类 macOS 编译失败）。
4. **截图与预览已更新**（无需托盘）：
   - `scripts/shot-shim.mjs` 新增 `DEMO.files` 演示数据与 8 个 `Files*` 绑定桩（与前端同口径，
     脱敏：虚构条目、`C:\Users\demo` 路径、固定时钟）；
   - `node scripts/build_mock.mjs` 重生成站点实时预览 `docs/mock/`（项目约定：前端改动后必跑）；
   - `node scripts/render_shots.mjs --lang zh|en --only sync` 重渲染 + `python scripts/convert_webp.py`
     转 WebP：`docs/shots/sync.webp`、`docs/shots-en/sync.webp` 已更新（文件卡与容量条入镜）。
   - 列表全貌可在浏览器打开 `docs/mock/index.html?page=sync&lang=zh` 查看（mock 与截图同一套演示数据）。
5. **项目回归脚本**：新增 `scripts/check-frontend-files-card.mjs`（元素 id 检查 + 30 项纯函数断言），
   与既有 `scripts/check-frontend-i18n.mjs` 并列，前端再改文件卡时可一键回归。
6. **错误路径补齐**（第四轮）：客户端新增 5 个用例——单文件上传失败不拖垮整批且下次重试清错、
   应用失败项保留待应用（可重试）、服务端本就无该文件时删除台账清空、`fileSyncAddPath` 的
   嵌套/父目录/同名后缀/接收目录四类校验；服务端新增 2 个用例——**墓碑经对账下发 `remove_file`**
   （跨设备删除收敛的关键契约）与对账入参校验（条数超限/路径穿越/sha 非法/条目名非法 → 400）。
   运行结果：客户端 `go test ./...` 77.9s 全绿（文件同步 25 个用例）；服务端 `tsc --noEmit` 0 错误、
   非 D1 用例 112 个全绿。


---

## 发布清单（待用户确认后执行）

**1. dsh-connect（服务端，先上线）**
```bash
git -C plugins/dsh-connect add -A && git commit -m "feat(files): file and folder sync endpoints" && git push origin main
```
- push main 触发 CI（`npm ci` → `typeclean` → `npm test`，含新增 D1+R2 集成用例）；
- deploy job 自动执行 `wrangler d1 migrations apply dsh-connect --remote` →
  `wrangler r2 bucket list | grep || wrangler r2 bucket create dsh-connect-files` → `wrangler deploy`；
- 上线后冒烟：`GET /v1/healthz`、`GET /v1/files/quota`（带令牌）、上传/下载/删除各一次。

**2. dsh-systray（客户端，服务端上线后再发）**
> 版本号以实际 tag 序列为准：当前最新已发布 tag 为 **v1.2.4**，本次为功能新增 → **v1.3.0**
> （目标文本里的 v0.8.0 是早期假设，已过时）。
- 本机无法用 `build.ps1`（会话宿主托盘在跑）：在非会话宿主机器上执行
  `.\scripts\build.ps1 -Version v1.3.0`；
- 截图回归已完成（`docs/shots/sync.webp`、`docs/shots-en/sync.webp` 与 `docs/mock/` 均已更新，见上）。
- `docs/RELEASE_NOTES.md` 已预置 `## v1.3.0` 区块（CI 取该区块作为 Release 正文）；
- `git add -A && git commit -m "feat(sync): file and folder sync"` → `git tag v1.3.0` → `git push origin main --tags`；
- CI 自动三平台构建 + 建 Release + 同步 README（既有流程）。

**3. 双机验收（关键路径）**
- A 机添加文件夹 → B 机登录同一账号 → 点「应用改动」→ 文件落到 `文档\DeepSeekSync\<条目名>\`；
- B 机改动该文件 → A 机同步后看到「待上传/已同步」状态正确；
- A 机删除某文件（选「同时删除本机文件」）→ B 机同步后副本消失；
- 灌满 10 MB → 再添加文件 → 弹出「可用容量不足」且状态标红；删除后自动恢复；
- 冲突：两端各改同一文件（一端离线）→ 应用后出现「(冲突-本机)」副本。





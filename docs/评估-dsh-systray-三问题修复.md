# 评估：网站轮播英文图不切换 + README 原地中英切换 + 0.8.0 默认英文/切中文卡 splash

> 2026-09-07 · 需求来源：用户提出三项改进的可行性评估（步骤 + 关键问题点）
> 现状代码基准：main @ 5552ebe（v0.8.0 已发布，i18n 双语骨架已落地）

## 0. 结论摘要

| # | 问题 | 根因（一句话） | 修复量级 | 建议顺序 |
| --- | --- | --- | --- | --- |
| 1 | 网站中文切 EN 后轮播仍是中文截图 | `show()` 的 `useLoaded` 快路径：zh 图已缓存时永远优先复用 zh，EN 图从未被加载；且 `applyLang` 只刷文案不重选当前图 | 小（index.html 内约 10 行） | ① |
| 2 | README 无跳转中英切换 | GitHub README 是静态消毒渲染（无 JS/无 style），按钮式互斥切换不可实现；最接近方案 = 双语文案合并进单文件 + `<details>` 原地折叠 | 中（纯文档合并） | ② |
| 3 | 升级 0.8.0 后默认英文；设置切中文卡 splash 页 | 两个独立根因：A) 旧 config.json 无 `language` 字段 → 缺省 `auto` → 跟随系统 → 用户系统 UI 语言为英文 → 默认 en；B) 前端 `lang:changed` 走 `location.reload()`，reload 后 init 无条件显示 splash，且进入设置视图只依赖 `splash:done`/`ui:show-settings` 两个事件（reload 不会触发）→ 永久卡 splash。**语言其实已切换成功（config 已落盘 zh），只是界面卡死** | 小（Go 默认值 1 处 + 前端切换逻辑 ~30 行） | ③ |

---

## 1. 问题 1：网站中→EN 切换后轮播图不换英文截图

### 1.1 现状与根因（`docs/index.html`）

- `shots` 数组（L373-380）已含 `en: 'shots-en/...'` 与 `zh: 'shots/...'` 两套路径，`docs/shots-en/` 六张英文截图已存在——**资产就绪，逻辑没接上**。
- `show(i)`（L428-441）的选择逻辑：
  ```js
  var useLoaded = (lang === 'en' && loaded[s.en]) || loaded[s.zh];
  if (useLoaded) { showShotSrc(lang === 'en' && loaded[s.en] ? s.en : s.zh); return; }
  ```
  触发链路：页面以中文打开 → 轮播转一圈 → 六张 zh 图全部进入 `loaded` 缓存 → 用户点 EN → `loaded[s.zh]` 恒为 true → `useLoaded` 恒 true → 永远显示 zh 图，`shots-en/` **从未被请求**。EN→中方向反而正常（zh 未缓存时会走 `pickShot` 加载 zh），与用户报告方向一致。
- `applyLang`（L320-343）切换时只调 `updateCarCaption(true)` 刷新气泡文案，不重选当前图片；即使 `show()` 修好，当前这张也要等 4.5s 自动轮播才换。

### 1.2 实施步骤（每步含验证）

1. **修 `show()` 的选择逻辑**：删掉 `useLoaded` 快路径，改为「按当前语言取目标图，已缓存直接用，未缓存走 `pickShot`（EN 缺图自动回退 zh）」：
   ```js
   var s = shots[cur];
   var want = (lang === 'en') ? s.en : s.zh;
   if (loaded[want]) { showShotSrc(want); return; }
   pickShot(cur, function (src) { if (my !== seq || !src) return; carImg.src = src; carImg.alt = curCaption(); });
   ```
   **验证**：中→EN 点击后下一次轮播（或手动箭头）即显示英文图；EN→中同理回中文图。
2. **`applyLang` 末尾补 `show(cur)`**：语言切换后立即重选当前页图片（EN 图加载失败仍自动回退 zh，`pickShot` 已有该机制）。
   **验证**：点击 EN 的瞬间当前大图即换成 `shots-en/general.webp`，无需等轮播；英文浏览器首访时首图即英文（顺带修复此前首图恒为中文的瑕疵）。
3. **顺带加固 `pickShot` 错误路径**（可选）：`im.onerror` 分支对每个排队回调都会触发一次 `next(k+1)`，多回调时重复请求回退图；改为失败结果缓存（`loaded[src] = false`）或只触发一次 fallback。
   **验证**：断网/删除某张 shots-en 图时，该 slide 回退 zh 且不重复发请求。
4. **发布**：`docs/` 是 GitHub Pages 源，push main 即生效；本地验证用静态服务器（`python -m http.server` 或 pwsh `Start-Job` 均可），版本徽章 fetch 失败可忽略。

### 1.3 关键问题点

- `loaded` 缓存键是真实 URL，两套语言互不污染；`seq` 竞态保护已存在（异步加载返回时 `my !== seq` 直接丢弃），改动不动这套机制。
- 不要改成「切换时清空 `loaded`」的方案：会让已看过的图重新闪烁加载；按需试加载 EN + 回退 zh 才是低开销路径。
- EN 图缺文件时回退 zh 图属**可接受过渡态**（评估前文已确认），本次不涉及重拍截图。

---

## 2. 问题 2：README 不跳转的中英切换

### 2.1 硬约束（先说清楚）

- GitHub 仓库首页 README 是**静态 Markdown 消毒渲染**：自定义 JS、`<style>` 标签都会被剥掉，无法实现按钮式互斥切换（点 EN 自动收起中文）。任何「无跳转真切换」承诺在 GitHub 页面上都做不到。
- 站点 `docs/index.html`（GitHub Pages）**有 JS 能力**，已有中/EN 切换器；真按钮式切换只能落在站点上（方案 B）。

### 2.2 方案 A（推荐）：合并单文件 + `<details>` 原地折叠

GitHub 允许 README 使用 `<details>/<summary>`，点击 summary 原地展开/收起对应语言块——这是 GitHub 上**不离开当前页面**的最接近方案。

1. **合并结构**：`README.md` 顶部保留共享区（logo/徽章/hero 图各一份，语言无关），语言切换行由跨文件链接改为锚点/折叠入口：
   ```html
   <p align="center"><a href="#简体中文">简体中文</a> · <a href="#english">English</a></p>
   ```
   正文拆成两个块：
   ```html
   <details open>
   <summary>简体中文（点击收起/展开）</summary>

   …（原 README.md 正文，从系统要求到结尾）…

   </details>

   <details>
   <summary>English (click to collapse/expand)</summary>

   …（原 README.en.md 正文）…

   </details>
   ```
2. **合并后删除 `README.en.md`**（或保留一个指向 README.md 的存根，避免外部旧链接 404——建议直接删除，互链行已移除）。
3. **CI 无影响**：v0.8.0 已「README 去包大小列」，release.yml 的 sizes 正则同步步骤已移除（仅残留 checkout 步骤名 `for README size sync`，可顺手改名），合并不破坏发版。
4. **验证**：push 后 GitHub 首页目测——点 summary 原地展开/收起；两语言表格/引用/代码块渲染正常；`docs/RELEASE_NOTES.md` 历史文字不改动。

### 2.3 方案 B（可选，真按钮式）：站点加 README 页

在 `docs/index.html`（或新增 `docs/readme.html`）加「文档」页签：fetch `raw.githubusercontent.com/refyon/dsh-systray/main/README.md` + 轻量 md 渲染器（marked.js），复用现有中/EN 切换器互斥切换。成本高：引入第三方渲染库、网络失败态、维护两份内容源；**不推荐本期做**。

### 2.4 关键问题点

- **两语言并存于单文件后维护成本不变**（改任何功能说明都要同步两段），合并主要收益是「同一页面切换」体验，需用户确认接受。
- `<details>` 内 Markdown 必须与标签之间**空行分隔**（GitHub 渲染规则），否则表格/标题解析异常；合并后逐段目测。
- GitHub 右上角浮动目录对 `<details>` 内标题的收录行为需发布后确认（不影响主阅读流，标记为可接受差异）。
- npm 等镜像站 README 渲染器对 `<details>` 支持度不一（npmjs 支持），项目未发布 npm 则无影响。
- 顶部共享 h1/徽章只保留一份；两语言块内不再重复 logo 头部，避免页面过长。

---

## 3. 问题 3：升级默认英文 + 切中文卡 splash（两个独立根因）

### 3.1 根因链

**A) 默认英文**
1. ≤0.7.3 无 `language` 概念，旧 `config.json`（`%APPDATA%\dsh-systray\config.json`）没有该字段；0.8.0 也未在升级时写入默认值。
2. `loadConfig`（main.go L162）读不到 → `Language` 空串；main.go L353 `langPref = normalizeLang("") = "auto"` → `resolveLang` → `detectSystemLang()`。
3. Windows 侧 `detectSystemLang`（platform_windows.go L26-37）用 `GetUserDefaultUILanguage` 的 LANGID 判主语言：`r&0x3FF==0x04` → zh，其余 → en。**该实现本身正确**（zh-CN 0x0804、zh-TW 0x0404 均命中 zh；失败时兜底 zh）。
4. 结论：用户机器的 Windows UI 语言（或 mac `AppleLanguages` 首选）是英文 → auto 解析为 en。这是「跟随系统」的设计行为，但产品是中文优先、旧版全中文，升级后静默变英文属观感回归。

**B) 切中文卡 splash**
1. 设置下拉 change → `SetLanguage`（app.go L131-146）：Go 侧 **正常完成**——langPref/curLang 更新、`saveCurrentConfig` 落盘、emit `lang:changed`。语言已持久化，重启/重开窗口即为中文。
2. 前端 handler（main.js L1431）：`EventsOn("lang:changed", () => location.reload())`。
3. reload 后 `init()`（L1603）无条件 `showSplash("startup")`；切到设置视图只依赖两个事件——`splash:done`（启动完成一次性发出）与 `ui:show-settings`（托盘点击「设置」时发出）。**reload 不触发任何事件** → 永久卡 splash（L338 的 `refreshService` 兜底切页只在截图模式生效）。
4. 用户视角「切换没成功」，实为界面卡死；语言实际已切成功。

### 3.2 实施步骤（每步含验证）

**A 修复默认语言（升级无感）**
1. main.go L353 改为：**空值缺省 zh，显式 auto 才跟随系统**：
   ```go
   if cfg.Language == "" { langPref = "zh" } else { langPref = normalizeLang(cfg.Language) }
   curLang = resolveLang(langPref)
   ```
   语义：旧版本升级用户、全新安装默认中文；用户显式选「跟随系统/English」后按显式值生效并落盘。i18n.go 顶部注释同步更新。
   **验证**：删除 config.json 中 `language` 字段后启动 → 中文；配置里写 `"language":"en"` 启动 → 英文；写 `"auto"` + 英文系统 → 英文。
2. **诊断日志**：启动处加一行 `log.Printf("[i18n] pref=%q system=%s → curLang=%s", cfg.Language, detectSystemLang(), curLang)`，供后续用户报障时一眼定位。
   **验证**：日志页可见该行；用户机器上先请跑 `Get-UICulture`（Windows）确认系统 UI 语言为英文，排除检测异常可能。
3. 已升级机器若 config.json 已被 0.8.0 写过 `"language":"auto"`（改过任意设置即会写），空值迁移不覆盖——此类用户修好 B 后在下拉选一次「简体中文」即永久落盘，无需特殊迁移脚本。

**B 修复卡 splash（两选一，推荐 A'）**
- **方案 A'（推荐）就地切换、无重载**：
  1. `init()` 开头（首次 `applyStaticI18n` 之前）对全部 `[data-i18n]`/`[data-i18n-ph]` 元素快照原始 zh innerHTML 到 `ZH_SNAP` 字典。
  2. `applyStaticI18n` 改双向：en → `I18N_EN[k]`；zh → `ZH_SNAP[k]` 还原（当前 zh 分支提前 return，无法回切，必须改）。
  3. `lang:changed` handler 替换 `location.reload()`：用事件 payload 更新 `state.cfg.language/curLang`、同步 `#sel-lang` 选中值，然后 `applyStaticI18n(); rerenderDynamicText(); showPage(state.page);`。
  **验证**：中↔EN 往返多次即时生效、无重载无 splash；五页走查无残留；下拉值与托盘生效语言一致。
- **方案 B（最小改动）保留重载、加切换标记**：切换前 `sessionStorage.setItem('dsh-lang-swap','1')`；`init()` 在 `refreshConfig` 后检测到标记 → `showSettings()` + 清标记。仅 3 行，但有整页闪动。
  **验证**：切换后直接回到设置页（原页面），语言生效。

**C 附带缺口（本次一并决策）**
- 托盘菜单（main.go L871-880）在 `onReady` 一次性构建，`SetLanguage` 不重建 → 切换后**托盘右键菜单/tooltip 仍旧语言直到重启**。vendored systray 的 `SetMenuNil` 仅 darwin/other 有实现，Windows 无删除项 API。选项：①扩展 vendored systray（Windows 侧 `SetMenuItemInfoW` 封装已有，加 SetMenuItemTitle/重建，成本中等）；②tooltip 即时更新（`SetTooltip` 已有）+ 菜单下次启动生效，设置页文案注明；③本期接受重启生效。建议 ②，① 留待后续。

### 3.3 关键问题点

- **默认值语义是本问题的产品决策**：空缺省 zh（推荐，升级无感、贴合中文用户群）vs 维持 auto（英文系统用户永远要手选一次中文）。评估推荐前者，需用户确认。
- **截图管线不受影响**：`capture_shots.ps1` 直接写 config 的 `language=en` 拍英文图、拍完还原，与默认值改动无交集；`-Lang zh` 默认行为不变。
- 方案 A' 的快照必须早于首次 `applyStaticI18n`（init 顺序敏感）；已通过 `fmt()` 渲染进 DOM 的插值句不在快照范围，靠 `rerenderDynamicText` 重渲染块级内容；瞬时 toast 保持旧语言可接受（下次动作即新语言）。
- 方案 B 的标记用 `sessionStorage`（窗口生命周期）而非 `localStorage`，避免残留污染后续启动。
- 已发出的 splash 进度文案/原生弹窗不回译（历史串），与「切换后生效」语义一致，文档注明即可。
- 回归面：`go vet ./...` + `go test ./...`（i18n 无单测，靠手工走查）；前端改动直接编辑 `frontend/dist/*`（无源码构建链，仓库惯例）；发布前 `wails build` 编译验证 + 设置页中↔EN 冒烟。版本建议 v0.8.1（补丁）。

---

## 4. 总体实施顺序与测试清单

1. 问题 1（网站 10 行，独立收益）→ 问题 3（应用语言，用户痛点最重）→ 问题 2（README 合并，纯文档）。
2. 测试清单：①轮播 zh→en→zh 即时换图 + EN 首访首图英文 + shots-en 删图回退；②升级机（无 language 字段）默认中文 + auto/en 显式值生效 + 下拉切换即时生效不卡 splash + 托盘菜单行为按决策验证；③GitHub README details 展开/收起与表格渲染、旧 README.en.md 链接处理。
3. 待用户决策：
   - 3a 默认值：空缺省 zh（推荐）还是维持 auto；
   - 3b 托盘菜单：重启生效（推荐过渡）还是扩展 systray 运行时重建；
   - 2 README 是否接受 `<details>` 折叠方案（GitHub 无 JS 的硬约束下），还是做站点 README 页（成本高）。

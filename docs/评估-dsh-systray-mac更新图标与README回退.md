# 评估：mac 更新报「缺少 dsh-systray.app」+ mac 图标异常 + README 改 HTML/回退链接版

> 2026-09-07 · 需求来源：用户提出三项改进的可行性评估（步骤 + 关键问题点）
> 现状代码基准：main @ 37306f3（三问题修复已提交，未发布）；已下载 GitHub 最新 Release 资产与 v0.7.3 资产逐字节取证

## 0. 结论摘要

| # | 问题 | 根因（已实证） | 修复量级 | 建议顺序 |
| --- | --- | --- | --- | --- |
| 1 | mac 检查更新报「更新包中缺少 dsh-systray.app」 | **CI 打包路径多一层 `dist/`**：`zip -r artifact "dist/dsh-systray.app"` → 包内条目为 `dist/dsh-systray.app/...`；更新器 `updatePayloadPath`（updater.go L1422）只认解压目录根级 `dsh-systray.app`。实测 0.7.3/0.8.0 两个 Release 的 mac zip 均如此（Windows zip 根级 exe，正常）→ mac 自更新自该功能上线起从未成功 | 小（CI 1 行 + 可选防御） | ① |
| 2 | mac app/Dock 图标异常 | 0.8.0 引入 e5058ae「鲸鱼收小 0.94 + 托盘 glyph 0.9→0.95」：实测 build/appicon.png 鲸鱼内容占比 85.6%→80.6%（留白变大），0.7.3 与 0.8.0 的 iconfile.icns 字节不同（147696B vs 73879B）。「异常」极可能是观感（变小/留白大）或手动替换 .app 后 LaunchServices 图标缓存未刷新；非资源损坏（icns 完整存在于包内） | 小（回退/重调一个文件 + 发布） | ② |
| 3 | README 改 readme.html 点击切换；不支持则回退链接跳转版 | GitHub **不支持** HTML 格式 README（官方支持的标记格式清单不含 HTML；社区有专门 feature request discussion #109580）；README.md 内嵌 HTML 也会被消毒（script/style 剥除）→ 按用户条件**回退到上一版链接跳转双文件**（37306f3~1 的 README.md + README.en.md） | 小（git 恢复 2 文件） | ③ |

---

## 1. 问题 1：mac 更新失败「更新包中缺少 dsh-systray.app」

### 1.1 取证（全部实测）

- **包布局**：`dsh-systray-macos-universal.zip`（latest = v0.8.0）内条目：
  `dist/dsh-systray.app/Contents/...`（9 个条目，.app 在 `dist/` 子目录内）；v0.7.3 同布局。
  Windows zip 条目 = 根级 `dsh-systray.exe`（Compress-Archive 单文件行为），所以 Windows 无此问题。
- **更新器**：`extractUpdateZip`（updater.go L1398-1411）mac 用 `ditto -x -k` 原样还原目录结构 →
  解压后是 `extractDir/dist/dsh-systray.app`；`updatePayloadPath`（L1422-1426）只 `os.Stat(extractDir/dsh-systray.app)` →
  必然报错「更新包中缺少 dsh-systray.app」，前端弹「更新失败：…请稍后重试」（platform_darwin.go L758 拼接）。
- **历史**：`updatePayloadPath` 自 b0d6907（自更新功能）起就要求根级；CI 的 `zip … "dist/dsh-systray.app"` 写法至少从 v0.7.1 延续至今 → **mac 自更新从未成功过**，本轮才被用户实测发现。

### 1.2 实施步骤（每步含验证）

1. **CI 打包改根级（主修复）**：`.github/workflows/release.yml` Package archive 步骤（L83）：
   ```bash
   (cd dist && zip -r -y "../${{ matrix.artifact }}" "dsh-systray.app")
   ```
   不改 Windows 分支。
   **验证**：本地用 zip CLI 复现新命令 → 包内首级即为 `dsh-systray.app/`；下一 tag 发版后下载资产复核条目列表与 SHA256SUMS。
2. **更新器加防御（次修复，推荐）**：`updatePayloadPath` 改为「根级命中 → 否则 `filepath.Glob(extractDir/*/dsh-systray.app)` 取第一个匹配」（Windows 同样对 exe 做一层兜底）。这样即使未来打包再带前缀也不复发，并让旧坏包在开发环境也能走通。
   **验证**：用已下载的 0.8.0 坏包构造用例：解压 → payload 定位到 `dist/dsh-systray.app`；`go test ./...` 全绿。
3. **（可选）存量资产救急**：为 v0.8.0 Release 重传正确的 mac zip（重新打包去掉 dist/ 前缀 + 更新 SHA256SUMS.txt）——需要 gh CLI/用户 token。**价值低**：0.8.1 发布后，0.7.x 与 0.8.0 用户「检查更新」下载的都是 latest 资产（即 0.8.1 修复包），可直接自更新成功，无需动 0.8.0 资产。
4. **发版**：按既有流程（RELEASE_NOTES 区块 → wails build → push tag）发 v0.8.1；发布后 mac 实机走「检查更新→确认→替换重启」全链路。

### 1.3 关键问题点

- 修复 CI 只影响**之后**的资产；存量 0.8.0 坏包不影响后续自更新（见步骤 3 说明），不必强改。
- `ditto -x -k` + `zip -y` 保留符号链接/权限（CI 已用 `-y`，改动后保持）。
- 更新失败路径已有回滚/清理（`cleanupStaleUpdateFiles` 清 `dsh-systray-update-*` 临时目录），修复后无需额外清理逻辑。
- 报错文案前后端/多语言已就绪（i18n 映射「更新失败，正在回退到上一可用版本…」等），不改文案；若加防御路径，保持错误信息与弹窗行为不变。

---

## 2. 问题 2：mac app / Dock 图标显示异常

### 2.1 现状与量化

- **图标管线**（见记忆条目）：Dock/Finder 图标 = `build/appicon.png` → Wails 生成 `iconfile.icns`（Info.plist `CFBundleIconFile=iconfile`，已核对 0.8.0 包内 plist 正确、icns 完整 73879B）；菜单栏图标 = `icon_gen.go` 的 `iconDataTemplate`（44px 纯黑鲸鱼）+ `systray_darwin.m` 缩放。
- **0.8.0 引入的变更（e5058ae）**：app/Dock 鲸鱼缩至 0.94（实测内容 bbox 85.6%→**80.6%**、留白明显变大）；菜单栏 glyph 0.9→0.95（变大）。
- **新旧产物对比**：v0.7.3 icns 147696B vs v0.8.0 73879B（内容确变，非同一图标）；均无损坏（icns/plist 完整）。
- **【已确认——用户截图 2026-09-07】**：Launchpad/Dock 中 dsh-systray 图标近乎空白、只剩极淡轮廓。像素定量分析（System.Drawing，步长 2 采样 + 逐像素）：
  | 项 | appicon-old（0.7.3，e5058ae~1） | appicon-new（0.8.0，e5058ae） |
  | --- | --- | --- |
  | 不透明像素占比 | ≈18.6%（186328 采样） | **≈0.65%（1641 采样）** |
  | 平均 alpha | 255（完全实心） | **139（高透明）** |
  | 平均 RGB | (86,123,226) 蓝 | (88,125,188) 蓝偏淡 |
  | 结论 | 实心蓝色鲸鱼（可见） | **只剩高透明细轮廓（近乎不可见）** |
- **根因**：e5058ae 的图标改动把 Wails 图标源 `build/appicon.png` 变成「高透明薄鲸鱼轮廓」，生成 icns 后 Dock/Finder 几乎白屏。注意区分：`app-icon.png`（根目录，restyle-appicon.mjs 产出，品牌蓝圆角底+白鲸鱼，实心可见）与 `build/appicon.png`（Wails 图标源，被改坏）是两套资源；旧版 0.7.3 的 build/appicon.png 是实心蓝鲸（可见）。**非图标缓存、非单纯「变小/留白」——是主体失效。**
- 菜单栏模板 glyph 0.9→0.95 是另一套资源（icon_gen.go / systray_darwin），与截图无关，除非用户菜单栏也异常才动。

### 2.2 实施步骤（先确认现象，再分支处理）

1. **向用户确认现象**（决定分支）：截图或描述——(a) 图标正常但鲸鱼变小/四周留白变大；(b) 显示通用白纸/旧图标；(c) 边缘毛糙/发虚。同时询问是否手动下载替换过 .app。
2. **分支 A（鲸鱼变淡/轮廓化、近乎不可见——截图已确认，优先处理）**：`build/appicon.png` 已被 e5058ae 改坏（高透明薄轮廓）。
   - 首选回退到可用版本：`git checkout e5058ae~1 -- build/appicon.png`（恢复实心蓝鲸，0.7.3 观感）；
   - 或改用品牌蓝风格：`cp app-icon.png build/appicon.png`（蓝圆角底+白鲸鱼，实心可见，见 restyle-appicon.mjs），更贴合当前深/浅色主题与网站配色；
   - 不要再用 `resize-icon.mjs` 链式缩放到已变淡的图（会继续放大低 alpha 缺陷）。
   **验证**：`node scripts/icon-metrics.mjs`（或上方像素分析）确认不透明像素占比回到 ~18%、平均 alpha≈255；重新 `wails build` 后 Dock/Launchpad 目测。
3. **分支 B（缓存旧/泛图标）**：用户侧执行 `lsregister -f` 重注册该 .app（或 `touch` 包体 + `killall Dock`），观察恢复；代码侧可选增强：`replaceAndRelaunch` 辅助脚本（platform_darwin.go L704）`open "$2"` 前加 `lsregister -f "$2"`，自动刷新（不推荐自动 `killall Dock`，会打断用户）。
   **验证**：更新/替换后 Dock 与 Finder 立即显示新图标，无需重启登录。
4. **分支 C（毛边/虚化）**：不走 resize 链式缩放，从源重生成 appicon（`scripts/restyle-appicon.mjs` / `gen-icon.mjs` 链路，源 `whale-src.png`），再按需调大小。
   **验证**：目测对比 + icon-metrics。
5. **发布**：改动的 appicon 随下一次 `wails build`（CI darwin 构建）进入 icns；发布后对比新旧 Release 的 icns 字节确认生效。

### 2.3 关键问题点

- Dock 图标与菜单栏模板是**两套资源**（appicon.png vs iconDataTemplate），用户只提了 app/Dock——先不动菜单栏 glyph；如菜单栏也觉得异常，再改 `systray_darwin.m` 的缩放常量（0.95→0.9）。
- 本机（Windows 沙箱）无法目测 mac 渲染，最终验收必须 mac 实机/截图；CI 产物可做字节级比对辅助。
- 若选回退 appicon，注意 e5058ae 同时改了模板 glyph 与 appicon，`git checkout` 只回退 appicon 一个文件即可，避免混入其它改动。
- icns 由 Wails 构建时从 appicon.png 生成，**改 PNG 后必须重新构建**才生效（改 Info.plist 无效）。

---

## 3. 问题 3：README 改 readme.html 点击切换 / 不支持则回退

### 3.1 结论：GitHub 不支持 HTML README → 按用户条件回退链接跳转版

- GitHub 仓库首页 README 支持的标记格式清单（官方文档「About READMEs」）**不含 HTML**：上传 `README.html` 不会被渲染成首页（显示为普通文件）；README.md 内嵌的 HTML 也会经过消毒（`<script>`/`<style>` 一律剥除）——**在 GitHub 页面上实现"点击切换中英文"不可行**（社区已有同诉求 feature request：discussion #109580）。
- 因此按用户给定的分支条件：**回退到上一版链接跳转双文件**（即 37306f3 合并前的状态）。

### 3.2 实施步骤（每步含验证）

1. **恢复双文件**：`git checkout 37306f3~1 -- README.md README.en.md`（两文件自带顶部互链「简体中文 · English」）→ commit（如 `revert(readme): 回到 README.md/README.en.md 链接跳转双语版`）。
   **验证**：`git show HEAD:README.md` 顶部互链行存在；GitHub 仓库首页显示中文版、点 English 跳到 README.en.md。
2. **（可选增强）README 顶部加站点双语入口**：互链行旁加 `<a href="https://refyon.github.io/dsh-systray/">在线文档（中/EN 切换）</a>`——网站已有 JS 切换器，是 GitHub 之外唯一能做"原地点击切换"的地方。
3. **（可选、若坚持 HTML 方案）GitHub Pages 托管 readme.html**：在 `docs/` 放 `readme.html`（带中/EN 互斥切换，内容静态内嵌，避免 fetch 失败态），README 里链接过去。**维护成本**：README 与 readme.html 内容双份同步，需后续机制（如发布时脚本从 README 生成 html），暂不推荐本期做。

### 3.3 关键问题点

- 本次回退**取代**上一轮 `<details>` 合并方案（用户最新决策优先，记录在案，避免后续会话误以为 details 版是终态）。
- 与 37306f3 同 commit 的其它修复（轮播/语言切换/托盘）不受影响，只回退两个 README 文件。
- release.yml 的 checkout 步骤名改动与 README 无关，保留。
- 若日后 GitHub 支持 HTML README（社区 discussion 落地），再迁移 readme.html 单文件方案。

---

## 4. 总体实施顺序与测试清单

1. 问题 1（CI 1 行 + 更新器防御，独立收益，解锁 mac 自更新）→ 问题 3（git 回退 2 文件，零风险）→ 问题 2（先等用户确认现象分支，再改图标）。
2. 测试清单：
   - 发 v0.8.1 后：下载 mac zip 复核条目为根级 `dsh-systray.app/`；mac 实机 0.7.x/0.8.0 → 检查更新 → 自动替换重启成功；
   - 更新器防御用例：坏包（dist/ 嵌套）可被定位；根级正常包行为不变；`go vet`/`go test` 全绿；
   - README：GitHub 首页互链跳转正常、README.en.md 可访问、无 404 引用；
   - 图标：按用户确认的分支执行对应验证（icon-metrics 占比 / lsregister 刷新 / 重建后 icns 字节变化）。
3. 待用户决策/输入：
   - 问题 2 现象确认（变小留白 / 缓存旧图标 / 毛边）+ 是否回退 appicon 观感；
   - 问题 1 是否顺带做「更新器一层兜底」防御（推荐做）；
   - 问题 3 是否加「在线文档」入口链接、是否要 GitHub Pages readme.html（可选）。

# dsh-systray

完整文档见仓库根目录的 [README.md](../README.md)。

## 目录说明

- `index.html`：官网（GitHub Pages 首页）
- `mock/`：**站点实时界面预览**——由 `scripts/build_mock.mjs` 从 `src/frontend/dist` 生成
  （真实前端 + 演示数据桩 `scripts/shot-shim.mjs`），官网轮播用 iframe 载入它。
  不用位图是因为位图最终总会被浏览器按容器宽度做一次非整数缩放，文字边缘会发虚；
  DOM 预览在任意缩放比例 / DPI 下都清晰。前端有改动时重跑 `node scripts/build_mock.mjs`。
  单页参数：`mock/index.html?page=general|about|logs|export|import|sync&lang=zh|en&scroll=bottom&auth=1`。
  直接双击 `index.html`（file://）也能看：此时浏览器不允许父页面操作 iframe 文档，
  轮播改用 URL 参数逐页重载（预览页内置 `?auth=1` 的授权弹层），效果与线上一致。
  本地更贴近线上：`python -m http.server --directory docs` 后访问 `http://127.0.0.1:8000/index.html`。
- `screenshot-hero*.webp`：README 主图。背景与前景都来自真实渲染：
  `node scripts/render_shots.mjs` 出素材（`docs/.shots-parts/`，常规页整窗 + 真实启动卡）
  → `python scripts/make_hero.py --lang zh|en` 合成 → `python scripts/convert_webp.py` 转 webp。
  一条命令重跑全部物料：`scripts\recapture.cmd`。
- `评估-*.md` / `验证指引-*.md`：需求评估与真机验收步骤（结论与遗留问题都在里面）。
- `DESIGN.md`：六节设计规范（Overview / Colors / Typography / Elevation / Components / Do's & Don'ts），
  同时约束桌面应用前端与官网 `index.html`；两侧 design token 必须一致，改界面样式前先看它。
- `RELEASE_NOTES.md`：各版本发布说明；打 `vX.Y.Z` tag 时 CI 按其中的「## vX.Y.Z」区块建 Release。

## 检查脚本（改前端后必跑）

- `node scripts/check-frontend-i18n.mjs`：文案键齐全（静态层 + `tr/fmt/msg` 动态层）。
- `node scripts/check-frontend-files-card.mjs`：文件列表的纯函数与交互规则（含导航、路径行）。
- `node scripts/check-frontend-lang.mjs`：真实浏览器里逐页比对「以目标语言启动」与
  「切换语言后」的逐元素文案，并覆盖交互场景（插件检查结论、重置弹层、双击进文件夹）；
  `--shot 目录/` 顺带截图、`--page sync` 只查一页、`--height 1100` 加高视口看全整页。

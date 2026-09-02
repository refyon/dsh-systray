# dsh-systray · 设计规范（DESIGN.md）

## 1. Overview

温暖克制的「工具感」界面：以品牌蓝为主色、中性灰阶打底，圆角卡片分组、留白分层。
设计目标是**一屏一个主操作、状态一目了然**。本规范同时约束桌面应用（Wails 前端 `frontend/dist/style.css`）与官网（`docs/index.html`），两侧 token 必须保持一致。

## 2. Colors

采用 CSS 变量（design tokens）管理，禁止硬编码色值。深浅两套主题通过 `prefers-color-scheme` 自动切换。

| Token | 浅色 | 深色 | 用途 |
| --- | --- | --- | --- |
| `--primary` | `#2563eb` | `#3b82f6` | 主按钮、选中态、进度填充 |
| `--primary-deep` | `#1d4ed8` | `#60a5fa` | 主按钮 hover / 按压 |
| `--primary-soft` | `#dbeafe` | `#1e3a5f` | 选中背景、激活导航项 |
| `--bg` | `#f8fafc` | `#0b1220` | 窗口底色 |
| `--surface` | `#ffffff` | `#111a2e` | 卡片 / 面板 |
| `--surface-2` | `#f1f5f9` | `#1a2540` | 次级填充（开关轨道、ghost 按钮） |
| `--text` | `#0f172a` | `#e2e8f0` | 正文（对比度 ≥ 4.5:1） |
| `--muted` | `#64748b` | `#94a3b8` | 次要说明 |
| `--border` | `#e2e8f0` | `#293548` | 分隔线 / 描边 |
| `--danger` / `--warn` / `--ok` | `#dc2626` / `#f59e0b` / `#16a34a` | `#ef4444` / `#fbbf24` / `#22c55e` | 服务状态 / 结果反馈 |
| `--focus` | `#3b82f6` | `#60a5fa` | 键盘焦点环 |

- 60-30-10 配比：底色与卡片占大面积，灰阶次之，品牌蓝只作强调。
- 不用纯黑（`#000`）作文字；灰阶代替纯黑与纯白。
- 禁止紫色渐变、无意义渐变滥用。

## 3. Typography

字体栈：`"Noto Sans SC", "PingFang SC", "Microsoft YaHei UI", "Segoe UI", system-ui`；等宽（日志）：`"SF Mono", "Cascadia Mono", "Consolas", monospace`。

| 层级 | 字号 | 字重 | 用途 |
| --- | --- | --- | --- |
| 标题 | 20px（`--fs-xl`） | 600 | 页面标题 |
| 正文 | 14px（`--fs-base`） | 400 / 600 | 卡片标题、按钮 |
| 次要 | 13px（`--fs-sm`） | 400 | 说明文字 |
| 辅助 / 日志 | 12px（`--fs-xs`） | 400 | 路径、日志行 |

- 行高 ≥ 1.5；字重层级 ≤ 3 档；最多 2 种字体族（UI + 等宽）。

## 4. Elevation

阴影只表达层级，用量克制：

| 层级 | 阴影 |
| --- | --- |
| 卡片 | `0 1px 2px rgba(15,23,42,.06)`（深色 `rgba(2,6,23,.4)`） |
| 弹层 / splash | `0 12px 28px rgba(15,23,42,.14)`（深色 `.6`） |

圆角：卡片 12px（`--radius`）、小组件 8px（`--radius-sm`）、按钮/开关 999px 胶囊、splash 卡片 16px。全应用一致，不做描边堆叠。

## 5. Components

- **侧栏导航**：168px 固定，5 项（常规/关于/日志/导出/导入）；激活项 `--primary-soft` 底 + `--primary` 字。
- **卡片**：`--surface` 底 + 1px `--border` 描边 + 12px 圆角；行内两栏（左说明 / 右控件）。
- **开关**：46×26px 轨道胶囊 + 20px 圆钮滑动；`aria-checked` 驱动状态。
- **按钮**：主（`--primary` 底白字）与幽灵（`--surface-2` 底）；最小高度 34px（触控目标充足）；`btn:disabled` 降透明度。
- **状态点**：8px 圆点，四态配色（starting=warn / running=ok / stopped=muted / failed=danger）。
- **进度条**：8px 轨道胶囊 + `--primary` 填充，`width` 过渡 180ms。
- **日志视图**：等宽 12px、`user-select: text`、行内级别着色（INFO=primary / WARN=warn / ERROR=danger）。
- **对话框**（Windows 原生自绘）：白色圆角窗口 + 胶囊按钮，主按钮 `--primary`。

## 6. Do's & Don'ts

**Do**
- 颜色、间距、字号一律走 token；间距用 4pt 网格（8/12/16/20/24）。
- 为 hover / active / focus / disabled / loading / empty / error 提供可见状态；键盘焦点环用 `:focus-visible`。
- 动效 ≤ 200ms、统一缓动（`cubic-bezier(.2,0,0,1)`），尊重 `prefers-reduced-motion`。
- 一屏一个主操作（如「导出…」「检查更新」）；图标与文案并置；渐进披露。
- 暗色模式三处联动：前端 CSS、WebView 主题（`Theme=SystemDefault`）、托盘图标深浅自适应。
- **任何弹窗 / 原生对话框 / 被显示的窗口都必须自动前台**：Windows 原生弹窗走 `forceForeground()`，
  目录/文件选择对话框把 owner 设为 Wails 主窗口（`wailsMainHWND()`），托盘唤起设置页走
  `ensureMainWindowForeground()`；macOS 弹窗前激活应用。新增弹窗代码必须遵守，违反视为缺陷。

**Don't**
- 不用魔法数字色值 / 间距；不叠卡片堆（一屏最多 3 张主卡片）。
- 不用无意义阴影与渐变；不在浅色模式下用低于 4.5:1 的正文对比度。
- 不在 UI 文案中出现技术术语（面向用户：说「重启后台服务」，不说「kill process」）。
- 不写不可访问的交互（仅 hover 可见、无焦点态、触控目标 < 44px）。
- 不用无 owner 的模态对话框（会导致弹窗落在后台、用户看不到）。

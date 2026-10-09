#!/usr/bin/env node
/**
 * build_mock.mjs：把 App 的真实前端（src/frontend/dist）复制成**站点用的实时界面预览**
 * （docs/mock/），供官网（docs/index.html）用 iframe 直接展示设置窗口。
 *
 * 为什么这么做（2026-09-30 用户反馈「截图被缩放后发虚」）：
 *   位图截图无论出多少倍，最终都要被浏览器按容器宽度**非整数比例**缩放一次（100% / 125% /
 *   150% 缩放屏各不相同），文字边缘必然发虚。改成 iframe + 真实 DOM 后，界面由浏览器
 *   自己排版渲染，任意缩放比例、任意 DPI 下都是矢量级清晰（与 App 内看到的完全一致）。
 *
 * 复用的东西：
 *   - 前端三件套原样复制（不改前端、不打补丁）；
 *   - shot-shim.mjs（与 README 截图渲染器同一份演示数据 + Wails 运行时桩），
 *     页面 / 语言 / 滚动位置由 URL 参数给出：docs/mock/?page=logs&lang=en&scroll=bottom
 *
 * 用法：node scripts/build_mock.mjs
 * 注意：src/frontend/dist 改动后需要重跑本脚本（否则站点预览是旧界面）。
 */
import { readFileSync, writeFileSync, mkdirSync, existsSync } from "node:fs";
import { join, dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { shimSource } from "./shot-shim.mjs";

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const DIST = join(ROOT, "src", "frontend", "dist");
const OUT = join(ROOT, "docs", "mock");

mkdirSync(OUT, { recursive: true });

const files = ["index.html", "main.js", "style.css"];
const written = [];

// 预览专用脚本（见下方注入点注释）：与站点脚本里的弹层文案同一份（i18n.go 的 ghAuthPromptMsg）
const AUTH_SNIPPET = `<script>
  // preview-auth：?auth=1 时打开 GitHub 授权弹层（私有仓库插件，与 App 内同一个 #modal）
  (function () {
    var q = new URLSearchParams(location.search);
    if (q.get('auth') !== '1') return;
    var en = (q.get('lang') || 'zh') === 'en';
    var msg = en
      ? 'Plugin prompt-assistant comes from the private repository example/prompt-assistant.\\n\\nChecking for updates requires GitHub authorization: clicking \\u201cSign in to GitHub\\u201d opens your browser and copies a one-time code to the clipboard \\u2014 just paste it.'
      : '\\u63d2\\u4ef6 prompt-assistant \\u6765\\u81ea\\u79c1\\u6709\\u4ed3\\u5e93 example/prompt-assistant\\u3002\\n\\n\\u68c0\\u67e5\\u66f4\\u65b0\\u9700\\u8981 GitHub \\u6388\\u6743\\uff1a\\u70b9\\u51fb\\u300c\\u767b\\u5f55 GitHub\\u300d\\u540e\\u4f1a\\u6253\\u5f00\\u6d4f\\u89c8\\u5668\\uff0c\\u4e00\\u6b21\\u6027\\u4ee3\\u7801\\u81ea\\u52a8\\u590d\\u5236\\u5230\\u526a\\u8d34\\u677f\\uff0c\\u7c98\\u8d34\\u5373\\u53ef\\u3002';
    var tries = 0;
    (function open() {
      var list = document.getElementById('plug-list');
      if (!list || !list.children.length) { if (++tries < 60) return setTimeout(open, 100); return; }
      var $ = function (id) { return document.getElementById(id); };
      var modal = $('modal'), title = $('modal-title'), msgEl = $('modal-msg'), okBtn = $('modal-ok'), skip = $('modal-skip');
      if (!modal || !msgEl || !okBtn) return;
      if (title) title.classList.add('hidden');
      msgEl.textContent = msg;
      okBtn.textContent = en ? 'Sign in to GitHub' : '\\u767b\\u5f55 GitHub';
      okBtn.className = 'btn btn-primary';
      if (skip) skip.classList.add('hidden');
      var mask = document.querySelector('#modal .modal-mask');
      if (mask) mask.style.background = 'transparent';
      var card = document.querySelector('#modal .modal-card');
      if (card) card.style.width = '460px';
      msgEl.style.wordBreak = 'keep-all';
      msgEl.style.overflowWrap = 'normal';
      modal.classList.remove('hidden');
    })();
  })();
</script>`;

// 预览专用脚本：?modal=reset 时自动打开重置弹层（配合 ?mode=desktop 看桌面端重置界面）。
// 与 preview-auth 同一套路：靠 URL 参数驱动，file:// 直接打开与站点 iframe 表现一致。
const RESET_SNIPPET = `<script>
  // preview-reset：?modal=reset 时点开重置弹层（desktop 形态走「重置桌面端」入口）
  (function () {
    var q = new URLSearchParams(location.search);
    if (q.get('modal') !== 'reset') return;
    var tries = 0;
    (function open() {
      var desktop = q.get('mode') === 'desktop';
      var btn = document.getElementById(desktop ? 'btn-reset-desktop' : 'btn-reset-harness');
      if (!btn || btn.offsetParent === null) { if (++tries < 60) return setTimeout(open, 100); return; }
      btn.click();
    })();
  })();
</script>`;

for (const f of files) {
  const src = join(DIST, f);
  if (!existsSync(src)) throw new Error(`缺少 ${src}`);
  let body = readFileSync(src, "utf8");
  if (f === "index.html") {
    // 真实程序里 window.go / window.runtime 由 Wails 注入；站点预览由 shim.js 提供。
    const shimTag = '<script src="shim.js"></script>';
    const anchor = '<script src="main.js"></script>';
    if (!body.includes(anchor)) throw new Error("index.html 里找不到 main.js 引用");
    if (!body.includes(shimTag)) body = body.replace(anchor, `${shimTag}\n  ${anchor}`);
    // 预览专用：?auth=1 时打开前端自己的 GitHub 授权弹层。
    // 为什么放在预览页而不是站点脚本里：file:// 直接打开 index.html 时父页面拿不到 iframe 文档，
    // 只能靠 URL 参数驱动——把这段放在被载入的页面内，两种打开方式表现一致。
    if (!body.includes("preview-auth")) body = body.replace(anchor, `${AUTH_SNIPPET}\n  ${anchor}`);
    if (!body.includes("preview-reset")) body = body.replace(anchor, `${RESET_SNIPPET}\n  ${anchor}`);
    body = body.replace(
      "<title>dsh-systray · 设置</title>",
      '<title>dsh-systray · 界面预览</title>\n  <meta name="robots" content="noindex">'
    );
    body = body.replace(
      "<!DOCTYPE html>",
      "<!DOCTYPE html>\n<!-- 由 scripts/build_mock.mjs 从 src/frontend/dist 生成：站点实时界面预览，勿手改 -->"
    );
  }
  writeFileSync(join(OUT, f), body, "utf8");
  written.push(`${f} (${body.length} B)`);
}

writeFileSync(join(OUT, "shim.js"), shimSource("zh"), "utf8");
written.push(`shim.js (${shimSource("zh").length} B)`);

console.log(`docs/mock/ 已更新：${written.join("、")}`);
console.log("预览：docs/mock/index.html?page=general|about|logs|export|import|sync&lang=zh|en&scroll=bottom&auth=1");
console.log("桌面端界面预览：docs/mock/index.html?page=general&mode=desktop（弹层加 &modal=reset）");

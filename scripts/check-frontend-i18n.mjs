// check-frontend-i18n.mjs：前端静态检查（本仓库前端是手写 JS + 手写 DOM，无构建步骤，
// 因此文案/结构的一致性只能靠脚本兜底）。
//
// 硬失败：
//   1) index.html 里 data-i18n / data-i18n-ph / data-i18n-title 的键必须在 I18N_EN 中存在英文译文；
//   2) PAGE_I18N_KEY 与 PAGE_TITLES 的页名集合必须一致，且 PAGE_I18N_KEY 的键在 I18N_EN 中存在；
//   3) index.html 不得有重复的 id（getElementById 会静默取第一个）。
// 仅告警（不失败）：main.js 中 tr("…") / fmt("…") 的中文字面量若不在 I18N_DYN 中，英文界面会回退中文。
//
// 用法：node scripts/check-frontend-i18n.mjs

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const root = join(dirname(fileURLToPath(import.meta.url)), "..", "src", "frontend", "dist");
const html = readFileSync(join(root, "index.html"), "utf8");
const js = readFileSync(join(root, "main.js"), "utf8");

const errors = [];
const warnings = [];

/** 抽取 `const NAME = { ... };` 顶层对象字面量（按缩进块粗切，足够这套手写代码使用）。 */
function objectLiteral(name) {
  const start = js.indexOf(`const ${name} = {`);
  if (start < 0) return "";
  const end = js.indexOf("\n};", start);
  return end < 0 ? js.slice(start) : js.slice(start, end);
}

// ---- I18N_EN 的键（同一行可以写多个键，故不能按行首匹配） ----
const enKeys = new Set();
for (const m of objectLiteral("I18N_EN").matchAll(/([A-Za-z0-9_]+):\s*"/g)) enKeys.add(m[1]);

// ---- I18N_DYN 的键（中文键，带引号） ----
const dynKeys = new Set();
for (const m of objectLiteral("I18N_DYN").matchAll(/"((?:[^"\\]|\\.)*)":\s*"/g)) dynKeys.add(m[1]);

// ---- 1) index.html 静态键 ----
for (const attr of ["data-i18n", "data-i18n-ph", "data-i18n-title"]) {
  for (const m of html.matchAll(new RegExp(`${attr}="([^"]+)"`, "g"))) {
    if (!enKeys.has(m[1])) errors.push(`index.html 的 ${attr}="${m[1]}" 在 I18N_EN 中缺少英文译文`);
  }
}

// ---- 2) 页面路由表 ----
const pageKeySrc = (js.match(/const PAGE_I18N_KEY = \{([^}]*)\}/) || [, ""])[1];
const pageTitleSrc = (js.match(/const PAGE_TITLES = \{([^}]*)\}/) || [, ""])[1];
const parsePairs = (src) =>
  [...src.matchAll(/([A-Za-z0-9_]+):\s*"([^"]*)"/g)].map((m) => [m[1], m[2]]);
const pageKeys = new Map(parsePairs(pageKeySrc));
const pageTitles = new Map(parsePairs(pageTitleSrc));
for (const [page, key] of pageKeys) {
  if (!pageTitles.has(page)) errors.push(`页面 ${page} 在 PAGE_I18N_KEY 中但不在 PAGE_TITLES 中`);
  if (!enKeys.has(key)) errors.push(`PAGE_I18N_KEY.${page} = ${key} 在 I18N_EN 中缺少英文译文`);
}
for (const page of pageTitles.keys()) {
  if (!pageKeys.has(page)) errors.push(`页面 ${page} 在 PAGE_TITLES 中但不在 PAGE_I18N_KEY 中`);
  if (!html.includes(`data-page="${page}"`)) errors.push(`页面 ${page} 在路由表中但没有对应的 nav 按钮`);
  if (!html.includes(`id="page-${page}"`)) errors.push(`页面 ${page} 在路由表中但没有对应的 <section id="page-${page}">`);
}

// ---- 3) 重复 id ----
const ids = [...html.matchAll(/\sid="([^"]+)"/g)].map((m) => m[1]);
const seen = new Set();
for (const id of ids) {
  if (seen.has(id)) errors.push(`index.html 中 id="${id}" 重复`);
  seen.add(id);
}

// ---- 动态文案（仅告警）----
const zhLiterals = new Set();
for (const m of js.matchAll(/\b(?:tr|fmt)\("([^"]*[\u4e00-\u9fa5][^"]*)"/g)) zhLiterals.add(m[1]);
for (const s of zhLiterals) {
  if (!dynKeys.has(s)) warnings.push(`动态文案未在 I18N_DYN 中登记（英文界面将回退中文）：${s}`);
}

console.log(`检查：${enKeys.size} 条静态译文 / ${dynKeys.size} 条动态译文 / ${ids.length} 个元素 id`);
for (const w of warnings) console.log(`警告：${w}`);
if (errors.length) {
  for (const e of errors) console.error(`错误：${e}`);
  process.exit(1);
}
console.log(`通过：静态键齐全，路由表一致，无重复 id（${warnings.length} 条告警）`);

#!/usr/bin/env node
/**
 * check-frontend-lang.mjs：界面语言「实机核对」（真实 Chromium 内核 + 演示垫片，不启动 exe）。
 *
 * 为什么需要它：语言切换是**运行时行为**——静态层就地覆盖（data-i18n 快照）+ 动态区块重渲染
 * （rerenderDynamicText）+ 各绑定回读。check-frontend-i18n.mjs 只能证明「译文键齐全」，
 * 证明不了「切了语言之后界面真的跟着变」：漏刷新时表现为某些按钮/行停留在旧语言，
 * 静态检查完全看不出来（2026-10-07 用户报「切换语言后部分按钮没立即更新」）。
 *
 * 判据（同一页面、同一演示数据，两条路径的结果必须一致）：
 *   A 基线：以目标语言**直接启动**（?lang=X）——期望的正确渲染
 *   B 实测：以另一种语言启动后**切到目标语言**（走真实 UI：改 #sel-lang + change 事件）
 *   A 与 B 的逐元素文案不一致 = 该元素没随语言更新（列出键与两侧文案）
 * 两个方向都比：zh→en（漏译成中文）与 en→zh（残留英文）。
 * 另外单独核对 en 页面不得含中文（白名单：语言下拉的「简体中文」等专有名词）。
 *
 * 用法：
 *   node scripts/check-frontend-lang.mjs                  # 全部页面 × 两个方向
 *   node scripts/check-frontend-lang.mjs --page sync      # 只查指定页
 *   node scripts/check-frontend-lang.mjs --dump out.txt   # 顺带导出各页文案（排查用）
 *   node scripts/check-frontend-lang.mjs --shot dir/      # 顺带截图各页（视觉复核用）
 * 退出码：0 = 一致；1 = 存在差异/漏译。
 */
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { existsSync, writeFileSync, mkdirSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { extname, join, dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";
import { shimSource } from "./shot-shim.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, "..");
const DIST = join(ROOT, "src", "frontend", "dist");
const PAGES = ["general", "about", "logs", "export", "import", "sync", "help"];

// en 界面允许出现的中文（专有名词/演示数据，不是漏译）
const CJK_ALLOW = [
  "简体中文",        // 语言下拉：语言名按自身语言书写
];

// 不参与比对的元素：日志正文是**诊断内容**，按设计不翻译（且演示日志中英本就不同源）
const IGNORE_KEYS = [
  /^div\.log-line/, /^span\.log-ts/, /^span\.lvl-/, /^div\.log-empty/,
];

const args = process.argv.slice(2);
const argVal = (name) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : "";
};
const onlyPages = argVal("--page").split(",").map((s) => s.trim()).filter(Boolean);
const pages = onlyPages.length ? PAGES.filter((p) => onlyPages.includes(p)) : PAGES;
const dumpFile = argVal("--dump");
const shotDir = argVal("--shot");
// 视口：默认与设置窗口一致（840×560）；--height 可加高（截图看全整页内容用，两侧对称不影响比对）
const VIEWPORT = { width: 840, height: Number(argVal("--height")) || 560 };

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ==================== CDP ====================
class CDP {
  constructor(ws) {
    this.ws = ws;
    this.id = 0;
    this.pending = new Map();
    ws.addEventListener("message", (ev) => {
      let msg;
      try { msg = JSON.parse(ev.data); } catch { return; }
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve: res, reject: rej } = this.pending.get(msg.id);
        this.pending.delete(msg.id);
        msg.error ? rej(new Error(msg.error.message)) : res(msg.result);
      }
    });
  }
  send(method, params = {}) {
    const id = ++this.id;
    return new Promise((res, rej) => {
      this.pending.set(id, { resolve: res, reject: rej });
      this.ws.send(JSON.stringify({ id, method, params }));
      setTimeout(() => {
        if (this.pending.has(id)) { this.pending.delete(id); rej(new Error(`CDP timeout: ${method}`)); }
      }, 30000);
    });
  }
  close() { try { this.ws.close(); } catch { /* ignore */ } }
}

// ==================== 静态服务 ====================
const MIME = { ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png", ".webp": "image/webp", ".ico": "image/x-icon" };

function serveDist() {
  return new Promise((res) => {
    const srv = createServer(async (req, rsp) => {
      const rel = decodeURIComponent(req.url.split("?")[0]).replace(/^\/+/, "") || "index.html";
      const file = join(DIST, rel);
      if (!file.startsWith(DIST)) { rsp.writeHead(403).end(); return; }
      try {
        const body = await readFile(file);
        rsp.writeHead(200, { "content-type": MIME[extname(file).toLowerCase()] || "application/octet-stream", "cache-control": "no-store" });
        rsp.end(body);
      } catch {
        rsp.writeHead(404).end("not found");
      }
    });
    srv.listen(0, "127.0.0.1", () => res(srv));
  });
}

async function findEdge() {
  const cands = [
    process.env.DSH_EDGE_PATH,
    "C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe",
    "C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe",
    "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
    "C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe",
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
    "/usr/bin/google-chrome",
  ].filter(Boolean);
  for (const c of cands) if (existsSync(c)) return c;
  throw new Error("找不到 Chromium 内核浏览器（Edge/Chrome）。可用 DSH_EDGE_PATH 指定路径。");
}

async function launchBrowser(edge, port) {
  const profile = join(tmpdir(), `dsh-lang-profile-${port}`);
  const proc = spawn(edge, [
    "--headless=new",
    `--remote-debugging-port=${port}`,
    `--user-data-dir=${profile}`,
    "--no-first-run", "--no-default-browser-check", "--disable-extensions",
    "--disable-background-networking", "--disable-sync", "--disable-features=Translate",
    "--hide-scrollbars", "--force-device-scale-factor=1",
    "--window-size=" + VIEWPORT.width + "," + VIEWPORT.height,
    "about:blank",
  ], { stdio: "ignore", detached: false });
  for (let i = 0; i < 100; i++) {
    try {
      const r = await fetch(`http://127.0.0.1:${port}/json/version`);
      if (r.ok) return proc;
    } catch { /* not yet */ }
    await sleep(120);
  }
  try { proc.kill(); } catch { /* ignore */ }
  throw new Error("浏览器 DevTools 端口未就绪");
}

async function newPage(port) {
  const list = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
  const target = list.find((t) => t.type === "page");
  if (!target) throw new Error("找不到可用的页面目标");
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => {
    ws.addEventListener("open", res, { once: true });
    ws.addEventListener("error", () => rej(new Error("DevTools WebSocket 连接失败")), { once: true });
  });
  return new CDP(ws);
}

// ==================== 页面文案采集 ====================
/** 采集可见元素的「自身文案」：键 = 结构定位（id/i18n 键/类名），值 = 该元素直接文本。
 *  用直接文本而非 innerText：避免父子重复计数，差异定位也更精确。 */
const DUMP_EXPR = `(() => {
  const out = [];
  const seen = new Map();
  const visible = (el) => {
    const st = getComputedStyle(el);
    if (st.display === "none" || st.visibility === "hidden") return false;
    return el.getClientRects().length > 0;
  };
  for (const el of document.querySelectorAll("body *")) {
    if (!visible(el)) continue;
    const own = [...el.childNodes]
      .filter((n) => n.nodeType === 3)
      .map((n) => n.textContent.replace(/\\s+/g, " ").trim())
      .filter(Boolean)
      .join(" ");
    const attrs = [];
    if (el.id) attrs.push("#" + el.id);
    if (el.dataset && el.dataset.i18n) attrs.push("[i18n=" + el.dataset.i18n + "]");
    if (el.dataset && el.dataset.sort) attrs.push("[sort=" + el.dataset.sort + "]");
    if (el.dataset && el.dataset.fact) attrs.push("[fact=" + el.dataset.fact + "]");
    const cls = typeof el.className === "string" ? el.className.trim().split(/\\s+/).filter(Boolean) : [];
    const base = el.tagName.toLowerCase() + attrs.join("") + (cls.length ? "." + cls.join(".") : "");
    let key = base;
    const n = (seen.get(base) || 0) + 1;
    seen.set(base, n);
    if (n > 1) key = base + "@" + n;
    const text = own || (el.placeholder ? "ph:" + el.placeholder : "");
    if (!text) continue;
    out.push({ k: key, t: text });
  }
  // 下拉框：选项本体没有布局盒（不可见判定会漏掉），单独把「当前选中项文字」纳入比对
  for (const sel of document.querySelectorAll("select")) {
    if (!visible(sel)) continue;
    const opt = sel.options[sel.selectedIndex];
    if (opt) out.push({ k: "select#" + (sel.id || "") + ">selected", t: opt.textContent.trim() });
  }
  out.sort((a, b) => a.k.localeCompare(b.k));
  return out;
})()`;

async function dump(cdp) {
  const r = await cdp.send("Runtime.evaluate", { expression: DUMP_EXPR, returnByValue: true });
  const list = (r.result && r.result.value) || [];
  return new Map(list.map((e) => [e.k, e.t]));
}

/** 等界面停稳：连续两次采集完全一致（异步重渲染都落地了）。 */
async function dumpSettled(cdp, timeoutMs = 10000) {
  const deadline = Date.now() + timeoutMs;
  let prev = await dump(cdp);
  while (Date.now() < deadline) {
    await sleep(300);
    const cur = await dump(cdp);
    if (cur.size === prev.size && [...cur].every(([k, v]) => prev.get(k) === v)) return cur;
    prev = cur;
  }
  return prev;
}

// ==================== 打开界面 ====================
async function openApp(port, baseURL, lang, page) {
  const cdp = await newPage(port);
  await cdp.send("Page.enable");
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: VIEWPORT.width, height: VIEWPORT.height, deviceScaleFactor: 1, mobile: false,
    screenWidth: VIEWPORT.width, screenHeight: VIEWPORT.height,
  });
  await cdp.send("Page.addScriptToEvaluateOnNewDocument", { source: shimSource(lang) });
  await cdp.send("Page.addScriptToEvaluateOnNewDocument", {
    source: `window.__shotPage = ${JSON.stringify(page)}; window.__shotScroll = "";`,
  });
  await cdp.send("Page.navigate", { url: baseURL });
  // 等首屏就绪（设置视图出现 + 目标页可见）
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    const r = await cdp.send("Runtime.evaluate", {
      expression: `(() => {
        const s = document.getElementById("settings");
        const p = document.getElementById(${JSON.stringify("page-" + page)});
        const splash = document.getElementById("splash");
        if (!s || !p) return "missing";
        if (s.classList.contains("hidden")) return "hidden";
        if (splash && !splash.classList.contains("hidden")) return "splash";
        if (p.classList.contains("hidden")) return "page-hidden";
        return "ok";
      })()`,
      returnByValue: true,
    });
    if (r.result.value === "ok") return cdp;
    await sleep(120);
  }
  throw new Error(`等待页面 ${page}（${lang}）渲染超时`);
}

/** 走真实 UI 切换语言：改 #sel-lang 并派发 change（前端 change → SetLanguage → lang:changed）。 */
async function switchLang(cdp, lang) {
  const r = await cdp.send("Runtime.evaluate", {
    expression: `(() => {
      const sel = document.getElementById("sel-lang");
      if (!sel) return "no-select";
      sel.value = ${JSON.stringify(lang)};
      sel.dispatchEvent(new Event("change", { bubbles: true }));
      return "ok";
    })()`,
    returnByValue: true,
  });
  if (r.result.value !== "ok") throw new Error("找不到语言下拉 #sel-lang，无法切换");
}

/** 在页面里执行一段交互脚本（点击按钮等），返回 null 或错误字符串。 */
async function runSetup(cdp, expr) {
  const r = await cdp.send("Runtime.evaluate", { expression: `(() => { ${expr}; return "ok"; })()`, returnByValue: true });
  if (r.result.value !== "ok") throw new Error("交互脚本未成功执行：" + expr);
}

/** 截图当前页面（视觉复核：布局/路径行/面包屑在改动后长什么样）。 */
async function shoot(cdp, file) {
  const { data } = await cdp.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false, fromSurface: true });
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, Buffer.from(data, "base64"));
  return file;
}

// ==================== 交互场景 ====================
// 「先操作、再切语言」的场景：状态型文案（检查结论、弹层内的动态文字）不在首屏快照里，
// 只比对首屏会漏掉它们；每个场景同样用 A/B 两条路径（目标语言直接操作 vs 另一种语言操作后切换）比对。
// keys 限定只比对相关元素（其余元素由首屏那批检查覆盖，避免无关动态数据干扰）。
const SCENARIOS = [
  {
    name: "插件检查更新结论",
    page: "about",
    keys: ["div.plug-note"],
    setup: `document.querySelector('#plug-list button[data-check]').click()`,
  },
  {
    name: "重置弹层（数量/目标版本/说明行）",
    page: "about",
    keys: ["div#reset-sessions-sub", "div#reset-plugins-sub", "select#reset-target>selected",
      "span#reset-target-cur", "div#reset-target-note"],
    setup: `document.getElementById("btn-reset-harness").click()`,
  },
  {
    // 双击进入文件夹（导航式列表）：进入后的层级行/面包屑在切换语言后同样要跟着换语言
    name: "双击进入文件夹后的层级列表",
    page: "sync",
    keys: ["div.files-row", "div.files-name", "div.files-meta", "div.files-path",
      "button.files-crumb", "span.files-crumb"],
    setup: `document.querySelector('#files-tree [data-row="dir"]').dispatchEvent(new MouseEvent("dblclick", { bubbles: true }))`,
  },
];

// 附加行为核对（与语言无关，但同样只能在真实浏览器里验）：
// 路径行超长时靠 title 气泡给出完整路径（不做悬停文字滚动——2026-10-07 用户反馈效果不好）。
const PATH_TOOLTIP_CHECK = {
  page: "sync",
};

// ==================== 比对 ====================
const hasCJK = (s) => /[\u4e00-\u9fff]/.test(s);
const cjkAllowed = (s) => CJK_ALLOW.some((a) => s.includes(a));
const ignored = (k) => IGNORE_KEYS.some((re) => re.test(k));

function diffMaps(base, actual) {
  const out = [];
  for (const [k, v] of base) {
    if (ignored(k)) continue;
    const a = actual.get(k);
    if (a === undefined) out.push({ k, base: v, actual: "<缺失>" });
    else if (a !== v) out.push({ k, base: v, actual: a });
  }
  for (const [k, v] of actual) if (!base.has(k) && !ignored(k)) out.push({ k, base: "<新增>", actual: v });
  return out;
}

// ==================== 主流程 ====================
async function main() {
  const srv = await serveDist();
  const baseURL = `http://127.0.0.1:${srv.address().port}/index.html`;
  const port = 9333 + (process.pid % 200);
  const edge = await findEdge();
  const proc = await launchBrowser(edge, port);
  const failures = [];
  const dumps = {};

  try {
    // ---- 1) 基线：每种语言 × 每个页面「直接以该语言启动」 ----
    for (const lang of ["zh", "en"]) {
      for (const page of pages) {
        const cdp = await openApp(port, baseURL, lang, page);
        const map = await dumpSettled(cdp);
        dumps[`${lang}/${page}/base`] = map;
        // en 基线不得出现中文（漏译）
        if (lang === "en") {
          for (const [k, t] of map) {
            if (hasCJK(t) && !cjkAllowed(t)) failures.push(`[en/${page}] 未翻译：${k} = ${t}`);
          }
        }
        if (shotDir) {
          await shoot(cdp, join(shotDir, `${page}-${lang}.png`));
          console.log(`截图：${join(shotDir, `${page}-${lang}.png`)}`);
        }
        cdp.close();
      }
    }

    // ---- 2) 实测：从另一种语言启动后切到目标语言，与基线逐元素比对 ----
    for (const [from, to] of [["zh", "en"], ["en", "zh"]]) {
      for (const page of pages) {
        const cdp = await openApp(port, baseURL, from, page);
        await switchLang(cdp, to);
        const map = await dumpSettled(cdp);
        dumps[`${to}/${page}/switch`] = map;
        const base = dumps[`${to}/${page}/base`];
        if (!base) continue; // 该页未纳入本次范围
        const d = diffMaps(base, map);
        for (const item of d) {
          failures.push(`[${page}] ${from}→${to} 未随语言更新：${item.k}\n      基线(${to})=${item.base}\n      切换后  =${item.actual}`);
        }
        cdp.close();
      }
    }

    // ---- 3) 交互场景：先操作再切语言（状态型文案同样要与目标语言基线一致） ----
    for (const sc of SCENARIOS) {
      const pick = (map) => new Map([...map].filter(([k]) => sc.keys.some((p) => k.startsWith(p))));
      const base = {};
      for (const lang of ["zh", "en"]) {
        const cdp = await openApp(port, baseURL, lang, sc.page);
        await runSetup(cdp, sc.setup);
        base[lang] = pick(await dumpSettled(cdp));
        dumps[`${lang}/${sc.name}/base`] = base[lang];
        if (shotDir && sc.name === "双击进入文件夹后的层级列表") {
          await shoot(cdp, join(shotDir, `sync-inside-${lang}.png`));
        }
        cdp.close();
      }
      if (!base.zh.size && !base.en.size) {
        failures.push(`[场景 ${sc.name}] 交互后没有采集到任何目标元素（keys 前缀写错？）`);
        continue;
      }
      for (const [from, to] of [["zh", "en"], ["en", "zh"]]) {
        const cdp = await openApp(port, baseURL, from, sc.page);
        await runSetup(cdp, sc.setup);
        await switchLang(cdp, to);
        const map = pick(await dumpSettled(cdp));
        dumps[`${to}/${sc.name}/switch`] = map;
        for (const item of diffMaps(base[to], map)) {
          failures.push(`[场景 ${sc.name}] ${from}→${to} 未随语言更新：${item.k}\n      基线(${to})=${item.base}\n      切换后  =${item.actual}`);
        }
        cdp.close();
      }
    }
    // ---- 4) 附加行为：路径行的完整路径气泡（title，超长截断时靠它看全） ----
    {
      const cdp = await openApp(port, baseURL, "zh", PATH_TOOLTIP_CHECK.page);
      const r = await cdp.send("Runtime.evaluate", {
        expression: `(() => {
          const paths = [...document.querySelectorAll("#files-tree .files-path")];
          if (!paths.length) return "no-path";
          const bad = paths.filter((el) => !el.getAttribute("title") || el.getAttribute("title") !== el.textContent.trim());
          const clipped = paths.some((el) => el.scrollWidth > el.clientWidth + 2);
          const noScroll = paths.every((el) => getComputedStyle(el).textOverflow === "ellipsis" && getComputedStyle(el).whiteSpace === "nowrap");
          return JSON.stringify({ n: paths.length, bad: bad.length, clipped, noScroll });
        })()`,
        returnByValue: true,
      });
      let v = null;
      try { v = JSON.parse(r.result.value); } catch { /* 保持 null */ }
      if (!v) failures.push(`[路径气泡] 未采集到路径行：${r.result.value}`);
      else if (v.bad) failures.push(`[路径气泡] ${v.bad} 行缺少/不匹配 title（悬停看不到完整路径）`);
      else if (!v.noScroll) failures.push("[路径气泡] 路径行应保持不换行 + 末尾省略");
      cdp.close();
    }

    // ---- 5) 附加行为：容量不足弹窗**整个运行期只出现一次**，之后由提示行常驻说明 ----
    {
      const cdp = await openApp(port, baseURL, "zh", "sync");
      const r = await cdp.send("Runtime.evaluate", {
        expression: `(() => {
          const modal = document.getElementById("modal");
          const hint = () => document.getElementById("files-hint-text").textContent;
          const render = (n) => {
            const st = window.__filesStatusCache;
            st.blockedCount = n;
            window.renderFilesCard(st, { forceTree: false });
          };
          if (typeof window.renderFilesCard !== "function" || !window.go || !window.go.main) {
            return JSON.stringify({ skip: "renderFilesCard 未暴露（改动 main.js 时注意保留顶层函数）" });
          }
          return window.go.main.App.FilesStatus().then((st) => {
            window.__filesStatusCache = st;
            render(3);
            const firstOpen = !modal.classList.contains("hidden");
            const cancelHidden = document.getElementById("modal-cancel").classList.contains("hidden");
            const title = document.getElementById("modal-title").textContent;
            document.getElementById("modal-ok").click();
            const closed = modal.classList.contains("hidden");
            const hint1 = hint();
            render(4); // 数量变化：不该再弹
            const secondOpen = !modal.classList.contains("hidden");
            const hint2 = hint();
            return JSON.stringify({ firstOpen, cancelHidden, title, closed, secondOpen, hint1, hint2 });
          });
        })()`,
        awaitPromise: true,
        returnByValue: true,
      });
      let v = null;
      try { v = JSON.parse(r.result.value); } catch { /* 保持 null */ }
      if (!v) failures.push(`[容量提示] 未能执行检查：${r.result.value}`);
      else if (v.skip) console.log(`跳过容量提示检查：${v.skip}`);
      else if (!v.firstOpen) failures.push("[容量提示] 首次出现容量不足时应弹一次提示");
      else if (!v.cancelHidden) failures.push("[容量提示] 纯告知弹层不该有「取消」按钮");
      else if (!v.closed) failures.push("[容量提示] 点「知道了」应关闭弹层");
      else if (v.secondOpen) failures.push("[容量提示] 数量变化时不该再次弹窗（用户要求只提示一次）");
      else if (!/容量不足/.test(v.hint2)) failures.push(`[容量提示] 关闭后应由提示行常驻说明：${v.hint2}`);
      cdp.close();
    }
  } finally {
    try { proc.kill(); } catch { /* ignore */ }
    srv.close();
  }

  if (dumpFile) {
    const lines = [];
    for (const [name, map] of Object.entries(dumps)) {
      lines.push(`===== ${name} =====`);
      for (const [k, v] of map) lines.push(`${k} = ${v}`);
    }
    writeFileSync(dumpFile, lines.join("\n"), "utf8");
    console.log(`文案已导出：${dumpFile}`);
  }

  const checks = pages.length * 4 + SCENARIOS.length * 4;
  if (failures.length) {
    console.error(`失败：${failures.length} 处语言不一致（${pages.length} 个页面 + ${SCENARIOS.length} 个交互场景 × 两个方向）`);
    for (const f of failures) console.error("  - " + f);
    process.exit(1);
  }
  console.log(`通过：${checks} 项检查——${pages.length} 个页面与 ${SCENARIOS.length} 个交互场景的 zh/en 基线和运行时切换文案一致，en 界面无中文残留`);
}

main().catch((e) => {
  console.error("运行失败：" + (e && e.stack ? e.stack : e));
  process.exit(1);
});

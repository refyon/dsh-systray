#!/usr/bin/env node
/**
 * dsh-systray 渲染器（无窗口 / DPI 无关）：渲染 README 主图素材
 * ============================================================================
 * 用途：在**不启动 exe、不占用系统托盘、不依赖桌面会话**的前提下，把
 *       `src/frontend/dist` 的真实界面渲染成 README 主图的两块素材（docs/.shots-parts/）：
 *         · hero-bg[-en].png   设置页「常规」整窗（主图背景）
 *         · splash-card[-en].png 真实启动进度卡片（主图前景，带透明底）
 *       以及画面可见文字 hero-bg[-en].txt（供 scripts/verify_shots.py 核对脱敏与内容完整）。
 *
 * 历史：这里原来还出网站轮播用的 8 张设置页截图（docs/shots、docs/shots-en）。
 *       官网已改为 iframe 载入 App 真实前端做实时预览（docs/mock，见 build_mock.mjs），
 *       位图截图不再被网站使用，2026-10-06 起这两个目录连同其渲染分支一并移除。
 *
 * 为什么不用「跑真程序 + PrintWindow 抓窗口」的老路子：
 *   老办法把输出尺寸绑定在「窗口客户区像素」上，客户区尺寸又受 **显示器缩放**
 *   影响（笔记本 125%/150% 缩放下抓到的是被裁掉右边的图）。渲染器改为**自己指定 CSS
 *   视口**，渲染引擎仍是 WebView2 同一套 Chromium（Edge），因此像素级等价，
 *   但尺寸恒定、可复现。
 *
 * 用法：
 *   node scripts/render_shots.mjs                 # 中英素材全出
 *   node scripts/render_shots.mjs --lang zh       # 只出中文
 *   node scripts/render_shots.mjs --width 840 --height 560
 *
 * 脱敏：界面里出现的每个值都来自 scripts/shot-shim.mjs 的 DEMO 常量（演示邮箱 /
 *       C:\Users\demo 路径 / 虚构插件名 / 演示日志），不读取本机任何真实配置、账号或日志。
 */
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdirSync, writeFileSync, existsSync, rmSync, readFileSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { extname, join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";
import { shimSource } from "./shot-shim.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, "..");
const DIST = join(ROOT, "src", "frontend", "dist");
const DOCS = join(ROOT, "docs");
const PARTS = join(DOCS, ".shots-parts");   // 主图素材（.gitignore：可随时重生成）

// ==================== 视口 & 时钟 ====================
// 840×560 = 设置窗口的逻辑尺寸（src/main.go 的 winW/winH）。渲染视口直接取它，
// 于是「素材里的排版」= 「用户实际看到的排版」，不掺任何窗口边框/裁剪。
const VIEWPORT = { width: 840, height: 560 };

// ==================== CDP 客户端 ====================
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

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ==================== 静态文件服务 ====================
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

// ==================== 渲染流程 ====================
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
  const profile = join(tmpdir(), `dsh-shot-profile-${port}`);
  rmSync(profile, { recursive: true, force: true });
  const args = [
    "--headless=new",
    `--remote-debugging-port=${port}`,
    `--user-data-dir=${profile}`,
    "--no-first-run", "--no-default-browser-check", "--disable-extensions",
    "--disable-background-networking", "--disable-sync", "--disable-features=Translate",
    "--hide-scrollbars", "--force-device-scale-factor=1",
    "--window-size=" + VIEWPORT.width + "," + VIEWPORT.height,
    "--allow-file-access-from-files",
    "about:blank",
  ];
  const proc = spawn(edge, args, { stdio: "ignore", detached: false });
  // 等 DevTools 端口就绪
  for (let i = 0; i < 100; i++) {
    try {
      const r = await fetch(`http://127.0.0.1:${port}/json/version`);
      if (r.ok) return { proc, profile };
    } catch { /* not yet */ }
    await sleep(120);
  }
  try { proc.kill(); } catch { /* ignore */ }
  throw new Error("浏览器 DevTools 端口未就绪");
}

/** 把 CSS 视口固定成指定尺寸（与显示器缩放无关，这是本方案的关键）。 */
async function setViewport(cdp, width, height) {
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width, height, deviceScaleFactor: 1, mobile: false,
    screenWidth: width, screenHeight: height,
  });
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

/** 等页面把界面渲染稳定：等 #settings 可见 + 目标页可见 + 两帧空闲。 */
async function waitStable(cdp, page, timeoutMs = 15000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const r = await cdp.send("Runtime.evaluate", {
      expression: `(() => {
        const settings = document.getElementById("settings");
        const el = document.getElementById(${JSON.stringify("page-" + page)});
        const splash = document.getElementById("splash");
        if (!settings || !el) return "missing";
        if (settings.classList.contains("hidden")) return "settings-hidden";
        if (splash && !splash.classList.contains("hidden")) return "splash-visible";
        if (el.classList.contains("hidden")) return "page-hidden";
        return "ok";
      })()`,
      returnByValue: true,
    });
    if (r.result.value === "ok") { await sleep(450); return; }
    await sleep(120);
  }
  throw new Error(`等待页面 ${page} 渲染超时`);
}

/** 独立截图（元素截图，避免被裁切）。 */
async function shoot(cdp, file) {
  const { data } = await cdp.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false, fromSurface: true });
  writeFileSync(file, Buffer.from(data, "base64"));
  return file;
}

// ==================== 打开一个渲染页面 ====================
/** 新开一个已注入 shim、已固定视口、已导航到界面的页面，等它停稳。 */
async function openPage(port, { lang, page, viewport, evaluateOnLoad = [] }) {
  const cdp = await newPage(port);
  await cdp.send("Page.enable");
  await setViewport(cdp, viewport.width, viewport.height);
  await cdp.send("Page.addScriptToEvaluateOnNewDocument", { source: shimSource(lang) });
  if (page !== null) {
    await cdp.send("Page.addScriptToEvaluateOnNewDocument", {
      source: `window.__shotPage = ${JSON.stringify(page)}; window.__shotScroll = "";`,
    });
  }
  for (const src of evaluateOnLoad) {
    await cdp.send("Page.addScriptToEvaluateOnNewDocument", { source: src });
  }
  await cdp.send("Page.navigate", { url: BASE_URL });
  if (page !== null) await waitStable(cdp, page);
  else await sleep(900);
  return cdp;
}

/** 把画面上的可见文字导出成文本：验收脚本据此核对脱敏与内容完整，比 OCR 更可靠。 */
async function dumpText(cdp, file) {
  const txt = await cdp.send("Runtime.evaluate", { expression: `document.body.innerText`, returnByValue: true });
  writeFileSync(file, txt.result.value || "", "utf8");
}

// ==================== 主流程 ====================
async function main() {
  const argv = process.argv.slice(2);
  const arg = (k, d) => { const i = argv.indexOf(k); return i >= 0 ? argv[i + 1] : d; };
  const langs = (arg("--lang", "zh,en")).split(",").map((s) => s.trim()).filter(Boolean);
  const probe = argv.includes("--probe");
  const width = parseInt(arg("--width", String(VIEWPORT.width)), 10);
  const height = parseInt(arg("--height", String(VIEWPORT.height)), 10);
  const port = parseInt(arg("--port", String(9300 + Math.floor(Math.random() * 400))), 10);

  const edge = await findEdge();
  const srv = await serveDist();
  BASE_URL = `http://127.0.0.1:${srv.address().port}/index.html`;
  const { proc } = await launchBrowser(edge, port);
  console.log(`renderer: ${edge}`);
  console.log(`viewport: ${width}x${height}   langs: ${langs.join(",")}`);

  mkdirSync(PARTS, { recursive: true });

  let cdp = null;
  try {
    // ---- 探针模式：打印关键元素的实际盒模型，便于核对排版是否被裁切 ----
    if (probe) {
      cdp = await openPage(port, { lang: langs[0], page: "general", viewport: { width, height } });
      const r = await cdp.send("Runtime.evaluate", {
        expression: `JSON.stringify((() => {
          const box = (el) => el ? { x: Math.round(el.getBoundingClientRect().x), y: Math.round(el.getBoundingClientRect().y), w: Math.round(el.getBoundingClientRect().width), h: Math.round(el.getBoundingClientRect().height) } : null;
          const navs = [...document.querySelectorAll(".nav-item")].map((b) => b.textContent.trim());
          const card = document.querySelector("#page-general .card");
          return {
            viewport: { w: innerWidth, h: innerHeight },
            sidebar: box(document.querySelector(".sidebar")),
            content: box(document.querySelector(".content")),
            firstCard: box(card),
            rightGap: card ? Math.round(innerWidth - card.getBoundingClientRect().right) : null,
            navs,
            bodyScroll: { sw: document.body.scrollWidth, sh: document.body.scrollHeight, cw: document.body.clientWidth, ch: document.body.clientHeight },
          };
        })())`,
        returnByValue: true,
      });
      console.log(r.result.value);
      return;
    }

    // ---- 主图背景：设置页「常规」整窗（按语言各出一张）----
    for (const lang of langs) {
      if (cdp) { cdp.close(); cdp = null; }
      cdp = await openPage(port, { lang, page: "general", viewport: { width, height } });
      const name = lang === "en" ? "hero-bg-en" : "hero-bg";
      await shoot(cdp, join(PARTS, `${name}.png`));
      await dumpText(cdp, join(PARTS, `${name}.txt`));
      console.log(`  parts/${name}.png  (${width}x${height})`);
    }

    // ---- 主图前景件：真实的启动进度卡片（按语言各出一张）----
    for (const lang of langs) {
      if (cdp) { cdp.close(); cdp = null; }
      const splashStatus = lang === "en"
        ? "Installing DeepSeek Harness dependencies…"
        : "正在安装 DeepSeek Harness 依赖…";
      cdp = await openPage(port, {
        lang, page: null, viewport: { width, height },
        evaluateOnLoad: [`window.__splashShown = true;`],
      });
      // 截图带上 alpha：卡片是圆角 + 投影，必须透明底才能贴到主图上
      await cdp.send("Emulation.setDefaultBackgroundColorOverride", { color: { r: 0, g: 0, b: 0, a: 0 } });
      const box = await cdp.send("Runtime.evaluate", {
        expression: `(async () => {
          const splash = document.getElementById("splash");
          const settings = document.getElementById("settings");
          if (settings) settings.classList.add("hidden");
          splash.classList.remove("hidden");
          splash.style.background = "transparent";
          document.getElementById("splash-status").textContent = ${JSON.stringify(splashStatus)};
          document.getElementById("splash-cancel").classList.add("hidden");
          document.getElementById("splash-fill").style.width = "68%";
          await new Promise((r) => setTimeout(r, 600));
          const b = document.querySelector(".splash-card").getBoundingClientRect();
          return JSON.stringify({ x: b.x, y: b.y, width: b.width, height: b.height });
        })()`,
        awaitPromise: true, returnByValue: true,
      });
      const rect = JSON.parse(box.result.value);
      const { data } = await cdp.send("Page.captureScreenshot", {
        format: "png", fromSurface: true, captureBeyondViewport: true,
        clip: { x: rect.x, y: rect.y, width: rect.width, height: rect.height, scale: 1 },
      });
      const name = lang === "en" ? "splash-card-en.png" : "splash-card.png";
      writeFileSync(join(PARTS, name), Buffer.from(data, "base64"));
      console.log(`  parts/${name}  (${Math.round(rect.width)}x${Math.round(rect.height)})`);
    }
  } finally {
    if (cdp) cdp.close();
    try { proc.kill(); } catch { /* ignore */ }
    srv.close();
  }

  console.log("done: 素材写入 docs/.shots-parts/（接着跑 python scripts/make_hero.py）");
}

let BASE_URL = "";

main().catch((e) => { console.error("render failed:", e.message); process.exit(1); });

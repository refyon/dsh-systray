#!/usr/bin/env node
/**
 * dsh-systray 截图渲染器（无窗口 / DPI 无关）
 * ============================================================================
 * 用途：在**不启动 exe、不占用系统托盘、不依赖桌面会话**的前提下，把
 *       `src/frontend/dist` 的真实界面渲染成截图物料（docs/shots、docs/shots-en）。
 *
 * 为什么不用「跑真程序 + PrintWindow 抓窗口」的老路子：
 *   老办法把输出尺寸绑定在「窗口客户区像素」上，客户区尺寸又受 **显示器缩放**
 *   影响（笔记本 125%/150% 缩放下抓到的是被裁掉右边的图，README 与网站轮播图
 *   因此出现右侧内容缺失）。渲染器改为**自己指定 CSS 视口**，渲染引擎仍是
 *   WebView2 同一套 Chromium（Edge），因此像素级等价，但尺寸恒定、可复现。
 *
 * 用法：
 *   node scripts/render_shots.mjs                 # 中英全量 + 主图
 *   node scripts/render_shots.mjs --lang zh       # 只出中文
 *   node scripts/render_shots.mjs --lang en --only general,logs
 *   node scripts/render_shots.mjs --keep-png      # 保留中间 PNG（默认转 webp 后删除）
 *   node scripts/render_shots.mjs --width 840 --height 560
 *
 * 脱敏：界面里出现的每个值都来自下面的 DEMO 常量（演示邮箱 / C:\Users\demo 路径 /
 *       虚构插件名 / 演示日志），不读取本机任何真实配置、账号或日志。
 */
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdirSync, writeFileSync, existsSync, rmSync, readFileSync } from "node:fs";
import { readFile, readdir, unlink } from "node:fs/promises";
import { extname, join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";
import { DEMO, FROZEN_NOW, shimSource } from "./shot-shim.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, "..");
const DIST = join(ROOT, "src", "frontend", "dist");
const DOCS = join(ROOT, "docs");

// ==================== 视口 & 时钟 ====================
// 840×560 = 设置窗口的逻辑尺寸（src/main.go 的 winW/winH）。渲染视口直接取它，
// 于是「截图里的排版」= 「用户实际看到的排版」，不掺任何窗口边框/裁剪。
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

// ==================== 页面清单 ====================
// 与网站轮播（docs/index.html 的 shots 数组）和 README 一致的 8 张。
const PAGES = [
  { name: "general", page: "general" },
  { name: "about-top", page: "about", after: "waitPlugins" },
  { name: "about-bottom", page: "about", after: "scrollBottom" },
  { name: "logs", page: "logs" },
  { name: "export", page: "export" },
  { name: "import", page: "import" },
  { name: "sync", page: "sync", after: "scrollBottom" },
  { name: "github-auth", page: "about", after: "waitPlugins", dialog: "github" },
];

/** 执行页面级的“摆拍”动作（全部通过真实 DOM，不注入假界面）。 */
async function runAfter(cdp, action) {
  if (!action) return;
  if (action === "waitPlugins") {
    await cdp.send("Runtime.evaluate", { expression: `(async () => {
      const t0 = Date.now();
      while (Date.now() - t0 < 6000) {
        const list = document.getElementById("plug-list");
        if (list && list.children.length) return true;
        await new Promise((r) => setTimeout(r, 100));
      }
      return false;
    })()`, awaitPromise: true, returnByValue: true });
    await sleep(350);
    return;
  }
  if (action === "scrollBottom") {
    await cdp.send("Runtime.evaluate", { expression: `(async () => {
      const t0 = Date.now();
      while (Date.now() - t0 < 6000) {
        const list = document.getElementById("plug-list");
        if (list && list.children.length) break;
        await new Promise((r) => setTimeout(r, 100));
      }
      const c = document.querySelector("#page-about");
      const scroller = document.querySelector(".content");
      if (scroller) scroller.scrollTop = scroller.scrollHeight;   // 关于页整页滚到底
      if (c) c.scrollIntoView({ block: "end" });
      await new Promise((r) => setTimeout(r, 400));
      return true;
    })()`, awaitPromise: true, returnByValue: true });
    await sleep(350);
    return;
  }
}

/**
 * GitHub 授权确认弹层（github-auth 轮播图专用）。
 * 弹层文案与按钮与真实程序一致（i18n.go 的 ghAuthPromptMsg + ghLoginLabel），
 * 只是把它渲染在设置窗口之上：真实里这是一个独立原生窗口，无头渲染器里没有第二个窗口，
 * 因此复用前端自己的 #modal（同一套 DOM/CSS，视觉与 App 内弹层完全一致）。
 */
async function openGitHubAuthDialog(cdp, lang) {
  const msg = lang === "en"
    ? "Plugin prompt-assistant comes from the private repository example/prompt-assistant.\n\nChecking for updates requires GitHub authorization: clicking “Sign in to GitHub” opens your browser and copies a one-time code to the clipboard — just paste it."
    : "插件 prompt-assistant 来自私有仓库 example/prompt-assistant。\n\n检查更新需要 GitHub 授权：点击「登录 GitHub」后会打开浏览器，一次性代码自动复制到剪贴板，粘贴即可。";
  const okLabel = lang === "en" ? "Sign in to GitHub" : "登录 GitHub";
  // 真实里这是一个独立原生弹窗（只有正文 + 按钮，没有标题行、没有遮罩压暗），
  // 这里按同样形态渲染：隐藏标题、去掉遮罩压暗，保证出图与 App 里看到的一致。
  const expr = `(() => {
    const dlg = ${JSON.stringify(msg)}, ok = ${JSON.stringify(okLabel)};
    const $ = (id) => document.getElementById(id);
    $("modal-title").classList.add("hidden");
    $("modal-msg").textContent = dlg;
    const okBtn = $("modal-ok");
    okBtn.textContent = ok;
    okBtn.className = "btn btn-primary";   // 主操作为品牌蓝（真实弹窗同一个按钮样式）
    $("modal-skip").classList.add("hidden");
    const mask = document.querySelector("#modal .modal-mask");
    if (mask) mask.style.background = "transparent";
    // 原生弹窗按文案自适应宽度（不是固定的 360px 应用内弹层宽），这里同样放宽到 420px，
    // 否则中文那行「来自私有仓库 example/prompt-assistant。」会在仓库名后面折行。
    const card = document.querySelector("#modal .modal-card");
    if (card) card.style.width = "460px";
    // 仓库名/插件名不在中间断行（原生弹窗按整段排版，不会把 example/prompt-assistant 拦腰截断）
    const msgEl = $("modal-msg");
    if (msgEl) { msgEl.style.wordBreak = "keep-all"; msgEl.style.overflowWrap = "normal"; }
    $("modal").classList.remove("hidden");
    return true;
  })()`;
  await cdp.send("Runtime.evaluate", { expression: expr, returnByValue: true });
  await sleep(400);
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

// ==================== 主流程 ====================
async function main() {
  const argv = process.argv.slice(2);
  const arg = (k, d) => { const i = argv.indexOf(k); return i >= 0 ? argv[i + 1] : d; };
  const langs = (arg("--lang", "zh,en")).split(",").map((s) => s.trim()).filter(Boolean);
  const only = arg("--only", "").split(",").map((s) => s.trim()).filter(Boolean);
  const keepPng = argv.includes("--keep-png");
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

  const outDirs = {};
  for (const lang of langs) {
    const dir = lang === "en" ? join(DOCS, "shots-en") : join(DOCS, "shots");
    mkdirSync(dir, { recursive: true });
    outDirs[lang] = dir;
  }
  const tmpDir = join(DOCS, ".shots-tmp");
  mkdirSync(tmpDir, { recursive: true });
  const partsDir = join(DOCS, ".shots-parts");
  mkdirSync(partsDir, { recursive: true });

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

    // ---- 设置页截图 ----
    for (const lang of langs) {
      for (const spec of PAGES) {
        if (only.length && !only.includes(spec.name)) continue;
        if (cdp) { cdp.close(); cdp = null; }
        cdp = await openPage(port, { lang, page: spec.page, viewport: { width, height } });
        await runAfter(cdp, spec.after);
        if (spec.dialog === "github") await openGitHubAuthDialog(cdp, lang);
        const png = join(tmpDir, `${lang}-${spec.name}.png`);
        await shoot(cdp, png);
        // 同时把画面上的可见文字导出成文本：验收脚本据此核对脱敏与内容完整，
        // 比 OCR 更可靠（不受字体/识别误差影响），也可人工扫一眼确认没有真实信息。
        const txt = await cdp.send("Runtime.evaluate", {
          expression: `document.body.innerText`,
          returnByValue: true,
        });
        writeFileSync(join(tmpDir, `${lang}-${spec.name}.txt`), txt.result.value || "", "utf8");
        const dst = join(outDirs[lang], `${spec.name}.png`);
        writeFileSync(dst, await readFile(png));
        console.log(`  ${lang}/${spec.name}.png  (${width}x${height})`);
      }
    }

    // ---- README 主图前景件：真实的启动进度卡片（按语言各出一张，主图取对应语言的那张）----
    if (!only.length || only.includes("hero-splash")) {
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
        writeFileSync(join(partsDir, name), Buffer.from(data, "base64"));
        console.log(`  parts/${name}  (${Math.round(rect.width)}x${Math.round(rect.height)})`);
      }
    }
  } finally {
    if (cdp) cdp.close();
    try { proc.kill(); } catch { /* ignore */ }
    srv.close();
  }

  if (!keepPng) rmSync(tmpDir, { recursive: true, force: true });
  console.log("done: PNG written next to docs/shots[-en]/ (run scripts/convert_webp.py to convert)");
}

let BASE_URL = "";

main().catch((e) => { console.error("render failed:", e.message); process.exit(1); });

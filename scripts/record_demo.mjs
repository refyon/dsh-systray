#!/usr/bin/env node
/**
 * record_demo.mjs：把设置界面录成 README 演示动图的**帧序列**（PNG + 逐帧时间戳）
 * ============================================================================
 * 为什么不用「跑真程序 + 抓窗口」：与本仓库 render_shots.mjs 同一原则——渲染引擎仍是
 * WebView2 同源的 Chromium（Edge），但视口、缩放、DPI 全部由脚本固定，任何机器上重跑
 * 都得到同一套帧；不启动托盘、不占用桌面会话。
 *
 * 与 render_shots.mjs 的分工：
 *   render_shots.mjs → 静态整窗截图（docs/.shots-parts，验收用）
 *   本脚本           → 动图帧序列（docs/.demo-frames，README 演示用）
 *
 * 录制内容（真实交互，不伪造界面状态）：
 *   常规 → 数据同步（平滑滚到文件同步卡）→ 日志 → 关于（下滚看插件清单）
 *        → 导出 → 导入 → 帮助（滚到页末项目入口）→ 回常规（循环无缝）
 *
 * 「顺滑」来源：1.5 倍图（README 按 1 倍宽度显示）+ 256 色全局色板 + 页内平滑滚动。
 *   为什么不做「页面淡入」：试过注入 CSS 动画与逐帧设 opacity 两种做法，都会留下半透明帧——
 *   无头 Chromium 的动画时间轴不随抓图推进，动画会冻结在中途（实测"日志"页停在 opacity≈0.5
 *   约 1 秒，页标题变成半透明的空心字）；显式设值虽然可控，但淡入帧本身仍是"看起来发虚"的
 *   画面。界面本来就只做瞬时切页，录成实打实的切页最忠实、也最不容易出脏帧。
 *   切页时统一把内容区滚回顶部：否则会继承上一页的滚动位置，标题被滚出画面。
 *
 * 背景板（--mat，默认 20px）：视口比窗口大一圈，窗口固定 840x560 居中，四周是纯色底
 *   （浅色 #DCE3EC / 深色 #1E293B）——浅色界面直接贴在白色 README 上时窗口边界看不出来。
 *   只注入到本次录制会话，产品与站点预览不受影响。
 *
 * 用法：
 *   node scripts/record_demo.mjs                          # 中英 × 浅深 四套
 *   node scripts/record_demo.mjs --lang zh --scheme light  # 只录一套
 *   node scripts/record_demo.mjs --scale 1                 # 出 1 倍图（README 用 2 倍图才清晰）
 *
 * 产物：docs/.demo-frames/<lang>-<scheme>/0001.png… + meta.json（逐帧 delay，毫秒）
 * 下一步：node scripts/encode_demo.mjs（合并静止帧并编码成 APNG）
 *
 * 脱敏：界面值全部来自 scripts/shot-shim.mjs 的 DEMO 常量，不读本机任何真实配置/账号/日志。
 */
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdirSync, writeFileSync, existsSync, rmSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import { extname, join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";
import { shimSource } from "./shot-shim.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, "..");
const DIST = join(ROOT, "src", "frontend", "dist");

// ==================== 参数 ====================
const argv = process.argv.slice(2);
const arg = (k, d) => { const i = argv.indexOf(k); return i >= 0 ? argv[i + 1] : d; };
const LANGS = arg("--lang", "zh,en").split(",").map((s) => s.trim()).filter(Boolean);
const SCHEMES = arg("--scheme", "light,dark").split(",").map((s) => s.trim()).filter(Boolean);
const VIEWPORT = { width: Number(arg("--width", 840)), height: Number(arg("--height", 560)) };
// 背景板：窗口四周留出一圈纯色底，让截屏边缘在 README 的浅色/深色底上都看得清
// （原先浅色界面直接贴在白色 README 上，窗口边界完全糊在一起）。
const MAT = Number(arg("--mat", 20));
const MAT_LIGHT = arg("--mat-light", "#DCE3EC");   // 浅色主题用的底：比界面底 #F8FAFC 明显更深一档
const MAT_DARK = arg("--mat-dark", "#1E293B");     // 深色主题用的底：比界面底 #0B1220 明显更浅一档
const SCALE = Number(arg("--scale", 1.5));       // 1.5 倍图：README 按 1 倍宽度显示，兼顾清晰度与体积
const FRAME_MS = Number(arg("--frame-ms", 120));// 抓帧间隔目标值
const OUT_ROOT = resolve(ROOT, arg("--out", join("docs", ".demo-frames")));
const PORT = 9400 + (process.pid % 300);
// 实际抓图尺寸 = 窗口 + 四周背景板
const CAPTURE = { width: VIEWPORT.width + MAT * 2, height: VIEWPORT.height + MAT * 2 };

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ==================== CDP 客户端 ====================
class CDP {
  constructor(ws) {
    this.ws = ws; this.id = 0; this.pending = new Map();
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
      }, 60000);
    });
  }
  close() { try { this.ws.close(); } catch { /* ignore */ } }
}

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

async function launchBrowser(edge) {
  const profile = join(tmpdir(), `dsh-demo-profile-${PORT}`);
  rmSync(profile, { recursive: true, force: true });
  const proc = spawn(edge, [
    "--headless=new",
    `--remote-debugging-port=${PORT}`,
    `--user-data-dir=${profile}`,
    "--no-first-run", "--no-default-browser-check", "--disable-extensions",
    "--disable-background-networking", "--disable-sync", "--disable-features=Translate",
    "--hide-scrollbars", "--force-device-scale-factor=1",
    `--window-size=${CAPTURE.width},${CAPTURE.height}`,
    "--allow-file-access-from-files",
    "about:blank",
  ], { stdio: "ignore" });
  for (let i = 0; i < 100; i++) {
    try {
      const r = await fetch(`http://127.0.0.1:${PORT}/json/version`);
      if (r.ok) return { proc, profile };
    } catch { /* not yet */ }
    await sleep(120);
  }
  try { proc.kill(); } catch { /* ignore */ }
  throw new Error("浏览器 DevTools 端口未就绪");
}

async function newPage() {
  const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
  const target = list.find((t) => t.type === "page");
  if (!target) throw new Error("找不到可用的页面目标");
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => {
    ws.addEventListener("open", res, { once: true });
    ws.addEventListener("error", () => rej(new Error("DevTools WebSocket 连接失败")), { once: true });
  });
  return new CDP(ws);
}

/** 等界面首屏就绪：设置容器与常规页都可见。 */
async function waitReady(cdp, timeoutMs = 20000) {
  const deadline = Date.now() + timeoutMs;
  let last = "missing";
  while (Date.now() < deadline) {
    const r = await cdp.send("Runtime.evaluate", {
      expression: `(() => {
        const s = document.getElementById("settings");
        const p = document.getElementById("page-general");
        const splash = document.getElementById("splash");
        if (!s || !p) return "missing";
        if (s.classList.contains("hidden")) return "settings-hidden";
        if (splash && !splash.classList.contains("hidden")) return "splash";
        if (p.classList.contains("hidden")) return "page-hidden";
        return "ok";
      })()`,
      returnByValue: true,
    });
    last = r.result.value;
    if (last === "ok") return;
    await sleep(150);
  }
  // 超时把现场打出来，省得再猜（无头下没有 DevTools 可看）
  const dump = await cdp.send("Runtime.evaluate", {
    expression: `JSON.stringify({
      state: ${JSON.stringify(last)},
      readyState: document.readyState,
      hasWindowGo: !!(window.go && window.go.main),
      hasRuntime: !!window.runtime,
      splashHidden: !!document.getElementById("splash")?.classList.contains("hidden"),
      settingsHidden: !!document.getElementById("settings")?.classList.contains("hidden"),
      pageGeneralHidden: !!document.getElementById("page-general")?.classList.contains("hidden"),
      bodyText: (document.body.innerText || "").replace(/\\s+/g, " ").slice(0, 300),
    })`,
    returnByValue: true,
  });
  throw new Error(`等待界面就绪超时（${last}）：${dump.result.value}`);
}

// ==================== 录制脚本（真实交互） ====================
// 每一步：动作 + 持续时间（真实毫秒，抓帧期间按 FRAME_MS 间隔连续抓图）
const TOUR = [
  { type: "wait", ms: 800 },                         // 起帧：常规页静止（循环起点）
  { type: "nav", page: "sync", ms: 650 },
  { type: "wait", ms: 400 },
  { type: "scroll", to: "bottom", ms: 700 },         // 平滑滚到「文件同步」卡
  { type: "wait", ms: 500 },
  { type: "nav", page: "logs", ms: 650 },
  { type: "wait", ms: 500 },
  { type: "nav", page: "about", ms: 650 },
  { type: "wait", ms: 700 },
  { type: "nav", page: "export", ms: 650 },
  { type: "wait", ms: 400 },
  { type: "nav", page: "import", ms: 650 },
  { type: "wait", ms: 400 },
  { type: "nav", page: "help", ms: 650 },
  { type: "wait", ms: 450 },
  { type: "scroll", to: "bottom", ms: 650 },         // 帮助页页末：项目入口
  { type: "wait", ms: 450 },
  { type: "nav", page: "general", ms: 650 },
  { type: "scroll", to: 0, ms: 0 },                  // 回起点滚动位置
  { type: "wait", ms: 600 },                         // 循环接缝
];

/** 等目标页可见并把内容区滚回顶部；再等两帧 + 短暂静置，确保新页面真的画完。
 *  不这么做会抓到「新页面尚未完成绘制」的画面（实测日志页整段发虚、标题像空心字）。 */
async function enterPage(cdp, page) {
  for (let i = 0; i < 40; i++) {
    const r = await cdp.send("Runtime.evaluate", {
      expression: `(() => {
        const el = document.getElementById("page-${page}");
        if (!el || el.classList.contains("hidden")) return "wait";
        const c = document.querySelector(".content");
        if (c) c.scrollTo({ top: 0, behavior: "auto" });
        return "ok";
      })()`,
      returnByValue: true,
    });
    if (r.result.value === "ok") break;
    await sleep(50);
  }
  await cdp.send("Runtime.evaluate", {
    expression: `new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => r("painted"))))`,
    awaitPromise: true, returnByValue: true,
  });
  await sleep(250);
}

async function act(cdp, step) {
  if (step.type === "nav") {
    await cdp.send("Runtime.evaluate", {
      expression: `(() => { const b = document.querySelector('.nav-item[data-page="${step.page}"]');
        if (!b) return "no-nav"; b.click(); return b.dataset.page; })()`,
      returnByValue: true,
    });
    await enterPage(cdp, step.page);
  } else if (step.type === "scroll") {
    await cdp.send("Runtime.evaluate", {
      expression: `(() => { const c = document.querySelector(".content"); if (!c) return "no-content";
        const top = ${step.to === "bottom" ? "c.scrollHeight" : String(step.to)};
        c.scrollTo({ top, behavior: ${step.ms > 0 ? '"smooth"' : '"auto"'} }); return String(top); })()`,
      returnByValue: true,
    });
  }
}

/** 注入背景板：视口比窗口大一圈，窗口按 840x560 固定，四周留出纯色底（只影响本次录制）。
 *  注意窗口自己要保留界面底色 var(--bg)——界面底色原本来自 body，若只改 body 会把窗口内部
 *  也刷成背景色，边界反而更看不清。 */
async function applyMat(cdp, scheme) {
  const color = scheme === "dark" ? MAT_DARK : MAT_LIGHT;
  const css = `html,body{background:${color}!important}` +
    `.settings{width:${VIEWPORT.width}px!important;height:${VIEWPORT.height}px!important;` +
    `margin:${MAT}px!important;background:var(--bg)!important}`;
  await cdp.send("Runtime.evaluate", {
    expression: `(() => { const s = document.createElement("style"); s.id = "dsh-demo-mat";
      s.textContent = ${JSON.stringify(css)}; document.head.appendChild(s); return "mat"; })()`,
    returnByValue: true,
  });
}

/** 录制一套（lang × scheme）。 */
async function recordVariant(cdp, baseURL, lang, scheme) {
  const tStart = Date.now();
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: CAPTURE.width, height: CAPTURE.height,
    deviceScaleFactor: SCALE, mobile: false,
    screenWidth: CAPTURE.width, screenHeight: CAPTURE.height,
  });
  await cdp.send("Emulation.setEmulatedMedia", {
    features: [{ name: "prefers-color-scheme", value: scheme }],
  });
  // page=general 必须带：shim 用它设置 __shotPage，前端据此在自动数据到位后收起 splash
  // （无此参数会永远停在「正在准备运行环境…」——这是本脚本第一次跑挂的原因）。
  const nav = await cdp.send("Page.navigate", { url: `${baseURL}?page=general&lang=${lang}` });
  if (nav.errorText) throw new Error(`导航失败：${nav.errorText}`);
  await waitReady(cdp);
  await applyMat(cdp, scheme);
  await sleep(600); // 让首屏异步数据（版本号 / 插件清单）落定，避免起帧是半成品
  const tRecord = Date.now();

  const dir = join(OUT_ROOT, `${lang}-${scheme}`);
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });

  const t0 = Date.now();
  const frames = [];
  const seen = new Set();

  /** 抓一帧：写盘 + 记录时间戳与内容 hash（hash 供编码阶段合并静止段）。 */
  const capture = async () => {
    const at = Date.now() - t0;
    const shot = await cdp.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false, fromSurface: true });
    const buf = Buffer.from(shot.data, "base64");
    const file = `${String(frames.length + 1).padStart(4, "0")}.png`;
    writeFileSync(join(dir, file), buf);
    frames.push({ file, at, hash: createHash("md5").update(buf).digest("hex") });
    seen.add(frames[frames.length - 1].hash);
    const spent = Date.now() - t0 - at;
    if (FRAME_MS - spent > 0) await sleep(FRAME_MS - spent);
  };

  for (const step of TOUR) {
    await act(cdp, step);
    const until = Date.now() + step.ms;
    do { await capture(); } while (Date.now() < until);
  }

  // 逐帧 delay：到下一帧的真实间隔（回放即实时）；末帧沿用前一帧的间隔
  const meta = {
    lang, scheme, viewport: VIEWPORT, capture: CAPTURE, mat: MAT, scale: SCALE,
    frames: frames.map((f, i) => {
      const next = frames[i + 1];
      const prev = frames[i - 1];
      const raw = next ? next.at - f.at : (prev ? f.at - prev.at : 500);
      return { file: f.file, delay: Math.max(20, Math.round(raw)) };
    }),
  };
  writeFileSync(join(dir, "meta.json"), JSON.stringify(meta, null, 2), "utf8");

  const total = meta.frames.reduce((a, f) => a + f.delay, 0);
  const ready = ((tRecord - tStart) / 1000).toFixed(1);
  const loop = ((Date.now() - tRecord) / 1000).toFixed(1);
  console.log(`  ${lang}-${scheme}: ${frames.length} 帧（去重后 ${seen.size} 种画面）/ 回放 ${(total / 1000).toFixed(1)} s / 就绪 ${ready}s + 抓帧 ${loop}s → ${join("docs", ".demo-frames", `${lang}-${scheme}`)}`);
  return { frames: frames.length, unique: seen.size, totalMs: total };
}

// ==================== 主流程 ====================
async function main() {
  const edge = await findEdge();
  const srv = await serveDist();
  const baseURL = `http://127.0.0.1:${srv.address().port}/index.html`;
  const { proc } = await launchBrowser(edge);
  console.log(`recorder: ${edge}`);
  console.log(`窗口 ${VIEWPORT.width}x${VIEWPORT.height} + 背景板 ${MAT}px → 抓图 ${CAPTURE.width}x${CAPTURE.height} @${SCALE}x  抓帧 ${FRAME_MS}ms`);

  let cdp = null;
  try {
    cdp = await newPage();
    await cdp.send("Page.enable");
    for (const lang of LANGS) {
      // shim 的语言在注入时就固定了（?lang 只影响 shim 读到的参数），换语言必须换一份注入脚本：
      // 每次进入新语言前注册该语言的 shim，离开时移除，避免脚本层层叠加。
      const { identifier } = await cdp.send("Page.addScriptToEvaluateOnNewDocument", { source: shimSource(lang) });
      for (const scheme of SCHEMES) {
        await recordVariant(cdp, baseURL, lang, scheme);
      }
      await cdp.send("Page.removeScriptToEvaluateOnNewDocument", { identifier });
    }
  } finally {
    if (cdp) cdp.close();
    try { proc.kill(); } catch { /* ignore */ }
    srv.close();
  }
  console.log("done: 帧序列写入 docs/.demo-frames/（下一步 node scripts/encode_demo.mjs）");
}

main().catch((e) => { console.error("录制失败:", e.message); process.exit(1); });

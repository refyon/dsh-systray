#!/usr/bin/env node
/**
 * encode_demo.mjs：把 record_demo.mjs 录下的帧序列编码成 README 用的 APNG
 * ============================================================================
 * 为什么是 APNG：GitHub README 的动图原来是 GIF（8-bit 256 色），840×560 的真实界面
 * 压进 256 色后渐变起色带、中文小字边缘发毛；APNG 是 PNG 容器（24 位色 + 完整 alpha），
 * 浏览器支持面与 GIF 相当（Chrome 59+ / Firefox 3+ / Safari 8+），GitHub 按原字节下发。
 *
 * 流程：
 *   1) 读取 docs/.demo-frames/<lang>-<scheme>/（帧 + meta.json 的逐帧 delay）；
 *   2) 合并相邻的相同画面（静止段只留一帧，delay 累加）——这一步通常能砍掉一半以上帧数；
 *   3) 交给 ffmpeg 量化 + 组装 APNG（-plays 0 无限循环、-pred mixed 提升压缩率）。
 *
 * 需要 ffmpeg（非本仓库依赖，用任何一种方式提供即可）：
 *   node scripts/encode_demo.mjs --ffmpeg <path/to/ffmpeg>
 *   DSH_FFMPEG=<path/to/ffmpeg> node scripts/encode_demo.mjs
 *   PATH 里已有 ffmpeg 时直接 node scripts/encode_demo.mjs
 *
 * 用法：
 *   node scripts/encode_demo.mjs                       # 全部变体
 *   node scripts/encode_demo.mjs --mode truecolor       # 不量化（体积大，画质无损）
 *   node scripts/encode_demo.mjs --colors 128           # 色板更小、体积更小
 *   node scripts/encode_demo.mjs --max-mb 3             # 超过该体积只告警不失败
 *
 * 产物：docs/demo/settings-<lang>.apng 与 settings-<lang>-dark.apng
 */
import { spawnSync } from "node:child_process";
import { mkdirSync, readFileSync, readdirSync, writeFileSync, existsSync, statSync, rmSync } from "node:fs";
import { createHash } from "node:crypto";
import { join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, "..");

const argv = process.argv.slice(2);
const arg = (k, d) => { const i = argv.indexOf(k); return i >= 0 ? argv[i + 1] : d; };
// --frames / --out 用于对比实验（例如把 1 倍图帧序列编到临时目录与 2 倍图比锐度）
const FRAMES_ROOT = resolve(ROOT, arg("--frames", join("docs", ".demo-frames")));
const OUT_DIR = resolve(ROOT, arg("--out", join("docs", "demo")));
const LANGS = arg("--lang", "zh,en").split(",").map((s) => s.trim()).filter(Boolean);
const SCHEMES = arg("--scheme", "light,dark").split(",").map((s) => s.trim()).filter(Boolean);
const MODE = arg("--mode", "palette");            // palette | truecolor
const COLORS = Number(arg("--colors", 256));
const DITHER = arg("--dither", "none");           // 默认不抖动：实测同色数下比 sierra2_4a 更小且 PSNR 更高
const STATS = arg("--stats", "diff");             // diff（偏向变化区域）| full（全画面等权，静止文字更准）
const MAX_MB = Number(arg("--max-mb", 3));

function findFfmpeg() {
  const explicit = arg("--ffmpeg", "") || process.env.DSH_FFMPEG || "";
  if (explicit) {
    if (!existsSync(explicit)) throw new Error(`指定的 ffmpeg 不存在：${explicit}`);
    return explicit;
  }
  const probe = spawnSync("ffmpeg", ["-version"], { stdio: "ignore" });
  if (probe.status === 0) return "ffmpeg";
  throw new Error("找不到 ffmpeg：请用 --ffmpeg <path> 或设置 DSH_FFMPEG，或把它放进 PATH。");
}

/** 输出文件名：浅色沿用 settings-<lang>.apng，深色加 -dark 后缀（README 靠 #gh-dark-mode-only 分派）。 */
const outName = (lang, scheme) => `settings-${lang}${scheme === "dark" ? "-dark" : ""}.apng`;

function encodeVariant(ffmpeg, lang, scheme) {
  const dir = join(FRAMES_ROOT, `${lang}-${scheme}`);
  const metaPath = join(dir, "meta.json");
  if (!existsSync(metaPath)) {
    console.log(`  跳过 ${lang}-${scheme}：没有 ${join("docs", ".demo-frames", `${lang}-${scheme}`)}`);
    return null;
  }
  const meta = JSON.parse(readFileSync(metaPath, "utf8"));

  // 1) 合并相邻同画面：hash 相同且相邻 → 只留第一帧，delay 累加到它身上
  const merged = [];
  let lastHash = "";
  for (const f of meta.frames) {
    const buf = readFileSync(join(dir, f.file));
    const hash = createHash("md5").update(buf).digest("hex");
    if (hash === lastHash && merged.length) {
      merged[merged.length - 1].delay += f.delay;
      continue;
    }
    lastHash = hash;
    merged.push({ file: f.file, delay: f.delay, hash });
  }

  // 2) ffconcat 清单（duration 必须写在每个 file 之后，末帧也要写）
  const listPath = join(dir, "frames.ffconcat");
  const lines = ["ffconcat version 1.0"];
  for (const f of merged) {
    lines.push(`file '${join(dir, f.file).replace(/\\/g, "/").replace(/'/g, "'\\''")}'`);
    lines.push(`duration ${(f.delay / 1000).toFixed(3)}`);
  }
  writeFileSync(listPath, lines.join("\n") + "\n", "utf8");

/**
 * 按 PNG 块拼装 APNG：**每帧都是整画布、dispose=NONE、blend=SOURCE**。
 *
 * 为什么不用 ffmpeg 直接出 APNG：ffmpeg 的 apng 编码器会做帧差分，产出形如
 * 「1216x1120@404,0 dispose=BACKGROUND blend=OVER」的局部帧（甚至 1x1@0,0 的怪帧）。
 * 这类文件的最终画面完全依赖浏览器的 APNG 合成实现，不同解码器表现不一致——实测用户
 * 反馈的「标题空心/发虚」就出现在这一类帧上。改为全画布、不透明、覆盖式写入后，
 * 任何实现都只有一种渲染结果。
 *
 * 数据不用重新压缩：ffmpeg 已按同一块全局色板输出了各帧 PNG，这里只搬 IDAT 负载。
 */
function crc32(buf) {
  let c, crc = 0xffffffff;
  for (let i = 0; i < buf.length; i++) {
    c = (crc ^ buf[i]) & 0xff;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    crc = (crc >>> 8) ^ c;
  }
  return (crc ^ 0xffffffff) >>> 0;
}

function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, "latin1"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
}

/** 解析 PNG，返回 {width, height, colorType, plte?, trns?, idat} */
function readPng(file) {
  const buf = readFileSync(file);
  if (buf.slice(0, 8).toString("latin1") !== "\x89PNG\r\n\x1a\n") throw new Error(`不是 PNG：${file}`);
  let pos = 8, ihdr = null, plte = null, trns = null;
  const idats = [];
  while (pos + 8 <= buf.length) {
    const len = buf.readUInt32BE(pos);
    const type = buf.slice(pos + 4, pos + 8).toString("latin1");
    const data = buf.slice(pos + 8, pos + 8 + len);
    if (type === "IHDR") ihdr = data;
    else if (type === "PLTE") plte = data;
    else if (type === "tRNS") trns = data;
    else if (type === "IDAT") idats.push(data);
    pos += 12 + len;
    if (type === "IEND") break;
  }
  if (!ihdr) throw new Error(`PNG 缺 IHDR：${file}`);
  return {
    width: ihdr.readUInt32BE(0), height: ihdr.readUInt32BE(4),
    bitDepth: ihdr[8], colorType: ihdr[9], plte, trns,
    idat: Buffer.concat(idats),
  };
}

function packApng(frameFiles, delaysMs, outPath) {
  const frames = frameFiles.map(readPng);
  const { width, height, bitDepth, colorType, plte, trns } = frames[0];
  if (frames.some((f) => f.width !== width || f.height !== height)) throw new Error("各帧尺寸不一致");
  for (const [i, f] of frames.entries()) {
    if ((f.plte === null) !== (plte === null)) throw new Error(`第 ${i + 1} 帧色板与首帧不一致`);
    if (f.plte && plte && !f.plte.equals(plte)) throw new Error(`第 ${i + 1} 帧色板与首帧不同（需用固定色板）`);
  }

  const parts = [Buffer.from("\x89PNG\r\n\x1a\n", "latin1")];
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0); ihdr.writeUInt32BE(height, 4);
  ihdr[8] = bitDepth; ihdr[9] = colorType; ihdr[10] = 0; ihdr[11] = 0; ihdr[12] = 0;
  parts.push(chunk("IHDR", ihdr));
  if (plte) parts.push(chunk("PLTE", plte));
  if (trns) parts.push(chunk("tRNS", trns));

  const actl = Buffer.alloc(8);
  actl.writeUInt32BE(frames.length, 0); actl.writeUInt32BE(0, 4);   // num_plays=0 → 无限循环
  parts.push(chunk("acTL", actl));

  let seq = 0;
  for (const [i, f] of frames.entries()) {
    const fctl = Buffer.alloc(26);
    fctl.writeUInt32BE(seq++, 0);
    fctl.writeUInt32BE(width, 4); fctl.writeUInt32BE(height, 8);
    fctl.writeUInt32BE(0, 12); fctl.writeUInt32BE(0, 16);          // 整画布，偏移 0,0
    const ms = Math.max(20, Math.round(delaysMs[i]));
    fctl.writeUInt16BE(Math.min(65535, ms), 20); fctl.writeUInt16BE(1000, 22);
    fctl[24] = 0;                                                   // dispose_op = NONE
    fctl[25] = 0;                                                   // blend_op = SOURCE（直接覆盖）
    parts.push(chunk("fcTL", fctl));
    if (i === 0) {
      parts.push(chunk("IDAT", f.idat));
    } else {
      const s = Buffer.alloc(4);
      s.writeUInt32BE(seq++);
      parts.push(chunk("fdAT", Buffer.concat([s, f.idat])));
    }
  }
  parts.push(chunk("IEND", Buffer.alloc(0)));
  const buf = Buffer.concat(parts);
  writeFileSync(outPath, buf);
  return buf.length;
}

  // 3) 两遍编码：第一遍按全片统计生成**全局色板**，第二遍按该色板逐帧导出 PNG（各帧色板一致），
  //    最后由 packApng 拼成全画布 APNG。
  mkdirSync(OUT_DIR, { recursive: true });
  const out = join(OUT_DIR, outName(lang, scheme));
  rmSync(out, { force: true });
  const frameDir = join(dir, "enc");
  rmSync(frameDir, { recursive: true, force: true });
  mkdirSync(frameDir, { recursive: true });

  const run = (args, what) => {
    const r = spawnSync(ffmpeg, args, { encoding: "utf8" });
    if (r.status !== 0) throw new Error(`ffmpeg ${what}失败（${lang}-${scheme}）：${(r.stderr || "").trim()}`);
  };
  const common = ["-y", "-hide_banner", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", listPath];

  if (MODE === "palette") {
    const pal = join(frameDir, "palette.png");
    run([...common, "-vf", `palettegen=max_colors=${COLORS}:stats_mode=${STATS}`, "-frames:v", "1", pal], "生成色板");
    // 不加 -pred mixed：调色板 + 抖动的画面用默认预测更小（实测 490 KB vs 579 KB）
    run([...common, "-i", pal, "-lavfi", `[0:v][1:v]paletteuse=dither=${DITHER}:diff_mode=rectangle`,
         "-fps_mode", "passthrough", join(frameDir, "f%04d.png")], "按色板导出帧");
  } else {
    run([...common, "-fps_mode", "passthrough", join(frameDir, "f%04d.png")], "导出帧");
  }

  const files = readdirSync(frameDir).filter((f) => /^f\d+\.png$/.test(f)).sort();
  if (files.length !== merged.length) {
    throw new Error(`导出帧数不符（${lang}-${scheme}）：得到 ${files.length}，期望 ${merged.length}`);
  }
  packApng(files.map((f) => join(frameDir, f)), merged.map((m) => m.delay), out);

  const kb = Math.round(statSync(out).size / 1024);
  const seconds = (merged.reduce((a, f) => a + f.delay, 0) / 1000).toFixed(1);
  const flag = statSync(out).size / 1048576 > MAX_MB ? "  ⚠ 超出体积预算" : "";
  console.log(`  ${outName(lang, scheme)}: ${merged.length} 帧（原 ${meta.frames.length}）/ ${seconds} s / ${kb} KB${flag}`);
  return { file: out, size: statSync(out).size, frames: merged.length };
}

function main() {
  const ffmpeg = findFfmpeg();
  const entries = readdirSync(FRAMES_ROOT, { withFileTypes: true }).filter((e) => e.isDirectory()).map((e) => e.name);
  if (!entries.length) throw new Error(`没有可编码的帧序列：${join("docs", ".demo-frames")} 为空，先跑 node scripts/record_demo.mjs`);
  console.log(`encoder: ${ffmpeg === "ffmpeg" ? "PATH 中的 ffmpeg" : ffmpeg}  模式: ${MODE}${MODE === "palette" ? ` / ${COLORS} 色 / 抖动 ${DITHER}` : ""}`);
  const results = [];
  for (const lang of LANGS) {
    for (const scheme of SCHEMES) {
      const item = encodeVariant(ffmpeg, lang, scheme);
      if (item) results.push(item);
    }
  }
  const total = results.reduce((a, r) => a + r.size, 0);
  console.log(`done: ${results.length} 个 APNG，合计 ${(total / 1048576).toFixed(2)} MB → docs/demo/`);
}

try { main(); } catch (e) { console.error("编码失败:", e.message); process.exit(1); }

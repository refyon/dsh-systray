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
const FRAMES_ROOT = join(ROOT, "docs", ".demo-frames");
const OUT_DIR = join(ROOT, "docs", "demo");

const argv = process.argv.slice(2);
const arg = (k, d) => { const i = argv.indexOf(k); return i >= 0 ? argv[i + 1] : d; };
const LANGS = arg("--lang", "zh,en").split(",").map((s) => s.trim()).filter(Boolean);
const SCHEMES = arg("--scheme", "light,dark").split(",").map((s) => s.trim()).filter(Boolean);
const MODE = arg("--mode", "palette");            // palette | truecolor
const COLORS = Number(arg("--colors", 256));
const DITHER = arg("--dither", "sierra2_4a");
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

  // 3) 编码：palette 模式先按全片统计生成色板（stats_mode=diff 更偏向变化区域），再抖动映射
  const vf = MODE === "palette"
    ? `split[a][b];[a]palettegen=max_colors=${COLORS}:stats_mode=diff[p];[b][p]paletteuse=dither=${DITHER}:diff_mode=rectangle`
    : "null";
  mkdirSync(OUT_DIR, { recursive: true });
  const out = join(OUT_DIR, outName(lang, scheme));
  rmSync(out, { force: true });
  const r = spawnSync(ffmpeg, [
    "-y", "-hide_banner", "-loglevel", "error",
    "-f", "concat", "-safe", "0", "-i", listPath,
    "-fps_mode", "vfr",
    "-vf", vf,
    "-plays", "0",
    "-pred", "mixed",
    "-f", "apng", out,
  ], { encoding: "utf8" });
  if (r.status !== 0) throw new Error(`ffmpeg 编码失败（${lang}-${scheme}）：${(r.stderr || "").trim()}`);

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

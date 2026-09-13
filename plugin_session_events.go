package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ==================== 插件删除前的会话数据风险检查 ====================
//
// 背景（2026-09-13 现场实证，dsh-systray.log / .dsh/sessions）：
//
//	插件 dsh-cost-meter 在联网搜索后向会话日志追加自定义事件
//	（cost-meter/native-search-usage），但没有按 harness 的外部插件约定带 `ignorable: true`。
//	用户从 profile 卸载该插件后，harness 读日志时对该类型 fail-closed——「事件类型未知且未标
//	ignorable」即拒绝解析整份日志，Web UI 报「历史加载失败」，这些会话再也打不开。
//	（读路径 dsh-session-persistence validateStoredEvents；写路径不校验类型，所以插件能写进去。）
//
// 插件自身的行为 harness 约束不了，但删除的动作由 systray 发起，所以把检查前移到删除登记环节：
//
//	「这个插件往会话日志里写过自定义事件吗？删掉后有多少会话会打不开？」
//
// 并提供「修复」：给这些事件补上 ignorable:true（该标记本就是 harness 为「可安全跳过的外部事件」
// 准备的兼容机制；计费/统计类记录正属于此列），修复后任何构建都能正常打开这些会话。
//
// 实现沿用 plugin_preflight.go 的既有形态：Node 一次性脚本 + 机器可读行输出。
// 为什么用 Node 而不是 Go：会话日志是**多帧 zstd**（node:zlib 的 zstd 就是 harness 自身用的实现），
// 为读日志给 Go 引入 zstd 依赖不划算；systray 本来就依赖 Node 运行时（pnpm / 插件预检）。
//
// 归因口径：事件类型命名空间 ↔ 插件包名。type `cost-meter/native-search-usage` 的命名空间是
// `cost-meter`，插件 `dsh-cost-meter` 去 `dsh-` 前缀后同名——据此判定「这个类型是该插件写的」。
// 命名空间与包名无关的插件不会被点名（检测不到而非误报），这是有意的保守取值。

// sessionEventToolScript 会话事件工具脚本（scan / repair 两种模式）。
// 参数：argv[1]=模式 argv[2]=sessions 目录 argv[3]=harness 目录 [argv[4..]=事件类型]
// 输出：每行以 DSHEVENTS\t 开头的机器可读结果（TYPE / SESSION / DONE / WARN / ERR）。
// 注意：Go 用反引号原文承载本脚本，脚本内因此不使用模板字符串。
const sessionEventToolScript = `import fs from "node:fs";
import path from "node:path";
import { zstdCompressSync, zstdDecompressSync, constants } from "node:zlib";

const MARK = "DSHEVENTS\t";
const MAGIC = 4247762216;

function say(line) { console.log(MARK + line); }
function fail(msg) { say("ERR\t" + String(msg).replace(/[\r\n\t]+/g, " ")); process.exit(1); }

// scanFrames 与 dsh-session-persistence-jsonl 的扫描口径一致：会话日志是**拼接的 zstd 帧**，
// 一帧一个追加批次；帧尾不完整（正在写入）时报 tornStart，由调用方决定是否容忍。
function scanFrames(buffer) {
  const frames = [];
  let offset = 0;
  while (offset < buffer.length) {
    const start = offset;
    if (buffer.length - offset < 4) return { frames, tornStart: start };
    if (buffer.readUInt32LE(offset) !== MAGIC) throw new Error("bad zstd magic at byte " + offset);
    offset += 4;
    if (offset === buffer.length) return { frames, tornStart: start };
    const descriptor = buffer.readUInt8(offset); offset += 1;
    const contentSizeFlag = descriptor >>> 6;
    const singleSegment = (descriptor & 32) !== 0;
    const checksum = (descriptor & 4) !== 0;
    const dictionaryFlag = descriptor & 3;
    const dictionaryBytes = dictionaryFlag === 3 ? 4 : dictionaryFlag;
    const contentSizeBytes = contentSizeFlag === 0 ? (singleSegment ? 1 : 0) : (1 << contentSizeFlag);
    offset += (singleSegment ? 0 : 1) + dictionaryBytes + contentSizeBytes;
    for (;;) {
      if (buffer.length - offset < 3) return { frames, tornStart: start };
      const blockHeader = buffer.readUIntLE(offset, 3); offset += 3;
      const lastBlock = (blockHeader & 1) !== 0;
      const blockType = (blockHeader >>> 1) & 3;
      const blockSize = blockHeader >>> 3;
      const payloadBytes = blockType === 1 ? 1 : blockSize;
      if (buffer.length - offset < payloadBytes) return { frames, tornStart: start };
      offset += payloadBytes;
      if (lastBlock) break;
    }
    if (checksum) {
      if (buffer.length - offset < 4) return { frames, tornStart: start };
      offset += 4;
    }
    frames.push({ start, end: offset });
  }
  return { frames };
}

function readLogText(file) {
  if (!file.endsWith(".zstd")) return fs.readFileSync(file, "utf8");
  const buffer = fs.readFileSync(file);
  const scanned = scanFrames(buffer);
  let text = "";
  for (const f of scanned.frames) text += zstdDecompressSync(buffer.subarray(f.start, f.end)).toString("utf8");
  if (scanned.tornStart !== undefined) {
    try { text += zstdDecompressSync(buffer.subarray(scanned.tornStart), { finishFlush: constants.ZSTD_e_flush }).toString("utf8"); } catch (error) {}
  }
  return text;
}

// findCatalog 定位当前构建的事件白名单文件（.pnpm 哈希目录名随安装变化，故按前缀扫描）。
function findCatalog(harnessDir) {
  const direct = path.join(harnessDir, "node_modules", "@deepseek-ai", "dsh-session", "lib", "types", "known-event-types.js");
  if (fs.existsSync(direct)) return direct;
  const pnpm = path.join(harnessDir, "node_modules", ".pnpm");
  let entries = [];
  try { entries = fs.readdirSync(pnpm, { withFileTypes: true }); } catch (error) { return ""; }
  const hits = [];
  for (const entry of entries) {
    if (!entry.isDirectory() || entry.name.indexOf("@deepseek-ai+dsh-session@") !== 0) continue;
    const p = path.join(pnpm, entry.name, "node_modules", "@deepseek-ai", "dsh-session", "lib", "types", "known-event-types.js");
    if (fs.existsSync(p)) hits.push(p);
  }
  hits.sort(function (a, b) { return fs.statSync(b).mtimeMs - fs.statSync(a).mtimeMs; });
  return hits.length ? hits[0] : "";
}

function readKnown(file) {
  const src = fs.readFileSync(file, "utf8");
  const start = src.indexOf("new Set([");
  const end = start < 0 ? -1 : src.indexOf("]);", start);
  if (start < 0 || end < 0) return null;
  const known = new Set();
  const re = /'([^']+)'/g;
  let m;
  while ((m = re.exec(src.slice(start, end))) !== null) known.add(m[1]);
  return known;
}

// eachSessionLog 罗列 sessions 根下的**当前世代**会话日志：兼容两级布局（sessions/<scope>/<session>/）
// 与单级布局（sessions/<session>/），zstd 与明文两种压缩都收。
// 只收带世代号的规范名（session.v3.jsonl.zstd 之类）：版本 0 的旧世代沿用无版本名
// （session.jsonl.zstd），其事件词表属历史格式、由 harness 的迁移路径处理，不是本检查的对象——
// 混进来会把旧格式当成「未知事件类型」，既制造噪音又可能误判成这次删除带来的风险。
function eachSessionLog(root) {
  const out = [];
  let scopes = [];
  try { scopes = fs.readdirSync(root, { withFileTypes: true }); } catch (error) { return out; }
  const isLog = function (name) { return /^session\.v\d+\.jsonl(\.zstd)?$/.test(name); };
  for (const scope of scopes) {
    if (!scope.isDirectory()) continue;
    const scopeDir = path.join(root, scope.name);
    let kids = [];
    try { kids = fs.readdirSync(scopeDir, { withFileTypes: true }); } catch (error) { continue; }
    let nested = false;
    for (const kid of kids) {
      if (!kid.isDirectory()) continue;
      nested = true;
      const sessionDir = path.join(scopeDir, kid.name);
      let files = [];
      try { files = fs.readdirSync(sessionDir, { withFileTypes: true }); } catch (error) { continue; }
      for (const f of files) if (f.isFile() && isLog(f.name)) out.push(path.join(sessionDir, f.name));
    }
    if (!nested) {
      for (const kid of kids) if (kid.isFile() && isLog(kid.name)) out.push(path.join(scopeDir, kid.name));
    }
  }
  return out;
}

// foreignCounts 统计一份日志里「当前构建不认识、且未标 ignorable」的事件类型 → 条数。
// 第 0 行是会话头部（type=session），不是事件，跳过。
function foreignCounts(text, known) {
  const counts = new Map();
  const lines = text.split("\n");
  for (let i = 1; i < lines.length; i++) {
    const line = lines[i];
    if (!line) continue;
    let ev;
    try { ev = JSON.parse(line); } catch (error) { continue; }
    if (!ev || typeof ev.type !== "string") continue;
    if (known.has(ev.type) || ev.ignorable === true) continue;
    counts.set(ev.type, (counts.get(ev.type) || 0) + 1);
  }
  return counts;
}

function stamp() { return new Date().toISOString().replace(/[:.]/g, ""); }

const mode = process.argv[1];
const sessionsRoot = process.argv[2];
const harnessDir = process.argv[3];
const extra = process.argv.slice(4);

if (!mode || !sessionsRoot || !harnessDir) fail("用法：<scan|repair> <sessionsDir> <harnessDir> [type...]");
if (!fs.existsSync(sessionsRoot)) fail("会话目录不存在：" + sessionsRoot);
const catalogFile = findCatalog(harnessDir);
if (!catalogFile) fail("未找到 harness 事件白名单（known-event-types.js），harnessDir=" + harnessDir);
const known = readKnown(catalogFile);
if (!known || known.size === 0) fail("无法解析事件白名单：" + catalogFile);

if (mode === "scan") {
  const stats = new Map();
  let sessions = 0;
  let examined = 0;
  for (const file of eachSessionLog(sessionsRoot)) {
    examined += 1;
    let text = "";
    try { text = readLogText(file); } catch (error) { continue; }
    const counts = foreignCounts(text, known);
    if (counts.size === 0) continue;
    sessions += 1;
    const types = Array.from(counts.keys());
    say("SESSION\t" + path.relative(sessionsRoot, file) + "\t" + types.join(";"));
    for (const type of types) {
      const cur = stats.get(type) || { sessions: 0, events: 0 };
      cur.sessions += 1;
      cur.events += counts.get(type) || 0;
      stats.set(type, cur);
    }
  }
  for (const [type, cur] of stats) say("TYPE\t" + type + "\t" + cur.sessions + "\t" + cur.events);
  say("DONE\t" + sessions + "\t" + stats.size + "\t" + examined);
} else if (mode === "repair") {
  const want = new Set(extra);
  if (want.size === 0) fail("repair 模式需要至少一个事件类型");
  let files = 0, events = 0, skipped = 0, failed = 0;
  for (const file of eachSessionLog(sessionsRoot)) {
    if (!file.endsWith(".zstd")) { skipped += 1; continue; }
    let buffer;
    try { buffer = fs.readFileSync(file); } catch (error) { continue; }
    let scanned;
    try { scanned = scanFrames(buffer); } catch (error) { failed += 1; say("WARN\t" + file + "\t" + error.message); continue; }
    // 正在写入的日志（尾帧不完整）不动：改写会与追加写冲突，留给下一次修复。
    if (scanned.tornStart !== undefined) { skipped += 1; continue; }
    const frames = scanned.frames;
    const rebuilt = [buffer.subarray(frames[0].start, frames[0].end)];
    let changed = 0;
    for (let i = 1; i < frames.length; i++) {
      const text = zstdDecompressSync(buffer.subarray(frames[i].start, frames[i].end)).toString("utf8");
      const endsWithNewline = text.endsWith("\n");
      const lines = text.split("\n");
      if (endsWithNewline) lines.pop();
      let touched = false;
      const next = lines.map(function (line) {
        let ev;
        try { ev = JSON.parse(line); } catch (error) { return line; }
        if (!ev || !want.has(ev.type) || ev.ignorable === true) return line;
        ev.ignorable = true;
        changed += 1;
        touched = true;
        return JSON.stringify(ev);
      });
      if (!touched) { rebuilt.push(buffer.subarray(frames[i].start, frames[i].end)); continue; }
      rebuilt.push(zstdCompressSync(Buffer.from(next.join("\n") + (endsWithNewline ? "\n" : ""), "utf8"), { params: { [constants.ZSTD_c_checksumFlag]: 1 } }));
    }
    if (changed === 0) continue;
    try {
      fs.copyFileSync(file, file + ".bak-" + stamp());
      fs.writeFileSync(file, Buffer.concat(rebuilt));
    } catch (error) { failed += 1; say("WARN\t" + file + "\t" + error.message); continue; }
    // 改写后自检：白名单 vocabulary 门 + seq 连续性（原文件已有 .bak 备份）。
    try {
      const after = fs.readFileSync(file);
      const rescan = scanFrames(after);
      if (rescan.tornStart !== undefined) throw new Error("改写后尾帧不完整");
      let index = 0;
      for (const f of rescan.frames.slice(1)) {
        const lines = zstdDecompressSync(after.subarray(f.start, f.end)).toString("utf8").split("\n");
        for (const line of lines) {
          if (!line) continue;
          let ev;
          try { ev = JSON.parse(line); } catch (error) { throw new Error("改写后存在非法 JSON 行"); }
          if (!known.has(ev.type) && ev.ignorable !== true) throw new Error("改写后仍存在未标记类型：" + ev.type);
          if (ev.seq !== index) throw new Error("seq 不连续：" + ev.seq + "，期望 " + index);
          index += 1;
        }
      }
    } catch (error) { failed += 1; say("WARN\t" + file + "\t自检失败：" + error.message); }
    files += 1;
    events += changed;
  }
  say("DONE\t" + files + "\t" + events + "\t" + skipped + "\t" + failed);
} else {
  fail("未知模式：" + mode);
}
`

// sessionEventMarker 工具脚本结果行前缀（与脚本内一致）。
const sessionEventMarker = "DSHEVENTS\t"

// pluginSessionRisk 删除某插件前检测到的会话数据风险。
type pluginSessionRisk struct {
	Sessions int      `json:"sessions"` // 受影响会话数（删除后打不开）
	Events   int      `json:"events"`   // 受影响事件条数
	Types    []string `json:"types"`    // 事件类型（如 cost-meter/native-search-usage）
}

// pluginEventStat 扫描结果里某个事件类型的统计。
type pluginEventStat struct {
	Type     string
	Sessions int
	Events   int
}

// sessionEventScan 一次全量扫描的结果：按类型的统计 + 每个受影响会话的类型清单
// （会话清单用于算「受影响会话数」的并集，避免按类型数量相加导致重复计数）。
type sessionEventScan struct {
	Stats     []pluginEventStat
	BySession map[string][]string
	Warnings  []string
}

// pluginEventNamespaces 由插件包名推导它可能使用的事件命名空间：包名、去 scope 的包名、
// 以及两者去 `dsh-` 前缀的形式（dsh-cost-meter → cost-meter）。全部小写，去重。
func pluginEventNamespaces(name string) []string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return nil
	}
	base := n
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(n)
	add(base)
	for _, v := range []string{n, base} {
		if strings.HasPrefix(v, "dsh-") {
			add(strings.TrimPrefix(v, "dsh-"))
		}
	}
	return out
}

// pluginEventNamespaceOf 取事件类型的命名空间（首个 `/` 之前），无斜杠返回空。
func pluginEventNamespaceOf(eventType string) string {
	if i := strings.Index(eventType, "/"); i > 0 {
		return strings.ToLower(eventType[:i])
	}
	return ""
}

// pluginRiskFromScan 按插件包名从扫描结果里摘出属于它的风险；无匹配返回 nil。
func pluginRiskFromScan(name string, scan sessionEventScan) *pluginSessionRisk {
	ns := pluginEventNamespaces(name)
	if len(ns) == 0 {
		return nil
	}
	owned := map[string]bool{}
	for _, n := range ns {
		owned[n] = true
	}
	sessions, events := 0, 0
	var types []string
	for _, st := range scan.Stats {
		if !owned[pluginEventNamespaceOf(st.Type)] {
			continue
		}
		types = append(types, st.Type)
		events += st.Events
	}
	for _, sessionTypes := range scan.BySession {
		for _, t := range sessionTypes {
			if owned[pluginEventNamespaceOf(t)] {
				sessions++
				break
			}
		}
	}
	if len(types) == 0 {
		return nil
	}
	sort.Strings(types)
	return &pluginSessionRisk{Sessions: sessions, Events: events, Types: types}
}

// parseSessionEventScan 解析 scan 模式输出（纯函数，便于单测）。
func parseSessionEventScan(out string) (sessionEventScan, string) {
	scan := sessionEventScan{BySession: map[string][]string{}}
	var errMsg string
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, sessionEventMarker) {
			continue
		}
		fields := strings.Split(strings.TrimPrefix(line, sessionEventMarker), "\t")
		switch fields[0] {
		case "TYPE":
			if len(fields) < 4 {
				continue
			}
			scan.Stats = append(scan.Stats, pluginEventStat{
				Type:     fields[1],
				Sessions: atoiSafe(fields[2]),
				Events:   atoiSafe(fields[3]),
			})
		case "SESSION":
			if len(fields) < 3 {
				continue
			}
			var types []string
			for _, t := range strings.Split(fields[2], ";") {
				if t = strings.TrimSpace(t); t != "" {
					types = append(types, t)
				}
			}
			scan.BySession[fields[1]] = types
		case "WARN":
			scan.Warnings = append(scan.Warnings, strings.Join(fields[1:], " "))
		case "ERR":
			errMsg = strings.TrimSpace(strings.Join(fields[1:], " "))
		}
	}
	return scan, errMsg
}

// parseSessionEventRepair 解析 repair 模式输出：修复文件数、修复事件数、跳过/失败文件数。
func parseSessionEventRepair(out string) (files, events, skipped, failed int, errMsg string) {
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, sessionEventMarker) {
			continue
		}
		fields := strings.Split(strings.TrimPrefix(line, sessionEventMarker), "\t")
		switch fields[0] {
		case "DONE":
			if len(fields) >= 3 {
				files = atoiSafe(fields[1])
				events = atoiSafe(fields[2])
			}
			if len(fields) >= 5 {
				skipped = atoiSafe(fields[3])
				failed = atoiSafe(fields[4])
			}
		case "WARN":
			log.Printf("session event repair warn: %s", strings.Join(fields[1:], " "))
		case "ERR":
			errMsg = strings.TrimSpace(strings.Join(fields[1:], " "))
		}
	}
	return files, events, skipped, failed, errMsg
}

// atoiSafe 宽松解析非负整数，失败返回 0。
func atoiSafe(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// runSessionEventTool 执行会话事件工具脚本。cwd 用 .dsh 主目录（脚本只按参数定位，不依赖 cwd）；
// 90 秒兜底超时按「进程树终止」处理，避免日志异常大时把界面卡在检查阶段。
func runSessionEventTool(mode string, args ...string) (string, error) {
	root := dshHomeDir()
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("未找到 .dsh 数据目录")
	}
	argv := append([]string{"--input-type=module", "-e", sessionEventToolScript, mode, sessionsSourceDir(), harnessDir}, args...)
	deadline := time.Now().Add(90 * time.Second)
	watch := func() bool { return time.Now().After(deadline) }
	return runProfileCmdCaptureWatch(root, watch, nodeCmd(), argv...)
}

// scanSessionPluginEvents 全量扫描会话日志里的外部事件（当前构建不认识且未标 ignorable）。
func scanSessionPluginEvents() (sessionEventScan, string) {
	if strings.TrimSpace(sessionsSourceDir()) == "" {
		return sessionEventScan{BySession: map[string][]string{}}, ""
	}
	out, _ := runSessionEventTool("scan")
	scan, errMsg := parseSessionEventScan(out)
	if scan.BySession == nil {
		scan.BySession = map[string][]string{}
	}
	return scan, errMsg
}

// checkPluginRemovalRisk 删除登记时的风险检查：扫描 → 按插件归因 → 记日志。检查失败只记日志
// （返回 nil 不误报），不阻断删除登记——本检查是提示而非门禁。
func checkPluginRemovalRisk(name string) *pluginSessionRisk {
	scan, errMsg := scanSessionPluginEvents()
	if errMsg != "" {
		logUI("会话风险检查未完成", fmt.Sprintf("%s：%s", name, errMsg))
		return nil
	}
	for _, w := range scan.Warnings {
		log.Printf("session risk scan warn: %s", w)
	}
	risk := pluginRiskFromScan(name, scan)
	if risk == nil {
		log.Printf("session risk scan: %s clean (%d foreign type(s) elsewhere)", name, len(scan.Stats))
		return nil
	}
	logUI("会话数据风险", fmt.Sprintf("%s：%d 个会话 / %d 条自定义事件（%s），删除后这些会话将无法打开",
		name, risk.Sessions, risk.Events, strings.Join(risk.Types, "、")))
	return risk
}

// repairSessionPluginEvents 修复：给指定类型的事件补 ignorable:true。返回修复文件数、事件数。
func repairSessionPluginEvents(types []string) (int, int, error) {
	if strings.TrimSpace(sessionsSourceDir()) == "" || len(types) == 0 {
		return 0, 0, fmt.Errorf("没有需要修复的会话记录")
	}
	out, runErr := runSessionEventTool("repair", types...)
	files, events, _, failed, errMsg := parseSessionEventRepair(out)
	if errMsg != "" {
		return files, events, fmt.Errorf("%s", errMsg)
	}
	if failed > 0 {
		return files, events, fmt.Errorf("%d 个会话文件修复失败（详情见日志）", failed)
	}
	if runErr != nil && files == 0 {
		return 0, 0, fmt.Errorf("修复未执行：%v", runErr)
	}
	return files, events, nil
}

// findPendingPluginTask 取一条待应用变更（加锁访问待应用队列）。
func findPendingPluginTask(id string) (*pluginOpTask, bool) {
	pluginQMu.Lock()
	defer pluginQMu.Unlock()
	for _, t := range pluginPending {
		if t.id == id {
			return t, true
		}
	}
	return nil, false
}

// setPendingPluginRisk 更新一条待应用变更的风险信息（修复后清零 / 复查残留）并持久化。
func setPendingPluginRisk(id string, risk *pluginSessionRisk) {
	pluginQMu.Lock()
	found := false
	for _, t := range pluginPending {
		if t.id == id {
			t.risk = risk
			found = true
			break
		}
	}
	pluginQMu.Unlock()
	if !found {
		return
	}
	saveCurrentConfig()
	emitPluginPendingChanged()
	if appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugins:changed", nil)
	}
}

// PluginRiskRepairResult 「修复会话数据风险」的结果（前端据此渲染行内提示）。
type PluginRiskRepairResult struct {
	OK                bool   `json:"ok"`
	Files             int    `json:"files"`             // 被修复的会话文件数
	Events            int    `json:"events"`            // 被标记的事件条数
	RemainingSessions int    `json:"remainingSessions"` // 修复后仍有风险的会话数（插件仍在写入时可能 >0）
	RemainingEvents   int    `json:"remainingEvents"`   // 修复后仍有风险的事件条数
	Reason            string `json:"reason"`            // 失败原因（成功为空）
}

// RepairPluginSessionEvents 修复某条「待删除」变更的会话数据风险：给该插件写入的自定义事件补
// ignorable:true（harness 只为「可安全跳过」的外部事件提供该标记，计费/统计类记录正属此列）。
// 修复只动会话日志、不碰插件与服务，因此不需要重启；原日志逐个备份为 *.bak-<时间戳>。
// 修复后会立刻复查：插件仍启用时可能又写入新记录，残留风险回填到待应用变更上供用户再次修复。
func (a *App) RepairPluginSessionEvents(id string) PluginRiskRepairResult {
	if shotMode {
		return PluginRiskRepairResult{Reason: T("演示模式下不执行修复")}
	}
	t, ok := findPendingPluginTask(id)
	if !ok || t.op != "remove" || t.risk == nil {
		return PluginRiskRepairResult{Reason: T("该插件当前没有待修复的会话数据风险")}
	}
	name := t.name
	types := append([]string{}, t.risk.Types...)
	files, events, err := repairSessionPluginEvents(types)
	if err != nil {
		logUI("修复会话数据风险失败", fmt.Sprintf("%s：%v", name, err))
		return PluginRiskRepairResult{OK: false, Files: files, Events: events, Reason: err.Error()}
	}
	remaining := checkPluginRemovalRisk(name)
	setPendingPluginRisk(id, remaining)
	res := PluginRiskRepairResult{OK: true, Files: files, Events: events}
	if remaining != nil {
		res.RemainingSessions = remaining.Sessions
		res.RemainingEvents = remaining.Events
	}
	logUI("修复会话数据风险", fmt.Sprintf("%s：%d 个会话 / %d 条事件已标记可跳过；残留 %d 个会话 / %d 条事件",
		name, files, events, res.RemainingSessions, res.RemainingEvents))
	return res
}

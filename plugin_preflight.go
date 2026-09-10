package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ==================== 插件树确定性预检 ====================
//
// 为什么需要（2026-09-10 现场实证，dsh-systray.log）：
//   - 16:52:14 / 16:52:51 / 16:54:28 三次启动全部死于
//     `failed to import loader entry codegraph-sqlite (dsh-plugin-codegraph-sqlite):
//     The requested module '@deepseek-ai/dsh-llm' does not provide an export named 'assertNever'`
//     ——这是**模块 import 期**的确定性错误：插件基于更新版 harness 开发，本机 harness
//     少了它要的具名导出（同类还有 dsh-settings 的 settingsNamespace）。
//   - 而导入流程 16:54:20 已经报过 `import: plugins restored and service verified healthy`，
//     8 秒后进程才死。根因：启动健康校验靠「启动日志追加段 + 时间窗」，加载错误可能晚于
//     窗口关闭（HTTP 就绪 ≠ 树健康，与 2026-09-05 评估的结论一致）。
//
// 对照一次成功的人工安装流程：先把树装好，再用 node 在 profile 目录里**逐个 import 插件入口**
// （纯 Node 原生解析，与 cordis loader 同一个 profile 目录锚点），确认「装得上、导得进」之后
// 才让服务带上这棵树启动。这个检查是确定性的、秒级的、与日志时序无关的——正是启动健康校验
// 缺的那一环。命中即精确点名插件与原因，而不是等服务死去再从日志里猜嫌疑。

// pluginEntryCheckScript 在 profile 目录内 import 一个包并打印一行机器可读结果。
// 包名经 argv 传入（`node -e` 下 process.argv[1] 即第一个用户参数），脚本内不做字符串拼接，
// 避免包名/路径里的引号转义问题。
const pluginEntryCheckScript = `const spec = process.argv[1];
import(spec).then((m) => {
  const d = m && m.default;
  const ok = typeof (m && m.apply) === 'function' || typeof d === 'function' || typeof (d && d.apply) === 'function';
  console.log('DSHPREFLIGHT\t' + (ok ? 'OK' : 'NOAPPLY'));
}).catch((e) => {
  const msg = String((e && (e.message || e)) || 'unknown').replace(/[\r\n\t]+/g, ' ');
  console.log('DSHPREFLIGHT\tERR\t' + msg);
});`

// pluginCheckMarker 预检结果行前缀（\t 与脚本内一致）。
const pluginCheckMarker = "DSHPREFLIGHT\t"

// bundleNameBase 取 bundle 行的包名（兼容 `name@version` 变体，与 appendBundleEntry 同口径）。
func bundleNameBase(s string) string {
	if i := strings.IndexByte(s, '@'); i > 0 {
		return s[:i]
	}
	return s
}

// profileBundleNames 返回 dir 的 dsh.profile.bundles 里的用户插件名（去官方包、去重、排序）。
// 只取非 @deepseek-ai/* 的包：官方包由 harness 自身保证可加载，不在插件兼容性预检范围。
func profileBundleNames(dir string) []string {
	root := readProfileRoot(dir)
	dsh, _ := root["dsh"].(map[string]interface{})
	if dsh == nil {
		return nil
	}
	prof, _ := dsh["profile"].(map[string]interface{})
	if prof == nil {
		return nil
	}
	raw, _ := prof["bundles"].([]interface{})
	seen := map[string]bool{}
	var out []string
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		name := bundleNameBase(strings.TrimSpace(s))
		if name == "" || isOfficialHarnessPkg(name) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// installedPluginDir 插件是否已安装在 profile 的 node_modules（含 package.json）。
func installedPluginDir(dir, name string) (string, bool) {
	p := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
	if _, err := os.Stat(filepath.Join(p, "package.json")); err != nil {
		return "", false
	}
	return p, true
}

// parsePluginCheckOutput 解析预检输出行，返回 kind：
//   - ("", "")               入口加载成功且导出 apply；
//   - ("no-apply", "")       入口能加载，但未导出 apply（可能不是标准 host 插件，只提示）；
//   - ("import-error", 原因) 加载失败（缺具名导出 / 缺模块 / 语法错误）；
//   - ("no-result", "")      没有任何结果行（node 未能执行，由调用方按失败处理）。
func parsePluginCheckOutput(out string) (string, string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, pluginCheckMarker) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, pluginCheckMarker))
		switch {
		case rest == "OK":
			return "", ""
		case rest == "NOAPPLY":
			return "no-apply", ""
		case strings.HasPrefix(rest, "ERR"):
			msg := strings.TrimSpace(strings.TrimPrefix(rest, "ERR"))
			if msg == "" {
				msg = "插件入口加载失败"
			}
			return "import-error", msg
		}
	}
	return "no-result", ""
}

// pluginImportCheck 在 dir 内 import 插件 name（cwd=profile 目录，解析锚点与 loader 一致）。
func pluginImportCheck(dir, name string) (string, string) {
	out, err := runProfileCmdCapture(dir, nodeCmd(), "--input-type=module", "-e", pluginEntryCheckScript, name)
	kind, reason := parsePluginCheckOutput(out)
	if kind != "no-result" {
		return kind, reason
	}
	// 无结果行：node 自身失败（未安装/被拦截/参数错误）。不静默放过，按失败归因。
	tail := strings.TrimSpace(out)
	if tail == "" {
		tail = fmt.Sprint(err)
	}
	tail = strings.ReplaceAll(strings.ReplaceAll(tail, "\r", " "), "\n", " ")
	if len(tail) > 300 {
		tail = "…" + tail[len(tail)-300:]
	}
	if strings.TrimSpace(tail) == "" {
		tail = "node 未返回结果"
	}
	return "import-error", "预检未能执行：" + strings.TrimSpace(tail)
}

// preflightPluginEntries 逐个预检 dir 的已启用用户插件入口。
// 返回 broken（name→原因，加载失败）、noApply（可加载但无 apply 导出）、checked（实际检查数）。
func preflightPluginEntries(dir string) (broken map[string]string, noApply []string, checked int) {
	broken = map[string]string{}
	for _, name := range profileBundleNames(dir) {
		if _, ok := installedPluginDir(dir, name); !ok {
			// 未安装（待重指定 / 未对齐）：由 unresolved-bundle 逻辑负责，预检不重复判定
			continue
		}
		checked++
		kind, reason := pluginImportCheck(dir, name)
		switch kind {
		case "":
			// 通过
		case "no-apply":
			noApply = append(noApply, name)
		default:
			broken[name] = reason
		}
	}
	return broken, noApply, checked
}

// disablePreflightBroken 预检失败时禁用插件：有依赖声明走 disablePluginInProfile；
// 无依赖（ghost bundle 行）走 disableGhostPluginInProfile——两条路径都摘 bundle 并记原因，
// 记录在关于页可见、可重新启用（与既有不兼容自愈语义一致）。
func disablePreflightBroken(dir, name, reason string) error {
	root := readProfileRoot(dir)
	deps, _ := root["dependencies"].(map[string]interface{})
	if _, ok := deps[name]; ok {
		return disablePluginInProfile(dir, name, reason)
	}
	return disableGhostPluginInProfile(dir, name, reason)
}

// preflightTreeCompatibility 对多个 profile 目录做确定性预检：入口加载失败的插件就地禁用，
// 返回被禁用的插件名与用户可见说明。
//
// 调用点：任何「改动了插件树、随后要拉起服务」的操作（导入 / 更新 / 删除）在做启动健康校验
// **之前**调用它——把「启动必然失败」提前成「精确点名 + 自动摘除」，不必等服务 fail-loud
// 退出后再从启动日志里猜嫌疑（日志还可能是空的，见 2026-09-05 评估的 server.log 失明问题）。
func preflightTreeCompatibility(dirs []string) (disabled []string, notes []string) {
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		broken, noApply, checked := preflightPluginEntries(dir)
		if checked == 0 {
			continue
		}
		log.Printf("preflight: checked %d plugin entries (dir=%s)", checked, dir)
		for _, name := range noApply {
			log.Printf("preflight: %s exports no apply (dir=%s)", name, dir)
			notes = append(notes, fmt.Sprintf("插件 %s 的入口未导出 apply，可能不是标准 host 插件，请留意启动日志", name))
		}
		names := make([]string, 0, len(broken))
		for n := range broken {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			reason := broken[name]
			if err := disablePreflightBroken(dir, name, "启动预检失败："+reason); err != nil {
				log.Printf("preflight: disable %s failed: %v", name, err)
				continue
			}
			disabled = append(disabled, name)
			notes = append(notes, fmt.Sprintf("插件 %s 与当前 harness 版本不兼容（%s），已自动禁用", name, reason))
			log.Printf("preflight: disabled %s: %s (dir=%s)", name, reason, dir)
		}
	}
	return disabled, notes
}

// appendNote 拼接两段用户可见说明（任一段为空时返回另一段）。
func appendNote(a, b string) string {
	switch {
	case strings.TrimSpace(a) == "":
		return b
	case strings.TrimSpace(b) == "":
		return a
	default:
		return a + "；" + b
	}
}

// guardProfileLocalDeps 在任何 profile pnpm 命令（remove / update / install）之前，消毒
// 「本地 spec 但目标路径已不存在」的依赖：能取到稳定副本就改指副本（插件继续可用），
// 取不到就摘除并记为待重指定。返回用户可见说明行。
//
// 为什么必须前置（2026-09-10 删除 dsh-codegraph 失败实证）：
//
//	pnpm 的 remove/update/install 都要解析整张依赖图，一个悬空的 file:/link: 会让**整条命令**
//	失败——日志实录：
//	  profile cmd: pnpm [remove dsh-codegraph] (dir=C:\Users\lenovo\.dsh\profiles\web)
//	  [ERROR] [profile] [ENOENT] ENOENT: no such file or directory, scandir 'D:\agent-env\qtz\plugins\dsh-ui-taste'
//	  [ERROR] [ui] 删除插件失败 | dsh-codegraph: 移除失败：exit status 0xfffff026
//
// 被删的 dsh-codegraph 与悬空的 dsh-ui-taste 毫无关系：工作区从 qtz/ 提到根目录后，
// 任何插件操作都会被这个悬空本地依赖连带打挂。先消毒再执行，操作才能落到目标插件本身。
func guardProfileLocalDeps(dirs ...string) []string {
	var present []string
	for _, d := range dirs {
		if strings.TrimSpace(d) != "" {
			present = append(present, d)
		}
	}
	if len(present) == 0 {
		return nil
	}
	return sanitizeProfileLocalDepsAll(present)
}

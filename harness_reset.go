package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ==================== 重置 DeepSeek Harness ====================
// 常规页「重置 DeepSeek Harness」：停服后全新安装到用户从「重置目标版本」下拉选择的、
// 任意官方 npm 已发布版本（高于当前版本=升级重装、低于=降级回退，默认选中当前版本=
// 同版本重装；弹窗勾选清除会话/插件）。
// 弹窗打开时一次查证候选与默认目标，执行期不再触网查版本。弹窗警告后执行，
// 全程 splash 进度；重置后启动受阻时优先自动禁用不兼容用户插件换取新版启动，
// 仅核心故障才还原重置前的目录快照。

// removeInstalledPlugins 物理删除用户安装的插件（profiles 下各 profile）：
//  1. package.json：dependencies 与 dsh.profile.bundles 移除非 @deepseek-ai/* 的条目
//     （保留 harness 官方 bundle，插件注册信息一并清除，使 harness 不再加载它们）；
//  2. node_modules：删除所有非 @deepseek-ai 的顶层目录；.pnpm 私有存储只删非官方条目
//     （@deepseek-ai+* 官方包实体保留——官方顶层目录是指向它的符号链接，整删 .pnpm
//     会让官方 client bundle 悬空、harness web 加载插件清单时报 failed to load）。
//
// 返回被清理的 profile 目录数。调用前须已 killServer()（避免运行中占用文件）。
// 注意：本函数不可逆，符合「重置会丢失所有已安装插件」的警告语义。
func removeInstalledPlugins() (int, error) {
	home := dshHomeDir()
	if home == "" {
		return 0, fmt.Errorf("无法确定 harness 数据目录（DSH_HOME）")
	}
	profilesRoot := filepath.Join(home, "profiles")
	if _, err := os.Stat(profilesRoot); err != nil {
		return 0, nil // 从未安装任何 profile/插件
	}
	var dirs []string
	if entries, err := os.ReadDir(profilesRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(profilesRoot, e.Name(), "package.json")); err == nil {
				dirs = append(dirs, filepath.Join(profilesRoot, e.Name()))
			}
		}
	}
	// 兼容旧布局：profiles/package.json（profiles 本身即 profile 根）
	if _, err := os.Stat(filepath.Join(profilesRoot, "package.json")); err == nil {
		dirs = append(dirs, profilesRoot)
	}
	cleaned := 0
	for _, dir := range dirs {
		if err := cleanProfilePlugins(dir); err != nil {
			return cleaned, fmt.Errorf("清理 profile %s 失败：%w", dir, err)
		}
		cleaned++
	}
	log.Printf("reset: removed installed plugins from %d profile(s)", cleaned)
	return cleaned, nil
}

// cleanProfilePlugins 清理单个 profile 目录中的用户插件（package.json 注册 + node_modules 文件）。
func cleanProfilePlugins(dir string) error {
	// 1) package.json：去掉非官方插件注册
	pj := filepath.Join(dir, "package.json")
	if data, err := os.ReadFile(pj); err == nil {
		var root map[string]interface{}
		if json.Unmarshal(data, &root) == nil {
			changed := false
			if deps, ok := root["dependencies"].(map[string]interface{}); ok {
				for k := range deps {
					if !isOfficialHarnessPkg(k) {
						delete(deps, k)
						changed = true
					}
				}
			}
			if dsh, ok := root["dsh"].(map[string]interface{}); ok {
				if prof, ok := dsh["profile"].(map[string]interface{}); ok {
					if bundles, ok := prof["bundles"].([]interface{}); ok {
						var keep []interface{}
						for _, b := range bundles {
							if s, ok := b.(string); ok && isOfficialHarnessPkg(s) {
								keep = append(keep, b)
							} else {
								changed = true
							}
						}
						prof["bundles"] = keep
					}
					// 同步清除用户插件的禁用记录（依赖已删除，记录无意义）
					if disabled, ok := prof["disabledPlugins"].(map[string]interface{}); ok {
						for k := range disabled {
							if !isOfficialHarnessPkg(k) {
								delete(disabled, k)
								changed = true
							}
						}
						if len(disabled) == 0 {
							delete(prof, "disabledPlugins")
						}
					}
				}
			}
			if changed {
				if out, err := json.MarshalIndent(root, "", "  "); err == nil {
					if err := os.WriteFile(pj, append(out, '\n'), 0o644); err != nil {
						return err
					}
				}
			}
		}
	}
	// 2) node_modules：删除非官方内容，但保持 @deepseek-ai 官方包实体完整：
	//   - 顶层：删除非 @deepseek-ai 目录（用户插件与其依赖闭包）；
	//   - .pnpm 私有存储：只删除非官方条目，保留 @deepseek-ai+*（官方包实体所在）。
	//     旧实现整删 .pnpm，顶层 @deepseek-ai/* 符号链接指向已删实体 → 悬空，
	//     harness web 拉官方 client bundle（如 dsh-client-ui-settings-plugin-inventory
	//     的 client.js）即报 failed to load —— 保留官方实体可避免该损坏。
	nm := filepath.Join(dir, "node_modules")
	entries, err := os.ReadDir(nm)
	if err != nil {
		return nil // 无 node_modules（从未装或已清）
	}
	for _, e := range entries {
		name := e.Name()
		if name == ".pnpm" {
			if err := prunePnpmNonOfficial(filepath.Join(nm, name)); err != nil {
				return err
			}
			continue
		}
		// scoped 目录（@scope）整体按 scope 判断：@deepseek-ai 保留，其它 scope 全删
		if !strings.HasPrefix(name, "@deepseek-ai") {
			target := filepath.Join(nm, name)
			log.Printf("reset: removing plugin dir %s", target)
			if err := os.RemoveAll(target); err != nil {
				return fmt.Errorf("删除 %s 失败：%w", target, err)
			}
		}
	}
	return nil
}

// prunePnpmNonOfficial 删除 .pnpm 虚拟仓库中的非官方条目：保留 @deepseek-ai+*（官方包实体，
// 顶层 @deepseek-ai/* 符号链接指向它们）与 . 开头的元数据文件；其余（用户插件与其依赖）
// 物理删除。
func prunePnpmNonOfficial(pnpmDir string) error {
	entries, err := os.ReadDir(pnpmDir)
	if err != nil {
		return nil // .pnpm 不存在/已删，无需处理
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "@deepseek-ai+") || strings.HasPrefix(name, ".") {
			continue // 官方包实体 / .modules.yaml 等元数据保留
		}
		target := filepath.Join(pnpmDir, name)
		log.Printf("reset: removing pnpm store entry %s", target)
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("删除 %s 失败：%w", target, err)
		}
	}
	return nil
}

// isOfficialHarnessPkg @deepseek-ai/* 为 harness 官方自带，重置时保留。
func isOfficialHarnessPkg(name string) bool {
	return strings.HasPrefix(name, "@deepseek-ai/")
}

// harnessGitTagForVersion 源码形态下把版本号还原为仓库 tag（优先 dsh-vX.Y.Z，兼容 dsh-X.Y.Z）。
func harnessGitTagForVersion(version string) (string, error) {
	candidates := []string{"dsh-v" + version, "dsh-" + version}
	for _, tag := range candidates {
		if out := runHarnessCmdCapture("git", "rev-parse", "--verify", "--quiet", tag+"^{commit}"); out != "" {
			return tag, nil
		}
	}
	return "", fmt.Errorf("harness 仓库中未找到版本 %s 对应的 tag（dsh-v%s / dsh-%s）", version, version, version)
}

// failReset 重置失败统一收尾：尽力把服务拉回可用后弹窗报告。
// runHarnessReset 在第 0 步就 killServer 了，各失败分支若只弹窗返回，用户会同时面对
// 「重置失败」与「服务停摆」——2026-09-10 mac 实证：重置安装失败虽已还原目录，服务却一直
// 停着（22:29:47 停 → 22:38:13 才被后续更新拉起），只能手动点重启。
func failReset(splash *SplashState, msg string) {
	recovered := true
	if !serverResponding(webURL) {
		recovered = restartAndVerifyServer()
	}
	if !recovered {
		msg += "\n\n服务未能自动恢复，请点击「重启服务」后重试。"
	}
	splash.Close()
	showMessageBox(msg, appName)
}

// runHarnessReset 重置 DeepSeek Harness：停服务 →（可选）清会话/清插件 →
// 全新安装 reqTarget（前端从「重置目标版本」下拉选择的任意官方 npm 版本，含更高版本与
// 预发布；默认选中当前版本=同版本重装）→ 重启校验。reqTarget 为空或格式非法为防御性
// 失败（前端已保证传具体版本：候选与默认目标均在弹窗打开时由 GetResetVersions 一次查证，
// 这里不再触网查询）。clearSessions / clearPlugins 由前端勾选弹窗传入（版本回退始终执行，
// 必选项）。重置后启动受阻时按「不回滚优先」处置：先禁用点名嫌疑用户插件，仍失败再禁用
// 全部用户插件，任一成功即保留新版本；两级都失败（核心故障）才还原备份目录。
// 异步执行（按钮触发后 go 调用）。
func runHarnessReset(clearSessions, clearPlugins bool, reqTarget string) {
	splash := startSplash(T("正在重置 DeepSeek Harness…"))
	defer splash.Close()

	// 0) 先停止服务（否则运行中的 node 占用文件，清空/重装会失败）
	splash.Update(T("正在停止后台服务…"), 0.1)
	killServer()
	time.Sleep(1 * time.Second)

	// 0.5) 形态判定：npm 预构建 / 缺失 → npm 全新安装；源码 checkout → 暂不支持自动清空重装。
	if isSourceHarnessDir() {
		failReset(splash, "重置失败：当前为源码 checkout 形态，暂不支持自动清空目录重装。\n"+
			"请先在 Web UI 切换到 npm 预构建形态后再重置，或手动处理源码目录。\n\n日志："+unifiedLogPath())
		return
	}

	// 1) 目标版本：弹窗已选（GetResetVersions 在弹窗打开时查过 npm 列表并给出默认目标）；
	//    此处只做本地格式校验，不再查询最新版本号——版本真实可装性由下方 pnpm add 决定，
	//    失败走备份还原兜底并提示。
	splash.Update(T("正在准备全新安装…"), 0.2)
	target := reqTarget
	if target == "" {
		failReset(splash, "重置失败：未选择重置目标版本，请重新打开弹窗选择后再试。\n\n日志："+unifiedLogPath())
		return
	}
	if !validResetTarget(target) {
		failReset(splash, fmt.Sprintf("重置失败：目标版本 %q 格式非法，请重新打开弹窗选择。\n\n日志：%s",
			target, unifiedLogPath()))
		return
	}
	log.Printf("reset: clean reinstall to %s (shape=npm) clearSessions=%v clearPlugins=%v explicitTarget=%v",
		orDash(target), clearSessions, clearPlugins, true)

	// 2) 清空原目录 + 全新安装所选版本：先把旧目录整体改名为备份（快），在新目录全新安装；
	//    成功删除备份，失败还原备份（保证不留下半成品）。
	bakDir := harnessDir + ".reset-bak"
	_ = os.RemoveAll(bakDir)
	if _, serr := os.Stat(harnessDir); serr == nil {
		splash.Update(T("正在清空原 harness 目录…"), 0.35)
		if rerr := os.Rename(harnessDir, bakDir); rerr != nil {
			failReset(splash, "重置失败：无法备份原目录（"+rerr.Error()+"）。\n\n请检查文件占用后重试。\n\n日志："+unifiedLogPath())
			return
		}
	}
	if err := os.MkdirAll(harnessDir, 0o755); err != nil {
		_ = os.Rename(bakDir, harnessDir) // 尽力还原
		failReset(splash, "重置失败：无法创建新目录（"+err.Error()+"）。\n\n日志："+unifiedLogPath())
		return
	}
	restoreBackup := func() {
		_ = os.RemoveAll(harnessDir)
		if os.Rename(bakDir, harnessDir) != nil {
			log.Printf("reset: restore backup dir failed, leftover at %s", bakDir)
		}
	}
	splash.Update(fmt.Sprintf("正在全新安装 %s…（原目录文件已清空）", withV(target)), 0.55)
	// 整族钉版：家族包名取自被替换下来的原目录（锁文件/已装包），把整族锁到目标版本——只钉
	// 根包时 pnpm 会顺着 caret 范围选中上游半发布的新版本，随后在其缺失依赖上整次失败。
	family := harnessFamilyNames(bakDir)
	rerr := ensureNpmHarnessVersionPinned(target, family)
	if rerr != nil {
		restoreBackup()
		failReset(splash, "重置失败：全新安装未能完成，已还原原目录。\n"+rerr.Error())
		return
	}
	// 旧目录备份保留到「重启健康校验通过」后再删除：重置后新版启动受阻（插件不兼容/核心
	// 故障）时可整体还原到重置前的可运行目录；备份在下方「校验通过 / 自愈成功 / 核心故障还原」
	// 三处收敛删除或还原。
	log.Printf("reset: clean reinstall done at %s (backup kept at %s)", harnessDir, bakDir)

	// 3) 可选清理（版本已回退成功；清理失败不阻断重启，仅记录并提示）
	cleanupNotes := ""
	if clearSessions {
		splash.Update(T("正在清除会话记录…"), 0.6)
		if err := removeSessions(); err != nil {
			log.Printf("reset: clear sessions failed: %v", err)
			cleanupNotes += "\n· 会话记录清理失败：" + err.Error()
		}
	}
	if clearPlugins {
		splash.Update(T("正在清除已安装的插件…"), 0.65)
		if _, err := removeInstalledPlugins(); err != nil {
			log.Printf("reset: clear plugins failed: %v", err)
			cleanupNotes += "\n· 已安装插件清理失败：" + err.Error()
		}
	}

	// 4) 重启并健康校验（就绪 + 启动日志无加载报错）；重置属于改版路径，用加长校验窗口——
	//    混装/接口不兼容的加载错误可能迟至启动后 45s 才刷出（0.1.5-rc.1 实测）。
	//    失败时优先不回滚（与「更新 Harness」同策略）：先禁用启动日志点名的用户插件
	//    （disableBootSuspects），仍失败再兜底禁用全部已激活用户插件（disableAllUserPlugins）
	//    换取新版可启动——任一成功即保留新版本并弹窗列出禁用清单；两级都失败（核心故障，
	//    官方包加载错误等禁用用户插件无法解决）才还原备份目录。
	splash.Update(T("正在重启服务…"), 0.9)
	if !restartAndVerifyServerAfterChange() {
		splash.Update(T("启动校验失败，正在排查不兼容插件…"), 0.92)
		var profileDirs []string
		for _, pf := range enumeratePluginProfiles() {
			profileDirs = append(profileDirs, pf.dir)
		}
		disabled, ok := disableBootSuspects(profileDirs)
		allDisabled := false
		if !ok {
			splash.Update(T("服务启动受阻，正在尝试禁用部分插件…"), 0.94)
			disabled, ok = disableAllUserPlugins(profileDirs)
			allDisabled = ok
		}
		if !ok {
			// 核心故障：还原重置前目录，不留半成品（备份此前一直保留）。还原后必须把服务拉回
			// 可用状态，否则用户同时面对「重置失败」与「服务停摆」（failReset 负责收尾弹窗）。
			restoreBackup()
			failReset(splash, "重置未能完成：已尝试自动禁用不兼容插件，服务仍无法启动（核心故障），已还原重置前的版本。\n\n日志："+unifiedLogPath()+cleanupNotes)
			return
		}
		// 自愈成功：保留新版本（禁用清单可于「关于页 → 已安装插件」检查更新/重新启用）
		_ = os.RemoveAll(bakDir)
		clearAllLkg()
		splash.Close()
		names := make([]string, 0, len(disabled))
		for _, d := range disabled {
			names = append(names, d.Name)
		}
		logUI("重置服务完成（含不兼容插件自动禁用）",
			fmt.Sprintf("v%s | 禁用 %s", target, strings.Join(names, "、")))
		detail := fmt.Sprintf("DeepSeek Harness 已重置到 %s，服务已重启。\n\n以下插件与新版本不兼容，已自动禁用（保留记录，可在「关于页 → 已安装插件」中检查更新后重新启用）：\n· %s",
			withV(target), strings.Join(names, "、"))
		if allDisabled {
			detail = fmt.Sprintf("DeepSeek Harness 已重置到 %s，服务已重启。\n\n未能定位到具体的不兼容插件，已禁用全部已激活的用户插件以保证新版启动（保留记录，可在「关于页 → 已安装插件」中逐个重新启用）：\n· %s",
				withV(target), strings.Join(names, "、"))
		}
		if clearSessions {
			detail += "\n· 会话记录已清除"
		}
		if clearPlugins {
			detail += "\n· 已安装插件已清除"
		}
		showMessageBox(detail+cleanupNotes, appName)
		return
	}
	// 校验通过：备份不再需要，回退后的状态即新的良好基线，旧 LKG 不应再用于回退
	_ = os.RemoveAll(bakDir)
	clearAllLkg()
	splash.Close()
	detail := T("DeepSeek Harness 已重置：\n")
	if clearSessions {
		detail += "· 会话记录已清除\n"
	}
	if clearPlugins {
		detail += "· 已安装插件已清除\n"
	}
	detail += "· 版本：已全新安装 " + withV(target) + "（原 harness 目录文件已全部清空）\n"
	detail += "服务已重启。" + cleanupNotes
	showMessageBox(detail, appName)
}

// ==================== 重置内容统计（弹窗勾选前展示数量） ====================

// countResetSessions 统计 ~/.dsh/sessions 下的会话数量。
// 兼容两级布局（sessions/<scope>/<session>，计 scope 下的 session 目录数）与
// 单级布局（sessions/<session>，计一级目录数）。与 removeSessions（删除整个 sessions 根）范围一致。
func countResetSessions() int {
	root := sessionsSourceDir()
	if root == "" {
		return 0
	}
	n := 0
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		inner, err := os.ReadDir(filepath.Join(root, e.Name()))
		if err != nil || len(inner) == 0 {
			n++
			continue
		}
		dirs := 0
		for _, ie := range inner {
			if ie.IsDir() {
				dirs++
			}
		}
		if dirs > 0 {
			n += dirs
		} else {
			n++ // 单级布局：一级目录即一个会话
		}
	}
	return n
}

// countInstalledPlugins 统计用户安装的插件数量（与 removeInstalledPlugins 清理口径一致：
// 各 profile package.json dependencies 中非 @deepseek-ai 条目总数；旧布局 profiles 根同样统计）。
func countInstalledPlugins() int {
	home := dshHomeDir()
	if home == "" {
		return 0
	}
	profilesRoot := filepath.Join(home, "profiles")
	dirs := []string{}
	if entries, err := os.ReadDir(profilesRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(profilesRoot, e.Name(), "package.json")); err == nil {
				dirs = append(dirs, filepath.Join(profilesRoot, e.Name()))
			}
		}
	}
	if _, err := os.Stat(filepath.Join(profilesRoot, "package.json")); err == nil {
		dirs = append(dirs, profilesRoot)
	}
	total := 0
	for _, dir := range dirs {
		if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
			var root struct {
				Dependencies map[string]string `json:"dependencies"`
			}
			if json.Unmarshal(data, &root) == nil {
				for k := range root.Dependencies {
					if !isOfficialHarnessPkg(k) {
						total++
					}
				}
			}
		}
	}
	return total
}

// removeSessions 清除全部历史会话（删除 ~/.dsh/sessions 整个目录）。调用前须已 killServer。
func removeSessions() error {
	dir := sessionsSourceDir()
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(dir); err != nil {
		return nil // 无会话目录
	}
	log.Printf("reset: removing sessions root %s", dir)
	return os.RemoveAll(dir)
}

// ==================== 重置目标版本选择（弹窗下拉） ====================

// ResetVersionOption 重置目标下拉的单个候选版本。
type ResetVersionOption struct {
	Version    string `json:"version"`
	Prerelease bool   `json:"prerelease"` // 预发布通道版本（-alpha/-beta/-rc 等），界面以警示色标注
}

// ResetVersionInfo GetResetVersions 返回的重置目标信息：当前版本、可选目标、默认选中与说明。
type ResetVersionInfo struct {
	Form    string               `json:"form"`    // "npm" | "source"（源码形态不支持自动重置）
	Current string               `json:"current"` // 当前已装版本（识别失败为空）
	Options []ResetVersionOption `json:"options"` // npm 全部已发布版本（按新→旧；含高于当前版本与预发布）
	Default string               `json:"default"` // 默认选中版本（优先当前版本=同版本重装；其次最近可用稳定版）
	Note    string               `json:"note"`    // 边界说明或错误原因（面向用户）
}

// buildResetVersionOptions 由 npm 已发布版本构建重置目标候选（任意版本均可选，不限 ≤ 当前）：
//   - 列出全部已发布版本（去重、去 dsh-/v 前缀，按新→旧排序，预发布标注）；
//   - Default = 优先当前版本（同版本重装，重置语义下最安全）；当前不在列表时取「不高于当前的
//     最近稳定版」；再取最新稳定版；全部为预发布时取最新发布；
//   - current 为空（当前版本识别失败）时列出全部版本、默认最新稳定版。
func buildResetVersionOptions(versions []string, current string) (opts []ResetVersionOption, def string) {
	seen := map[string]bool{}
	for _, v := range versions {
		v = strings.TrimPrefix(strings.TrimPrefix(v, "dsh-"), "v")
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		opts = append(opts, ResetVersionOption{Version: v, Prerelease: !isStableVersion(v)})
	}
	sort.Slice(opts, func(i, j int) bool { return compareVersions(opts[i].Version, opts[j].Version) > 0 })
	if current != "" {
		for _, o := range opts {
			if o.Version == current {
				def = current
				break
			}
		}
	}
	if def == "" && current != "" {
		// 当前版本不在已发布列表：取「不高于当前的最近稳定版」（列表新→旧，首个命中即最近）
		for _, o := range opts {
			if !o.Prerelease && compareVersions(o.Version, current) <= 0 {
				def = o.Version
				break
			}
		}
	}
	if def == "" {
		for _, o := range opts {
			if !o.Prerelease {
				def = o.Version
				break
			}
		}
	}
	if def == "" && len(opts) > 0 {
		def = opts[0].Version // 全部为预发布：最新发布
	}
	return opts, def
}

// validResetTarget 本地校验目标版本格式（近似 npm semver；纯防御——防止异常入参进入
// pnpm add 的版本拼接，如空值/路径/参数注入）。弹窗候选本身来自 npm 已发布版本列表。
func validResetTarget(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	if v[0] == '.' || v[len(v)-1] == '.' {
		return false // 前导/尾随点：非合法 semver
	}
	digit, dot := false, false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c == '.':
			if dot && i > 0 && v[i-1] == '.' {
				return false // 连续点：非合法 semver
			}
			dot = true
		case c == '-' || c == '+' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			// 预发布/构建后缀与字母
		default:
			return false
		}
	}
	return digit && dot
}

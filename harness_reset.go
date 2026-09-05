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
// 早于当前运行版本的官方版本（弹窗勾选清除会话/插件）；无更早版本等边界降级放行，
// 按官方默认目标执行。弹窗警告后执行，全程 splash 进度，失败自动回滚版本快照。

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

// runHarnessReset 重置 DeepSeek Harness：停服务 →（可选）清会话/清插件 →
// 全新安装 reqTarget（前端从「重置目标版本」下拉选择的、早于当前运行版本的官方版本）→
// 重启校验。reqTarget 为空为边界降级放行（无可更早版本 / 当前版本识别失败 / 网络查询失败）：
// 沿用 fetchNpmResetTarget 的官方默认目标（最新稳定版，仅预发布时最新发布）。
// clearSessions / clearPlugins 由前端勾选弹窗传入（版本回退始终执行，必选项）。
// 失败自动回退到重置前的可运行快照并弹窗报告。异步执行（按钮触发后 go 调用）。
func runHarnessReset(clearSessions, clearPlugins bool, reqTarget string) {
	splash := startSplash("正在重置 DeepSeek Harness…")
	defer splash.Close()

	// 0) 先停止服务（否则运行中的 node 占用文件，清空/重装会失败）
	splash.Update("正在停止后台服务…", 0.1)
	killServer()
	time.Sleep(1 * time.Second)

	// 0.5) 形态判定：npm 预构建 / 缺失 → npm 全新安装；源码 checkout → 暂不支持自动清空重装。
	if isSourceHarnessDir() {
		splash.Close()
		showMessageBox("重置失败：当前为源码 checkout 形态，暂不支持自动清空目录重装。\n"+
			"请先在 Web UI 切换到 npm 预构建形态后再重置，或手动处理源码目录。\n\n日志："+unifiedLogPath(), appName)
		return
	}

	// 1) 目标版本：用户显式选择的版本（执行时二次校验其真实存在于 npm——弹窗打开与
	//    确认之间列表可能过期）；reqTarget 为空 = 边界降级放行，按官方默认目标解析。
	splash.Update("正在查询官方可用版本…", 0.2)
	target := ""
	targetNote := ""
	if reqTarget != "" {
		versions, err := npmHarnessPublishedVersions()
		if err != nil {
			splash.Close()
			showMessageBox("重置失败：无法查询官方可用版本。\n"+err.Error()+"\n\n请检查网络后重试。", appName)
			return
		}
		if !containsVersion(versions, reqTarget) {
			splash.Close()
			showMessageBox(fmt.Sprintf("重置失败：所选版本 %s 在 npm 上已不存在，请关闭弹窗后重试。", withV(reqTarget)), appName)
			return
		}
		target = reqTarget
	} else {
		latest, note, err := fetchNpmResetTarget()
		if err != nil {
			splash.Close()
			showMessageBox("重置失败：无法获取官方可用版本。\n"+err.Error()+"\n\n请检查网络后重试。", appName)
			return
		}
		target, targetNote = latest, note
	}
	log.Printf("reset: clean reinstall to %s (shape=npm) clearSessions=%v clearPlugins=%v explicitTarget=%v",
		orDash(target), clearSessions, clearPlugins, reqTarget != "")

	// 2) 清空原目录 + 全新安装官方最新版：先把旧目录整体改名为备份（快），在新目录全新安装；
	//    成功删除备份，失败还原备份（保证不留下半成品）。
	bakDir := harnessDir + ".reset-bak"
	_ = os.RemoveAll(bakDir)
	if _, serr := os.Stat(harnessDir); serr == nil {
		splash.Update("正在清空原 harness 目录…", 0.35)
		if rerr := os.Rename(harnessDir, bakDir); rerr != nil {
			splash.Close()
			showMessageBox("重置失败：无法备份原目录（"+rerr.Error()+"）。\n\n请检查文件占用后重试。", appName)
			return
		}
	}
	if err := os.MkdirAll(harnessDir, 0o755); err != nil {
		_ = os.Rename(bakDir, harnessDir) // 尽力还原
		splash.Close()
		showMessageBox("重置失败：无法创建新目录（"+err.Error()+"）。", appName)
		return
	}
	restoreBackup := func() {
		_ = os.RemoveAll(harnessDir)
		if os.Rename(bakDir, harnessDir) != nil {
			log.Printf("reset: restore backup dir failed, leftover at %s", bakDir)
		}
	}
	splash.Update(fmt.Sprintf("正在全新安装 %s…（原目录文件已清空）", withV(target)), 0.55)
	rerr := ensureNpmHarnessVersion(target)
	if rerr != nil {
		restoreBackup()
		splash.Close()
		showMessageBox("重置失败：全新安装未能完成，已还原原目录。\n"+rerr.Error()+"\n\n日志："+unifiedLogPath(), appName)
		return
	}
	_ = os.RemoveAll(bakDir) // 全新安装成功：旧目录备份不再需要
	log.Printf("reset: clean reinstall done at %s", harnessDir)

	// 3) 可选清理（版本已回退成功；清理失败不阻断重启，仅记录并提示）
	cleanupNotes := ""
	if clearSessions {
		splash.Update("正在清除会话记录…", 0.6)
		if err := removeSessions(); err != nil {
			log.Printf("reset: clear sessions failed: %v", err)
			cleanupNotes += "\n· 会话记录清理失败：" + err.Error()
		}
	}
	if clearPlugins {
		splash.Update("正在清除已安装的插件…", 0.65)
		if _, err := removeInstalledPlugins(); err != nil {
			log.Printf("reset: clear plugins failed: %v", err)
			cleanupNotes += "\n· 已安装插件清理失败：" + err.Error()
		}
	}

	// 4) 重启并健康校验（就绪 + 启动日志无加载报错）
	splash.Update("正在重启服务…", 0.9)
	if !restartAndVerifyServer() {
		splash.Close()
		showMessageBox("重置后服务未能正常启动，请查看日志：\n"+unifiedLogPath()+cleanupNotes, appName)
		return
	}
	// 重置成功：回退后的状态即新的良好基线，旧 LKG 不应再用于回退
	clearAllLkg()
	splash.Close()
	detail := "DeepSeek Harness 已重置：\n"
	if clearSessions {
		detail += "· 会话记录已清除\n"
	}
	if clearPlugins {
		detail += "· 已安装插件已清除\n"
	}
	detail += "· 版本：已全新安装 " + withV(target) + "（原 harness 目录文件已全部清空）\n"
	detail += "服务已重启。" + targetNote + cleanupNotes
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
	Options []ResetVersionOption `json:"options"` // 仅早于当前版本的候选（按新→旧；当前未知=识别失败时列出全部）
	Default string               `json:"default"` // 默认选中版本；空=无更早候选（走官方默认目标）
	Note    string               `json:"note"`    // 边界/降级说明或错误原因（面向用户）
}

// buildResetVersionOptions 由 npm 已发布版本与当前已装版本构建重置目标候选：
//   - 只保留早于 current 的版本（compareVersions < 0；相等与更新都不提供——仅可重置到更早版本）；
//     current 为空（当前版本识别失败）时为降级放行列出全部版本；
//   - 去重并按新→旧排序；
//   - Default = 最靠前的稳定版（即最新稳定版）；全部为预发布时取最新的预发布。
func buildResetVersionOptions(versions []string, current string) (opts []ResetVersionOption, def string) {
	seen := map[string]bool{}
	for _, v := range versions {
		v = strings.TrimPrefix(strings.TrimPrefix(v, "dsh-"), "v")
		if v == "" || seen[v] {
			continue
		}
		if current != "" && compareVersions(v, current) >= 0 {
			continue
		}
		seen[v] = true
		opts = append(opts, ResetVersionOption{Version: v, Prerelease: !isStableVersion(v)})
	}
	sort.Slice(opts, func(i, j int) bool { return compareVersions(opts[i].Version, opts[j].Version) > 0 })
	for _, o := range opts {
		if !o.Prerelease {
			def = o.Version
			break
		}
	}
	if def == "" && len(opts) > 0 {
		def = opts[0].Version // 仅预发布可回退时：最新预发布
	}
	return opts, def
}

// containsVersion 判断版本列表是否包含该版本（忽略前导 v / dsh- 前缀）。
func containsVersion(list []string, v string) bool {
	v = strings.TrimPrefix(strings.TrimPrefix(v, "dsh-"), "v")
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

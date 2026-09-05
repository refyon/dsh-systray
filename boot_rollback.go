package main

// ==================== 启动失败自动回退（LKG：最近已知良好状态） ====================
// 目标：双击启动（bootstrapService）失败时，自动回退到「上次正常运行的 harness 与插件状态」。
//
// 机制（保留式 LKG，磁盘双份时间最短）：
//  1. 更新（harness / 插件）成功后不再删除更新前的快照，而是把 .dshbak 提升为 .lkgbak
//     （此时该快照 = 更新前、经 restartAndVerifyServer 验证过的状态）；
//  2. 每次「由本进程真正拉起服务」的冷启动验证通过（服务就绪 + 启动日志无加载错误）后，
//     把当前状态提升为新 LKG（即清理 .lkgbak——当前即已知良好，不再需要旧备份）；
//  3. 启动失败且失败特征指向环境本身（服务进程异常退出 / 启动日志命中加载错误）时，
//     停服务 → 恢复 harness 与各 profile 的 .lkgbak → 重启校验；成功即把恢复出的状态
//     视为新的当前良好态（清理 LKG 防止跨启动反复回退），失败则保留现场并报错。
//
// 说明：LKG 只对「本进程拉起服务」的启动生效；端口已有外部服务在跑时不参与判定与清理。

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// lkgSuffix LKG 备份后缀（区别于更新流程进行中的 .dshbak）。
const lkgSuffix = ".lkgbak"

// lkgMarker LKG 元信息（仅用于回退提示文案；LKG 存在性以文件系统为准）。
type lkgMarker struct {
	HarnessVersion string `json:"harnessVersion"` // 提升 LKG 时 harness 的上一个版本
	UpdatedAt      string `json:"updatedAt"`
}

// lkgMarkerPath 标记文件：用户配置目录下的 lkg.json。
func lkgMarkerPath() string {
	if dir := filepath.Dir(configFilePath()); dir != "" {
		return filepath.Join(dir, "lkg.json")
	}
	return ""
}

func writeLkgMarker(m lkgMarker) {
	p := lkgMarkerPath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		log.Printf("lkg: mkdir marker dir failed: %v", err)
		return
	}
	if m.UpdatedAt == "" {
		m.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	if data, err := json.Marshal(m); err == nil {
		_ = os.WriteFile(p, data, 0o644)
	}
}

func readLkgMarker() (lkgMarker, bool) {
	p := lkgMarkerPath()
	if p == "" {
		return lkgMarker{}, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return lkgMarker{}, false
	}
	var m lkgMarker
	if json.Unmarshal(data, &m) != nil {
		return lkgMarker{}, false
	}
	return m, true
}

func clearLkgMarker() {
	if p := lkgMarkerPath(); p != "" {
		_ = os.Remove(p)
	}
}

// lkgBakPaths 目录中可能存在的 LKG 备份路径（package.json / pnpm-lock.yaml / node_modules）。
func lkgBakPaths(dir string) []string {
	return []string{
		filepath.Join(dir, "package.json"+lkgSuffix),
		filepath.Join(dir, "pnpm-lock.yaml"+lkgSuffix),
		filepath.Join(dir, "node_modules"+lkgSuffix),
	}
}

// hasLkgInDir 该目录是否留有 LKG 备份。
func hasLkgInDir(dir string) bool {
	for _, p := range lkgBakPaths(dir) {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// hasAnyLkg 是否存在任何 LKG 状态（harness / 各 profile / 标记文件）。
func hasAnyLkg() bool {
	if hasLkgInDir(harnessDir) {
		return true
	}
	for _, pf := range enumeratePluginProfiles() {
		if hasLkgInDir(pf.dir) {
			return true
		}
	}
	if _, ok := readLkgMarker(); ok {
		return true
	}
	return false
}

// promoteDirToLkg 把更新流程留下的 .dshbak 快照提升为 LKG（新 LKG 覆盖旧 LKG）。
func promoteDirToLkg(dir string) {
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		src := filepath.Join(dir, name+".dshbak")
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(dir, name+lkgSuffix)
		_ = os.Remove(dst)
		_ = os.Rename(src, dst)
	}
	srcNM := filepath.Join(dir, "node_modules.dshbak")
	if _, err := os.Stat(srcNM); err == nil {
		dstNM := filepath.Join(dir, "node_modules"+lkgSuffix)
		_ = os.RemoveAll(dstNM)
		_ = os.Rename(srcNM, dstNM)
	}
}

// promoteHarnessLkg 更新成功后：harness 快照提升为 LKG，并记录更新前版本号。
func promoteHarnessLkg(prevVersion string) {
	promoteDirToLkg(harnessDir)
	writeLkgMarker(lkgMarker{HarnessVersion: prevVersion})
	log.Printf("lkg: harness promoted (prev=%s)", prevVersion)
}

// promoteProfileLkg 插件更新成功后：profile 快照提升为 LKG。
func promoteProfileLkg(dir string) {
	promoteDirToLkg(dir)
	log.Printf("lkg: profile promoted (%s)", dir)
}

// restoreLkgInDir 恢复该目录的 LKG（package.json/lock 回写；node_modules 移回；
// 无 nm 备份但有锁文件还原时按还原后的锁文件重装）。返回是否确实恢复了内容。
func restoreLkgInDir(dir string) bool {
	restored := false
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		bak := filepath.Join(dir, name+lkgSuffix)
		if data, err := os.ReadFile(bak); err == nil {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				log.Printf("lkg: restore %s failed: %v", name, err)
				continue
			}
			restored = true
		}
	}
	nm := filepath.Join(dir, "node_modules")
	if _, err := os.Stat(nm + lkgSuffix); err == nil {
		_ = os.RemoveAll(nm)
		if os.Rename(nm+lkgSuffix, nm) == nil {
			restored = true
		} else {
			log.Printf("lkg: restore node_modules rename failed (%s)", dir)
		}
	} else if restored {
		// 锁文件已还原但无 nm 备份：重装还原依赖（需网络）
		if err := runProfileCmd(dir, pnpmCmd(), "install", "--frozen-lockfile"); err != nil {
			log.Printf("lkg: frozen reinstall failed (%s): %v", dir, err)
		}
	}
	return restored
}

// clearLkgInDir 清理该目录的 LKG 备份。
func clearLkgInDir(dir string) {
	for _, p := range lkgBakPaths(dir) {
		_ = os.RemoveAll(p)
	}
}

// clearAllLkg 冷启动验证通过 / 重置成功后：当前状态即新的良好态，清理全部 LKG。
func clearAllLkg() {
	clearLkgInDir(harnessDir)
	for _, pf := range enumeratePluginProfiles() {
		clearLkgInDir(pf.dir)
	}
	clearLkgMarker()
	log.Printf("lkg: cleared (current state verified good)")
}

// tryBootRollback 启动失败时尝试回退 LKG 并重启校验。
// 返回 (是否回退并重启成功, 回退到的 harness 版本描述)。失败时不动 LKG 现场，便于用户排查。
func tryBootRollback(why string) (bool, string) {
	if !hasAnyLkg() {
		log.Printf("lkg: boot failed (%s) but no LKG available, skip rollback", why)
		return false, ""
	}
	prev, _ := readLkgMarker()
	log.Printf("lkg: boot failed (%s), attempting rollback to last known good state", why)

	rsplash := maybeStartSplash("启动失败，正在回退到上次正常状态…")
	defer rsplash.Close()

	killServer()
	time.Sleep(1 * time.Second)

	// 1) 恢复 harness 与各 profile 的 LKG
	restored := false
	if hasLkgInDir(harnessDir) {
		if restoreLkgInDir(harnessDir) {
			restored = true
		}
	}
	for _, pf := range enumeratePluginProfiles() {
		if hasLkgInDir(pf.dir) {
			if restoreLkgInDir(pf.dir) {
				restored = true
			}
		}
	}
	if !restored {
		log.Printf("lkg: nothing to restore, abort rollback")
		return false, ""
	}

	// 2) 重启并健康校验（就绪 + 启动日志无加载错误）
	before := rotateServerLog() // 轮转留档：本次回退启动的现场独立成档，便于失败排查
	started, exitCh := startServer()
	if !started {
		return false, prev.HarnessVersion
	}
	if ok, msg := waitForServerReady(webURL, exitCh, startupTimeout); !ok {
		log.Printf("lkg: rollback restart failed: %s", msg)
		return false, prev.HarnessVersion
	}
	// 健康窗口校验（错误可能迟于就绪数秒出现；直接短窗口扫描会漏判并把回退当成功）
	if !verifyServerBoot(before, exitCh) {
		log.Printf("lkg: rollback restart has boot errors")
		return false, prev.HarnessVersion
	}

	// 3) 回退成功：恢复出的状态已验证可运行，视为新的当前良好态（清理 LKG 防跨启动反复回退）
	clearAllLkg()
	serverReady.Store(true)
	serviceFailed.Store(false)
	refreshServiceMenu()
	return true, prev.HarnessVersion
}

// reportBootRollback 回退结果提示：交互场景弹窗，自启动静默场景仅记日志。
func reportBootRollback(rolled bool, prevVersion, why string) {
	detail := ""
	if prevVersion != "" {
		detail = "（DeepSeek Harness v" + strings.TrimPrefix(prevVersion, "v") + "）"
	}
	if rolled {
		msg := "DeepSeek Harness 启动失败（" + why + "），已自动回退到上次正常运行的版本与插件状态" + detail + "，服务已重新启动。\n\n若问题持续，请查看日志："
		log.Printf("[UI] 启动失败自动回退成功 | %s", why)
		if !autostartLaunch {
			showMessageBox(msg+filepath.Join(logDir, "server.log"), appName)
		}
		return
	}
	msg := "DeepSeek Harness 启动失败（" + why + "），且自动回退到上次正常状态后仍未能启动。请查看日志："
	log.Printf("[UI] 启动失败自动回退也失败 | %s", why)
	if !autostartLaunch {
		showMessageBox(msg+filepath.Join(logDir, "server.log"), appName)
	}
}

// lkgStateSummary 诊断用：LKG 存在情况（供日志/测试）。
func lkgStateSummary() string {
	parts := []string{}
	if hasLkgInDir(harnessDir) {
		parts = append(parts, "harness")
	}
	for _, pf := range enumeratePluginProfiles() {
		if hasLkgInDir(pf.dir) {
			parts = append(parts, fmt.Sprintf("profile:%s", pf.label))
		}
	}
	return strings.Join(parts, ",")
}

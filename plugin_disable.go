package main

// ==================== 插件禁用 / 启用（启动阻碍自愈） ====================
// 目标：排除一切阻碍服务启动的因素——更新 harness、安装/更新/删除插件、导入恢复后，若服务因
// 某个插件与当前核心不兼容而启动失败，不再整体回退操作，而是「禁用」冲突插件（保留依赖声明与
// node_modules 文件，仅从 dsh.profile.bundles 激活清单移除并记录原因），服务即可恢复启动，
// agent 可持续使用。关于页保留插件记录并显示禁用状态与原因，用户可检查/更新/重新启用。
//
// 禁用状态存储：profile package.json 的 dsh.profile.disabledPlugins: {<name>: <原因>}
// （pnpm/npm 忽略未知字段；与 bundles 同文件；随导出/恢复流转，见 exportimport.go 联动）。
// 启用（手动或更新后自动）：清除记录 + 重新加入 bundles；若启用后启动失败则按启动日志原因
// 重新禁用并重启服务——保证最终服务可启动。

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

// disableBakSuffix 自动禁用前对受影响 profile package.json 的轻量备份后缀
// （只备份包文件文本，不搬 node_modules；回退 = 写回即“重新启用”）。
const disableBakSuffix = ".disbak"

// ==================== profile package.json 通用读写 ====================

func readProfileRoot(dir string) map[string]interface{} {
	root := map[string]interface{}{}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err == nil {
		_ = json.Unmarshal(data, &root)
	}
	if root == nil {
		root = map[string]interface{}{}
	}
	return root
}

func writeProfileRoot(dir string, root map[string]interface{}) error {
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "package.json"), append(b, '\n'), 0o644)
}

// profileSection 取出/创建 root["dsh"]["profile"] 两级嵌套 map。
func profileSection(root map[string]interface{}) (map[string]interface{}, map[string]interface{}) {
	dsh, _ := root["dsh"].(map[string]interface{})
	if dsh == nil {
		dsh = map[string]interface{}{}
		root["dsh"] = dsh
	}
	prof, _ := dsh["profile"].(map[string]interface{})
	if prof == nil {
		prof = map[string]interface{}{}
		dsh["profile"] = prof
	}
	return dsh, prof
}

// profileDisabledMap 取 root 中禁用记录 map（无则返回空 map 且不写盘）。
func profileDisabledMap(root map[string]interface{}) map[string]interface{} {
	_, prof := profileSection(root)
	m, _ := prof["disabledPlugins"].(map[string]interface{})
	if m == nil {
		m = map[string]interface{}{}
	}
	return m
}

// appendBundleEntry 把 name 追加进 dsh.profile.bundles（已存在则不动；兼容 name@… 变体重复判断）。
func appendBundleEntry(root map[string]interface{}, name string) {
	_, prof := profileSection(root)
	bundles, _ := prof["bundles"].([]interface{})
	seen := false
	for _, b := range bundles {
		s, ok := b.(string)
		if !ok {
			continue
		}
		base := s
		if i := strings.IndexByte(base, '@'); i > 0 {
			base = base[:i]
		}
		if base == name {
			seen = true
			break
		}
	}
	if !seen {
		prof["bundles"] = append(bundles, name)
	}
}

// stripProfileBundleEntry 从 dir 的 package.json 摘除 name 的 bundle 激活声明（含 name@… 变体）。
// pnpm remove 不感知 dsh.profile.bundles：残留声明会让服务启动报
// 「cannot resolve profile bundle」硬失败，删除插件流程必须同步摘除。
func stripProfileBundleEntry(dir, name string) error {
	root := readProfileRoot(dir)
	stripBundleEntry(root, name)
	return writeProfileRoot(dir, root)
}

// ==================== 单个 profile 的禁用 / 启用 ====================

// disablePluginInProfile 禁用 profile 中的插件：依赖仍声明时，从 bundles 移除并记录原因。
func disablePluginInProfile(dir, name, reason string) error {
	root := readProfileRoot(dir)
	deps, _ := root["dependencies"].(map[string]interface{})
	if deps == nil {
		return nil
	}
	if _, ok := deps[name]; !ok {
		return nil // 依赖已不存在（如已删除），无需禁用
	}
	stripBundleEntry(root, name)
	dm := profileDisabledMap(root)
	dm[name] = reason
	_, prof := profileSection(root)
	prof["disabledPlugins"] = dm
	return writeProfileRoot(dir, root)
}

// disableGhostPluginInProfile 禁用「无依赖声明」的插件：摘除 bundle 激活 + 记录禁用原因。
// 不补依赖（避免 pnpm 重新下载失败）；记录使关于页按 disabledPlugins 合成「已自动禁用」行。
func disableGhostPluginInProfile(dir, name, reason string) error {
	root := readProfileRoot(dir)
	stripBundleEntry(root, name)
	dm := profileDisabledMap(root)
	dm[name] = reason
	_, prof := profileSection(root)
	prof["disabledPlugins"] = dm
	return writeProfileRoot(dir, root)
}

// enablePluginInProfile 启用 profile 中的插件：依赖仍声明时，清除禁用记录并加回 bundles。
func enablePluginInProfile(dir, name string) error {
	root := readProfileRoot(dir)
	deps, _ := root["dependencies"].(map[string]interface{})
	if deps == nil {
		return nil
	}
	if _, ok := deps[name]; !ok {
		return nil // 依赖不存在（被删除/卸载），无从启用
	}
	dm := profileDisabledMap(root)
	if _, ok := dm[name]; !ok {
		appendBundleEntry(root, name) // 未在禁用名单：也补回 bundles（幂等）
		return writeProfileRoot(dir, root)
	}
	delete(dm, name)
	_, prof := profileSection(root)
	prof["disabledPlugins"] = dm
	appendBundleEntry(root, name)
	return writeProfileRoot(dir, root)
}

// profilePluginDisabledReason 读取单个 profile 中插件的禁用原因（未禁用返回空）。
func profilePluginDisabledReason(dir, name string) string {
	dm := profileDisabledMap(readProfileRoot(dir))
	if v, ok := dm[name]; ok {
		s, _ := v.(string)
		return s
	}
	return ""
}

// clearProfileDisabledRecord 清除 profile 中某插件的禁用记录（不触碰 bundles；
// 供插件删除 / 重置等「依赖已不存在」场景清理残留）。无记录时不写盘。
func clearProfileDisabledRecord(dir, name string) error {
	root := readProfileRoot(dir)
	dm := profileDisabledMap(root)
	if _, ok := dm[name]; !ok {
		return nil
	}
	delete(dm, name)
	_, prof := profileSection(root)
	if len(dm) == 0 {
		delete(prof, "disabledPlugins")
	} else {
		prof["disabledPlugins"] = dm
	}
	return writeProfileRoot(dir, root)
}

// pluginDisabledAcross 检查插件在其声明目录中是否被禁用（任一目录禁用即视为禁用，
// 原因取首个禁用目录的文案）。返回 (disabled, reason)。
func pluginDisabledAcross(locs []string, name string) (bool, string) {
	for _, d := range locs {
		if r := profilePluginDisabledReason(d, name); r != "" {
			return true, r
		}
	}
	return false, ""
}

// ==================== package.json 轻量备份（.disbak） ====================

func backupProfilePkgJSON(dir string) {
	pj := filepath.Join(dir, "package.json")
	if data, err := os.ReadFile(pj); err == nil {
		_ = os.WriteFile(pj+disableBakSuffix, data, 0o644)
	}
}

func restoreProfilePkgJSONBackup(dir string) {
	pj := filepath.Join(dir, "package.json")
	if data, err := os.ReadFile(pj + disableBakSuffix); err == nil {
		if err := os.WriteFile(pj, data, 0o644); err != nil {
			log.Printf("restore .disbak failed (%s): %v", dir, err)
		}
	}
	_ = os.Remove(pj + disableBakSuffix)
}

func clearProfilePkgJSONBackup(dir string) {
	_ = os.Remove(filepath.Join(dir, "package.json"+disableBakSuffix))
}

// ==================== 启动嫌疑定位 ====================

// userSuspectPlugins 把启动日志点名的嫌疑（parseBootLogSuspects）收敛为「确属已安装用户插件」
// 的插件行：过滤官方包（@deepseek-ai/*）与依赖清单中不存在的名字，按名排序。
func userSuspectPlugins() []PluginRow {
	byName := map[string]PluginRow{}
	for _, r := range buildPluginRows() {
		if _, ok := byName[r.Name]; !ok {
			byName[r.Name] = r
		}
	}
	var out []PluginRow
	for _, n := range parseBootLogSuspects(0) {
		if isOfficialHarnessPkg(n) {
			continue
		}
		if r, ok := byName[n]; ok {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// disableBootSuspects 健康校验已失败后的自动禁用尝试（把启动归因到具体用户插件时，用禁用
// 换取服务可启动，而不是整体回退正在进行的操作）：
//  1. 无嫌疑（核心故障 / 官方 bundle 故障）→ 返回 healthy=false，调用方走各自回退；
//  2. 有嫌疑 → 备份受影响 profile 的 package.json（.disbak）→ 逐个禁用（记录启动日志原因）
//     → 重启健康校验；
//     - 成功：保持禁用、清理 .disbak，返回被禁用插件与 healthy=true（调用方按流程收尾，
//     如 LKG 提升 / 清理快照 / 提示）；
//     - 仍失败：还原 .disbak（重新启用），返回 healthy=false（调用方整体回退）。
//
// dirs：调用方认为「受影响」的 profile（如导入目标、插件所在目录）；备份范围取 dirs ∪ 嫌疑
// 插件声明目录，确保任何情况都能还原。
func disableBootSuspects(dirs []string) (disabled []PluginRow, healthy bool) {
	suspects := userSuspectPlugins() // boot 点名 ∩ 插件行（可记录禁用原因）
	// boot 点名但无插件行的（依赖已摘除/从未声明、仅 bundle 激活残留，如历史中断恢复
	// 遗留的 codegraph-*）——直接摘除其 bundle 激活声明即可让启动不再加载（new_device.log
	// 实证：只禁用有行的 2 个后仍因这两个残留 bundle 加载失败）。
	bootNames := parseBootLogSuspects(0)
	var bundleOnly []string
	if len(bootNames) > 0 {
		rowSet := map[string]bool{}
		for _, s := range suspects {
			rowSet[s.Name] = true
		}
		for _, n := range bootNames {
			if n == "" || isOfficialHarnessPkg(n) || rowSet[n] {
				continue
			}
			for _, pf := range enumeratePluginProfiles() {
				if bundleEntryExists(readProfileRoot(pf.dir), n) {
					bundleOnly = append(bundleOnly, n)
					break
				}
			}
		}
	}
	if len(suspects) == 0 && len(bundleOnly) == 0 {
		log.Printf("disable: no user-plugin suspects, keep existing rollback path")
		return nil, false
	}
	names := make([]string, 0, len(suspects)+len(bundleOnly))
	for _, s := range suspects {
		names = append(names, s.Name)
	}
	names = append(names, bundleOnly...)
	reasons := bootSuspectReasons(0, names)

	all := map[string]bool{}
	for _, d := range dirs {
		all[d] = true
	}
	for _, s := range suspects {
		for _, d := range s.Locs {
			all[d] = true
		}
	}
	profiles := enumeratePluginProfiles()
	for _, pf := range profiles {
		all[pf.dir] = true
	}
	var bak []string
	for d := range all {
		backupProfilePkgJSON(d)
		bak = append(bak, d)
	}

	killServer()
	time.Sleep(700 * time.Millisecond)
	for _, s := range suspects {
		reason := reasons[s.Name]
		if reason == "" {
			reason = "与当前 harness 版本不兼容（启动日志存在加载错误）"
		}
		for _, d := range s.Locs {
			if err := disablePluginInProfile(d, s.Name, reason); err != nil {
				log.Printf("disable suspect %s in %s failed: %v", s.Name, d, err)
			}
		}
		log.Printf("disable suspect %s: %s", s.Name, reason)
	}
	for _, n := range bundleOnly {
		reason := reasons[n]
		if reason == "" {
			reason = "与当前 harness 版本不兼容（依赖组件不满足所需 API）"
		}
		for d := range all {
			if bundleEntryExists(readProfileRoot(d), n) {
				if err := disableGhostPluginInProfile(d, n, reason); err != nil {
					log.Printf("disable bundle-only suspect %s in %s failed: %v", n, d, err)
				} else {
					// 记录 disabledPlugins 原因（无依赖行）：关于页据此显示「已自动禁用」
					log.Printf("disable bundle-only suspect %s in %s (no dependency row)", n, d)
				}
			}
		}
	}

	if restartAndVerifyServer() {
		for _, d := range bak {
			clearProfilePkgJSONBackup(d)
		}
		logUI("自动禁用不兼容插件", strings.Join(names, "、")+"，服务已恢复启动")
		return suspects, true
	}
	// 禁用后仍无法启动：还原全部受影响 profile（重新启用），交回调用方整体回退
	for _, d := range bak {
		restoreProfilePkgJSONBackup(d)
	}
	log.Printf("disable suspects (%s) did not fix boot, profiles restored", strings.Join(names, "、"))
	return suspects, false
}

// ==================== 手动启用（失败自动重新禁用） ====================

// enablePluginAndVerify 手动启用插件并保证服务可启动：
//   - 清除禁用记录并把插件加回 bundles → 重启健康校验；成功返回 (true, "")；
//   - 失败：按启动日志原因重新禁用该插件 → 再重启一次；返回 (false, 原因文案)。
//     （重新禁用后仍无法就绪属异常——启用前服务是健康的，此路径几乎不会到达，
//     此时原样返回原因与日志路径，交由调用方提示用户查看日志。）
func enablePluginAndVerify(row PluginRow) (enabled bool, msg string) {
	for _, dir := range row.Locs {
		if err := enablePluginInProfile(dir, row.Name); err != nil {
			return false, fmt.Sprintf("写回启用状态失败：%v", err)
		}
	}
	ok := restartAndVerifyServer()
	if ok {
		return true, ""
	}
	reason := ""
	if m := bootSuspectReasons(0, []string{row.Name}); m[row.Name] != "" {
		reason = m[row.Name]
	}
	if reason == "" {
		reason = "与当前 harness 版本不兼容（启用后服务启动失败）"
	}
	killServer()
	time.Sleep(700 * time.Millisecond)
	for _, dir := range row.Locs {
		if err := disablePluginInProfile(dir, row.Name, reason); err != nil {
			log.Printf("re-disable %s in %s failed: %v", row.Name, dir, err)
		}
	}
	if restartAndVerifyServer() {
		return false, reason
	}
	return false, reason + "；重新禁用后服务仍未就绪，请查看日志：" + unifiedLogPath()
}

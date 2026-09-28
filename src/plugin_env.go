package main

// ==================== 当前启动方式对应的插件环境（web | desktop） ====================
// 托盘管着两套 harness 环境：
//   - web：profiles/web，托盘自带 dsh web 服务用的环境（旧布局 machines 上 profiles 根即它）；
//   - desktop：profiles/desktop，官方桌面端自己的环境。
// 关于页的插件清单、插件恢复/导出都只针对**当前启动方式**那一套：另一套既不显示、
// 也不被改动（跨环境导入时插件文件会改写落点到当前环境，见 exportimport.go）。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// activePluginProfile 当前启动方式对应的插件环境名：web 启动 → web，desktop 启动 → desktop。
func activePluginProfile() string {
	if launchTargetIsDesktop() {
		return launchTargetDesktop
	}
	return accountPluginProfile
}

// profileDirForEnv 定位某个插件环境的 profile 目录：
//   - 命名 profile（profiles/web、profiles/desktop）里有 package.json → 用它；
//   - web 环境额外兼容旧布局（profiles/package.json：历史机器上 profiles 根就是 Web 环境）；
//   - 都没有 → ("", false)。
func profileDirForEnv(profile string) (string, bool) {
	if dir, ok := pluginProfileDir(profile); ok {
		return dir, true
	}
	if profile == accountPluginProfile {
		return legacyProfileDir()
	}
	return "", false
}

// legacyProfileDir 旧布局的 profile 根（profiles/package.json），不存在返回 false。
func legacyProfileDir() (string, bool) {
	home := dshHomeDir()
	if home == "" {
		return "", false
	}
	root := filepath.Join(home, "profiles")
	if st, err := os.Stat(filepath.Join(root, "package.json")); err == nil && !st.IsDir() {
		return root, true
	}
	return "", false
}

// activeProfileDir 当前环境的 profile 目录；命名 profile 与旧布局都不存在时返回按需创建的
// <DSH_HOME>/profiles/<name>（exists=false）——插件恢复据此把内容装进当前环境。
func activeProfileDir() (dir string, exists bool) {
	name := activePluginProfile()
	if d, ok := profileDirForEnv(name); ok {
		return d, true
	}
	home := dshHomeDir()
	if home == "" {
		return "", false
	}
	return filepath.Join(home, "profiles", name), false
}

// dirsInEnv 从目录列表中筛出属于 dir 这个环境的目录（顺序保持）。
func dirsInEnv(dirs []string, dir string) []string {
	if dir == "" {
		return nil
	}
	var out []string
	for _, d := range dirs {
		if sameProfileDir(d, dir) {
			out = append(out, d)
		}
	}
	return out
}

// scopeRowToDir 把插件行的作用目录收窄到 dir：返回 false 表示该插件不在这个环境里。
// 版本与禁用状态按收窄后的目录重算——同一插件在两套环境里可能版本不同、一边禁用一边启用，
// 直接用合并行的值会把另一环境的状态显示/作用到当前环境上。
func scopeRowToDir(r PluginRow, dir string) (PluginRow, bool) {
	locs := dirsInEnv(r.Locs, dir)
	if len(locs) == 0 {
		return PluginRow{}, false
	}
	r.Locs = locs
	r.Profile = "" // 清单本身已按环境过滤，逐行再标环境名是噪音
	r.Version = installedPluginVersion(locs[0], r.Name)
	if dis, reason := pluginDisabledAcross(locs, r.Name); dis {
		r.Disabled, r.DisabledReason = true, reason
	} else {
		r.Disabled, r.DisabledReason = false, ""
	}
	return r, true
}

// rowsInProfile 把插件行收窄到指定环境：只保留在该环境声明/安装的插件，行的 Locs 也只留该
// 环境目录（行内更新/删除/启用因此只作用于该环境，另一环境的同名插件不受影响）。
func rowsInProfile(rows []PluginRow, profile string) []PluginRow {
	dir, ok := profileDirForEnv(profile)
	if !ok {
		return nil
	}
	var out []PluginRow
	for _, r := range rows {
		scoped, ok := scopeRowToDir(r, dir)
		if !ok {
			continue
		}
		out = append(out, scoped)
	}
	return out
}

// activePluginRows 当前环境的插件行（关于页「已安装插件」）。
func activePluginRows() []PluginRow {
	return rowsInProfile(buildPluginRows(), activePluginProfile())
}

// findPluginRowInEnv 按行 ID（或包名）查找指定环境的插件行，Locs 收窄到该环境；该插件不在
// 该环境时返回 false。行内操作都经它解析，因此只改一个环境。
func findPluginRowInEnv(id, profile string) (PluginRow, bool) {
	row, ok := findPluginRowByID(id)
	if !ok {
		return PluginRow{}, false
	}
	dir, ok := profileDirForEnv(profile)
	if !ok {
		return PluginRow{}, false
	}
	return scopeRowToDir(row, dir)
}

// findActivePluginRowByID 按行 ID（或包名）在当前环境的插件行；不在当前环境时返回 false。
func findActivePluginRowByID(id string) (PluginRow, bool) {
	return findPluginRowInEnv(id, activePluginProfile())
}

// sameProfileDir 两个目录是否指同一环境（Windows / macOS 文件系统大小写不敏感）。
func sameProfileDir(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return false
}

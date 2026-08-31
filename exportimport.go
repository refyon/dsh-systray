package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ==================== harness 数据导入导出（核心逻辑，双平台共享） ====================
// 导出：会话 / 插件 / 用户目录 → 各自 zip → 打包进总 zip（dsh-systray-export-时间戳-uuid.zip）。
// 导入：解析总 zip → 罗列可恢复项 → 逐项恢复；恢复会话/插件前检查冲突、询问覆盖；
//       恢复期间暂停后台服务，避免损坏正在运行的 harness 环境。

const (
	exportFormatName    = "dsh-systray-export"
	exportFormatVersion = 1
	exportZipSessions   = "sessions.zip"
	exportZipPlugins    = "plugins.zip"
	exportZipFiles      = "files.zip"
)

// exportItemInfo 总 zip 内某一子包的信息（写入 manifest.json）。
type exportItemInfo struct {
	Kind  string `json:"kind"`  // sessions | plugins | files
	Label string `json:"label"` // 展示名
	Zip   string `json:"zip"`   // 总 zip 内的文件名
	Size  int64  `json:"size"`  // 子 zip 字节数
}

// exportManifest 总 zip 的 manifest.json。
type exportManifest struct {
	Format     string           `json:"format"`
	Version    int              `json:"version"`
	AppVersion string           `json:"appVersion"`
	Platform   string           `json:"platform"`
	CreatedAt  string           `json:"createdAt"`
	Items      []exportItemInfo `json:"items"`
	Plugins    exportPlugins    `json:"plugins,omitempty"` // 已安装插件清单（用于导入后注册回 harness profile）
}

// exportPlugins 导出时记录源 profile 的插件配置：dependencies + dsh.profile.bundles，
// 供导入后合并写入目标机器 profile 的 package.json，使恢复的插件被 harness 识别为已安装。
type exportPlugins struct {
	Profile      string            `json:"profile,omitempty"`      // 源 profile 名（空 = 旧布局 profiles 根）
	Dependencies map[string]string `json:"dependencies,omitempty"` // 插件名 → 版本规格
	Bundles      []string          `json:"bundles,omitempty"`      // dsh.profile.bundles 插件清单
}

// importItem 解析出的可恢复项。
type importItem struct {
	Kind  string `json:"kind"`  // sessions | plugins | files
	Label string `json:"label"` // 展示名
	Zip   string `json:"zip"`   // 总 zip 内的子包文件名
	Size  int64  `json:"size"`  // 子包字节数
}

// dshHomeDir harness 数据主目录（$DSH_HOME，默认 ~/.dsh），与 harness 的 resolveDshHome 一致：
// 非空（非纯空白）$DSH_HOME 优先，支持 ~ 前缀展开；sessions / profiles 均位于此根下。
func dshHomeDir() string {
	if h := os.Getenv("DSH_HOME"); h != "" && strings.TrimSpace(h) != "" {
		return expandTildePath(h)
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".dsh")
	}
	return ""
}

// expandTildePath 展开路径开头的 ~ / ~/ / ~\ 为当前用户主目录；无前缀则原样返回。
func expandTildePath(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// sessionsSourceDir 历史会话数据目录（不存在返回空串）。
func sessionsSourceDir() string {
	if dshHomeDir() == "" {
		return ""
	}
	return filepath.Join(dshHomeDir(), "sessions")
}

// pluginsSourceDir 已安装插件目录（profile 的 node_modules；优先带名称的 profile，如 web）。
func pluginsSourceDir() string {
	if dshHomeDir() == "" {
		return ""
	}
	dir, _, err := profilePlugins()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "node_modules")
}

// profilePlugins 定位 profile 目录（含 package.json）并读取其 dependencies ——
// 即通过 `dsh add` / `dsh plugin add` 安装的插件清单（harness 自带依赖不在其中）。
func profilePlugins() (profileDir string, deps []string, err error) {
	home := dshHomeDir()
	if home == "" {
		return "", nil, fmt.Errorf("无法确定 harness 数据目录（DSH_HOME）")
	}
	profilesRoot := filepath.Join(home, "profiles")
	dir := ""
	if ents, e := os.ReadDir(profilesRoot); e == nil {
		for _, ent := range ents {
			if !ent.IsDir() {
				continue
			}
			p := filepath.Join(profilesRoot, ent.Name(), "package.json")
			if st, e2 := os.Stat(p); e2 == nil && !st.IsDir() {
				dir = filepath.Join(profilesRoot, ent.Name())
				break
			}
		}
	}
	if dir == "" {
		// 兼容旧布局：~/.dsh/profiles/package.json
		if st, e := os.Stat(filepath.Join(profilesRoot, "package.json")); e == nil && !st.IsDir() {
			dir = profilesRoot
		}
	}
	if dir == "" {
		return "", nil, fmt.Errorf("未找到 profile 清单，无法识别通过 dsh add 安装的插件")
	}
	data, e := os.ReadFile(filepath.Join(dir, "package.json"))
	if e != nil {
		return "", nil, fmt.Errorf("读取 profile 清单失败：%w", e)
	}
	var m struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	_ = json.Unmarshal(data, &m)
	for name := range m.Dependencies {
		if name != "" {
			deps = append(deps, name)
		}
	}
	sort.Strings(deps)
	return dir, deps, nil
}

// profilePluginConfig 读取 profile 的 package.json，返回其插件配置（dependencies + dsh.profile.bundles + profile 名）。
func profilePluginConfig(dir string) exportPlugins {
	cfg := exportPlugins{}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return cfg
	}
	var m struct {
		Dependencies map[string]string `json:"dependencies"`
		Dsh          struct {
			Profile struct {
				Bundles []string `json:"bundles"`
			} `json:"profile"`
		} `json:"dsh"`
	}
	_ = json.Unmarshal(data, &m)
	cfg.Dependencies = m.Dependencies
	cfg.Bundles = m.Dsh.Profile.Bundles
	if home := dshHomeDir(); home != "" {
		if rel, err := filepath.Rel(home, dir); err == nil {
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "profiles/") {
				rest := strings.TrimPrefix(rel, "profiles/")
				if i := strings.IndexByte(rest, '/'); i >= 0 {
					rest = rest[:i]
				}
				cfg.Profile = rest
			}
		}
	}
	return cfg
}

// pluginsRelPrefix 插件在导出 zip 内的路径前缀（相对 DSH_HOME，恢复侧同源），
// 如 "profiles/web/node_modules/"（命名 profile）或 "profiles/node_modules/"（旧布局）。
func pluginsRelPrefix() string {
	home := dshHomeDir()
	if home == "" {
		return "profiles/node_modules/"
	}
	dir, _, err := profilePlugins()
	if err != nil {
		return "profiles/node_modules/"
	}
	rel, err := filepath.Rel(home, filepath.Join(dir, "node_modules"))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "profiles/node_modules/"
	}
	return filepath.ToSlash(rel) + "/"
}

// resolveNodeModules 在 profile 的 node_modules 中解析包名（兼容 pnpm 符号链接与 .pnpm/node_modules 布局）。
func resolveNodeModules(root, name string) (string, bool) {
	candidates := []string{
		filepath.Join(root, filepath.FromSlash(name)),
		filepath.Join(root, ".pnpm", "node_modules", filepath.FromSlash(name)),
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			if real, err := filepath.EvalSymlinks(p); err == nil {
				return real, true
			}
			return p, true
		}
	}
	return "", false
}

// collectPluginClosure 从插件包出发递归收集其依赖闭包（跳过 @deepseek-ai/* harness 自带包），
// 返回 包名 → 解析后真实目录。
func collectPluginClosure(root string, roots []string) map[string]string {
	out := map[string]string{}
	visited := map[string]bool{}
	var walk func(name string)
	walk = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		real, ok := resolveNodeModules(root, name)
		if !ok {
			log.Printf("export: plugin dep not found in node_modules: %s", name)
			return
		}
		out[name] = real
		data, err := os.ReadFile(filepath.Join(real, "package.json"))
		if err != nil {
			return
		}
		var m struct {
			Dependencies map[string]string `json:"dependencies"`
		}
		if json.Unmarshal(data, &m) != nil {
			return
		}
		for dep := range m.Dependencies {
			if strings.HasPrefix(dep, "@deepseek-ai/") {
				continue // harness 自身包：恢复目标机必然存在，不打包
			}
			walk(dep)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return out
}

// packBaseName 用户所选目录在 files.zip 内的顶层名（取目录名，重名加序号）。
func packBaseName(dir string, used map[string]bool) string {
	base := filepath.Base(filepath.Clean(dir))
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = "files"
	}
	name := base
	for i := 2; used[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	used[name] = true
	return name
}

// buildExportZip 构建导出总 zip 并放入 destDir，返回最终文件路径。
// includeSessions/includePlugins 勾选会话/插件；dirs 为要打包的目录列表（可空）。
// 子包在总 zip 内布局：manifest.json + sessions.zip（sessions/…）+ plugins.zip（profiles/node_modules/…）
// + files.zip（<目录名>/…，恢复时由用户选解压位置）。
func buildExportZip(includeSessions, includePlugins bool, dirs []string, destDir string, onStatus func(text string, pct float64)) (string, error) {
	home := dshHomeDir()
	if home == "" {
		return "", fmt.Errorf("无法确定 harness 数据目录（DSH_HOME）")
	}
	tmp, err := os.MkdirTemp("", "dsh-systray-export-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	manifest := exportManifest{
		Format:     exportFormatName,
		Version:    exportFormatVersion,
		AppVersion: appVersion,
		Platform:   runtime.GOOS,
		CreatedAt:  time.Now().Format("2006-01-02T15:04:05Z07:00"),
	}
	staged := map[string]string{} // zipName → 临时文件路径

	progress(onStatus, "正在打包历史会话…", 0)
	if includeSessions {
		src := sessionsSourceDir()
		if _, err := os.Stat(src); err != nil {
			log.Printf("export: sessions dir missing %s: %v", src, err)
		} else {
			zp := filepath.Join(tmp, exportZipSessions)
			if err := zipCreate(zp, map[string]string{"sessions": src}, func(p float64) {
				progress(onStatus, "正在打包历史会话…", p)
			}); err != nil {
				return "", fmt.Errorf("打包历史会话失败：%w", err)
			}
			if st, err := os.Stat(zp); err == nil {
				manifest.Items = append(manifest.Items, exportItemInfo{Kind: "sessions", Label: "所有历史会话", Zip: exportZipSessions, Size: st.Size()})
				staged[exportZipSessions] = zp
			}
		}
	}

	progress(onStatus, "正在打包已安装的插件…", 0)
	if includePlugins {
		dir, deps, err := profilePlugins()
		if err != nil {
			return "", err
		}
		if len(deps) == 0 {
			return "", fmt.Errorf("没有通过 dsh add 安装的插件，无需导出")
		}
		manifest.Plugins = profilePluginConfig(dir)
		root := filepath.Join(pluginsSourceDir())
		if _, err := os.Stat(root); err != nil {
			log.Printf("export: plugins dir missing %s: %v", root, err)
		} else {
			// 仅打包用户通过 dsh add 安装的插件及其非 harness 依赖闭包
			closure := collectPluginClosure(root, deps)
			prefix := pluginsRelPrefix()
			entries := make(map[string]string, len(closure))
			for name, real := range closure {
				entries[filepath.ToSlash(filepath.Join(filepath.FromSlash(prefix), filepath.FromSlash(name)))] = real
			}
			zp := filepath.Join(tmp, exportZipPlugins)
			if err := zipCreate(zp, entries, func(p float64) {
				progress(onStatus, "正在打包已安装的插件…", p)
			}); err != nil {
				return "", fmt.Errorf("打包已安装的插件失败：%w", err)
			}
			if st, err := os.Stat(zp); err == nil {
				manifest.Items = append(manifest.Items, exportItemInfo{Kind: "plugins", Label: fmt.Sprintf("已安装的插件（%d 个）", len(deps)), Zip: exportZipPlugins, Size: st.Size()})
				staged[exportZipPlugins] = zp
			}
		}
	}

	progress(onStatus, "正在打包文件目录…", 0)
	if len(dirs) > 0 {
		entries := map[string]string{}
		used := map[string]bool{}
		for _, d := range dirs {
			if st, err := os.Stat(d); err != nil || !st.IsDir() {
				log.Printf("export: skip invalid dir %s", d)
				continue
			}
			entries[packBaseName(d, used)] = d
		}
		if len(entries) > 0 {
			zp := filepath.Join(tmp, exportZipFiles)
			if err := zipCreate(zp, entries, func(p float64) {
				progress(onStatus, "正在打包文件目录…", p)
			}); err != nil {
				return "", fmt.Errorf("打包文件目录失败：%w", err)
			}
			if st, err := os.Stat(zp); err == nil {
				manifest.Items = append(manifest.Items, exportItemInfo{Kind: "files", Label: "文件目录", Zip: exportZipFiles, Size: st.Size()})
				staged[exportZipFiles] = zp
			}
		}
	}

	if len(manifest.Items) == 0 {
		return "", fmt.Errorf("没有可导出的内容：请至少勾选一项，或为「文件目录」添加目录")
	}

	progress(onStatus, "正在生成导出包…", 0.9)
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	manifestPath := filepath.Join(tmp, "manifest.json")
	if err := os.WriteFile(manifestPath, mb, 0o644); err != nil {
		return "", err
	}

	name := fmt.Sprintf("dsh-systray-export-%s-%s.zip", time.Now().Format("20060102-150405"), newExportUUID())
	entries := map[string]string{"manifest.json": manifestPath}
	for n, p := range staged {
		entries[n] = p
	}
	tmpMaster := filepath.Join(tmp, name)
	if err := zipCreate(tmpMaster, entries, nil); err != nil {
		return "", fmt.Errorf("生成导出包失败：%w", err)
	}

	final := filepath.Join(destDir, name)
	if err := moveFile(tmpMaster, final); err != nil {
		return "", fmt.Errorf("保存导出包失败：%w", err)
	}
	progress(onStatus, "", 1)
	return final, nil
}

// moveFile 移动文件（跨卷失败时复制）。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// parseExportZip 解析导出总 zip：返回可恢复项列表；解析失败返回错误（页面显示解析异常）。
// 优先读 manifest.json；缺失/损坏时按子包文件名探测（sessions.zip/plugins.zip/files.zip）。
func parseExportZip(zipPath string) ([]importItem, error) {
	names, err := zipListNames(zipPath)
	if err != nil {
		return nil, fmt.Errorf("无法打开压缩包：%w", err)
	}
	has := map[string]bool{}
	for _, n := range names {
		has[n] = true
	}
	if has["manifest.json"] {
		if data, err := zipReadFile(zipPath, "manifest.json"); err == nil {
			var m exportManifest
			if json.Unmarshal(data, &m) == nil && m.Format == exportFormatName {
				items := make([]importItem, 0, len(m.Items))
				for _, it := range m.Items {
					if has[it.Zip] {
						items = append(items, importItem{Kind: it.Kind, Label: it.Label, Zip: it.Zip, Size: it.Size})
					}
				}
				if len(items) == 0 {
					return nil, fmt.Errorf("导出包中没有可恢复的内容")
				}
				return items, nil
			}
			log.Printf("import: manifest.json invalid, fallback to filename detection")
		}
	}
	known := []importItem{
		{Kind: "sessions", Label: "所有历史会话", Zip: exportZipSessions},
		{Kind: "plugins", Label: "已安装的插件", Zip: exportZipPlugins},
		{Kind: "files", Label: "文件目录", Zip: exportZipFiles},
	}
	var items []importItem
	for _, it := range known {
		if has[it.Zip] {
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("压缩包中没有可恢复的内容（未找到 manifest.json 或 sessions.zip / plugins.zip / files.zip）")
	}
	return items, nil
}

// registerRestoredPlugins 读取总 zip 的 manifest，将导出的插件配置（dependencies + dsh.profile.bundles）
// 合并写入目标 profile 的 package.json，使恢复后的插件被 harness 识别为已安装。
// 无 manifest（旧包）或未勾选插件时静默跳过。返回错误只对真正失败的情形。
func registerRestoredPlugins(masterZipPath string) error {
	data, err := zipReadFile(masterZipPath, "manifest.json")
	if err != nil {
		return nil
	}
	var m exportManifest
	if err := json.Unmarshal(data, &m); err != nil || m.Plugins.Dependencies == nil {
		return nil
	}
	home := dshHomeDir()
	if home == "" {
		return fmt.Errorf("无法确定 harness 数据目录（DSH_HOME）")
	}
	// 目标 profile 目录集合：恢复到的 profile（源名，插件文件所在）+ harness 当前激活 profile。
	dirs := map[string]bool{}
	if m.Plugins.Profile != "" {
		dirs[filepath.Join(home, "profiles", m.Plugins.Profile)] = true
	} else {
		dirs[filepath.Join(home, "profiles")] = true
	}
	if dir, _, err := profilePlugins(); err == nil {
		dirs[dir] = true
	}
	for dir := range dirs {
		if err := mergePluginConfigIntoProfile(dir, m.Plugins); err != nil {
			return err
		}
	}
	return nil
}

// mergePluginConfigIntoProfile 把插件依赖与 bundles 合并进指定 profile 的 package.json（不存在则创建）。
func mergePluginConfigIntoProfile(dir string, cfg exportPlugins) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	pj := filepath.Join(dir, "package.json")
	root := map[string]interface{}{}
	if data, err := os.ReadFile(pj); err == nil {
		_ = json.Unmarshal(data, &root)
	}
	if root == nil {
		root = map[string]interface{}{}
	}
	// dependencies（插件名 → 版本规格），合并去重（保留已有项，新增缺失项）。
	deps, _ := root["dependencies"].(map[string]interface{})
	if deps == nil {
		deps = map[string]interface{}{}
	}
	for k, v := range cfg.Dependencies {
		if _, exists := deps[k]; exists {
			continue
		}
		deps[k] = v
	}
	root["dependencies"] = deps
	// dsh.profile.bundles：合并到已有序号末尾，避免破坏已激活 bundle 的顺序。
	dsh, _ := root["dsh"].(map[string]interface{})
	if dsh == nil {
		dsh = map[string]interface{}{}
	}
	prof, _ := dsh["profile"].(map[string]interface{})
	if prof == nil {
		prof = map[string]interface{}{}
	}
	bundles, _ := prof["bundles"].([]interface{})
	seen := map[string]bool{}
	for _, b := range bundles {
		if s, ok := b.(string); ok {
			seen[s] = true
		}
	}
	for _, b := range cfg.Bundles {
		if !seen[b] {
			bundles = append(bundles, b)
			seen[b] = true
		}
	}
	prof["bundles"] = bundles
	dsh["profile"] = prof
	root["dsh"] = dsh

	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pj, append(b, '\n'), 0o644)
}

// restoreEntryPrefix 子包在总 zip 内的条目前缀（sessions/ 或插件前缀 profiles/…/node_modules/）。
func restoreEntryPrefix(kind string) string {
	if kind == "plugins" {
		return pluginsRelPrefix()
	}
	return "sessions/"
}

// conflictTops 子包顶层条目名（sessions 的 scope 目录 / plugins 的包目录）。
func conflictTops(kind, zipPath string) ([]string, error) {
	names, err := zipListNames(zipPath)
	if err != nil {
		return nil, err
	}
	prefix := restoreEntryPrefix(kind)
	seen := map[string]bool{}
	var tops []string
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) || n == prefix {
			continue
		}
		rest := strings.TrimPrefix(n, prefix)
		if rest == "" {
			continue
		}
		top := rest
		if i := strings.Index(rest, "/"); i >= 0 {
			top = rest[:i]
		}
		if !seen[top] {
			seen[top] = true
			tops = append(tops, top)
		}
	}
	sort.Strings(tops)
	return tops, nil
}

// countRestoreConflicts 恢复会话/插件前统计与当前环境的冲突项数（顶层目录已存在即冲突）。
func countRestoreConflicts(kind, zipPath string) (int, error) {
	tops, err := conflictTops(kind, zipPath)
	if err != nil {
		return 0, err
	}
	home := dshHomeDir()
	if home == "" {
		return 0, fmt.Errorf("无法确定 harness 数据目录（DSH_HOME）")
	}
	n := 0
	for _, top := range tops {
		p := filepath.Join(home, filepath.FromSlash(restoreEntryPrefix(kind)), filepath.FromSlash(top))
		if _, err := os.Stat(p); err == nil {
			n++
		}
	}
	return n, nil
}

// restoreItem 恢复子包：
// kind=sessions → 解压到 DSH_HOME（条目 sessions/…）；kind=plugins → 解压到 DSH_HOME（条目 profiles/node_modules/…）；
// kind=files → 解压到 filesDest（条目 <目录名>/…）。
// overwrite=true 覆盖已有（冲突顶层目录先改名备份，失败回滚，成功后删除备份）；false 跳过已有。
// 返回备份目录信息文本（无备份时为空）。
func restoreItem(kind, zipPath, filesDest string, overwrite bool, onStatus func(text string, pct float64)) (string, error) {
	if err := validateZipSafe(zipPath); err != nil {
		return "", err
	}
	label := "历史会话"
	if kind == "plugins" {
		label = "已安装的插件"
	} else if kind == "files" {
		label = "文件目录"
	}
	progress(onStatus, "正在恢复"+label+"…", 0)

	dest := filesDest
	backups := map[string]string{} // 原路径 → 备份路径
	if kind != "files" {
		home := dshHomeDir()
		if home == "" {
			return "", fmt.Errorf("无法确定 harness 数据目录（DSH_HOME）")
		}
		dest = home
		if overwrite {
			// 冲突顶层目录先改名备份（同卷瞬间完成），失败可回滚
			tops, err := conflictTops(kind, zipPath)
			if err != nil {
				return "", err
			}
			ts := time.Now().Format("20060102-150405") + "-" + newExportUUID()[:4]
			for _, top := range tops {
				orig := filepath.Join(home, filepath.FromSlash(restoreEntryPrefix(kind)), filepath.FromSlash(top))
				if _, err := os.Stat(orig); err != nil {
					continue
				}
				bak := orig + ".dshbak-" + ts
				if err := os.Rename(orig, bak); err != nil {
					// 备份失败：回滚已做的备份，放弃覆盖
					for o, b := range backups {
						_ = os.Rename(b, o)
					}
					return "", fmt.Errorf("备份现有数据失败（%s）：%w", top, err)
				}
				backups[orig] = bak
			}
		}
	}

	err := zipExtract(zipPath, dest, overwrite)
	if err != nil {
		for o, b := range backups {
			_ = os.Rename(b, o) // 回滚备份
		}
		return "", err
	}
	// 成功：清理备份
	for _, b := range backups {
		_ = os.RemoveAll(b)
	}
	progress(onStatus, "", 1)
	return strings.TrimSpace(strings.Join(func() []string {
		var out []string
		for o := range backups {
			out = append(out, o)
		}
		return out
	}(), "\n")), nil
}

// extractInnerZip 从总 zip 中提取子包到临时文件，返回临时路径与清理函数。
func extractInnerZip(masterPath, zipName string) (string, func(), error) {
	zr, err := zip.OpenReader(masterPath)
	if err != nil {
		return "", nil, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != zipName {
			continue
		}
		tmp, err := os.CreateTemp("", "dsh-systray-inner-*")
		if err != nil {
			return "", nil, err
		}
		tmpPath := tmp.Name()
		rc, err := f.Open()
		if err != nil {
			tmp.Close()
			_ = os.Remove(tmpPath)
			return "", nil, err
		}
		_, cerr := io.Copy(tmp, rc)
		rc.Close()
		terr := tmp.Close()
		if cerr != nil || terr != nil {
			_ = os.Remove(tmpPath)
			return "", nil, fmt.Errorf("提取 %s 失败", zipName)
		}
		return tmpPath, func() { _ = os.Remove(tmpPath) }, nil
	}
	return "", nil, fmt.Errorf("导出包中未找到 %s", zipName)
}

// pauseServiceForRestore 恢复前暂停后台服务（true=确实停止了服务）。
func pauseServiceForRestore() bool {
	if !serverResponding(webURL) {
		return false
	}
	killServer()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !serverResponding(webURL) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return !serverResponding(webURL)
}

// resumeServiceAfterRestore 恢复完成后重新拉起后台服务并等待就绪。
func resumeServiceAfterRestore() {
	if serverResponding(webURL) {
		return
	}
	started, exitCh := startServer()
	if !started {
		log.Printf("resume service failed: startServer returned false")
		return
	}
	if ok, _ := waitForServerReady(webURL, exitCh, 2*time.Minute); !ok {
		log.Printf("service not ready within 2 minutes after restore")
	}
}

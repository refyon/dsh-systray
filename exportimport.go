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
// includeSessions/includePlugins/includeFiles 分别勾选会话/插件/文件目录；dirs 为要打包的目录列表（可空）。
// destFile 非空时把压缩包保存到该完整路径（用户通过 SaveFileDialog 选择的位置），否则在 destDir 内自动命名。
// 子包在总 zip 内布局：manifest.json + sessions.zip（sessions/…）+ plugins.zip（profiles/node_modules/…）
// + files.zip（<目录名>/…，恢复时由用户选解压位置）。
func buildExportZip(includeSessions, includePlugins, includeFiles bool, dirs []string, destDir string, onStatus func(text string, pct float64), destFile string) (string, error) {
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
		dir, deps, perr := profilePlugins()
		switch {
		case perr != nil:
			// 无插件清单（从未通过 dsh add 安装插件）：跳过，不中断其余内容的导出
			log.Printf("export: no plugin profile found, skipping plugins: %v", perr)
		case len(deps) == 0:
			log.Printf("export: no plugins installed via dsh add, skipping plugins")
		default:
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
	}

	progress(onStatus, "正在打包文件目录…", 0)
	if includeFiles && len(dirs) > 0 {
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

	final := destFile
	if final == "" {
		final = filepath.Join(destDir, name)
	}
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

// innerZipContentPrefix 从子包 zip 条目自身推导内容在 DSH_HOME 下的公共前缀（冲突检测/备份基准）：
// sessions → "sessions/"；plugins → 扫描条目得出 "profiles/<profile>/node_modules/"
// （兼容旧布局 "profiles/node_modules/"）。不依赖目标机器当前 profile 布局，避免源/目标
// profile 名不一致时冲突检测错位（此前取目标机 pluginsRelPrefix，与 zip 内源机前缀不匹配，
// 会导致冲突统计恒为 0、覆盖模式不备份）。
// 无法推导（如 files 类目）返回 ""，调用方按"无冲突"处理。
func innerZipContentPrefix(kind, zipPath string) string {
	if kind == "sessions" {
		return "sessions/"
	}
	names, err := zipListNames(zipPath)
	if err != nil {
		return ""
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "profiles/") {
			continue
		}
		parts := strings.Split(n, "/")
		if len(parts) >= 2 && parts[1] == "node_modules" {
			return "profiles/node_modules/" // 旧布局：profiles 即 profile 根
		}
		for i := 2; i < len(parts); i++ {
			if parts[i] == "node_modules" {
				return strings.Join(parts[:i+1], "/") + "/"
			}
		}
	}
	return ""
}

// conflictTops 子包顶层条目名（sessions 的 scope 目录 / plugins 的包目录），
// 前缀以 zip 内容推导为准；files 类目（无公共前缀）返回空。
func conflictTops(kind, zipPath string) ([]string, error) {
	names, err := zipListNames(zipPath)
	if err != nil {
		return nil, err
	}
	prefix := innerZipContentPrefix(kind, zipPath)
	if prefix == "" {
		return nil, nil
	}
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
	prefix := innerZipContentPrefix(kind, zipPath)
	if prefix == "" {
		return 0, nil
	}
	home := dshHomeDir()
	if home == "" {
		return 0, fmt.Errorf("无法确定 harness 数据目录（DSH_HOME）")
	}
	n := 0
	for _, top := range tops {
		p := filepath.Join(home, filepath.FromSlash(prefix), filepath.FromSlash(top))
		if _, err := os.Stat(p); err == nil {
			n++
		}
	}
	return n, nil
}

// topDirConflicts 统计 zip 顶层目录（去掉第一级后的首个路径段）在 destDir 下已存在的项数。
// 用于 files 类目：用户选定解压位置后，提示同名顶层目录会被覆盖。
func topDirConflicts(zipPath, destDir string) (int, []string, error) {
	names, err := zipListNames(zipPath)
	if err != nil {
		return 0, nil, err
	}
	seen := map[string]bool{}
	var tops []string
	for _, n := range names {
		top := n
		if i := strings.Index(n, "/"); i >= 0 {
			top = n[:i]
		}
		if top == "" || seen[top] {
			continue
		}
		seen[top] = true
		p := filepath.Join(destDir, filepath.FromSlash(top))
		if _, err := os.Stat(p); err == nil {
			tops = append(tops, top)
		}
	}
	sort.Strings(tops)
	return len(tops), tops, nil
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
	prefix := innerZipContentPrefix(kind, zipPath)
	if kind != "files" {
		home := dshHomeDir()
		if home == "" {
			return "", fmt.Errorf("无法确定 harness 数据目录（DSH_HOME）")
		}
		dest = home
		if overwrite && prefix != "" {
			// 冲突顶层目录先改名备份（同卷瞬间完成），失败可回滚
			tops, err := conflictTops(kind, zipPath)
			if err != nil {
				return "", err
			}
			ts := time.Now().Format("20060102-150405") + "-" + newExportUUID()[:4]
			for _, top := range tops {
				orig := filepath.Join(home, filepath.FromSlash(prefix), filepath.FromSlash(top))
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

// resumeServiceAfterRestore 恢复完成后重新拉起后台服务并等待就绪；
// 成功后刷新服务状态与托盘菜单（serverReady/menuStatus）。
func resumeServiceAfterRestore() {
	if serverResponding(webURL) {
		return
	}
	started, exitCh := startServer()
	if !started {
		log.Printf("resume service failed: startServer returned false")
		return
	}
	if ok, _ := waitForServerReady(webURL, exitCh, 2*time.Minute); ok {
		serverReady.Store(true)
		serviceFailed.Store(false)
		refreshServiceMenu()
		log.Printf("service resumed after restore")
	} else {
		log.Printf("service not ready within 2 minutes after restore")
	}
}

// ==================== 插件导入事务（快照 → 对齐 → 校验 → 回退/提升） ====================
// 背景：直接把导出的插件文件解压进 profile 的 node_modules 会留下 pnpm 从未产生过的
// 不一致树（版本与锁文件/.modules.yaml 脱节、junction 与真实目录混杂），dsh web 启动
// 是 fail-loud——任一 bundle 解析/激活失败即进程退出（“服务无法启动”）。因此插件导入
// 采用与「插件更新/删除」同级的事务保障：
//  1. 暂停服务后把受影响 profile 的 package.json / pnpm-lock.yaml / node_modules 快照
//     为 *.importbak（node_modules 整目录改名暂存，解压落在全新空树，杜绝 7z 穿过
//     junction 写进 .pnpm 商店污染共享依赖；回退可离线直接移回）；
//  2. 解压 + 注册依赖后执行 pnpm install 让 pnpm 重建一致树（网络失败降级，由健康校验定夺）；
//  3. 拉起服务并健康校验；失败自动对齐重试一次；仍失败回退快照并恢复服务；
//  4. 成功把导入前快照提升为 LKG（与更新/删除成功后的 LKG 语义一致）。

// importBakSuffix 插件导入事务快照后缀（区别于更新流程进行中的 .dshbak 与 LKG .lkgbak）。
const importBakSuffix = ".importbak"

// restoredPluginProfileDirs 插件导入将影响的 profile 目录集合（manifest 源 profile +
// 当前激活 profile 的并集，去重排序）——与 registerRestoredPlugins 的写入目标一致，
// 供快照 / 回退使用。manifest 缺失或未含插件配置时返回空。
func restoredPluginProfileDirs(masterZipPath string) []string {
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
		return nil
	}
	set := map[string]bool{}
	if m.Plugins.Profile != "" {
		set[filepath.Join(home, "profiles", m.Plugins.Profile)] = true
	} else {
		set[filepath.Join(home, "profiles")] = true
	}
	if dir, _, err := profilePlugins(); err == nil {
		set[dir] = true
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// snapshotImportProfiles 导入插件前快照各 profile：备份 package.json / pnpm-lock.yaml，
// 并把 node_modules 整目录改名暂存（同盘 rename，秒级；回退可直接移回、不依赖网络）。
// 返回每个目录是否成功暂存了 node_modules。前提：服务已停止（killServer 后调用——
// 运行中的 node 进程占用文件会导致改名失败）。
func snapshotImportProfiles(dirs []string) []bool {
	had := make([]bool, len(dirs))
	for i, dir := range dirs {
		for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
			src := filepath.Join(dir, name)
			if data, err := os.ReadFile(src); err == nil {
				_ = os.WriteFile(src+importBakSuffix, data, 0o644)
			}
		}
		nm := filepath.Join(dir, "node_modules")
		if _, err := os.Stat(nm); err != nil {
			continue
		}
		bak := nm + importBakSuffix
		_ = os.RemoveAll(bak)
		if os.Rename(nm, bak) == nil {
			had[i] = true
		} else {
			log.Printf("import: park node_modules failed (%s)", nm)
		}
	}
	return had
}

// rollbackImportProfiles 回退插件导入：还原 package.json / pnpm-lock.yaml，并把暂存的
// node_modules 移回（离线可用）。had[i] 指示该目录是否暂存了 node_modules。
func rollbackImportProfiles(dirs []string, had []bool) {
	for i, dir := range dirs {
		for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
			bak := filepath.Join(dir, name+importBakSuffix)
			if data, err := os.ReadFile(bak); err == nil {
				_ = os.WriteFile(filepath.Join(dir, name), data, 0o644)
			}
		}
		nm := filepath.Join(dir, "node_modules")
		if i < len(had) && had[i] {
			_ = os.RemoveAll(nm)
			if os.Rename(nm+importBakSuffix, nm) != nil {
				log.Printf("import: restore node_modules failed (%s)", nm)
			}
		}
	}
}

// cleanupImportProfiles 导入成功：清理导入快照残留。
func cleanupImportProfiles(dirs []string) {
	for _, dir := range dirs {
		for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
			_ = os.Remove(filepath.Join(dir, name+importBakSuffix))
		}
		_ = os.RemoveAll(filepath.Join(dir, "node_modules"+importBakSuffix))
	}
}

// promoteImportProfilesToLkg 导入成功且服务校验通过：把导入前快照提升为 LKG——
// 未来冷启动失败时可按「导入前状态」回退（与插件更新/删除成功后的 LKG 语义一致）。
func promoteImportProfilesToLkg(dirs []string) {
	for _, dir := range dirs {
		for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
			src := filepath.Join(dir, name+importBakSuffix)
			if _, err := os.Stat(src); err != nil {
				continue
			}
			dst := filepath.Join(dir, name+lkgSuffix)
			_ = os.Remove(dst)
			_ = os.Rename(src, dst)
		}
		srcNM := filepath.Join(dir, "node_modules"+importBakSuffix)
		if _, err := os.Stat(srcNM); err == nil {
			dstNM := filepath.Join(dir, "node_modules"+lkgSuffix)
			_ = os.RemoveAll(dstNM)
			_ = os.Rename(srcNM, dstNM)
		}
	}
}

// finishPluginImport 插件导入收尾：拉起服务并健康校验（restartAndVerifyHealing，
// 失败自动 pnpm 对齐重试一次）；仍失败则回退导入前快照并恢复服务。
// 成功返回 nil；失败返回面向用户的说明文案（含可疑插件与回退结果）。
func finishPluginImport(dirs []string, hadNM []bool) error {
	healthy, suspects := restartAndVerifyHealing(dirs)
	if healthy {
		promoteImportProfilesToLkg(dirs)
		cleanupImportProfiles(dirs)
		markServiceResumed()
		log.Printf("import: plugins restored and service verified healthy")
		return nil
	}
	reason := "启动日志存在加载错误（版本/插件不兼容）"
	if len(suspects) > 0 {
		reason += "（疑似插件：" + strings.Join(suspects, "、") + "）"
	}
	log.Printf("import: service not healthy after restore heal (%s), rolling back", reason)
	killServer()
	time.Sleep(1 * time.Second)
	rollbackImportProfiles(dirs, hadNM)
	if !restartAndVerifyServer() {
		return fmt.Errorf("导入的插件导致服务启动失败（%s）。已回退导入内容，但回退后的服务仍未能就绪，请查看日志：%s", reason, filepath.Join(logDir, "server.log"))
	}
	markServiceResumed()
	return fmt.Errorf("导入的插件导致服务启动失败（%s）。已自动回退到导入前状态，服务已恢复正常，本次导入未生效。", reason)
}

// markServiceResumed 服务已就绪并刷新托盘菜单（与 resumeServiceAfterRestore 的状态口径一致）。
func markServiceResumed() {
	serverReady.Store(true)
	serviceFailed.Store(false)
	refreshServiceMenu()
}

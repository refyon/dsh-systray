package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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

// exportPlugins 导出时记录源 profile 的插件配置：dependencies + dsh.profile.bundles + 禁用记录，
// 供导入后合并写入目标机器 profile 的 package.json，使恢复的插件被 harness 识别为已安装
// （禁用插件保持禁用——bundles 不含它，Disabled 记录其禁用原因，供恢复侧展示/维持状态）。
type exportPlugins struct {
	Profile      string            `json:"profile,omitempty"`      // 源 profile 名（空 = 旧布局 profiles 根）
	Dependencies map[string]string `json:"dependencies,omitempty"` // 插件名 → 版本规格
	Bundles      []string          `json:"bundles,omitempty"`      // dsh.profile.bundles 插件清单
	Disabled     map[string]string `json:"disabled,omitempty"`     // dsh.profile.disabledPlugins（禁用原因）
	Versions     map[string]string `json:"versions,omitempty"`     // 插件名 → 导出时实际已装版本（node_modules 读取）：
	// 导入侧据此做版本感知的副本裁决/刷新（本地插件重复导入后新机仍显示旧版本——
	// 若无版本快照，目标机已有旧 local-plugins 副本时无法判断导入包是否更新）。
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

// profilePluginConfig 读取 profile 的 package.json，返回其插件配置
// （dependencies + dsh.profile.bundles + disabledPlugins + profile 名）。
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
				Bundles  []string          `json:"bundles"`
				Disabled map[string]string `json:"disabledPlugins"`
			} `json:"profile"`
		} `json:"dsh"`
	}
	_ = json.Unmarshal(data, &m)
	cfg.Dependencies = m.Dependencies
	cfg.Bundles = m.Dsh.Profile.Bundles
	cfg.Disabled = m.Dsh.Profile.Disabled
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

// readPkgVersionField 读取目录 package.json 的 version（缺失/解析失败返回空）。
func readPkgVersionField(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(m.Version), "v")
}

// pluginVersionSnapshot 导出时记录每个已装插件的实际版本（name → version，node_modules 读取），
// 写入 manifest.Plugins.Versions 供导入侧版本感知裁决（见 exportPlugins.Versions 注释）。
func pluginVersionSnapshot(profileDir string, deps map[string]string) map[string]string {
	out := map[string]string{}
	names := make([]string, 0, len(deps))
	for n := range deps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "" || isOfficialHarnessPkg(name) {
			continue
		}
		if v := installedPluginVersion(profileDir, name); v != "" {
			out[name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// collectPluginClosure 从插件包出发递归收集其依赖闭包（跳过 @deepseek-ai/* harness 自带包），
// 返回 包名 → 打包源真实目录。deps 为 profile 顶层依赖（name → spec）：对本地 spec
// （file:/link:/workspace:）且目标目录存在、其 package.json 的 name 与依赖名一致时，打包源
// 直取该目标目录——保证导出包携带开发目录的当前版本，而非 node_modules 中 pnpm 安装时的旧
// 快照（本地插件 0.2.1 导出后新机仍显示 0.2.0 的根因之二）。嵌套依赖仍从 profile node_modules
// 解析（不含本地 spec 偏好）。
func collectPluginClosure(root, profileDir string, deps map[string]string) map[string]string {
	out := map[string]string{}
	visited := map[string]bool{}
	var walk func(name, from string)
	walk = func(name, from string) {
		if visited[name] {
			return
		}
		visited[name] = true
		real := from
		if real == "" {
			var ok bool
			real, ok = resolveNodeModules(root, name)
			if !ok {
				log.Printf("export: plugin dep not found in node_modules: %s", name)
				return
			}
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
			walk(dep, "")
		}
	}
	for name, spec := range deps {
		src := ""
		if p, ok := localSpecPath(spec); ok {
			target := p
			if !filepath.IsAbs(target) {
				target = filepath.Join(profileDir, filepath.FromSlash(target))
			}
			if st, err := os.Stat(filepath.Clean(target)); err == nil && st.IsDir() {
				if pkgName, _, perr := packageMeta(filepath.Clean(target)); perr == nil && pkgName == name {
					src = filepath.Clean(target) // 本地 spec 且目录有效：直取开发目录当前内容
				}
			}
		}
		walk(name, src)
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
				manifest.Items = append(manifest.Items, exportItemInfo{Kind: "sessions", Label: T("所有历史会话"), Zip: exportZipSessions, Size: st.Size()})
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
			// 版本快照随 manifest 写入（name → node_modules 实际已装版本），供导入侧版本感知裁决
			manifest.Plugins.Versions = pluginVersionSnapshot(dir, manifest.Plugins.Dependencies)
			root := filepath.Join(pluginsSourceDir())
			if _, err := os.Stat(root); err != nil {
				log.Printf("export: plugins dir missing %s: %v", root, err)
			} else {
				// 仅打包用户通过 dsh add 安装的插件及其非 harness 依赖闭包；
				// 本地 spec 插件打包源直取 spec 目标目录（当前版本），而非 node_modules 旧快照
				closure := collectPluginClosure(root, dir, manifest.Plugins.Dependencies)
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
					manifest.Items = append(manifest.Items, exportItemInfo{Kind: "plugins", Label: TF("已安装的插件（%d 个）", len(deps)), Zip: exportZipPlugins, Size: st.Size()})
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
				manifest.Items = append(manifest.Items, exportItemInfo{Kind: "files", Label: T("文件目录"), Zip: exportZipFiles, Size: st.Size()})
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
		{Kind: "sessions", Label: T("所有历史会话"), Zip: exportZipSessions},
		{Kind: "plugins", Label: T("已安装的插件"), Zip: exportZipPlugins},
		{Kind: "files", Label: T("文件目录"), Zip: exportZipFiles},
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
	// dsh.profile.bundles：恢复的插件**先无条件激活**（用户确认范围：只激活 cfg.Bundles 覆盖的
	// 名字；cfg.Dependencies 里源机从未激活过的依赖不自动激活）。启动时若某插件与当前核心不兼容，
	// 由 finishPluginImport 自愈链路自动禁用并写入当前环境的原因——因此合并阶段：
	//  1) 不合并源机的 disabledPlugins（源机原因未必适用于目标环境，避免恢复后一直禁用）；
	//  2) 不跳过任何 bundles 名（含目标机上曾禁用的）——全部追加，交给启动校验裁决；
	//  3) 对被激活名字清除目标 profile 的旧禁用记录（记录与激活必须一致，否则关于页显示
	//     「已禁用」徽标但插件实际已激活，造成误导）。
	dsh, _ := root["dsh"].(map[string]interface{})
	if dsh == nil {
		dsh = map[string]interface{}{}
	}
	prof, _ := dsh["profile"].(map[string]interface{})
	if prof == nil {
		prof = map[string]interface{}{}
	}
	bundleBase := func(s string) string {
		if i := strings.IndexByte(s, '@'); i > 0 {
			return s[:i]
		}
		return s
	}
	bundles, _ := prof["bundles"].([]interface{})
	seen := map[string]bool{}
	for _, b := range bundles {
		if s, ok := b.(string); ok {
			seen[s] = true
		}
	}
	activated := 0
	for _, b := range cfg.Bundles {
		if b == "" || seen[b] {
			continue
		}
		bundles = append(bundles, b)
		seen[b] = true
		activated++
	}
	if activated > 0 {
		prof["bundles"] = bundles
	}
	// 清除被激活名字的目标禁用记录（同时清理因 bundleBase 变体造成的孤儿记录）
	if disabled, ok := prof["disabledPlugins"].(map[string]interface{}); ok && len(disabled) > 0 {
		cleared := false
		for _, b := range cfg.Bundles {
			base := bundleBase(b)
			if _, exists := disabled[base]; exists {
				delete(disabled, base)
				cleared = true
			}
		}
		if cleared {
			if len(disabled) == 0 {
				delete(prof, "disabledPlugins")
			} else {
				prof["disabledPlugins"] = disabled
			}
		}
	}
	dsh["profile"] = prof
	root["dsh"] = dsh

	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pj, append(b, '\n'), 0o644)
}

// ==================== 本地链接依赖跨机恢复：优先「副本继续加载」，无副本才「待重指定挂起」 ====================
// 背景：源机以 link:/file:/workspace:（或相对/绝对路径）安装的开发态插件，spec 记录的是源机
// 绝对路径。跨机恢复时该路径在本机不存在，pnpm install 直接 ERR_PNPM_LINKED_PKG_DIR_NOT_FOUND
// 硬失败（new_device.log 实证 dsh-ui-taste），且此前 reconcile 失败只记日志、不修复，导致恢复
// 必然回退。
// 早期消毒把这类依赖改写为 npm:name@version（registry 版本可能与目标机 harness 核心不兼容，
// new_device.log 实证 dsh-ui-taste 因 @deepseek-ai/dsh-settings 缺 settingsNamespace 导出、
// codegraph 插件因 @deepseek-ai/dsh-llm 缺 assertNever 导出而启动失败）或直接删除（插件从
// 列表消失、无法再指回本地目录）。
// sanitizeProfileLocalDepsAll 在合并写回 profile package.json 之后、pnpm 对齐之前执行：
//   - 目标路径在本机存在 → 保持原样（同机恢复的开发态链接不受影响）；
//   - 目标路径缺失但导入包在 profile node_modules 恢复了该插件副本 → 副本迁到
//     <dshHome>/profiles/local-plugins/<name>，spec 改写为 link:<副本>：bundle 激活保持，
//     插件继续加载启动；用户仍可点「更新…」改指自己的开发目录；
//   - 目标路径缺失且无副本（插件不在导入包内等）→ 移出 dependencies、记录为「待重指定」
//     （dsh.profile.pendingLocalPlugins，含原 spec 与是否曾激活），并清理 bundle / 禁用记录。
//     插件列表仍显示该本地插件行，用户点「更新…」重新选择本地目录后由更新事务落回 link: spec
//     并恢复激活。
// 返回面向用户的说明行；package.json 改写仅在真正变化时落盘（回退路径由 .importbak 快照兜底）。

// localSpecPath 从依赖 spec 提取本地目录路径（link:/file:/workspace: 或相对/绝对目录形态）。
// 非本地 spec 返回 ("", false)。
func localSpecPath(spec string) (string, bool) {
	s := strings.TrimSpace(spec)
	low := strings.ToLower(s)
	var p string
	switch {
	case strings.HasPrefix(low, "link:"):
		p = strings.TrimSpace(s[len("link:"):])
	case strings.HasPrefix(low, "file:"):
		p = strings.TrimSpace(s[len("file:"):])
	case strings.HasPrefix(low, "workspace:"):
		p = strings.TrimSpace(s[len("workspace:"):])
	case strings.HasPrefix(s, "./"), strings.HasPrefix(s, "../"),
		strings.HasPrefix(s, "/"), strings.HasPrefix(s, `\`):
		p = s
	case isDriveAbsPath(s):
		p = s
	default:
		return "", false
	}
	if p == "" {
		return "", false
	}
	return p, true
}

// isDriveAbsPath 是否为 Windows 盘符绝对路径（C:\… / C:/…）。
func isDriveAbsPath(s string) bool {
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') || s[1] != ':' {
		return false
	}
	return s[2] == '\\' || s[2] == '/'
}

// sanitizeProfileLocalDeps 单目录版（多目录走 sanitizeProfileLocalDepsAll，副本裁决跨目录共享）。
func sanitizeProfileLocalDeps(dir string) []string {
	return sanitizeProfileLocalDepsAll([]string{dir})
}

// sanitizeProfileLocalDepsAll 多目录版本地依赖恢复（见文件头注释）：先全局收集目标缺失的
// 本地依赖，再统一裁决「副本继续加载 / 待重指定挂起」，随后逐目录改写，最后做「版本感知
// 刷新」——spec 已指向本地稳定副本但本次导入包携带更新副本时升级副本（重复导入场景，
// 修复「本地 0.2.1 导出、新机导入后仍显示 0.2.0」）。同名插件跨目录只输出一条说明。
// 返回用户可见说明行。
func sanitizeProfileLocalDepsAll(dirs []string) []string {
	missing := map[string]string{} // name → 任一目录的原始本地 spec
	for _, dir := range dirs {
		for name, spec := range missingLocalDeps(dir) {
			if _, ok := missing[name]; !ok {
				missing[name] = spec
			}
		}
	}
	var notes []string
	// 副本裁决：存在恢复副本的插件迁到稳定目录（link 指向副本继续加载）；无副本的维持待重指定。
	adopted := map[string]string{}
	replaced := map[string]string{} // name → 旧版本号（已用导入包更新版本替换稳定副本）
	if root := localPluginsRoot(); root != "" {
		for name := range missing {
			if canon, oldVer, ok := adoptRestoredCopy(dirs, root, name); ok {
				adopted[name] = canon
				if oldVer != "" {
					replaced[name] = oldVer
				}
				pruneRestoredCopies(dirs, name)
			}
		}
	}
	noted := map[string]bool{}
	for _, dir := range dirs {
		notes = append(notes, rewriteProfileLocalDeps(dir, missing, adopted, noted)...)
	}
	for name, oldVer := range replaced {
		if noted[name] {
			continue
		}
		noted[name] = true
		newVer := readPkgVersionField(adopted[name])
		notes = append(notes, fmt.Sprintf(
			"本地插件 %s 的原依赖路径在本机不可用，已用导入包内版本更新本地副本（v%s → v%s，旧副本已备份）——如需改用你的开发目录，可点「更新…」重新指定",
			name, oldVer, orDash(newVer)))
	}
	// 版本感知刷新：spec 已指向稳定副本（此前进过 adopt）且导入包副本更新 → 升级稳定副本
	notes = append(notes, migrateLegacyLocalCopies(dirs, noted)...)
	notes = append(notes, refreshStableLocalCopies(dirs, noted)...)
	return notes
}

// localPluginsRoot 导入副本的稳定落点目录（<dshHome>/profiles/local-plugins）；home 不可得
// 返回空（此时副本裁决跳过，全部走待重指定挂起，不触碰任何磁盘位置）。
// 落点必须在 profiles 工作区树内：dsh 的 loader 以 Node ESM 解析插件依赖，link: 依赖的
// 真实路径决定了 node_modules 上溯链——旧落点 <dshHome>/local-plugins 在 profiles 之外，
// 上溯链找不到提升的 profiles/node_modules（@deepseek-ai/dsh-settings 等官方 peer），
// 插件加载报 ERR_MODULE_NOT_FOUND（dsh-ui-taste 跨机导入实证）；本落点的上溯链命中
// profiles/node_modules，peer 依赖可解析。
func localPluginsRoot() string {
	home := dshHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "profiles", "local-plugins")
}

// legacyLocalPluginsRoot 旧版稳定副本落点（<dshHome>/local-plugins，v0.8.x 及更早；
// 因位于 profiles 工作区树外导致插件无法解析 peer 依赖，见 localPluginsRoot 注释）。
func legacyLocalPluginsRoot() string {
	home := dshHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "local-plugins")
}

// migrateLegacyLocalCopies 把 spec 指向旧落点（<dshHome>/local-plugins/<name>）的稳定副本
// 迁移到新落点（<dshHome>/profiles/local-plugins/<name>）并改写 spec——修复历史版本在旧落点
// 留下的「副本存在但插件加载 ERR_MODULE_NOT_FOUND」状态（副本继续加载的路子本应生效，
// 但旧落点使 peer 依赖不可解析）。迁移为 rename（同盘瞬时完成），失败保守保留原状仅记日志；
// 新落点已有同名副本时只改 spec 指向（不覆盖）。返回用户可见说明行。
func migrateLegacyLocalCopies(dirs []string, noted map[string]bool) []string {
	oldRoot := legacyLocalPluginsRoot()
	root := localPluginsRoot()
	if oldRoot == "" || oldRoot == root {
		return nil
	}
	var notes []string
	for _, dir := range dirs {
		profileRoot := readProfileRoot(dir)
		deps, _ := profileRoot["dependencies"].(map[string]interface{})
		if deps == nil {
			continue
		}
		changed := false
		for name, v := range deps {
			spec, _ := v.(string)
			raw, ok := localSpecPath(spec)
			if !ok {
				continue
			}
			target := raw
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, filepath.FromSlash(target))
			}
			oldCanon := filepath.Join(oldRoot, filepath.FromSlash(name))
			if filepath.Clean(target) != oldCanon {
				continue
			}
			if _, err := os.Stat(oldCanon); err != nil {
				continue // 旧副本已不存在：交给 missingLocalDeps/adopt 或挂起流程
			}
			newCanon := filepath.Join(root, filepath.FromSlash(name))
			if _, err := os.Stat(newCanon); err != nil {
				if err := os.MkdirAll(filepath.Dir(newCanon), 0o755); err != nil {
					log.Printf("migrate local plugin: mkdir %s: %v", filepath.Dir(newCanon), err)
					continue
				}
				if err := moveDirTree(oldCanon, newCanon); err != nil {
					log.Printf("migrate local plugin: move %s -> %s: %v", oldCanon, newCanon, err)
					continue
				}
			}
			deps[name] = localLinkSpec(newCanon)
			changed = true
			if !noted[name] {
				noted[name] = true
				notes = append(notes, fmt.Sprintf(
					"本地插件 %s 的副本已迁移到可加载位置（%s）——旧位置无法解析依赖导致插件加载失败，现已修复",
					name, newCanon))
			}
		}
		if changed {
			if err := writeProfileRoot(dir, profileRoot); err != nil {
				log.Printf("migrate local plugins: write package.json failed (%s): %v", dir, err)
			}
		}
	}
	return notes
}

// missingLocalDeps 枚举 dir 的 package.json 中「本地 spec 且目标路径缺失」的依赖（name → spec）。
func missingLocalDeps(dir string) map[string]string {
	root := readProfileRoot(dir)
	deps, _ := root["dependencies"].(map[string]interface{})
	if deps == nil {
		return nil
	}
	out := map[string]string{}
	for name, v := range deps {
		spec, _ := v.(string)
		raw, ok := localSpecPath(spec)
		if !ok {
			continue
		}
		target := raw
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, filepath.FromSlash(target))
		}
		if _, err := os.Stat(filepath.Clean(target)); err == nil {
			continue // 本机路径存在：同机开发态，保留原 spec
		}
		out[name] = spec
	}
	return out
}

// adoptRestoredCopy 把 dirs 中可用的 node_modules/<name> 恢复副本迁到 <root>/<name> 稳定副本：
//   - 稳定副本不存在 → 取版本最高的副本迁入（oldVer 为空）；
//   - 稳定副本已存在 → 版本比较：任一恢复副本更新才替换（旧目录改名 .dshbak-<ts> 备份，
//     替换失败回退），否则复用旧副本（oldVer 为空，说明无替换）。
//
// 返回稳定副本路径、被替换的旧版本号（未替换为空）、是否就绪。副本/稳定副本缺 package.json
// 版本信息时不比较不替换（保守复用，日志留痕）。local-plugins 不在 .importbak 快照保护内，
// 因此替换自带 .dshbak 备份回退。
func adoptRestoredCopy(dirs []string, root, name string) (string, string, bool) {
	canon := filepath.Join(root, filepath.FromSlash(name))
	type cand struct {
		dir string
		ver string
	}
	var copies []cand
	for _, dir := range dirs {
		copyDir := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
		if _, err := os.Stat(filepath.Join(copyDir, "package.json")); err != nil {
			continue
		}
		copies = append(copies, cand{dir: copyDir, ver: readPkgVersionField(copyDir)})
	}
	if len(copies) == 0 {
		return "", "", false
	}
	if _, err := os.Stat(canon); err != nil {
		// 尚无稳定副本：取版本最高者迁入（同版本取第一个）
		best := copies[0]
		for _, c := range copies[1:] {
			if c.ver != "" && (best.ver == "" || compareVersions("v"+c.ver, "v"+best.ver) > 0) {
				best = c
			}
		}
		if err := os.MkdirAll(filepath.Dir(canon), 0o755); err != nil {
			log.Printf("adopt local plugin: mkdir %s: %v", filepath.Dir(canon), err)
			return "", "", false
		}
		if err := moveDirTree(best.dir, canon); err != nil {
			log.Printf("adopt local plugin: move %s -> %s: %v", best.dir, canon, err)
			return "", "", false
		}
		return canon, "", true
	}
	// 稳定副本已存在：仅当恢复副本版本更新才替换（重复导入场景的核心修复）
	canonVer := readPkgVersionField(canon)
	var best *cand
	for i := range copies {
		c := copies[i]
		if c.ver == "" || canonVer == "" {
			continue
		}
		if compareVersions("v"+c.ver, "v"+canonVer) > 0 &&
			(best == nil || compareVersions("v"+c.ver, "v"+best.ver) > 0) {
			b := c
			best = &b
		}
	}
	if best != nil {
		bak := canon + ".dshbak-" + time.Now().Format("20060102-150405")
		_ = os.RemoveAll(bak)
		if os.Rename(canon, bak) == nil {
			if err := moveDirTree(best.dir, canon); err != nil {
				_ = os.Rename(bak, canon) // 替换失败：恢复旧副本
				log.Printf("adopt local plugin: replace rollback %s: %v", name, err)
				return canon, "", true
			}
			log.Printf("adopt local plugin: replaced stale copy %s v%s -> v%s (backup %s)",
				name, orDash(canonVer), orDash(best.ver), bak)
			return canon, canonVer, true
		}
		log.Printf("adopt local plugin: backup rename failed, reuse existing copy %s", name)
	}
	return canon, "", true
}

// refreshStableLocalCopies 版本感知刷新：profile 依赖 spec 已指向 <dshHome>/local-plugins/<name>
// 的稳定副本（此前导入已 adopt，重复导入时 spec 路径在本机存在，missingLocalDeps 不会命中，
// adoptRestoredCopy 的复用分支也不触发），而本次导入包在 node_modules 恢复出更高版本副本——
// 此时把稳定副本升级为导入包版本（旧副本 .dshbak-<ts> 备份，失败回退），spec 不变。
// 修复「同一导出机器升级后再次导入，新机仍显示旧版本」。同名插件跨目录去重，返回说明行。
func refreshStableLocalCopies(dirs []string, noted map[string]bool) []string {
	root := localPluginsRoot()
	if root == "" {
		return nil
	}
	stale := map[string]string{} // name → 需要刷新的稳定副本路径
	for _, dir := range dirs {
		profileRoot := readProfileRoot(dir)
		deps, _ := profileRoot["dependencies"].(map[string]interface{})
		if deps == nil {
			continue
		}
		for name, v := range deps {
			spec, _ := v.(string)
			raw, ok := localSpecPath(spec)
			if !ok {
				continue
			}
			target := raw
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, filepath.FromSlash(target))
			}
			target = filepath.Clean(target)
			want := filepath.Join(root, filepath.FromSlash(name))
			if target != want {
				continue // 仅刷新指向稳定副本的依赖（开发目录同机恢复不受影响）
			}
			if _, err := os.Stat(target); err != nil {
				continue
			}
			canonVer := readPkgVersionField(target)
			if canonVer == "" {
				continue
			}
			copyVer := readPkgVersionField(filepath.Join(dir, "node_modules", filepath.FromSlash(name)))
			if copyVer != "" && compareVersions("v"+copyVer, "v"+canonVer) > 0 {
				stale[name] = target
			}
		}
	}
	if len(stale) == 0 {
		return nil
	}
	var notes []string
	for name, canon := range stale {
		// 用版本最高的恢复副本刷新
		bestCopy, bestVer := "", ""
		for _, dir := range dirs {
			copyDir := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
			v := readPkgVersionField(copyDir)
			if v == "" {
				continue
			}
			if bestVer == "" || compareVersions("v"+v, "v"+bestVer) > 0 {
				bestCopy, bestVer = copyDir, v
			}
		}
		if bestCopy == "" {
			continue
		}
		oldVer := readPkgVersionField(canon)
		bak := canon + ".dshbak-" + time.Now().Format("20060102-150405")
		_ = os.RemoveAll(bak)
		if os.Rename(canon, bak) != nil {
			log.Printf("refresh local plugin: backup rename failed %s", canon)
			continue
		}
		if err := moveDirTree(bestCopy, canon); err != nil {
			_ = os.Rename(bak, canon) // 回退
			log.Printf("refresh local plugin: replace rollback %s: %v", name, err)
			continue
		}
		pruneRestoredCopies(dirs, name)
		log.Printf("refresh local plugin: %s v%s -> v%s (backup %s)", name, orDash(oldVer), bestVer, bak)
		if !noted[name] {
			noted[name] = true
			notes = append(notes, fmt.Sprintf(
				"本地插件 %s 已从导入包更新到新版本 v%s（原 v%s 已备份）", name, bestVer, orDash(oldVer)))
		}
	}
	return notes
}

// pruneRestoredCopies 副本裁决完成后清理各 profile node_modules 中的同名残留副本，
// 让后续 pnpm install 在 node_modules/<name> 处创建指向稳定副本的 link（真实目录挡路会冲突）。
func pruneRestoredCopies(dirs []string, name string) {
	rel := filepath.Join("node_modules", filepath.FromSlash(name))
	for _, dir := range dirs {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err == nil {
			if err := os.RemoveAll(p); err != nil {
				log.Printf("prune restored copy %s: %v", p, err)
			}
		}
	}
}

// moveDirTree 移动目录（同盘 rename 直接成功；跨盘失败时递归复制后删除源）。
func moveDirTree(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyDirTree(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// copyDirTree 递归复制目录（文件级；插件目录来自 zip 解压，不含符号链接/特殊文件）。
func copyDirTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// rewriteProfileLocalDeps 按裁决结果改写单个 profile 的 package.json：
//   - adopted 含该插件 → spec 改写为 link:<稳定副本>（bundle 保持原样），并清除历史挂起记录；
//   - 否则 → 移出 dependencies、清理 bundle/禁用记录、记录待重指定（原挂起逻辑）。
//
// 返回该目录产生的说明行；同名插件跨目录去重（noted 为共享集合，避免重复打扰用户）。
func rewriteProfileLocalDeps(dir string, missing map[string]string, adopted map[string]string, noted map[string]bool) []string {
	root := readProfileRoot(dir)
	deps, _ := root["dependencies"].(map[string]interface{})
	if deps == nil {
		return nil
	}
	var notes []string
	changed := false
	names := make([]string, 0, len(deps))
	for n := range deps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, isMissing := missing[name]; !isMissing {
			continue
		}
		spec, _ := deps[name].(string)
		if canon, ok := adopted[name]; ok {
			deps[name] = localLinkSpec(canon)
			// 历史挂起记录随激活一并清除（记录与激活必须一致）
			if dsh, _ := root["dsh"].(map[string]interface{}); dsh != nil {
				if prof, _ := dsh["profile"].(map[string]interface{}); prof != nil {
					if pm, _ := prof[pendingLocalKey].(map[string]interface{}); pm != nil {
						if _, had := pm[name]; had {
							delete(pm, name)
							if len(pm) == 0 {
								delete(prof, pendingLocalKey)
							}
						}
					}
				}
			}
			changed = true
			if !noted[name] {
				noted[name] = true
				notes = append(notes, fmt.Sprintf(
					"本地插件 %s 的原依赖路径在本机不可用，已改用导入包内副本继续加载（%s）——如需改用你的开发目录，可点「更新…」重新指定",
					name, canon))
			}
			continue
		}
		// 无副本：挂起待重指定（见文件头注释——不改为 npm、不删除）
		bundled := bundleEntryExists(root, name)
		stripBundleEntry(root, name)
		if dm := profileDisabledMap(root); dm != nil {
			if _, had := dm[name]; had {
				delete(dm, name)
				_, prof := profileSection(root)
				if len(dm) == 0 {
					delete(prof, "disabledPlugins")
				} else {
					prof["disabledPlugins"] = dm
				}
			}
		}
		delete(deps, name)
		setPendingLocalEntry(root, name, spec, bundled)
		changed = true
		if !noted[name] {
			noted[name] = true
			notes = append(notes, fmt.Sprintf(
				"本地插件 %s 的原依赖路径在本机不可用（隐私起见不显示），已保留为待重指定状态——请点「更新…」重新选择本地目录", name))
		}
	}
	if !changed {
		return notes
	}
	if err := writeProfileRoot(dir, root); err != nil {
		log.Printf("sanitize local deps: write package.json failed (%s): %v", dir, err)
	}
	return notes
}

// stripBundleEntry 从 package.json 的 dsh.profile.bundles 中移除 name（含 name@… 变体）。
// 依赖被移除时若 bundle 仍声明该插件，服务启动会因「无法解析 bundle」失败——必须一并清理。
func stripBundleEntry(root map[string]interface{}, name string) {
	dsh, _ := root["dsh"].(map[string]interface{})
	if dsh == nil {
		return
	}
	prof, _ := dsh["profile"].(map[string]interface{})
	if prof == nil {
		return
	}
	bundles, _ := prof["bundles"].([]interface{})
	out := make([]interface{}, 0, len(bundles))
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
			continue
		}
		out = append(out, b)
	}
	if len(out) != len(bundles) {
		prof["bundles"] = out
	}
}

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
func restoreItem(kind, zipPath, filesDest string, overwrite bool, onStatus func(text string, pct float64), stop ...func() bool) (string, error) {
	var stopFn func() bool
	if len(stop) > 0 {
		stopFn = stop[0]
	}
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
	if stopFn != nil && stopFn() {
		for o, b := range backups {
			_ = os.Rename(b, o) // 取消时先还原已改名备份
		}
		return "", errRestoreCanceled
	}

	err := zipExtractStop(zipPath, dest, overwrite, stopFn)
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

// ==================== 导入事务日志（重启自愈） ====================
// importJournal 导入事务阶段日志：进程在恢复任务中途被杀/窗口被关时，重启据此决定
// 回退（importing：解压/对齐阶段中断，改动可安全回退）或续跑收尾自愈（healing：
// 已进入自愈、必须续到确定结果）——见 main.go recoverInterruptedImport。
type importJournal struct {
	Stage string   `json:"stage"` // importing | healing
	Kind  string   `json:"kind"`
	Dirs  []string `json:"dirs"`
	HadNM []bool   `json:"hadNM"`
}

// importJournalDirOverride 测试注入：替代 config 目录（单测自包含，不触碰真实用户配置目录）。
var importJournalDirOverride string

// importJournalPath 事务日志文件（与 config.json 同目录；测试可注入覆盖）。
func importJournalPath() string {
	if importJournalDirOverride != "" {
		return filepath.Join(importJournalDirOverride, "import-journal.json")
	}
	if p := configFilePath(); p != "" {
		return filepath.Join(filepath.Dir(p), "import-journal.json")
	}
	return ""
}

// writeImportJournal 原子写入事务日志（临时文件 + rename）。
func writeImportJournal(j importJournal) error {
	p := importJournalPath()
	if p == "" {
		return fmt.Errorf("no config dir for import journal")
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// readImportJournal 读取事务日志（不存在/损坏返回错误，调用方按无日志处理）。
func readImportJournal() (*importJournal, error) {
	p := importJournalPath()
	if p == "" {
		return nil, fmt.Errorf("no config dir for import journal")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var j importJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// clearImportJournal 删除事务日志（恢复任务各终局路径收尾时调用）。
func clearImportJournal() {
	if p := importJournalPath(); p != "" {
		_ = os.Remove(p)
	}
}

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
// 还原完成后删除快照副本（package.json/pnpm-lock 的 .importbak）：若残留，重启自愈扫描
// （recoverInterruptedImport）会把已回退完成的目录误判为「中断的导入」。
func rollbackImportProfiles(dirs []string, had []bool) {
	for i, dir := range dirs {
		for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
			bak := filepath.Join(dir, name+importBakSuffix)
			if data, err := os.ReadFile(bak); err == nil {
				_ = os.WriteFile(filepath.Join(dir, name), data, 0o644)
				_ = os.Remove(bak)
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

// restoredPluginNames 从导入包 manifest 取本次恢复覆盖的插件名（dependencies 与 bundles 的
// 并集，兼容 name@version 变体），供「按最后操作生效」对账待应用变更。
func restoredPluginNames(masterZipPath string) []string {
	data, err := zipReadFile(masterZipPath, "manifest.json")
	if err != nil {
		return nil
	}
	var m exportManifest
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	set := map[string]bool{}
	add := func(s string) {
		if s == "" {
			return
		}
		base := s
		if i := strings.IndexByte(base, '@'); i > 0 {
			base = base[:i]
		}
		if !isOfficialHarnessPkg(base) {
			set[base] = true
		}
	}
	for name := range m.Plugins.Dependencies {
		add(name)
	}
	for _, b := range m.Plugins.Bundles {
		add(b)
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// reconcilePendingAfterImport 导入恢复成功后对账待应用变更：本次恢复到的插件，其先前登记的
// 更新/删除一律作废（按最后操作生效——重新导入是该插件最新的用户意图；典型场景：先登记删除、
// 随后又把它导回来，则不应再删）。返回提示文案（空=无变更作废）。
func reconcilePendingAfterImport(masterZipPath string) string {
	dropped := pluginPendingDropRestored(restoredPluginNames(masterZipPath))
	if len(dropped) == 0 {
		return ""
	}
	return "已按最后一次操作作废先前的待应用变更（重新导入生效）：" + strings.Join(dropped, "、")
}

// finishPluginImport 插件导入收尾：拉起服务并健康校验（restartAndVerifyHealing，
// 失败自动 pnpm 对齐重试一次）；仍失败且启动日志点名用户插件时，自动禁用这些插件换取
// 「导入保留 + 服务可启动」（不兼容自愈，而非整体回退导入）；禁用后仍失败才回退导入前快照。
// 本阶段为不可中断的启动自愈（CancelRestore 忽略请求）：取消只对之前的解压/对齐有效。
// 返回 (note, error)：note 为成功路径的附加说明（如被自动禁用的插件），error 失败时
// 为面向用户的说明文案（含可疑插件与回退结果）。
func finishPluginImport(dirs []string, hadNM []bool) (string, error) {
	// 确定性预检（对齐之后、拉起服务之前）：逐个 import 插件入口，把「启动必然失败」提前成
	// 「精确点名 + 自动禁用」。不依赖启动日志时序——2026-09-10 实证：导入 16:54:20 报
	// `plugins restored and service verified healthy`，16:54:28 进程即死于
	// `does not provide an export named 'assertNever'`（日志健康窗口早于加载错误数秒关闭）。
	pfDisabled, pfNotes := preflightTreeCompatibility(dirs)
	pfNote := strings.Join(pfNotes, "；")
	if len(pfDisabled) > 0 {
		log.Printf("import: preflight disabled incompatible plugins: %s", strings.Join(pfDisabled, "、"))
	}
	healthy, suspects := restartAndVerifyHealing(dirs)
	if healthy {
		promoteImportProfilesToLkg(dirs)
		cleanupImportProfiles(dirs)
		markServiceResumed()
		log.Printf("import: plugins restored and service verified healthy")
		// 按最后操作生效：本次恢复到的插件，其先前登记的待应用变更（更新/删除）一并作废
		return appendNote(pfNote, reconcilePendingAfterImport(importZipPath)), nil
	}
	reason := "启动日志存在加载错误（版本/插件不兼容）"
	if len(suspects) > 0 {
		reason += "（疑似插件：" + strings.Join(suspects, "、") + "）"
	}
	// 不兼容自愈：禁用点名用户插件后重启，健康则保留本次导入（这些插件记为禁用、可后续更新/启用）。
	// 点名禁用未奏效或无点名嫌疑时，按用户决策（尽量不回退导入与当前版本）禁用全部已激活的
	// 用户插件再试；仍失败才回退导入前快照。
	disabled, ok := disableBootSuspects(dirs)
	allDisabled := false
	if !ok {
		disabled, ok = disableAllUserPlugins(dirs)
		allDisabled = ok
	}
	if ok {
		promoteImportProfilesToLkg(dirs)
		cleanupImportProfiles(dirs)
		markServiceResumed()
		names := make([]string, 0, len(disabled))
		for _, d := range disabled {
			names = append(names, d.Name)
		}
		log.Printf("import: plugins restored with incompatible ones disabled: %s", strings.Join(names, "、"))
		if allDisabled {
			return appendNote(appendNote(pfNote, "已恢复导入，但服务启动失败且未能定位具体的不兼容插件，已自动禁用全部已激活的用户插件以保证服务启动"+
				"（保留记录，可在关于页逐个检查更新或重新启用）："+strings.Join(names, "、")), reconcilePendingAfterImport(importZipPath)), nil
		}
		return appendNote(appendNote(pfNote, "已恢复导入，但以下插件与当前版本不兼容，已自动禁用（可在关于页检查更新，或确认修复后点击「启用」重试）："+
			strings.Join(names, "、")), reconcilePendingAfterImport(importZipPath)), nil
	}
	log.Printf("import: service not healthy after restore heal (%s), rolling back", reason)
	killServer()
	time.Sleep(1 * time.Second)
	rollbackImportProfiles(dirs, hadNM)
	if !restartAndVerifyServer() {
		return "", fmt.Errorf("导入的插件导致服务启动失败（%s）。已回退导入内容，但回退后的服务仍未能就绪，请查看日志：%s", reason, unifiedLogPath())
	}
	markServiceResumed()
	return "", fmt.Errorf("导入的插件导致服务启动失败（%s）。已自动回退到导入前状态，服务已恢复正常，本次导入未生效。", reason)
}

// markServiceResumed 服务已就绪并刷新托盘菜单（与 resumeServiceAfterRestore 的状态口径一致）。
func markServiceResumed() {
	serverReady.Store(true)
	serviceFailed.Store(false)
	refreshServiceMenu()
}

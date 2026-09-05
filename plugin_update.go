package main

// ==================== 插件检查 / 更新（设置页「插件」卡片） ====================
// 罗列用户通过 dsh add / dsh plugin --profile <p> add 安装到各 profile 的用户插件
// （profile package.json dependencies 中非 @deepseek-ai/* 的条目），每个插件单独检查更新：
//   - npm registry 安装（如 ^1.5.42）→ 查 registry dist-tag latest；
//   - GitHub 安装（github:owner/repo / git+https://… / owner/repo 简写）→ 按默认分支
//     package.json 的 version 判定（决策：不跟随安装时指定的 #branch/#tag，统一以默认分支为准）；
//   - file:/link:/tarball URL 等无远程版本来源 → 不可更新，按钮禁用并附小号原因文字。
// 更新在对应 profile 目录内执行 pnpm add（npm 类装 @latest；github 类重解析原 spec），
// 成功后重启后台服务并做健康校验，失败自动回退到更新前的 profile 快照。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// pluginRegistries npm registry 查询候选：先官方，失败回退国内镜像（与 GitHub 镜像策略同理）。
var pluginRegistries = []string{
	"https://registry.npmjs.org",
	"https://registry.npmmirror.com",
}

// pluginCheckDeadline 单插件一次检查的最长耗时（多个候选源共用该预算，超时即报错返回）。
const pluginCheckDeadline = 15 * time.Second

// installRegistry 探测/缓存安装用 registry：官方可达用官方，不可达自动切 npmmirror——
// 坏网络/被墙时 pnpm install 不再因 registry.npmjs.org error(23) 整体失败
// （new_device.log 实证 deepseek-idesign-0.2.2.tgz 反复下载失败）。
var (
	installRegistryMu   sync.Mutex
	installRegistryDone bool
	installRegistryVal  = "https://registry.npmjs.org"
)

func installRegistry() string {
	installRegistryMu.Lock()
	defer installRegistryMu.Unlock()
	if installRegistryDone {
		return installRegistryVal
	}
	installRegistryDone = true
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if req, err := http.NewRequestWithContext(ctx, "GET",
		"https://registry.npmjs.org/@deepseek-ai%2fdsh/latest", nil); err == nil {
		req.Header.Set("User-Agent", "dsh-systray/"+appVersion)
		if resp, rerr := http.DefaultClient.Do(req); rerr == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				installRegistryVal = "https://registry.npmjs.org"
				return installRegistryVal
			}
		}
	}
	installRegistryVal = "https://registry.npmmirror.com"
	return installRegistryVal
}

// PluginRow 插件列表中一个用户插件的展示与动作信息。
type PluginRow struct {
	ID             string   `json:"id"`             // 前端行的稳定标识（同名多 spec 场景带 spec 区分）
	Name           string   `json:"name"`           // 包名
	Version        string   `json:"version"`        // 当前已安装版本（node_modules 中 package.json；缺失为空）
	Spec           string   `json:"spec"`           // 原始依赖 spec（如 ^0.2.2 / github:refyon/xxx / file:…）
	Source         string   `json:"source"`         // npm | github | file | tarball | unknown
	Profile        string   `json:"profile"`        // 声明该插件的 profile（多环境时顿号分隔；空=旧布局）
	Locs           []string `json:"-"`              // 声明该插件的各 profile 目录（更新/回滚对象，内部使用，不外发）
	CanUpdate      bool     `json:"canUpdate"`      // 是否有远程来源、可执行检查与更新
	Reason         string   `json:"reason"`         // 不可更新的原因说明（canUpdate=false 时展示）
	LocalDir       string   `json:"localDir"`       // 本地插件当前生效的本地路径（仅 file 来源且已重指定时展示；为空不显示）
	PendingLocal   bool     `json:"pendingLocal"`   // 本地插件「待重指定」：原依赖路径在本机不存在，等用户点「更新…」重新指定
	GhostDisabled  bool     `json:"ghostDisabled"`  // 「已自动禁用且无依赖声明」：自愈禁用后保留展示，可删除/重装，不可直接启用
	Disabled       bool     `json:"disabled"`       // 是否处于禁用状态（不兼容自愈：不在 bundles 激活清单）
	DisabledReason string   `json:"disabledReason"` // 禁用原因（启动日志错误摘要）
}

// PluginCheckResult 单个插件的检查结果。
type PluginCheckResult struct {
	Name      string `json:"name"`
	Current   string `json:"current"` // 当前已装版本
	Latest    string `json:"latest"`  // 远端最新版本（获取失败为空）
	HasUpdate bool   `json:"hasUpdate"`
	Error     string `json:"error"` // 检查失败原因（网络/解析）
}

// pluginProfile 一个含 package.json 的 profile 目录。
type pluginProfile struct {
	dir   string // 目录绝对路径
	label string // 展示名：命名 profile 用目录名；旧布局（profiles 根）为空
}

// enumeratePluginProfiles 罗列全部含 package.json 的 profile 目录（命名 profile + 旧布局根），
// 口径与 removeInstalledPlugins / countInstalledPlugins 一致。命名 profile 按名称排序，旧布局最后。
func enumeratePluginProfiles() []pluginProfile {
	home := dshHomeDir()
	if home == "" {
		return nil
	}
	profilesRoot := filepath.Join(home, "profiles")
	var out []pluginProfile
	if ents, err := os.ReadDir(profilesRoot); err == nil {
		var names []string
		for _, e := range ents {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			p := filepath.Join(profilesRoot, n, "package.json")
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				out = append(out, pluginProfile{dir: filepath.Join(profilesRoot, n), label: n})
			}
		}
	}
	// 兼容旧布局：profiles/package.json（profiles 本身即 profile 根）
	if st, err := os.Stat(filepath.Join(profilesRoot, "package.json")); err == nil && !st.IsDir() {
		out = append(out, pluginProfile{dir: profilesRoot, label: ""})
	}
	return out
}

// installedPluginVersion 读取某 profile 中已安装插件包的版本号（node_modules/<pkg>/package.json）。
// 兼容 scoped 包（node_modules/@scope/name）与 pnpm 符号链接（ReadFile 自动跟随）。
func installedPluginVersion(profileDir, pkgName string) string {
	rel := filepath.Join("node_modules", filepath.FromSlash(pkgName))
	data, err := os.ReadFile(filepath.Join(profileDir, rel, "package.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &m) != nil || m.Version == "" {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(m.Version), "v")
}

// ---- 依赖 spec 来源分类 ----

// specGitHubRe GitHub 依赖 spec 的显式形态：github: / git+https://github.com / git@github.com / git://。
var specGitHubRe = regexp.MustCompile(`(?i)^(?:github:|git\+https?://github\.com/|git@github\.com:|git://github\.com/)([^/#]+)/([^/#]+?)(?:\.git)?(?:#[^ ]*)?$`)

// specGitSSHRe git+ssh://git@github.com:user/repo.git 形态。
var specGitSSHRe = regexp.MustCompile(`(?i)^git\+ssh://git@github\.com[:/]([^/#]+)/([^/#]+?)(?:\.git)?(?:#[^ ]*)?$`)

// specGitHubShorthandRe owner/repo 简写（npm/pnpm 语义：非 scoped、不含版本运算符）。
var specGitHubShorthandRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// classifyPluginSpec 根据依赖 spec 判定插件来源与是否可远程更新。返回 (source, canUpdate, reason)。
//   - file：file:/link:/workspace:/本地路径 → 无远程来源；
//   - tarball：http(s) 压缩包固定地址；
//   - npm：registry 版本/标签/范围（含 scoped、npm: 别名按不可更新处理——语义复杂，建议重装）；
//   - github：github:/git+https/git@/owner/repo 简写等 GitHub 形态（可重解析默认分支）；
//   - unknown：其余不可识别形态（如非 GitHub 的 git 源）。
func classifyPluginSpec(spec string) (source string, canUpdate bool, reason string) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "unknown", false, "依赖声明为空，无法检查更新"
	}
	low := strings.ToLower(spec)
	switch {
	case strings.HasPrefix(low, "file:"), strings.HasPrefix(low, "link:"), strings.HasPrefix(low, "workspace:"):
		return "file", false, "本地路径安装，无远程来源，无法更新"
	case strings.HasPrefix(low, "npm:"):
		return "npm", false, "npm 别名安装（npm:…），请先卸载后重新安装以获取更新"
	case specGitHubRe.MatchString(spec), specGitSSHRe.MatchString(spec),
		(!strings.HasPrefix(spec, "@") && specGitHubShorthandRe.MatchString(spec)):
		return "github", true, ""
	case strings.HasPrefix(low, "http://"), strings.HasPrefix(low, "https://"):
		if isTarballSpec(spec) {
			return "tarball", false, "以固定压缩包地址安装，无法判断更新"
		}
		return "unknown", false, "以外部链接安装，无法检查更新"
	case strings.Contains(low, "git+"):
		return "unknown", false, "非 GitHub 的 git 来源，无法检查更新"
	default:
		return "npm", true, "" // registry 版本/标签/范围写法（含 ^、~、x、*、latest 等）
	}
}

// isTarballSpec 是否指向压缩包（.zip/.tgz/.tar.gz 等）。
func isTarballSpec(spec string) bool {
	low := strings.ToLower(spec)
	for _, ext := range []string{".zip", ".tgz", ".tar.gz", ".tar.bz2", ".tar.xz"} {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	return false
}

// githubSpecParts 提取 github spec 的 owner/repo（决策：版本判定以默认分支为准，忽略 #branch）。
func githubSpecParts(spec string) (owner, repo string, ok bool) {
	if m := specGitHubRe.FindStringSubmatch(spec); m != nil {
		return m[1], m[2], true
	}
	if m := specGitSSHRe.FindStringSubmatch(spec); m != nil {
		return m[1], m[2], true
	}
	if specGitHubShorthandRe.MatchString(spec) {
		i := strings.IndexByte(spec, '/')
		return spec[:i], spec[i+1:], true
	}
	return "", "", false
}

// buildPluginRows 枚举所有 profile 的用户插件并组装展示行。
// 同一包名 + 同一 spec 出现在多个 profile 时合并为一行（locs 收集全部目录，更新时逐一执行）；
// 同一包名存在不同 spec（极少见）时分行展示，行 ID 带 spec 区分。
func buildPluginRows() []PluginRow {
	profiles := enumeratePluginProfiles()
	if len(profiles) == 0 {
		return nil
	}
	// 分组：包名 -> spec 组（spec 值 -> 声明目录列表）。待重指定（pendingLocal）记录与依赖
	// 走同一分组（其 spec 为原始本地 spec），行级差异用 pendingKeys 标记、建行时覆盖。
	type specGroup struct {
		spec string
		locs []string
	}
	groups := map[string][]*specGroup{}
	var order []string               // 首次出现的包名顺序（与展示排序解耦，纯 key 记录）
	pendingKeys := map[string]bool{} // name+"\x00"+spec → 该组来自待重指定记录
	// addDecl 把 (name, spec) 声明加入对应 spec 组（跨 profile 同名同 spec 合并 locs）
	addDecl := func(name, spec, dir string) {
		if _, exists := groups[name]; !exists {
			order = append(order, name)
		}
		for _, sg := range groups[name] {
			if sg.spec == spec {
				sg.locs = append(sg.locs, dir)
				return
			}
		}
		g := &specGroup{spec: spec}
		g.locs = append(g.locs, dir)
		groups[name] = append(groups[name], g)
	}
	for _, pf := range profiles {
		data, err := os.ReadFile(filepath.Join(pf.dir, "package.json"))
		if err != nil {
			continue
		}
		var m struct {
			Dependencies map[string]string `json:"dependencies"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		names := make([]string, 0, len(m.Dependencies))
		for n := range m.Dependencies {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			if name == "" || isOfficialHarnessPkg(name) {
				continue // @deepseek-ai/* 为官方自带，非用户插件
			}
			addDecl(name, strings.TrimSpace(m.Dependencies[name]), pf.dir)
		}
		// 待重指定记录（恢复时原依赖路径在本机缺失、从 dependencies 移出）同样合成插件行：
		// 保持 source=file 的「更新…」入口，供用户重新指定本地目录。
		pends := listPendingLocalEntries(pf.dir)
		if len(pends) > 0 {
			declared := map[string]bool{}
			for n := range m.Dependencies {
				declared[n] = true
			}
			var pnames []string
			for n := range pends {
				if !declared[n] {
					pnames = append(pnames, n)
				}
			}
			sort.Strings(pnames)
			for _, name := range pnames {
				if name == "" || isOfficialHarnessPkg(name) {
					continue
				}
				spec := strings.TrimSpace(pends[name].spec)
				if spec == "" {
					continue
				}
				addDecl(name, spec, pf.dir)
				pendingKeys[name+"\x00"+spec] = true
			}
		}
	}
	var rows []PluginRow
	for _, name := range order {
		for _, g := range groups[name] {
			if len(g.locs) == 0 {
				continue
			}
			source, canUpdate, reason := classifyPluginSpec(g.spec)
			// 已装版本以第一个目录为准（多目录同名同 spec 版本应一致）
			ver := installedPluginVersion(g.locs[0], name)
			labels := make([]string, 0, len(g.locs))
			labelSet := map[string]bool{}
			for _, d := range g.locs {
				if l := profileLabelOf(d); l != "" && !labelSet[l] {
					labels = append(labels, l)
					labelSet[l] = true
				}
			}
			id := name
			if len(groups[name]) > 1 {
				id = fmt.Sprintf("%s|%s", name, g.spec) // 同名多 spec 时用 spec 区分行
			}
			rows = append(rows, PluginRow{
				ID:        id,
				Name:      name,
				Version:   ver,
				Spec:      g.spec,
				Source:    source,
				Profile:   strings.Join(labels, "、"),
				Locs:      append([]string(nil), g.locs...),
				CanUpdate: canUpdate,
				Reason:    reason,
			})
			if pendingKeys[name+"\x00"+g.spec] {
				// 待重指定行：固定按本地来源展示（可用「更新…」重指定目录）；
				// 原因不给原路径（跨机路径含隐私，待用户重指定后才展示所选本地路径）。
				rows[len(rows)-1].Source = "file"
				rows[len(rows)-1].CanUpdate = false
				rows[len(rows)-1].Reason = "原依赖路径在本机不可用（已隐藏），请点「更新…」重新选择本地目录"
				rows[len(rows)-1].PendingLocal = true
				rows[len(rows)-1].LocalDir = ""
			} else if source == "file" {
				// 本地（file/link/workspace 等）来源且路径可用：仅展示当前生效路径
				//（用户重指定后即为此处用户所选目录；不暴露历史旧路径）。
				if p, ok := localSpecPath(g.spec); ok && strings.TrimSpace(p) != "" {
					rows[len(rows)-1].LocalDir = filepath.Clean(p)
				}
			}
			if dis, disReason := pluginDisabledAcross(g.locs, name); dis {
				rows[len(rows)-1].Disabled = true
				rows[len(rows)-1].DisabledReason = disReason
			}
		}
	}
	// 「已自动禁用且无依赖声明」行：不兼容自愈对无依赖行的插件（如历史残留 bundle、
	// 被自动禁用）只记录 disabledPlugins 原因并摘除激活，不补依赖（避免 pnpm 重新下载失败）。
	// 这里把这类记录合成为关于页可见的「已禁用」行（可删除/重装；无依赖故不可直接启用）。
	rowNames := map[string]bool{}
	for _, r := range rows {
		rowNames[r.Name] = true
	}
	type ghost struct {
		dirs []string
		why  string
	}
	ghosts := map[string]*ghost{}
	for _, pf := range profiles {
		root := readProfileRoot(pf.dir)
		dm := profileDisabledMap(root)
		if len(dm) == 0 {
			continue
		}
		names := make([]string, 0, len(dm))
		for n := range dm {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if n == "" || isOfficialHarnessPkg(n) || rowNames[n] {
				continue
			}
			g := ghosts[n]
			if g == nil {
				g = &ghost{}
				ghosts[n] = g
			}
			g.dirs = append(g.dirs, pf.dir)
			if g.why == "" {
				if s, ok := dm[n].(string); ok {
					g.why = s
				}
			}
		}
	}
	if len(ghosts) > 0 {
		gkeys := make([]string, 0, len(ghosts))
		for n := range ghosts {
			gkeys = append(gkeys, n)
		}
		sort.Strings(gkeys)
		for _, n := range gkeys {
			g := ghosts[n]
			labels := map[string]bool{}
			var ls []string
			for _, d := range g.dirs {
				if l := profileLabelOf(d); l != "" && !labels[l] {
					labels[l] = true
					ls = append(ls, l)
				}
			}
			rows = append(rows, PluginRow{
				ID:             n,
				Name:           n,
				Version:        installedPluginVersion(g.dirs[0], n),
				Source:         "npm",
				Profile:        strings.Join(ls, "、"),
				Locs:           append([]string(nil), g.dirs...),
				CanUpdate:      false,
				Reason:         "已自动禁用：其依赖的组件版本不满足该插件所需 API",
				GhostDisabled:  true,
				Disabled:       true,
				DisabledReason: g.why,
			})
		}
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Name < rows[b].Name })
	return rows
}

// profileLabelOf 目录的展示名：位于 profiles/<name> 时返回 <name>，旧布局根返回空。
func profileLabelOf(dir string) string {
	home := dshHomeDir()
	if home == "" {
		return ""
	}
	rel, err := filepath.Rel(filepath.Join(home, "profiles"), dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	if i := strings.IndexByte(rel, filepath.Separator); i > 0 {
		return rel[:i]
	}
	return rel
}

// findPluginRowByID 按行 ID（或包名）查找插件行（含全部声明目录）。
func findPluginRowByID(id string) (PluginRow, bool) {
	for _, r := range buildPluginRows() {
		if r.ID == id || r.Name == id {
			return r, true
		}
	}
	return PluginRow{}, false
}

// ---- 网络查询 ----

// getWithMirrors 带多候选（直连 + 镜像前缀）的 GET，把候选 URL 依次请求直至成功。
// deadline 为整体预算（上下文超时，逐候选共享），避免镜像全挂时长时间卡 UI。
func getWithMirrors(candidates []string, deadline time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	client := &http.Client{}
	var lastErr error
	for _, u := range candidates {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "dsh-systray/"+appVersion)
		req.Header.Set("Accept", "application/vnd.github+json, application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, updateMaxBodySize))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return body, nil
	}
	return nil, lastErr
}

// mirrorCandidates 把原始 URL 扩展为「直连 + 各镜像前缀」候选列表。
func mirrorCandidates(rawURL string) []string {
	out := []string{rawURL}
	for _, m := range buildMirrors() {
		if m != "" {
			out = append(out, m+rawURL)
		}
	}
	return out
}

// npmRegistryPath 转义包名（scoped：@scope/name → @scope%2fname）。
func npmRegistryPath(name string) string {
	return strings.ReplaceAll(name, "/", "%2f")
}

// fetchNpmLatestWithSource 查询 npm dist-tag latest 版本，并返回应答的 registry 地址
// （更新安装时显式指定同一 registry，保证「检查到的版本」与「安装到的版本」同源）。
func fetchNpmLatestWithSource(name string) (ver, registry string, err error) {
	for _, reg := range pluginRegistries {
		body, e := getWithMirrors([]string{reg + "/" + npmRegistryPath(name) + "/latest"}, pluginCheckDeadline)
		if e != nil {
			continue
		}
		var m struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(body, &m) != nil || m.Version == "" {
			continue
		}
		return strings.TrimPrefix(strings.TrimSpace(m.Version), "v"), reg, nil
	}
	return "", "", fmt.Errorf("npm registry 查询失败")
}

// fetchNpmLatestVersion 查询 npm registry 的 dist-tag latest 版本。
func fetchNpmLatestVersion(name string) (string, error) {
	ver, _, err := fetchNpmLatestWithSource(name)
	return ver, err
}

// ==================== 依赖版本预检（pnpm 对齐前） ====================
// 目的：无效版本/不存在的包应在请求 npm 下载**之前**被发现并摘除，避免 pnpm 下载失败
// 拖慢甚至中断恢复（如 deepseek-idesign-0.2.2.tgz 下载失败反复重试）。网络整体不可达时
// 不误删（交给对齐失败归因兜底）。

// plainVerRe 纯版本号 spec（v1.2.3 / 1.2.3，无 ^ ~ 等运算符）——可做版本端点精确校验。
var plainVerRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+([-.][0-9A-Za-z.-]+)?$`)

// npmRangeExactRe 带单一基础版本的 ^/~ 形态（如 ^0.2.2）——只做包存在性预检，
// 不做版本成员校验（避免基准版缺失但更高版本满足时误删）。
var npmRangeExactRe = regexp.MustCompile(`^[\^~]\s*v?\d+\.\d+\.\d+([-.][0-9A-Za-z.-]+)?$`)

// preflightNpmDepSpec 该依赖是否值得做 registry 预检：跳过本地/链接/git/http/npm: 别名
// 与官方包（非 registry 语义不预检）。
func preflightNpmDepSpec(name, spec string) bool {
	if name == "" || isOfficialHarnessPkg(name) {
		return false
	}
	low := strings.ToLower(strings.TrimSpace(spec))
	for _, p := range []string{"file:", "link:", "workspace:", "npm:", "http://", "https://", "github:", "git+", "git@"} {
		if strings.HasPrefix(low, p) {
			return false
		}
	}
	return true
}

// npmDocVersions 从 registry 元数据应答解析 versions 集合（去掉 v 前缀）。
func npmDocVersions(body []byte) map[string]bool {
	var m struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	out := map[string]bool{}
	for v := range m.Versions {
		out[strings.TrimPrefix(strings.TrimSpace(v), "v")] = true
	}
	return out
}

// registryPreflightGET 依次尝试 registry 候选 URL；返回首个 200 应答；全部失败时返回
// lastStatus（最后一个 HTTP 状态，0=纯网络错误）与聚合错误。
func registryPreflightGET(candidates []string) (body []byte, status int, err error) {
	lastErr := fmt.Errorf("无候选 registry")
	for _, u := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, rerr := http.NewRequestWithContext(ctx, "GET", u, nil)
		if rerr == nil {
			req.Header.Set("User-Agent", "dsh-systray/"+appVersion)
			var resp *http.Response
			resp, rerr = http.DefaultClient.Do(req)
			if rerr == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
				resp.Body.Close()
				status = resp.StatusCode
				if resp.StatusCode == http.StatusOK {
					cancel()
					return b, 200, nil
				}
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			} else {
				lastErr = rerr // 网络错误
			}
		} else {
			lastErr = rerr
		}
		cancel()
	}
	return nil, status, lastErr
}

// preflightProfileDeps 对 dir 的 profile 用户依赖做 registry 预检：
// 包不存在或精确 spec 的版本缺失（所有 registry 候选均非 200 且至少有一个 HTTP 应答）→
// 摘除依赖（dropProfileDependency 同步清理 bundle）并返回原因。网络整体不可达返回空。
// 返回 map[依赖名]原因。
func preflightProfileDeps(dir string) map[string]string {
	pj := filepath.Join(dir, "package.json")
	data, err := os.ReadFile(pj)
	if err != nil {
		return nil
	}
	var m struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if json.Unmarshal(data, &m) != nil || len(m.Dependencies) == 0 {
		return nil
	}
	type cand struct {
		name  string
		spec  string
		exact bool // true=纯版本号，做版本端点精确校验；false=仅包存在性校验
	}
	var cands []cand
	seen := map[string]bool{}
	names := make([]string, 0, len(m.Dependencies))
	for n := range m.Dependencies {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := strings.TrimSpace(m.Dependencies[name])
		if seen[name] || !preflightNpmDepSpec(name, spec) {
			continue
		}
		seen[name] = true
		cands = append(cands, cand{name: name, spec: spec, exact: plainVerRe.MatchString(spec)})
	}
	if len(cands) == 0 {
		return nil
	}
	dropped := map[string]string{}
	for _, c := range cands {
		base := strings.TrimSpace(c.spec)
		base = strings.TrimPrefix(strings.TrimPrefix(base, "^"), "~")
		base = strings.TrimPrefix(base, "v")
		var candidates []string
		if c.exact && base != "" {
			// 纯版本号 spec：请求版本端点；仅当包整体 404（所有候选 HTTP 非 200）才摘除
			for _, reg := range pluginRegistries {
				candidates = append(candidates, reg+"/"+npmRegistryPath(c.name)+"/"+url.PathEscape(base))
			}
		} else {
			// 范围/标签：请求整包元数据，验证包存在即可
			for _, reg := range pluginRegistries {
				candidates = append(candidates, reg+"/"+npmRegistryPath(c.name))
			}
		}
		_, status, err := registryPreflightGET(candidates)
		if err != nil {
			if status != 0 {
				// 所有候选有 HTTP 应答但均失败：包不存在 / 精确版本不存在
				if c.exact {
					dropped[c.name] = fmt.Sprintf("registry 无此版本 %s@%s（HTTP %d）", c.name, base, status)
				} else {
					dropped[c.name] = fmt.Sprintf("registry 无此包 %s（HTTP %d）", c.name, status)
				}
			}
			continue // 纯网络错误：不误删，交给对齐失败归因兜底
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	for name, reason := range dropped {
		if dropProfileDependency(dir, name) {
			log.Printf("preflight: dropped %s (%s)", name, reason)
		}
	}
	return dropped
}

// githubDefaultBranch 查询 GitHub 仓库默认分支名。
func githubDefaultBranch(owner, repo string) (string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	body, err := getWithMirrors(mirrorCandidates(u), pluginCheckDeadline)
	if err != nil {
		return "", fmt.Errorf("GitHub 仓库查询失败：%w", err)
	}
	var m struct {
		DefaultBranch string `json:"default_branch"`
	}
	if json.Unmarshal(body, &m) != nil || m.DefaultBranch == "" {
		return "", fmt.Errorf("GitHub 响应缺少默认分支信息")
	}
	return m.DefaultBranch, nil
}

// githubRawFile 读取仓库默认分支上的指定文件（raw.githubusercontent，带镜像回退）。
func githubRawFile(owner, repo, branch, file string) ([]byte, error) {
	raw := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch), file)
	return getWithMirrors(mirrorCandidates(raw), pluginCheckDeadline)
}

// fetchGithubLatestVersion 按默认分支 package.json 的 version 判定最新版本
// （用户确认的决策：不跟随安装 spec 里可能带的 #branch/#tag，统一以默认分支为准）。
func fetchGithubLatestVersion(spec string) (string, error) {
	owner, repo, ok := githubSpecParts(spec)
	if !ok {
		return "", fmt.Errorf("无法解析 GitHub 来源：%s", spec)
	}
	branch, err := githubDefaultBranch(owner, repo)
	if err != nil {
		return "", err
	}
	body, err := githubRawFile(owner, repo, branch, "package.json")
	if err != nil {
		return "", fmt.Errorf("读取默认分支 package.json 失败：%w", err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &m) != nil || strings.TrimSpace(m.Version) == "" {
		return "", fmt.Errorf("仓库默认分支未声明有效 version")
	}
	return strings.TrimPrefix(strings.TrimSpace(m.Version), "v"), nil
}

// fetchPluginLatest 按来源查询单个插件的最新版本。
func fetchPluginLatest(row PluginRow) (string, error) {
	switch row.Source {
	case "npm":
		return fetchNpmLatestVersion(row.Name)
	case "github":
		return fetchGithubLatestVersion(row.Spec)
	default:
		return "", fmt.Errorf("该插件无远程更新来源：%s", row.Reason)
	}
}

// checkPluginUpdateByRow 检查单个插件是否有新版本（纯查询，供前端行内展示）。
func checkPluginUpdateByRow(row PluginRow) PluginCheckResult {
	res := PluginCheckResult{Name: row.Name, Current: row.Version}
	if !row.CanUpdate {
		res.Error = row.Reason
		return res
	}
	latest, err := fetchPluginLatest(row)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Latest = latest
	if row.Version == "" {
		res.HasUpdate = true // 声明了依赖但未安装（异常态）：视为可安装
	} else {
		res.HasUpdate = compareVersions("v"+latest, "v"+row.Version) > 0
	}
	return res
}

// ---- 更新执行 ----

// snapshotPluginProfile 单个 profile 目录更新前快照：备份 package.json / pnpm-lock.yaml，
// 并把 node_modules 整体改名备份（同盘 rename，秒级；回退可本地直接移回，不依赖网络）。
// 返回是否成功备份了 node_modules。调用前必须先 killServer()（运行中占用文件会阻止改名）。
func snapshotPluginProfile(dir string) bool {
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		src := filepath.Join(dir, name)
		if data, err := os.ReadFile(src); err == nil {
			_ = os.WriteFile(src+".dshbak", data, 0o644)
		}
	}
	nm := filepath.Join(dir, "node_modules")
	if _, err := os.Stat(nm); err != nil {
		return false
	}
	bak := nm + ".dshbak"
	_ = os.RemoveAll(bak)
	return os.Rename(nm, bak) == nil
}

// restorePluginProfileSnapshot 回退 profile 快照：还原 package.json / pnpm-lock.yaml；
// 有 node_modules 备份直接移回（秒级），否则按还原后的锁文件重装。
func restorePluginProfileSnapshot(dir string, hadNM bool) {
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		bak := filepath.Join(dir, name+".dshbak")
		if data, err := os.ReadFile(bak); err == nil {
			_ = os.WriteFile(filepath.Join(dir, name), data, 0o644)
		}
	}
	nm := filepath.Join(dir, "node_modules")
	if hadNM {
		_ = os.RemoveAll(nm)
		_ = os.Rename(nm+".dshbak", nm)
		return
	}
	if err := runProfileCmd(dir, pnpmCmd(), "install", "--frozen-lockfile"); err != nil {
		log.Printf("plugin rollback reinstall failed (%s): %v", dir, err)
	}
}

// cleanupPluginProfileSnapshot 更新成功：删除 profile 的更新前快照。
func cleanupPluginProfileSnapshot(dir string) {
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		_ = os.Remove(filepath.Join(dir, name+".dshbak"))
	}
	_ = os.RemoveAll(filepath.Join(dir, "node_modules.dshbak"))
}

// runProfileCmd 在指定目录执行命令，输出按行改写进统一日志（模块 profile）。
func runProfileCmd(dir, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), pnpmTunedEnv()...)
	hideCmdWindow(cmd)
	w := newModuleLogWriter("profile")
	log.Printf("profile cmd: %s %v (dir=%s)", name, args, dir)
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	w.Flush()
	return err
}

// pluginUpdateArgs 根据来源生成 pnpm 安装参数：
//   - npm：钉住检查到的精确版本并显式指定与检查一致的 registry——pnpm 对 dist-tag 的解析
//     受本地元数据缓存影响（`add name@latest` 可能落在旧版本，出现“远程有新版本但更新后版本号没变”），
//     精确版本 + 显式 registry 是确定性解法（本机已实证：latest=1.7.10 时 @latest 曾解析到 1.7.7）；
//   - github：pnpm update 重解析远程 ref（pnpm add 同一 spec 会复用锁文件已固定的提交，
//     已实证 update 能把 v0.6.1 tag 重解析到默认分支 main 的 0.6.2）。
func pluginUpdateArgs(row PluginRow, target, registry string) ([]string, error) {
	switch row.Source {
	case "npm":
		if target == "" {
			return nil, fmt.Errorf("未获取到 npm 目标版本")
		}
		if registry == "" {
			registry = pluginRegistries[0]
		}
		return []string{"add", row.Name + "@" + target, "--registry", registry}, nil
	case "github":
		return []string{"update", row.Name}, nil
	default:
		return nil, fmt.Errorf("该插件无远程更新来源：%s", row.Reason)
	}
}

// noteBuildScriptWarning github 类插件更新依赖其 prepare 构建脚本（pnpm ≥10 会受
// onlyBuiltDependencies / allowBuilds 白名单约束）；若被跳过会出现「更新成功但插件未构建」。
// 成功提示末尾附一句说明，供用户排查（服务健康校验失败路径已自动回退）。
func noteBuildScriptWarning(source string) string {
	if source == "github" {
		return "\n\n提示：GitHub 来源插件的更新依赖其 prepare 构建脚本。若更新后插件功能异常，" +
			"请确认 profile 的 pnpm-workspace.yaml 已将该插件加入 allowBuilds / onlyBuiltDependencies。"
	}
	return ""
}

// runPluginUpdate 更新单个插件到最新版：停服务 → 各 profile 快照 → pnpm 安装（npm 钉精确版本；
// github 重解析默认分支）→ 安装后版本对照 → 重启校验 → 成功提升 LKG 并提示 / 失败逐 profile 回退。
// 异步执行（按钮触发后 go 调用）。
func runPluginUpdate(id string) {
	row, ok := findPluginRowByID(id)
	if !ok {
		showMessageBox("未找到该插件，可能已被移除。", appName)
		return
	}
	// 记录本次更新前的禁用状态：被禁用插件更新成功后需尝试重新启用（见 3c 分支）
	wasDisabled := row.Disabled
	// 先检查一次，确认目标版本并用于文案
	check := checkPluginUpdateByRow(row)
	if !row.CanUpdate || check.Error != "" {
		msg := check.Error
		if msg == "" {
			msg = "该插件无远程更新来源。"
		}
		showMessageBox("无法更新插件：\n"+msg, appName)
		return
	}
	// 确定安装目标与同源 registry（npm 必须与检查结果同源，避免镜像/缓存漂移）
	target := check.Latest
	registry := ""
	if row.Source == "npm" {
		if fresh, reg, err := fetchNpmLatestWithSource(row.Name); err == nil && fresh != "" {
			target, registry = fresh, reg
		} else {
			registry = pluginRegistries[0]
		}
	}
	args, err := pluginUpdateArgs(row, target, registry)
	if err != nil {
		showMessageBox(err.Error(), appName)
		return
	}
	logUI("开始更新插件", fmt.Sprintf("%s (%s -> %s)", row.Name, orDash(row.Version), orDash(target)))

	splash := startSplash("正在更新插件 " + row.Name + "…")
	// 全程计数：插件更新同样占用「检查/更新窗口」，自动更新提示不再重复弹窗
	openUpdateCheckFlow()
	defer closeUpdateCheckFlow()

	// 0) 先停止服务（运行中的 node 占用 profile node_modules 文件，快照改名会失败）
	killServer()
	time.Sleep(1 * time.Second)

	// 1) 快照每个声明目录
	splash.Update("正在备份当前版本…", 0.12)
	hadNM := make([]bool, len(row.Locs))
	for i, dir := range row.Locs {
		hadNM[i] = snapshotPluginProfile(dir)
	}

	// 2) pnpm 安装（逐 profile 执行）
	var perr error
	for i, dir := range row.Locs {
		splash.Update(fmt.Sprintf("正在更新 %s（%d/%d）…", row.Name, i+1, len(row.Locs)),
			0.25+0.4*float64(i)/float64(len(row.Locs)))
		if perr = runProfileCmd(dir, pnpmCmd(), args...); perr != nil {
			break
		}
	}
	if perr != nil {
		// 3a) 失败：回退所有 profile 快照并重启校验
		rollbackPluginUpdate(splash, row, hadNM, fmt.Sprintf("安装失败：%v", perr))
		return
	}

	// 3b) 安装后版本对照：安装结果必须达到检查到的目标，否则视为失败（回退 + 明确原因）
	newVer := installedPluginVersion(row.Locs[0], row.Name)
	switch row.Source {
	case "npm":
		if newVer == "" || compareVersions("v"+newVer, "v"+target) < 0 {
			rollbackPluginUpdate(splash, row, hadNM, fmt.Sprintf(
				"安装后版本仍为 %s（预期 %s，registry 元数据缓存/镜像可能滞后）", orDash(newVer), orDash(target)))
			return
		}
	case "github":
		if newVer != "" && check.Latest != "" && compareVersions("v"+newVer, "v"+check.Latest) < 0 {
			rollbackPluginUpdate(splash, row, hadNM, fmt.Sprintf(
				"安装后版本 %s 低于预期 %s，未能重解析到默认分支最新状态", orDash(newVer), orDash(check.Latest)))
			return
		}
	}

	// 3) 重启并健康校验。失败时先尝试「禁用启动日志点名的用户插件」换取服务可启动
	//    （可能含本次被更新的插件：新版与核心不兼容时保留新版本并保持/转为禁用，不再整体回退）；
	//    禁用后仍失败或无嫌疑（核心故障）→ 回退到更新前版本。
	splash.Update("正在重启服务…", 0.85)
	if !restartAndVerifyServer() {
		splash.Update("启动校验失败，正在排查不兼容插件…", 0.9)
		disabled, ok := disableBootSuspects(row.Locs)
		if ok {
			// 保留新版本：把更新前快照提升为 LKG（未来失败回退到可用状态）
			for _, dir := range row.Locs {
				promoteProfileLkg(dir)
			}
			splash.Close()
			names := make([]string, 0, len(disabled))
			for _, d := range disabled {
				names = append(names, d.Name)
			}
			logUI("更新插件完成（含不兼容插件禁用）",
				fmt.Sprintf("%s: %s -> %s | 禁用 %s", row.Name, orDash(row.Version), orDash(newVer), strings.Join(names, "、")))
			showMessageBox(fmt.Sprintf("插件 %s 已更新：\n· %s → %s\n· 服务已重启。\n\n"+
				"以下插件与新版本不兼容，已自动禁用（保留记录，可在关于页重新启用或继续更新）：\n· %s",
				row.Name, orDash(row.Version), orDash(newVer), strings.Join(names, "、")), appName)
			if appCtx != nil {
				wruntime.EventsEmit(appCtx, "plugins:changed", nil)
			}
			return
		}
		rollbackPluginUpdate(splash, row, hadNM, "更新后服务启动失败")
		return
	}

	// 3c) 此前被禁用（不兼容自愈）的插件：更新成功后尝试重新启用——兼容则恢复启用；
	//     仍不兼容则自动重新禁用并重启服务（保证可启动）。
	if wasDisabled {
		splash.Update("正在尝试重新启用插件…", 0.92)
		enabled, why := enablePluginAndVerify(row)
		for _, dir := range row.Locs {
			promoteProfileLkg(dir)
		}
		splash.Close()
		if enabled {
			logUI("更新插件并重新启用", fmt.Sprintf("%s: %s -> %s", row.Name, orDash(row.Version), orDash(newVer)))
			showMessageBox(fmt.Sprintf("插件 %s 已更新到 %s 并重新启用，服务已重启。",
				row.Name, orDash(newVer)), appName)
		} else {
			logUI("更新插件后仍禁用", fmt.Sprintf("%s: %s（%s）", row.Name, orDash(newVer), why))
			showMessageBox(fmt.Sprintf("插件 %s 已更新到 %s，但启用后仍不兼容，继续保持禁用。\n原因：%s\n\n"+
				"可稍后再更新，或在插件确认修复后手动「启用」。", row.Name, orDash(newVer), why), appName)
		}
		if appCtx != nil {
			wruntime.EventsEmit(appCtx, "plugins:changed", nil)
		}
		return
	}

	// 4) 成功（启用态插件）：快照提升为 LKG（下次冷启动失败时可用它自动回退）+ 提示 + 刷新
	for _, dir := range row.Locs {
		promoteProfileLkg(dir)
	}
	splash.Close()
	logUI("更新插件完成", fmt.Sprintf("%s: %s -> %s", row.Name, orDash(row.Version), orDash(newVer)))
	msg := fmt.Sprintf("插件 %s 已更新：\n· %s → %s\n· 服务已重启。",
		row.Name, orDash(row.Version), orDash(newVer))
	if newVer == "" || newVer == row.Version {
		if row.Source == "github" {
			// github 按默认分支判定：上游可能未递增版本号——如实提示而非暗示失败
			msg = fmt.Sprintf("插件 %s 已更新到默认分支最新提交（版本号仍为 %s，上游未递增版本号）。服务已重启。",
				row.Name, orDash(row.Version))
		} else {
			msg = fmt.Sprintf("插件 %s 已更新到 %s（版本号显示未变化，可能为 registry 元数据延迟）。服务已重启。",
				row.Name, orDash(target))
		}
	}
	showMessageBox(msg+noteBuildScriptWarning(row.Source), appName)
	// 用户关闭完成弹窗后再通知前端刷新插件列表，避免窗口隐藏期间事件送达的不确定性
	if appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugins:changed", nil)
	}
}

// ==================== 截图 / 演示模式 ====================

// shotPlugins 截图模式插件清单：插件名一律使用虚构示例名（脱敏——不暴露开发者真实安装的
// 插件名/来源仓库），与真实环境完全隔离（不暴露本地路径与来源仓库细节）。
func shotPlugins() []PluginRow {
	return []PluginRow{
		{ID: "chat-billing", Name: "chat-billing", Version: "1.2.0", Spec: "^1.2.0", Source: "npm", CanUpdate: true},
		{ID: "session-indexer", Name: "session-indexer", Version: "0.4.1", Spec: "^0.4.1", Source: "npm", CanUpdate: true},
		{ID: "prompt-assistant", Name: "prompt-assistant", Version: "0.7.3", Spec: "github:example/prompt-assistant", Source: "github", CanUpdate: true},
		{ID: "my-dev-tool", Name: "my-dev-tool", Version: "0.2.0", Spec: "file:…/my-dev-tool", Source: "file", CanUpdate: false, Reason: "本地路径安装，无远程来源，无法更新"},
		{ID: "legacy-bundle", Name: "legacy-bundle", Version: "1.8.0", Spec: "https://example.com/packages/legacy-bundle-1.8.0.tgz", Source: "tarball", CanUpdate: false, Reason: "以固定压缩包地址安装，无法判断更新"},
	}
}

// shotPluginCheck 截图模式插件检查结果（演示：chat-billing 有新版本，其余已是最新）。
func shotPluginCheck(id string) PluginCheckResult {
	for _, r := range shotPlugins() {
		if r.ID == id || r.Name == id {
			if r.Name == "chat-billing" {
				return PluginCheckResult{Name: r.Name, Current: r.Version, Latest: "1.4.0", HasUpdate: true}
			}
			return PluginCheckResult{Name: r.Name, Current: r.Version, Latest: r.Version}
		}
	}
	return PluginCheckResult{Name: id, Error: "未找到该插件。"}
}

// rollbackPluginUpdate 插件更新失败：回退全部 profile 快照 → 重启校验 → 弹窗报告。
func rollbackPluginUpdate(splash *SplashState, row PluginRow, hadNM []bool, reason string) {
	splash.Update("更新失败，正在回退插件版本…", 0.55)
	killServer()
	for i, dir := range row.Locs {
		had := false
		if i < len(hadNM) {
			had = hadNM[i]
		}
		restorePluginProfileSnapshot(dir, had)
	}
	splash.Update("正在重启服务…", 0.85)
	restartAndVerifyServer()
	splash.Close()
	logUI("更新插件失败", fmt.Sprintf("%s: %s", row.Name, reason))
	showMessageBox("插件 "+row.Name+" 更新失败（"+reason+"），已回退到更新前版本。\n\n日志："+unifiedLogPath(), appName)
}

// ==================== 插件删除 ====================

// profileDeclaresPlugin profile 的 package.json 是否仍声明该依赖（pnpm remove 后校验用）。
func profileDeclaresPlugin(dir, name string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var m struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	_, ok := m.Dependencies[name]
	return ok
}

// runPluginRemove 删除单个插件（覆盖其声明的全部 profile/目录）：
// 停服务 → 各目录快照 → pnpm remove（逐目录）→ 校验 package.json 已不再声明 →
// 重启健康校验 → 成功清理快照与 profile LKG（删除后即新的良好基线，避免冷启动失败时
// LKG 回退把已删插件“复活”）/ 失败逐目录回退并报告。异步执行（前端确认后 go 调用）。
// 「待重指定」行（未安装、只有挂起记录）不走上述事务，直接清记录返回。
func runPluginRemove(id string) {
	row, ok := findPluginRowByID(id)
	if !ok {
		showMessageBox("未找到该插件，可能已被移除。", appName)
		return
	}
	logUI("开始删除插件", fmt.Sprintf("%s（v%s，%d 个环境）", row.Name, orDash(row.Version), len(row.Locs)))

	// 待重指定行：依赖并未安装（只有挂起记录），无需停服务/快照/pnpm remove——
	// 直接清除各目录的记录（含可能的残留禁用记录）即可，删除不触发服务重启。
	if row.PendingLocal {
		for _, dir := range row.Locs {
			clearPendingLocal(dir, row.Name)
			_ = clearProfileDisabledRecord(dir, row.Name)
		}
		logUI("删除待重指定插件", fmt.Sprintf("%s（%d 个环境）", row.Name, len(row.Locs)))
		showMessageBox(fmt.Sprintf("插件 %s 的「待重指定」记录已移除。", row.Name), appName)
		if appCtx != nil {
			wruntime.EventsEmit(appCtx, "plugins:changed", nil)
		}
		return
	}
	// 「已自动禁用且无依赖声明」行：只有禁用记录与（可能残留的）文件，无依赖可移除——
	// 清除各目录的禁用记录即可，不触发服务重启（重装请走 Web UI 的 dsh add）。
	if row.GhostDisabled {
		for _, dir := range row.Locs {
			_ = clearProfileDisabledRecord(dir, row.Name)
			if st, err := os.Stat(filepath.Join(dir, "node_modules", filepath.FromSlash(row.Name))); err == nil {
				if st.IsDir() {
					_ = os.RemoveAll(filepath.Join(dir, "node_modules", filepath.FromSlash(row.Name)))
				}
			}
		}
		logUI("删除自动禁用插件（无依赖）", row.Name)
		showMessageBox(fmt.Sprintf("插件 %s 的自动禁用记录已移除（如需重新安装请使用 Web UI 的插件安装）。", row.Name), appName)
		if appCtx != nil {
			wruntime.EventsEmit(appCtx, "plugins:changed", nil)
		}
		return
	}

	splash := startSplash("正在删除插件 " + row.Name + "…")
	// 全程计数：删除同样占用「检查/更新窗口」，自动更新提示不再重复弹窗
	openUpdateCheckFlow()
	defer closeUpdateCheckFlow()

	// 0) 先停止服务（运行中的 node 占用 profile node_modules 文件，快照/移除会失败）
	killServer()
	time.Sleep(1 * time.Second)

	// 1) 快照每个声明目录（失败可整体回退）
	splash.Update("正在备份当前状态…", 0.12)
	hadNM := make([]bool, len(row.Locs))
	for i, dir := range row.Locs {
		hadNM[i] = snapshotPluginProfile(dir)
	}

	// 2) pnpm remove（逐目录执行）；pnpm 不感知 dsh.profile.bundles，删除必须同步摘除
	// bundle 激活声明——残留会导致服务启动报「cannot resolve profile bundle」硬失败
	// （本机删除 deepseek-idesign 实证，健康校验失败后整体回退、删除永远不生效）。
	var perr error
	for i, dir := range row.Locs {
		splash.Update(fmt.Sprintf("正在移除 %s（%d/%d）…", row.Name, i+1, len(row.Locs)),
			0.25+0.4*float64(i)/float64(len(row.Locs)))
		if perr = runProfileCmd(dir, pnpmCmd(), "remove", row.Name); perr != nil {
			break
		}
		if err := stripProfileBundleEntry(dir, row.Name); err != nil {
			perr = fmt.Errorf("清理激活清单失败：%v", err)
			break
		}
	}
	if perr == nil {
		// 3a) 移除后校验：任一目录的 package.json 仍声明该插件即视为失败
		for _, dir := range row.Locs {
			if profileDeclaresPlugin(dir, row.Name) {
				perr = fmt.Errorf("删除后 %s 的 package.json 仍声明 %s", dir, row.Name)
				break
			}
		}
	}
	if perr != nil {
		rollbackPluginRemove(splash, row, hadNM, fmt.Sprintf("移除失败：%v", perr))
		return
	}

	// 3b) 重启并健康校验。失败时尝试禁用启动日志点名的其它插件（删除本身已生效，
	//     以禁用其它阻碍者换取服务可启动）；仍失败或无嫌疑则回退删除。
	splash.Update("正在重启服务…", 0.85)
	if !restartAndVerifyServer() {
		splash.Update("启动校验失败，正在排查不兼容插件…", 0.9)
		disabled, ok := disableBootSuspects(row.Locs)
		if ok {
			// 删除成功 + 禁用其它冲突插件：清理快照与 LKG（已删除插件不应被回退“复活”）
			for _, dir := range row.Locs {
				cleanupPluginProfileSnapshot(dir)
				clearLkgInDir(dir)
			}
			splash.Close()
			names := make([]string, 0, len(disabled))
			for _, d := range disabled {
				names = append(names, d.Name)
			}
			logUI("删除插件完成（含其它插件禁用）", fmt.Sprintf("%s | 禁用 %s", row.Name, strings.Join(names, "、")))
			showMessageBox(fmt.Sprintf("插件 %s 已删除，服务已重启。\n\n删除后以下插件与当前版本不兼容，已自动禁用"+
				"（可在关于页检查更新后重新启用）：\n· %s", row.Name, strings.Join(names, "、")), appName)
			if appCtx != nil {
				wruntime.EventsEmit(appCtx, "plugins:changed", nil)
			}
			return
		}
		rollbackPluginRemove(splash, row, hadNM, "删除后服务启动失败")
		return
	}

	// 4) 成功：清理快照；并清理 profile LKG（删除后的状态即新的良好基线）；清除残留禁用记录
	for _, dir := range row.Locs {
		cleanupPluginProfileSnapshot(dir)
		clearLkgInDir(dir)
		_ = clearProfileDisabledRecord(dir, row.Name)
	}
	splash.Close()
	logUI("删除插件完成", row.Name)
	showMessageBox(fmt.Sprintf("插件 %s 已删除，服务已重启。", row.Name), appName)
	// 用户关闭完成弹窗后再通知前端刷新插件列表（与更新流程一致）
	if appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugins:changed", nil)
	}
}

// rollbackPluginRemove 插件删除失败：回退全部目录快照 → 重启校验 → 弹窗报告。
func rollbackPluginRemove(splash *SplashState, row PluginRow, hadNM []bool, reason string) {
	splash.Update("删除失败，正在回退…", 0.55)
	killServer()
	for i, dir := range row.Locs {
		had := false
		if i < len(hadNM) {
			had = hadNM[i]
		}
		restorePluginProfileSnapshot(dir, had)
	}
	splash.Update("正在重启服务…", 0.85)
	restartAndVerifyServer()
	splash.Close()
	logUI("删除插件失败", fmt.Sprintf("%s: %s", row.Name, reason))
	showMessageBox("插件 "+row.Name+" 删除失败（"+reason+"），已回退到删除前状态。\n\n日志："+unifiedLogPath(), appName)
}

// ==================== 本地插件「待重指定」挂起记录 ====================
// 跨机恢复（导入导出包）时，本地链接依赖（link:/file:/workspace: 或裸路径）的目标目录在本机
// 不存在。为不让 pnpm 对齐硬失败（ERR_PNPM_LINKED_PKG_DIR_NOT_FOUND）也不改写为 npm 引入与
// 目标机核心不兼容的 registry 版本，sanitizeProfileLocalDeps（exportimport.go）把这类依赖
// 移出 dependencies、记录为「待重指定」：保留在 package.json 的 dsh.profile.pendingLocalPlugins
// 下（随既有快照/回退机制覆盖），插件列表仍合成显示该本地插件行，用户点「更新…」重新选择
// 本地目录后，runLocalPluginUpdate 落回 link: spec 并在成功事务内恢复激活（bundled=true 时）。

// pendingLocalKey 待重指定记录在 dsh.profile 下的键。
// 记录形态：{ "<插件名>": { "spec": "<原始依赖 spec>", "bundled": <是否曾在 bundles 激活> } }
const pendingLocalKey = "pendingLocalPlugins"

// pendingLocalData 一条待重指定记录。
type pendingLocalData struct {
	spec    string
	bundled bool
}

// profilePendingMap 取/建 root 中待重指定记录 map（create=false 且无记录时返回 nil）。
func profilePendingMap(root map[string]interface{}, create bool) map[string]interface{} {
	_, prof := profileSection(root)
	m, _ := prof[pendingLocalKey].(map[string]interface{})
	if m == nil && create {
		m = map[string]interface{}{}
		prof[pendingLocalKey] = m
	}
	return m
}

// setPendingLocalEntry 记录 name 为待重指定（保留原始 spec 与是否曾激活）；已存在则不动。
func setPendingLocalEntry(root map[string]interface{}, name, spec string, bundled bool) {
	pm := profilePendingMap(root, true)
	if _, exists := pm[name]; exists {
		return
	}
	pm[name] = map[string]interface{}{"spec": spec, "bundled": bundled}
}

// readPendingLocal 读 dir 的 package.json 中 name 的待重指定记录。
func readPendingLocal(dir, name string) (pendingLocalData, bool) {
	root := readProfileRoot(dir)
	raw, ok := profilePendingMap(root, false)[name].(map[string]interface{})
	if !ok {
		return pendingLocalData{}, false
	}
	spec, _ := raw["spec"].(string)
	bd, _ := raw["bundled"].(bool)
	return pendingLocalData{spec: spec, bundled: bd}, true
}

// listPendingLocalEntries 枚举 dir 的 package.json 中全部待重指定记录（name → 数据）。
func listPendingLocalEntries(dir string) map[string]pendingLocalData {
	root := readProfileRoot(dir)
	out := map[string]pendingLocalData{}
	for n, raw := range profilePendingMap(root, false) {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		spec, _ := m["spec"].(string)
		bd, _ := m["bundled"].(bool)
		out[n] = pendingLocalData{spec: spec, bundled: bd}
	}
	return out
}

// clearPendingLocal 删除 dir 的 package.json 中 name 的待重指定记录（空表连同键移除）。
// 返回记录是否存在并已删除。
func clearPendingLocal(dir, name string) bool {
	root := readProfileRoot(dir)
	_, prof := profileSection(root)
	pm, _ := prof[pendingLocalKey].(map[string]interface{})
	if pm == nil {
		return false
	}
	if _, ok := pm[name]; !ok {
		return false
	}
	delete(pm, name)
	if len(pm) == 0 {
		delete(prof, pendingLocalKey)
	}
	if err := writeProfileRoot(dir, root); err != nil {
		log.Printf("clearPendingLocal: write %s package.json: %v", dir, err)
	}
	return true
}

// relinkPendingLocal 本地插件重指定（更新事务逐目录调用）：若该目录存在 name 的待重指定记录，
// 在写回 link: spec 前恢复其 bundle 激活（原记录 bundled=true 时）并删除挂起记录。
func relinkPendingLocal(dir, name string) {
	root := readProfileRoot(dir)
	_, prof := profileSection(root)
	pm, _ := prof[pendingLocalKey].(map[string]interface{})
	if pm == nil {
		return
	}
	raw, ok := pm[name].(map[string]interface{})
	if !ok {
		return
	}
	bd, _ := raw["bundled"].(bool)
	delete(pm, name)
	if len(pm) == 0 {
		delete(prof, pendingLocalKey)
	}
	if bd {
		appendBundleEntry(root, name)
	}
	if err := writeProfileRoot(dir, root); err != nil {
		log.Printf("relinkPendingLocal: write %s package.json: %v", dir, err)
	}
}

// bundleEntryExists root 的 dsh.profile.bundles 是否含 name（兼容 name@… 变体）。
func bundleEntryExists(root map[string]interface{}, name string) bool {
	dsh, _ := root["dsh"].(map[string]interface{})
	if dsh == nil {
		return false
	}
	prof, _ := dsh["profile"].(map[string]interface{})
	if prof == nil {
		return false
	}
	for _, b := range prof["bundles"].([]interface{}) {
		s, ok := b.(string)
		if !ok {
			continue
		}
		base := s
		if i := strings.IndexByte(base, '@'); i > 0 {
			base = base[:i]
		}
		if base == name {
			return true
		}
	}
	return false
}

// ==================== 本地插件更新（选择目录 → 比较 → 覆盖） ====================
// 本地来源（file:/link:/workspace:/本地路径）插件没有远程版本来源，原「更新」按钮灰置。
// 现在开放「选择本地目录更新」：用户选定新插件目录 → 比较版本 →
// 有差异（更新或回退）确认后覆盖安装，版本相同提示已是最新。
// 执行与远程插件更新同级事务：停服务 → 各 profile 快照 → 改写依赖 spec 为 link:<所选目录>
// → pnpm install → 重启健康校验 → 成功清理快照并提升 LKG / 失败整体回退。

// PluginLocalPick 本地插件「选择目录」第一步的结果（前端据此提示覆盖更新或已是最新）。
type PluginLocalPick struct {
	Error    string `json:"error"`    // 校验失败原因（目录不是插件 / 无 package.json 等）
	Canceled bool   `json:"canceled"` // 用户取消了目录选择
	Path     string `json:"path"`     // 所选目录绝对路径
	Version  string `json:"version"`  // 所选目录 package.json 的 version（缺失为空）
	Current  string `json:"current"`  // 当前已安装版本
	Relation string `json:"relation"` // same | newer | older | unknown（版本缺失/无法比较）
}

// packageMeta 读取目录 package.json 的 name 与 version。
func packageMeta(dir string) (name, version string, err error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", "", fmt.Errorf("所选目录没有 package.json：%v", err)
	}
	var m struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", "", fmt.Errorf("所选目录 package.json 解析失败：%v", err)
	}
	if m.Name == "" {
		return "", "", fmt.Errorf("所选目录的 package.json 缺少 name 字段，不是有效的插件包")
	}
	return m.Name, strings.TrimSpace(m.Version), nil
}

// localPickRelation 比较所选版本与已装版本（版本号缺失时返回 unknown）。
func localPickRelation(cur, picked string) string {
	if strings.TrimSpace(cur) == "" || strings.TrimSpace(picked) == "" {
		return "unknown"
	}
	c := compareVersions("v"+picked, "v"+cur)
	switch {
	case c == 0:
		return "same"
	case c > 0:
		return "newer"
	default:
		return "older"
	}
}

// localLinkSpec 生成本地目录的 link: 依赖 spec（Windows 反斜杠归一为正斜杠）。
func localLinkSpec(dir string) string {
	p := filepath.Clean(dir)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return "link:" + filepath.ToSlash(p)
}

// setProfileDepSpec 改写 profile package.json 中某依赖的 spec（不存在则创建依赖项）。
func setProfileDepSpec(dir, name, spec string) error {
	pj := filepath.Join(dir, "package.json")
	root := map[string]interface{}{}
	if data, err := os.ReadFile(pj); err == nil {
		_ = json.Unmarshal(data, &root)
	}
	if root == nil {
		root = map[string]interface{}{}
	}
	deps, _ := root["dependencies"].(map[string]interface{})
	if deps == nil {
		deps = map[string]interface{}{}
	}
	deps[name] = spec
	root["dependencies"] = deps
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pj, append(b, '\n'), 0o644)
}

// runLocalPluginUpdate 把本地插件覆盖更新为所选目录（前端已确认覆盖）。异步执行。
func runLocalPluginUpdate(row PluginRow, srcDir string) {
	name, pickedVer, err := packageMeta(srcDir)
	if err != nil {
		showMessageBox("无法更新本地插件：\n"+err.Error(), appName)
		return
	}
	if name != row.Name {
		showMessageBox(fmt.Sprintf("无法更新本地插件：\n所选目录不是插件 %s（目录 package.json 的 name=%s）。", row.Name, name), appName)
		return
	}
	spec := localLinkSpec(srcDir)
	logUI("开始更新本地插件", fmt.Sprintf("%s → %s（v%s）", row.Name, srcDir, orDash(pickedVer)))

	splash := startSplash("正在更新本地插件 " + row.Name + "…")
	// 全程计数：占用「检查/更新窗口」，自动更新提示不再重复弹窗
	openUpdateCheckFlow()
	defer closeUpdateCheckFlow()

	// 0) 先停止服务（运行中的 node 占用 profile node_modules 文件，快照改名会失败）
	killServer()
	time.Sleep(1 * time.Second)

	// 1) 快照每个声明目录
	splash.Update("正在备份当前版本…", 0.12)
	hadNM := make([]bool, len(row.Locs))
	for i, dir := range row.Locs {
		hadNM[i] = snapshotPluginProfile(dir)
	}

	// 2) 逐 profile 改写 spec 并 pnpm install（失败即回退）
	var perr error
	for i, dir := range row.Locs {
		splash.Update(fmt.Sprintf("正在更新 %s（%d/%d）…", row.Name, i+1, len(row.Locs)),
			0.25+0.4*float64(i)/float64(len(row.Locs)))
		// 待重指定行的重指定：先恢复 bundle 激活（原记录 bundled=true 时）并清除挂起记录，
		// 使 package.json 在安装前即为最终一致形态（服务重启健康校验可正确裁决兼容性）。
		relinkPendingLocal(dir, row.Name)
		if perr = setProfileDepSpec(dir, row.Name, spec); perr == nil {
			perr = runProfileCmd(dir, pnpmCmd(), "install")
		}
		if perr != nil {
			break
		}
	}
	if perr != nil {
		rollbackPluginUpdate(splash, row, hadNM, fmt.Sprintf("安装失败：%v", perr))
		return
	}

	// 3) 安装后版本对照：本地链接应安装为所选目录的版本
	newVer := installedPluginVersion(row.Locs[0], row.Name)
	if pickedVer != "" && (newVer == "" || compareVersions("v"+newVer, "v"+pickedVer) < 0) {
		rollbackPluginUpdate(splash, row, hadNM, fmt.Sprintf(
			"安装后版本仍为 %s（预期 %s）", orDash(newVer), orDash(pickedVer)))
		return
	}

	// 4) 重启并健康校验
	splash.Update("正在重启服务…", 0.85)
	if !restartAndVerifyServer() {
		rollbackPluginUpdate(splash, row, hadNM, "更新后服务启动失败")
		return
	}

	// 5) 成功：快照提升为 LKG + 提示 + 通知前端刷新
	for _, dir := range row.Locs {
		promoteProfileLkg(dir)
	}
	splash.Close()
	logUI("更新本地插件完成", fmt.Sprintf("%s → %s（v%s）", row.Name, srcDir, orDash(newVer)))
	showMessageBox(fmt.Sprintf("插件 %s 已覆盖更新：\n· 来源目录：%s\n· 版本：%s → %s\n· 服务已重启。",
		row.Name, srcDir, orDash(row.Version), orDash(newVer)), appName)
	if appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugins:changed", nil)
	}
}

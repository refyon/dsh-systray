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
	"encoding/base64"
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

// ==================== 单插件操作结果事件（Go → 前端行状态刷新） ====================
// 此前更新/删除/启用完成后只发无负载的 plugins:changed：成功路径 plugState 残留
// 「有新版本」提示、失败路径（回退等）完全无事件——行永久停留「正在更新插件…」。
// plugin:op:done 带结果负载，前端据此更新对应行的提示语与更新按钮状态。

// PluginOpDone 单插件操作（update/remove/enable）收尾事件负载。
type PluginOpDone struct {
	Name    string `json:"name"`              // 插件名
	Op      string `json:"op"`                // update | remove | enable
	OK      bool   `json:"ok"`                // 操作是否成功（保留版本/删除生效/启用成功）
	Version string `json:"version,omitempty"` // 操作后的插件版本（失败/删除可为空）
	Reason  string `json:"reason,omitempty"`  // 失败原因；成功时若有「连带禁用的其它插件」则为其名单（"、" 连接）
}

func emitPluginOpDone(d PluginOpDone) {
	if appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugin:op:done", d)
	}
}

// disabledNames 禁用插件列表 → 名单文案（弹窗与事件 reason 共用）。
func disabledNames(rows []PluginRow) string {
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	return strings.Join(names, "、")
}

// pluginCheckDeadline 单插件一次检查的最长耗时（多个候选源共用该预算，超时即报错返回）。
const pluginCheckDeadline = 15 * time.Second

// pluginCheckCandidateTimeout 单候选请求超时（且不超共享 deadline 剩余预算）：
// 某候选被墙/挂起时不再独占整个共享预算，后续镜像与 API 兜底候选仍有机会尝试
// （2026-09-08 复盘：mirror.ghproxy.com 挂起吃掉整段 15s，restrict-discipline 检查更新报超时）。
const pluginCheckCandidateTimeout = 6 * time.Second

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
	PendingOp      string   `json:"pendingOp"`      // 待应用变更：update | remove（空=无）；需重启服务才生效
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
// deadline 为整体预算（上下文超时，逐候选共享），避免镜像全挂时长时间卡 UI；
// 单候选另有 pluginCheckCandidateTimeout 上限（取剩余预算更小者），防止单个挂起候选独占预算。
func getWithMirrors(candidates []string, deadline time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	client := &http.Client{}
	var lastErr error
	for _, u := range candidates {
		candCtx, candCancel := context.WithTimeout(ctx, pluginCheckCandidateTimeout)
		req, err := http.NewRequestWithContext(candCtx, "GET", u, nil)
		if err != nil {
			candCancel()
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "dsh-systray/"+appVersion)
		req.Header.Set("Accept", "application/vnd.github+json, application/json")
		resp, err := client.Do(req)
		if err != nil {
			candCancel()
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			candCancel()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, updateMaxBodySize))
		resp.Body.Close()
		candCancel()
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

// githubRawFile 读取仓库默认分支上的指定文件。
// 通道 1：raw.githubusercontent.com（直连 + 镜像前缀）；通道 2（兜底）：GitHub API
// contents 端点（api.github.com 与 raw 属不同通道——2026-09-08 复盘实证 raw 通道全挂时
// API 通道仍可达，restrict-discipline 检查更新曾因此报「读取默认分支 package.json 失败」）。
func githubRawFile(owner, repo, branch, file string) ([]byte, error) {
	raw := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch), file)
	if body, err := getWithMirrors(mirrorCandidates(raw), pluginCheckDeadline); err == nil {
		return body, nil
	}
	api := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(file), url.QueryEscape(branch))
	body, err := getWithMirrors(mirrorCandidates(api), pluginCheckDeadline)
	if err != nil {
		return nil, err
	}
	return decodeGithubContents(body)
}

// decodeGithubContents 解析 GitHub API contents 响应（encoding=base64 的 content 字段，容忍换行拆分）。
func decodeGithubContents(body []byte) ([]byte, error) {
	var m struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if json.Unmarshal(body, &m) != nil || m.Encoding != "base64" || m.Content == "" {
		return nil, fmt.Errorf("GitHub API contents 响应缺 base64 内容")
	}
	dec, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(m.Content), ""))
	if err != nil {
		return nil, fmt.Errorf("GitHub API contents base64 解码失败：%w", err)
	}
	return dec, nil
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

// pluginSnapSuffix 单插件路径（更新/本地更新/删除）的默认快照后缀。
const pluginSnapSuffix = ".dshbak"

// snapshotPluginProfile 单个 profile 目录更新前快照：备份 package.json / pnpm-lock.yaml，
// 并把 node_modules 整体改名备份（同盘 rename，秒级；回退可本地直接移回，不依赖网络）。
// 返回是否成功备份了 node_modules。调用前必须先 killServer()（运行中占用文件会阻止改名）。
func snapshotPluginProfile(dir string) bool {
	return snapshotPluginProfileSuffix(dir, pluginSnapSuffix)
}

// snapshotPluginProfileSuffix 带后缀的 profile 快照。批处理给每一项分配独立后缀（.pbak<N>）：
// 同一 profile 上先后两项操作若共用后缀，后一项的快照会覆盖前一项的，整批回退就无法逐项回到
// 各自操作前的状态（多插件同环境场景；单测 TestPluginBatchRollbackOrder 覆盖）。
func snapshotPluginProfileSuffix(dir, suffix string) bool {
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		src := filepath.Join(dir, name)
		if data, err := os.ReadFile(src); err == nil {
			_ = os.WriteFile(src+suffix, data, 0o644)
		}
	}
	nm := filepath.Join(dir, "node_modules")
	if _, err := os.Stat(nm); err != nil {
		return false
	}
	bak := nm + suffix
	_ = os.RemoveAll(bak)
	return os.Rename(nm, bak) == nil
}

// restorePluginProfileSnapshot 回退 profile 快照：还原 package.json / pnpm-lock.yaml；
// 有 node_modules 备份直接移回（秒级），否则按还原后的锁文件重装。
func restorePluginProfileSnapshot(dir string, hadNM bool) {
	restorePluginProfileSnapshotSuffix(dir, pluginSnapSuffix, hadNM)
}

// restorePluginProfileSnapshotSuffix 带后缀的快照回退（见 snapshotPluginProfileSuffix）。
func restorePluginProfileSnapshotSuffix(dir, suffix string, hadNM bool) {
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		bak := filepath.Join(dir, name+suffix)
		if data, err := os.ReadFile(bak); err == nil {
			_ = os.WriteFile(filepath.Join(dir, name), data, 0o644)
		}
	}
	nm := filepath.Join(dir, "node_modules")
	if hadNM {
		_ = os.RemoveAll(nm)
		_ = os.Rename(nm+suffix, nm)
		return
	}
	if err := runProfileCmd(dir, pnpmCmd(), "install", "--frozen-lockfile"); err != nil {
		log.Printf("plugin rollback reinstall failed (%s): %v", dir, err)
	}
}

// cleanupPluginProfileSnapshot 更新成功：删除 profile 的更新前快照。
func cleanupPluginProfileSnapshot(dir string) {
	cleanupPluginProfileSnapshotSuffix(dir, pluginSnapSuffix)
}

// cleanupPluginProfileSnapshotSuffix 带后缀的快照清理。
func cleanupPluginProfileSnapshotSuffix(dir, suffix string) {
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		_ = os.Remove(filepath.Join(dir, name+suffix))
	}
	_ = os.RemoveAll(filepath.Join(dir, "node_modules"+suffix))
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

// supplyChainViolation pnpm 输出是否为供应链策略拦截（lockfile 条目发布年龄校验失败）。
// 实测形态（pnpm 11.x）：
//
//	✗ Lockfile failed supply-chain policy check …
//	[ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION] 1 lockfile entries failed verification:
//	  <name>@<ver> was published at …, within the minimumReleaseAge cutoff (…)
//
// 拦截对象可能是与本次更新无关的其它插件（lockfile 任一条目被拒，整条命令即失败）——
// 调用方据此刻意以 --config.minimumReleaseAge=0 临时跳过年龄校验重试一次（仅本次，不动 profile 配置）。
func supplyChainViolation(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(out, "ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION") ||
		strings.Contains(low, "minimumreleaseage") ||
		strings.Contains(low, "minimum release age") ||
		strings.Contains(low, "supply-chain policy") ||
		strings.Contains(low, "um_release_age_violation")
}

// profileInstallErr 包装 pnpm 安装失败：附带命令输出尾部（截断 ~600 字），使
// 「安装失败：exit status 1」类干瘪原因可直接定位根因（此前失败输出只进统一日志，
// 随轮转归档到 .1、界面不可见，需翻档排查——mac 实证：github 源依赖 SSH 解析失败）。
func profileInstallErr(err error, out string) error {
	if err == nil {
		return nil
	}
	tail := outputTail(out, 600)
	if tail == "" {
		return err
	}
	return fmt.Errorf("%v：%s", err, tail)
}

// rollbackPluginUpdate 插件更新失败：回退全部 profile 快照 → 重启校验 → 弹窗报告。
func rollbackPluginUpdate(splash *SplashState, row PluginRow, hadNM []bool, reason string) {
	splash.Update(T("更新失败，正在回退插件版本…"), 0.55)
	killServer()
	for i, dir := range row.Locs {
		had := false
		if i < len(hadNM) {
			had = hadNM[i]
		}
		restorePluginProfileSnapshot(dir, had)
	}
	splash.Update(T("正在重启服务…"), 0.85)
	restartAndVerifyServer()
	splash.Close()
	logUI("更新插件失败", fmt.Sprintf("%s: %s", row.Name, reason))
	showMessageBox("插件 "+row.Name+" 更新失败（"+reason+"），已回退到更新前版本。\n\n日志："+unifiedLogPath(), appName)
	// 结果事件：行内显示失败原因（此前失败路径无任何事件，行永久停留「正在更新插件…」）
	emitPluginOpDone(PluginOpDone{Name: row.Name, Op: "update", OK: false, Reason: reason})
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

// ==================== 本地插件「待重指定」挂起记录 ====================
// 跨机恢复（导入导出包）时，本地链接依赖（link:/file:/workspace: 或裸路径）的目标目录在本机
// 不存在。优先路径：导入包在 profile node_modules 恢复了该插件的副本时，sanitizeProfileLocalDepsAll
//（exportimport.go）把副本迁到 <dshHome>/local-plugins/<name> 并改写 spec 为 link:<副本>，
// 插件继续加载启动。仅当无副本时，为不让 pnpm 对齐硬失败（ERR_PNPM_LINKED_PKG_DIR_NOT_FOUND）
// 也不改写为 npm 引入与目标机核心不兼容的 registry 版本，才把这类依赖移出 dependencies、
// 记录为「待重指定」：保留在 package.json 的 dsh.profile.pendingLocalPlugins 下（随既有快照/
// 回退机制覆盖），插件列表仍合成显示该本地插件行，用户点「更新…」重新选择本地目录后，
// runLocalPluginUpdate 落回 link: spec 并在成功事务内恢复激活。
// 语义（用户决策）：重选目录 = 显式期望加载该插件——无论历史记录 bundled 真假，一律激活进
// dsh.profile.bundles（源机未激活只是历史状态，不阻挠用户本次的显式激活意图）。

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
// 在写回 link: spec 前无条件把 name 加回 bundle 激活清单并删除挂起记录——重选目录即用户显式
// 期望加载该插件（bundled 仅记录源机历史激活状态，不决定本次是否激活；此前 bundled=false 时
// 只清记录不激活，重选后 spec/node_modules 均已就位但 harness 激活清单里没有它 → 插件永不
// 加载且无任何提示，mac 实证的「重选后依旧没进 harness」无症状形态）。
func relinkPendingLocal(dir, name string) {
	root := readProfileRoot(dir)
	_, prof := profileSection(root)
	pm, _ := prof[pendingLocalKey].(map[string]interface{})
	if pm == nil {
		return
	}
	if _, ok := pm[name].(map[string]interface{}); !ok {
		return
	}
	delete(pm, name)
	if len(pm) == 0 {
		delete(prof, pendingLocalKey)
	}
	appendBundleEntry(root, name)
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

// logPluginTerminalState 记录本地插件重选/更新成功后的终态（依赖 spec、bundles 激活、
// 待重指定、禁用状态与 node_modules 链接是否建立），供「更新/重选后插件未加载」类问题
// 凭日志一次定案。
func logPluginTerminalState(dir, name, spec string) {
	root := readProfileRoot(dir)
	_, prof := profileSection(root)
	deps, _ := root["dependencies"].(map[string]interface{})
	pending, _ := prof[pendingLocalKey].(map[string]interface{})
	dm, _ := prof["disabledPlugins"].(map[string]interface{})
	_, depOK := deps[name]
	_, pendOK := pending[name]
	_, disOK := dm[name]
	linkState := "missing"
	if _, err := os.Stat(filepath.Join(dir, "node_modules", filepath.FromSlash(name))); err == nil {
		linkState = "exists"
	}
	log.Printf("plugin %s terminal state: declared=%v spec=%s bundles=%v pendingLocal=%v disabled=%v node_modules:%s",
		name, depOK, spec, bundleEntryExists(root, name), pendOK, disOK, linkState)
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
// 语义与远程更新对齐：待重指定行重选 = 显式激活（无论历史 bundled 真假，见 relinkPendingLocal）；
// 此前被自动禁用（不兼容自愈）的插件更新成功后自动尝试重新启用并健康校验——
// 更新路径即修复尝试，保证下次启动加载。
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
	// 记录本次更新前的禁用状态：被禁用的本地插件更新成功后需尝试重新启用（见 5a 分支）
	wasDisabled := row.Disabled
	spec := localLinkSpec(srcDir)
	logUI("开始更新本地插件", fmt.Sprintf("%s → %s（v%s）", row.Name, srcDir, orDash(pickedVer)))

	splash := startSplash(TF("正在更新本地插件 %s…", row.Name))
	// 全程计数：占用「检查/更新窗口」，自动更新提示不再重复弹窗
	openUpdateCheckFlow()
	defer closeUpdateCheckFlow()

	// 0) 先停止服务（运行中的 node 占用 profile node_modules 文件，快照改名会失败）
	killServer()
	time.Sleep(1 * time.Second)

	// 1) 快照每个声明目录
	splash.Update(T("正在备份当前版本…"), 0.12)
	hadNM := make([]bool, len(row.Locs))
	for i, dir := range row.Locs {
		hadNM[i] = snapshotPluginProfile(dir)
	}

	// 2) 逐 profile 改写 spec 并 pnpm install（失败即回退；输出捕获入原因，避免干瘪 exit status；
	//    供应链策略拦截时同远程更新路径自动临时跳过发布年龄校验重试一次）。
	var perr error
	for i, dir := range row.Locs {
		splash.Update(fmt.Sprintf("正在更新 %s（%d/%d）…", row.Name, i+1, len(row.Locs)),
			0.25+0.4*float64(i)/float64(len(row.Locs)))
		// 待重指定行的重指定：先恢复 bundle 激活（原记录 bundled=true 时）并清除挂起记录，
		// 使 package.json 在安装前即为最终一致形态（服务重启健康校验可正确裁决兼容性）。
		relinkPendingLocal(dir, row.Name)
		if perr = setProfileDepSpec(dir, row.Name, spec); perr != nil {
			break
		}
	}
	// 目标插件的 spec 已改写为可用路径，再消毒其余悬空本地依赖，最后 pnpm install。
	for _, n := range guardProfileLocalDeps(row.Locs...) {
		logUI("本地依赖消毒", n)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt == 2 {
			splash.Update("供应链策略拦截（发布年龄校验），正在以临时跳过校验重试…", 0.55)
		}
		perr = nil
		var failRaw string
		for _, dir := range row.Locs {
			var out string
			cmdArgs := []string{"install"}
			if attempt == 2 {
				cmdArgs = append(cmdArgs, "--config.minimumReleaseAge=0")
			}
			out, perr = runProfileCmdCapture(dir, pnpmCmd(), cmdArgs...)
			if perr != nil {
				failRaw = out
				perr = profileInstallErr(perr, out)
				break
			}
		}
		if perr == nil {
			break // 全部目录安装成功
		}
		if attempt == 1 && supplyChainViolation(failRaw) {
			logUI("供应链策略拦截，重试", fmt.Sprintf("%s：lockfile 发布年龄校验失败，临时跳过重试", row.Name))
			continue
		}
		break
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
	splash.Update(T("正在重启服务…"), 0.85)
	if !restartAndVerifyServer() {
		rollbackPluginUpdate(splash, row, hadNM, "更新后服务启动失败")
		return
	}

	// 4a) 终态快照日志（每个声明目录各一行）：「重选/更新后插件未加载」类问题凭日志即可定案
	//（依赖 spec 是否写入、bundles 是否激活、pending/禁用记录是否清除、node_modules 链接是否建立）。
	for _, dir := range row.Locs {
		logPluginTerminalState(dir, row.Name, spec)
	}

	// 5a) 此前被自动禁用（不兼容自愈）的本地插件：更新路径即修复尝试——成功后尝试重新启用
	//     （清除禁用记录 + 加回 bundles 并重启健康校验），保证下次启动加载到 harness；
	//     仍不兼容则自动重新禁用并重启服务（保留新版本 + 禁用状态，语义同远程更新 3c）。
	if wasDisabled {
		splash.Update(T("正在尝试重新启用插件…"), 0.92)
		enabled, why := enablePluginAndVerify(row)
		for _, dir := range row.Locs {
			promoteProfileLkg(dir)
		}
		splash.Close()
		if enabled {
			logUI("更新本地插件并重新启用", fmt.Sprintf("%s → %s（v%s）", row.Name, srcDir, orDash(newVer)))
			showMessageBox(fmt.Sprintf("插件 %s 已更新并重新启用：\n· 来源目录：%s\n· 版本：%s → %s\n· 服务已重启，下次启动将正常加载。",
				row.Name, srcDir, orDash(row.Version), orDash(newVer)), appName)
			emitPluginOpDone(PluginOpDone{Name: row.Name, Op: "update", OK: true, Version: newVer})
		} else {
			logUI("更新本地插件后仍禁用", fmt.Sprintf("%s（%s）", row.Name, why))
			showMessageBox(fmt.Sprintf("插件 %s 已更新到 %s，但启用后仍不兼容，继续保持禁用。\n原因：%s\n\n可稍后再更新，或在插件确认修复后手动「启用」。",
				row.Name, orDash(newVer), why), appName)
			emitPluginOpDone(PluginOpDone{Name: row.Name, Op: "update", OK: true, Version: newVer, Reason: "已更新但仍不兼容，继续保持禁用：" + why})
		}
		if appCtx != nil {
			wruntime.EventsEmit(appCtx, "plugins:changed", nil)
		}
		return
	}

	// 5b) 成功（启用态插件）：快照提升为 LKG（下次冷启动失败可回退）+ 提示 + 通知前端刷新
	for _, dir := range row.Locs {
		promoteProfileLkg(dir)
	}
	splash.Close()
	logUI("更新本地插件完成", fmt.Sprintf("%s → %s（v%s）", row.Name, srcDir, orDash(newVer)))
	msg := fmt.Sprintf("插件 %s 已覆盖更新：\n· 来源目录：%s\n· 版本：%s → %s\n· 服务已重启。",
		row.Name, srcDir, orDash(row.Version), orDash(newVer))
	// 所选目录缺少 node_modules（依赖未安装/未构建）时插件大概率无法被 harness 加载——
	// 重选/更新成功弹窗如实提示（此前静默不加载、无从排查，mac 实证）。
	if st, err := os.Stat(filepath.Join(srcDir, "node_modules")); err != nil || !st.IsDir() {
		msg += "\n\n提示：所选目录没有 node_modules（依赖未安装或未构建）。" +
			"若插件自身有依赖或需要构建产物，harness 可能无法正常加载——请先在所选目录内安装依赖/构建后重试。"
	}
	showMessageBox(msg, appName)
	if appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugins:changed", nil)
	}
	emitPluginOpDone(PluginOpDone{Name: row.Name, Op: "update", OK: true, Version: newVer})
}

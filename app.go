package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// app Wails 绑定实例（main.go 的 Bind 列表引用）。
var app = &App{}

// App 暴露给前端的全部后端能力（Wails Bindings）。
type App struct{}

// ==================== 截图模式脱敏 ====================

// shotMode 截图/演示模式：DSH_SYSTRAY_SHOT_PAGE 非空时启用，
// 避免截图泄露真实路径、用户名、版本构建信息（技术栈）。
var shotMode = os.Getenv("DSH_SYSTRAY_SHOT_PAGE") != ""

// sanitizeShotPath 脱敏单个路径（用户目录前缀 → C:\Users\demo）。
func sanitizeShotPath(p string) string {
	if !shotMode || p == "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return strings.ReplaceAll(p, home, filepath.Join("C:", "Users", "demo"))
}

// sanitizeShotVersion 脱敏版本号：去掉 "-wails"/"-dev" 等构建后缀，避免透露技术栈。
func sanitizeShotVersion(v string) string {
	if !shotMode || v == "" {
		return v
	}
	if i := strings.Index(v, "-"); i > 0 {
		return v[:i]
	}
	return v
}

// sanitizeShotHarnessDir 常规页 Harness 目录：截图模式固定显示脱敏后的 .dsh 数据目录（不暴露真实位置）。
func sanitizeShotHarnessDir() string {
	if runtime.GOOS == "darwin" {
		return "/Users/demo/.dsh"
	}
	return `C:\Users\demo\.dsh`
}

// shotDemoLog 截图模式展示的中性演示日志（与真实环境完全隔离，避免泄露路径/主机名等信息）。
var shotDemoLog = []string{
	"2026-08-11 09:12:01 INFO  dsh-systray v0.5.0 启动完成",
	"2026-08-11 09:12:03 INFO  后台服务已就绪：http://127.0.0.1:3080/",
	"2026-08-11 09:12:05 INFO  已检测到 3 个历史会话",
	"2026-08-11 09:12:07 WARN  日志文件较大，已截断显示最近 4000 行",
	"2026-08-11 09:12:10 INFO  导出完成：dsh-systray-export-20260811-091210-1a2b3c4d.zip",
	"2026-08-11 09:12:12 INFO  已是最新版本（v0.5.0）",
}

// sanitizeShotLine 脱敏一行文本：替换用户路径前缀，并移除版本构建后缀（如 -wails）。
func sanitizeShotLine(s string) string {
	if !shotMode || s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "-wails", "")
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		s = strings.ReplaceAll(s, home, filepath.Join("C:", "Users", "demo"))
	}
	return s
}

// ==================== 配置 ====================

// logUI 把设置页用户操作写入 app.log（统一前缀，便于检索与追溯）。
func logUI(action, detail string) {
	if detail == "" {
		log.Printf("[UI] %s", action)
	} else {
		log.Printf("[UI] %s | %s", action, detail)
	}
}

// ConfigInfo 设置页「常规/关于」所需配置快照。
type ConfigInfo struct {
	Port              int    `json:"port"`
	HarnessDir        string `json:"harnessDir"`
	StartupTimeoutSec int    `json:"startupTimeoutSec"`
	UpdateMirror      string `json:"updateMirror"`
	HarnessPrerelease bool   `json:"harnessPrerelease"`
	WebURL            string `json:"webURL"`
	Autostart         bool   `json:"autostart"`
	AutostartLaunch   bool   `json:"autostartLaunch"`
}

func (a *App) GetConfig() ConfigInfo {
	hd := harnessDir
	if shotMode {
		hd = sanitizeShotHarnessDir() // 截图模式：不暴露真实目录
	}
	return ConfigInfo{
		Port:              port,
		HarnessDir:        hd,
		StartupTimeoutSec: int(startupTimeout / time.Second),
		UpdateMirror:      updateMirrorOverride,
		HarnessPrerelease: harnessPrereleaseOverride,
		WebURL:            webURL,
		Autostart:         isAutostartEnabled(),
		AutostartLaunch:   autostartLaunch,
	}
}

// saveCurrentConfig 把当前全局配置写回 config.json。
func saveCurrentConfig() {
	saveConfig(appConfig{
		Port:              port,
		HarnessDir:        harnessDir,
		StartupTimeoutSec: int(startupTimeout / time.Second),
		UpdateMirror:      updateMirrorOverride,
		HarnessPrerelease: harnessPrereleaseOverride,
	})
}

func (a *App) SetAutostart(on bool) {
	logUI("设置开机自启动", map[bool]string{true: "开启", false: "关闭"}[on])
	setAutostartOn(on)
}

func (a *App) SetHarnessPrerelease(on bool) {
	logUI("设置预发布通道", map[bool]string{true: "开启", false: "关闭"}[on])
	harnessPrereleaseOverride = on
	saveCurrentConfig()
}

func (a *App) SetHarnessDir(d string) {
	d = strings.TrimSpace(d)
	if d == "" {
		return
	}
	logUI("设置 Harness 目录", d)
	harnessDir = d
	harnessDirExplicit = true
	saveCurrentConfig()
}

func (a *App) SetUpdateMirror(m string) {
	m = strings.TrimSpace(m)
	logUI("设置更新镜像", m)
	updateMirrorOverride = m
	saveCurrentConfig()
}

func (a *App) SetPort(p int) {
	if p <= 0 || p > 65535 {
		return
	}
	logUI("修改服务端口", fmt.Sprintf("%d", p))
	port = p
	webURL = fmt.Sprintf("http://127.0.0.1:%d/", p)
	saveCurrentConfig()
}

func (a *App) SetStartupTimeoutSec(s int) {
	if s <= 0 {
		return
	}
	logUI("设置启动超时", fmt.Sprintf("%d 秒", s))
	startupTimeout = time.Duration(s) * time.Second
	saveCurrentConfig()
}

// ==================== 服务 ====================

// ServiceState 服务四态：starting / running / stopped / failed。
type ServiceState struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
	WebURL string `json:"webURL"`
}

func (a *App) GetServiceState() ServiceState {
	if serverResponding(webURL) {
		return ServiceState{State: "running", WebURL: webURL}
	}
	if serviceFailed.Load() {
		s, _ := serviceFailReason.Load().(string)
		return ServiceState{State: "failed", Reason: s, WebURL: webURL}
	}
	if serverReady.Load() {
		return ServiceState{State: "stopped", WebURL: webURL}
	}
	return ServiceState{State: "starting", WebURL: webURL}
}

// RestartService 重启后台服务（含 harness 自愈）。
// RestartService 重启后台服务（含 harness 自愈）。成功返回 true（服务就绪，
// 且已按需求弹出「是否打开 Web UI」询问）；失败返回 false（已弹错误提示）。
// 阻塞直到重启完成或失败，前端据此刷新按钮/状态。
func (a *App) RestartService() bool {
	logUI("重启后台服务", "")
	return restartBackgroundService(func(stage string) {
		wruntime.EventsEmit(appCtx, "service:restart", map[string]interface{}{"stage": stage})
	})
}

// ==================== 日志 ====================

// LogTail 日志增量读取：Lines 为新增行，NextOffset 为下一次读取的起点。
type LogTail struct {
	Lines      []string `json:"lines"`
	NextOffset int64    `json:"nextOffset"`
}

// LogFile 日志文件选项。
type LogFile struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Size   int64  `json:"size"`
}

// logFileName 规范化日志文件名（仅允许 app.log / server.log，防路径穿越）。
func logFileName(name string) string {
	if name == "server.log" {
		return "server.log"
	}
	return "app.log"
}

// GetLogFiles 返回可查看的日志文件列表。
func (a *App) GetLogFiles() []LogFile {
	out := make([]LogFile, 0, 2)
	for _, n := range []string{"app.log", "server.log"} {
		p := filepath.Join(logDir, n)
		fi, err := os.Stat(p)
		lf := LogFile{Name: n, Path: sanitizeShotPath(p)}
		if err == nil {
			lf.Exists = true
			lf.Size = fi.Size()
		}
		out = append(out, lf)
	}
	return out
}

func (a *App) GetLogPath(name string) string {
	return sanitizeShotPath(filepath.Join(logDir, logFileName(name)))
}

// ReadLogTail 从 offset 起增量读取指定日志（前端定时轮询；offset 超出文件长度时重置为 0）。
// 截图模式下返回固定演示日志，与真实环境完全隔离（防止截图泄露路径/主机名等敏感信息）。
func (a *App) ReadLogTail(name string, offset int64) LogTail {
	if shotMode {
		return LogTail{Lines: shotDemoLog, NextOffset: int64(len(strings.Join(shotDemoLog, "\n")) + 1)}
	}
	p := filepath.Join(logDir, logFileName(name))
	f, err := os.Open(p)
	if err != nil {
		return LogTail{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return LogTail{}
	}
	if offset > st.Size() || offset < 0 {
		offset = st.Size() // 文件被清空/截断：回到当前末尾
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return LogTail{}
	}
	buf := make([]byte, st.Size()-offset)
	n, _ := f.Read(buf)
	chunk := string(buf[:n])
	var lines []string
	for _, ln := range strings.Split(chunk, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln != "" {
			lines = append(lines, sanitizeShotLine(ln))
		}
	}
	return LogTail{Lines: lines, NextOffset: offset + int64(n)}
}

// ClearLog 清空指定日志。
func (a *App) ClearLog(name string) {
	logUI("清空日志", name)
	_ = os.WriteFile(filepath.Join(logDir, logFileName(name)), nil, 0o644)
}

// ==================== 更新 ====================

// Versions 关于页版本信息。
type Versions struct {
	App     string `json:"app"`
	Harness string `json:"harness"`
}

func (a *App) GetVersions() Versions {
	return Versions{
		App:     sanitizeShotVersion(appVersion),
		Harness: sanitizeShotLine(installedHarnessVersion()),
	}
}

// UpdateInfo 手动检查更新的结果：同时包含 dsh-systray 自身与 DeepSeek Harness 两个检查结果
// （harness 的版本选择已按「预发布通道」开关联动，见 pickHarnessVersion）。
type UpdateInfo struct {
	HasUpdate bool   `json:"hasUpdate"`
	Latest    string `json:"latest"`
	Current   string `json:"current"`
	Error     string `json:"error"`

	HarnessHasUpdate bool   `json:"harnessHasUpdate"`
	HarnessLatest    string `json:"harnessLatest"`
	HarnessCurrent   string `json:"harnessCurrent"`
}

// CheckUpdateManual 手动检查更新：dsh-systray 自身新版本 + DeepSeek Harness 最新版本（按预发布通道）。
// 不弹原生对话框，结果交前端展示。
func (a *App) CheckUpdateManual() UpdateInfo {
	info := UpdateInfo{}
	rel, err := fetchLatestRelease()
	if err != nil {
		info.Error = err.Error()
	} else if isNewerVersion(rel.TagName, appVersion) {
		info.HasUpdate = true
		info.Latest = rel.TagName
		info.Current = appVersion
	} else {
		info.Current = appVersion
	}
	harnessLatest, harnessCur, harnessNewer := queryHarnessUpdate()
	info.HarnessHasUpdate = harnessNewer
	info.HarnessLatest = harnessLatest
	info.HarnessCurrent = harnessCur
	logUI("手动检查更新", fmt.Sprintf("systray: %s -> %s | harness: %s -> %s",
		orDash(info.Current), orDash(map[bool]string{true: info.Latest, false: "最新"}[info.HasUpdate]),
		orDash(info.HarnessCurrent), orDash(map[bool]string{true: info.HarnessLatest, false: "最新"}[info.HarnessHasUpdate])))
	return info
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// StartHarnessUpdate 按最新版更新 DeepSeek Harness（快照/回滚/重启校验，进度走 splash）。
func (a *App) StartHarnessUpdate() {
	latest, _, newer := queryHarnessUpdate()
	if !newer || latest == "" {
		return
	}
	logUI("开始更新 DeepSeek Harness", "v"+latest)
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
	}
	go runHarnessUpdate(latest)
}

// StartUpdate 开始下载并应用更新（进度走 splash 事件，窗口自动显示）。
func (a *App) StartUpdate() {
	rel, err := fetchLatestRelease()
	if err != nil {
		showMessageBox("检查更新失败：\n"+err.Error(), appName)
		return
	}
	if !isNewerVersion(rel.TagName, appVersion) {
		return
	}
	logUI("开始更新 dsh-systray", rel.TagName)
	setSplashPhase("update")
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
	}
	go startUpdateApply(rel)
}

// CancelUpdate 取消进行中的更新（前端 splash 视图「取消更新」按钮）。
func (a *App) CancelUpdate() {
	logUI("取消更新", "")
	cancelActiveUpdate()
}

// ResetHarness 重置 DeepSeek Harness（前端已弹过警告确认）：
// 删除全部用户安装的插件并把 harness 回退到官方最后发布的稳定版本。
// 异步执行：进度走 splash 事件，完成/失败以弹窗提示（见 harness_reset.go）。
func (a *App) ResetHarness() {
	logUI("重置 DeepSeek Harness", "")
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
	}
	go runHarnessReset()
}

// ==================== 导出 ====================

// ExportOption 导出页可选项状态。
type ExportOption struct {
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Sub      string `json:"sub"`
	Selected bool   `json:"selected"`
}

// GetExportOptions 返回导出页三选项的默认状态（sessions 默认选中）。
func (a *App) GetExportOptions() []ExportOption {
	return []ExportOption{
		{Kind: "sessions", Label: "所有历史会话", Sub: "sessions.zip · ~/.dsh/sessions", Selected: true},
		{Kind: "plugins", Label: "已安装的插件", Sub: "plugins.zip · 通过 dsh add 安装的插件", Selected: false},
		{Kind: "files", Label: "需要打包的文件目录", Sub: "files.zip · 恢复时选择解压位置", Selected: false},
	}
}

// PickExportDir 让用户选择一个要打包的目录（添加到 files 列表）。
func (a *App) PickExportDir() (string, error) {
	p, err := wruntime.OpenDirectoryDialog(appCtx, wruntime.OpenDialogOptions{
		Title: "选择要打包的目录",
	})
	if err != nil {
		return "", err
	}
	if p != "" {
		logUI("选择导出目录", p)
	}
	return p, nil
}

// normalizeDirKey 目录去重键：Windows 忽略大小写，消除尾部分隔符差异。
func normalizeDirKey(d string) string {
	d = filepath.Clean(d)
	if runtime.GOOS == "windows" {
		d = strings.ToLower(d)
	}
	return d
}

// PickSavePath 让用户选择导出压缩包的保存位置（SaveFileDialog），返回完整文件路径；取消返回空。
func (a *App) PickSavePath() (string, error) {
	p, err := wruntime.SaveFileDialog(appCtx, wruntime.SaveDialogOptions{
		Title:                "选择导出压缩包的保存位置",
		DefaultFilename:      fmt.Sprintf("dsh-systray-export-%s.zip", time.Now().Format("20060102-150405")),
		DefaultDirectory:     homeDownloads(),
		CanCreateDirectories: true,
		Filters: []wruntime.FileFilter{
			{DisplayName: "ZIP 压缩包 (*.zip)", Pattern: "*.zip"},
		},
	})
	if err != nil {
		return "", err
	}
	return p, nil
}

// StartExport 开始导出：勾选项打包为 dsh-systray-export-*.zip。
// savePath 为导出压缩包的保存路径（用户通过 SaveFileDialog 选择）；为空时保存到系统下载目录。
// 目录列表做去重兜底；进度与结果通过事件 export:progress / export:done 推送。
func (a *App) StartExport(includeSessions, includePlugins, includeFiles bool, dirs []string, savePath string) {
	// 去重（与前端规则一致：Windows 大小写不敏感 + 消除尾分隔符）
	seen := map[string]bool{}
	uniq := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d == "" {
			continue
		}
		k := normalizeDirKey(d)
		if seen[k] {
			continue
		}
		seen[k] = true
		uniq = append(uniq, d)
	}
	logUI("开始导出", fmt.Sprintf("sessions=%v plugins=%v files=%v 目录数=%d 保存到=%s",
		includeSessions, includePlugins, includeFiles, len(uniq), orDash(strings.TrimSpace(savePath))))
	go func() {
		destFile := strings.TrimSpace(savePath)
		final, err := buildExportZip(includeSessions, includePlugins, includeFiles, uniq, homeDownloads(), func(t string, pct float64) {
			if appCtx != nil {
				wruntime.EventsEmit(appCtx, "export:progress", map[string]interface{}{"text": t, "pct": pct})
			}
		}, destFile)
		if appCtx == nil {
			return
		}
		if err != nil {
			logUI("导出失败", err.Error())
			wruntime.EventsEmit(appCtx, "export:done", map[string]interface{}{"error": err.Error()})
			return
		}
		logUI("导出完成", final)
		wruntime.EventsEmit(appCtx, "export:done", map[string]interface{}{"path": final})
	}()
}

// OpenExportDir 打开导出包所在目录（需求：导出完成后便于核对落盘位置）。
func (a *App) OpenExportDir(path string) {
	if path == "" {
		return
	}
	logUI("打开导出目录", filepath.Dir(path))
	openDir(filepath.Dir(path))
}

// ==================== 导入 ====================

// ImportItem 解析出的可恢复项。
type ImportItem struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Size  int64  `json:"size"`
}

var (
	importZipPath string
	importItems   []importItem
	// pendingRestore PreviewRestore 成功后暂存的内层子包临时文件（ApplyRestore 消费后清理）。
	// 一次性只允许一个待恢复项：恢复按钮逐个操作，前端 await Preview → Apply 串行完成。
	pendingRestore struct {
		mu        sync.Mutex
		kind      string
		innerZip  string
		innerCln  func()
		filesDest string
	}
)

// RestorePreview 恢复前的冲突预览（前端据此弹窗询问是否覆盖）。
type RestorePreview struct {
	Canceled  bool     `json:"canceled"`  // files 目录选择被取消，或对话框未选
	Conflict  bool     `json:"conflict"`  // 检测到与现有内容冲突
	Conflicts int      `json:"conflicts"` // 冲突项数
	Tops      []string `json:"tops"`      // 冲突顶层名称（最多列前 5 个）
	Error     string   `json:"error"`     // 准备失败原因
}

// ImportPickResult 选择压缩包后的结果（完整路径 + 可恢复项）。
type ImportPickResult struct {
	Path  string       `json:"path"`
	Items []ImportItem `json:"items"`
}

// ImportPick 选择导入压缩包并解析（原生文件对话框）。
func (a *App) ImportPick() (*ImportPickResult, error) {
	p, err := wruntime.OpenFileDialog(appCtx, wruntime.OpenDialogOptions{
		Title: "选择 dsh-systray 导出压缩包",
		Filters: []wruntime.FileFilter{
			{DisplayName: "dsh-systray 导出包 (*.zip)", Pattern: "*.zip"},
			{DisplayName: "ZIP 压缩包 (*.zip)", Pattern: "*.zip"},
			{DisplayName: "所有文件 (*.*)", Pattern: "*.*"},
		},
	})
	if err != nil || p == "" {
		return nil, err
	}
	items, err := parseExportZip(p)
	if err != nil {
		return nil, err
	}
	logUI("选择导入压缩包", fmt.Sprintf("%s（%d 项）", p, len(items)))
	importZipPath = p
	importItems = items
	out := make([]ImportItem, 0, len(items))
	for _, it := range items {
		out = append(out, ImportItem{Kind: it.Kind, Label: it.Label, Size: it.Size})
	}
	return &ImportPickResult{Path: p, Items: out}, nil
}

// GetImportItems 返回当前已解析的导入项。
func (a *App) GetImportItems() []ImportItem {
	out := make([]ImportItem, 0, len(importItems))
	for _, it := range importItems {
		out = append(out, ImportItem{Kind: it.Kind, Label: it.Label, Size: it.Size})
	}
	return out
}

// PreviewRestore 恢复前的准备与冲突检测（供前端弹窗询问是否覆盖）：
//  1. files 类目先让用户选择解压位置（取消 → Canceled）；
//  2. 从总 zip 解出内层子包到临时文件并暂存（ApplyRestore 消费后清理）；
//  3. 统计与目标位置的冲突项：sessions/plugins 与 DSH_HOME 下对应位置比对，
//     files 与用户所选解压位置比对。
//
// 冲突 >0 时前端弹「检测到冲突，是否覆盖」；随后调 ApplyRestore(kind, overwrite)。
func (a *App) PreviewRestore(kind string) RestorePreview {
	pv := RestorePreview{}
	if importZipPath == "" {
		pv.Error = "尚未选择导出压缩包"
		return pv
	}
	var target *importItem
	for i := range importItems {
		if importItems[i].Kind == kind {
			target = &importItems[i]
			break
		}
	}
	if target == nil {
		pv.Error = "导出包中没有可恢复的该项"
		return pv
	}
	filesDest := ""
	if kind == "files" {
		p, err := wruntime.OpenDirectoryDialog(appCtx, wruntime.OpenDialogOptions{Title: "选择解压位置"})
		if err != nil || p == "" {
			pv.Canceled = true
			return pv
		}
		filesDest = p
	}
	innerPath, cln, err := extractInnerZip(importZipPath, target.Zip)
	if err != nil {
		pv.Error = "解出恢复内容失败：" + err.Error()
		return pv
	}
	pendingRestore.mu.Lock()
	if pendingRestore.innerCln != nil {
		pendingRestore.innerCln() // 清理上一次未消费的残留临时文件
	}
	pendingRestore.kind = kind
	pendingRestore.innerZip = innerPath
	pendingRestore.innerCln = cln
	pendingRestore.filesDest = filesDest
	pendingRestore.mu.Unlock()
	logUI("预览恢复导入项", fmt.Sprintf("kind=%s", kind))

	var n int
	var tops []string
	if kind == "files" {
		n, tops, err = topDirConflicts(innerPath, filesDest)
	} else {
		n, err = countRestoreConflicts(kind, innerPath)
		if err == nil && n > 0 {
			if ct, cerr := conflictTops(kind, innerPath); cerr == nil {
				for _, t := range ct {
					tops = append(tops, t)
				}
			}
		}
	}
	if err != nil {
		pv.Error = "检测冲突失败：" + err.Error()
		return pv
	}
	if len(tops) > 5 {
		tops = tops[:5]
	}
	pv.Conflicts = n
	pv.Conflict = n > 0
	pv.Tops = tops
	return pv
}

// ApplyRestore 执行恢复（须先 PreviewRestore 同 kind 成功）。
// overwrite=true 覆盖冲突项（冲突顶层先改名备份，失败回滚）；false 跳过已有。
// sessions/plugins 恢复前暂停后台服务、完成后自动重新拉起并刷新状态；
// 插件恢复成功后把源 profile 的 dependencies/bundles 合并写回目标 profile，使插件被 harness 识别。
// 进度与结果通过事件 import:progress / import:done 推送。
func (a *App) ApplyRestore(kind string, overwrite bool) {
	pendingRestore.mu.Lock()
	ok := pendingRestore.kind == kind && pendingRestore.innerZip != ""
	innerPath := pendingRestore.innerZip
	filesDest := pendingRestore.filesDest
	cln := pendingRestore.innerCln
	if ok {
		pendingRestore.kind = ""
		pendingRestore.innerZip = ""
		pendingRestore.innerCln = nil
		pendingRestore.filesDest = ""
	}
	pendingRestore.mu.Unlock()
	if !ok {
		logUI("恢复导入项被跳过", "尚未预览或 kind 不匹配: "+kind)
		return
	}
	logUI("恢复导入项", fmt.Sprintf("kind=%s overwrite=%v", kind, overwrite))
	go func() {
		defer func() {
			if cln != nil {
				cln() // 清理内层子包临时文件
			}
		}()
		// 恢复 sessions/plugins 前暂停后台服务，避免写入运行中的 harness 环境
		paused := false
		if kind != "files" {
			paused = pauseServiceForRestore()
		}
		_, err := restoreItem(kind, innerPath, filesDest, overwrite, func(t string, pct float64) {
			if appCtx != nil {
				wruntime.EventsEmit(appCtx, "import:progress", map[string]interface{}{"kind": kind, "text": t, "pct": pct})
			}
		})
		if err == nil && kind == "plugins" {
			// 插件文件已就位：把依赖/ bundles 写回目标 profile 的 package.json，否则 harness 不识别
			err = registerRestoredPlugins(importZipPath)
		}
		if paused {
			resumeServiceAfterRestore()
		}
		if appCtx == nil {
			return
		}
		if err != nil {
			logUI("恢复失败", err.Error())
			wruntime.EventsEmit(appCtx, "import:done", map[string]interface{}{"kind": kind, "error": err.Error()})
			return
		}
		logUI("恢复完成", "kind="+kind)
		wruntime.EventsEmit(appCtx, "import:done", map[string]interface{}{"kind": kind, "ok": true})
	}()
}

// ==================== 杂项 ====================

// OpenWebUI 打开 harness Web 界面。
func (a *App) OpenWebUI() {
	if serverResponding(webURL) {
		openBrowser(webURL)
	}
}

// PickHarnessDir 让用户选择 harness 源码/安装目录。
// 防重复弹窗：选择窗口打开期间再次点击直接返回空（不新开窗口）。
var harnessPickBusy atomic.Bool

func (a *App) PickHarnessDir() string {
	if !harnessPickBusy.CompareAndSwap(false, true) {
		return ""
	}
	defer harnessPickBusy.Store(false)
	logUI("打开 Harness 目录选择窗口", "")
	return pickHarnessDir("选择 DeepSeek Harness 目录", harnessDir)
}

// OpenLogDir 打开日志目录（资源管理器/Finder）。
func (a *App) OpenLogDir() {
	if logDir == "" {
		return
	}
	logUI("打开日志目录", logDir)
	openDir(logDir)
}

// HideWindow 前端请求隐藏窗口（如 splash 完成按钮）。
func (a *App) HideWindow() {
	hideMainWindow()
}

// GetShotPage 调试用：DSH_SYSTRAY_SHOT_PAGE 指定启动后直接显示的页面（截图/预览）。
func (a *App) GetShotPage() string {
	return os.Getenv("DSH_SYSTRAY_SHOT_PAGE")
}

// homeDownloads 返回系统下载目录。
// 优先使用平台层解析的真实「下载」位置（Windows 走 Known Folder，避免 OneDrive/重定向
// 导致导出包落到空壳目录或 %TEMP% 而找不到）；依次回退 ~/Downloads → 系统临时目录。
func homeDownloads() string {
	if d := downloadsDir(); d != "" {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, "Downloads")
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p
		}
	}
	return os.TempDir()
}

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// sanitizeShotHarnessDir 常规页 Harness 目录：截图模式下显示通用示例路径（不暴露真实位置）。
func sanitizeShotHarnessDir() string {
	if runtime.GOOS == "darwin" {
		return "/Users/demo/deepseek-harness"
	}
	return filepath.Join("D:", "deepseek-harness")
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
	setAutostartOn(on)
}

func (a *App) SetHarnessPrerelease(on bool) {
	harnessPrereleaseOverride = on
	saveCurrentConfig()
}

func (a *App) SetHarnessDir(d string) {
	d = strings.TrimSpace(d)
	if d == "" {
		return
	}
	harnessDir = d
	harnessDirExplicit = true
	saveCurrentConfig()
}

func (a *App) SetUpdateMirror(m string) {
	updateMirrorOverride = strings.TrimSpace(m)
	saveCurrentConfig()
}

func (a *App) SetPort(p int) {
	if p <= 0 || p > 65535 {
		return
	}
	port = p
	webURL = fmt.Sprintf("http://127.0.0.1:%d/", p)
	saveCurrentConfig()
}

func (a *App) SetStartupTimeoutSec(s int) {
	if s <= 0 {
		return
	}
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
func (a *App) RestartService() {
	restartBackgroundService(func(stage string) {
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
	Name    string `json:"name"`
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Size    int64  `json:"size"`
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
func (a *App) ReadLogTail(name string, offset int64) LogTail {
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

// UpdateInfo 手动检查更新的结果。
type UpdateInfo struct {
	HasUpdate bool   `json:"hasUpdate"`
	Latest    string `json:"latest"`
	Current   string `json:"current"`
	Error     string `json:"error"`
}

// CheckUpdateManual 手动检查 dsh-systray 自身新版本（不弹原生对话框，结果交前端展示）。
func (a *App) CheckUpdateManual() UpdateInfo {
	rel, err := fetchLatestRelease()
	if err != nil {
		log.Printf("manual update check failed: %v", err)
		return UpdateInfo{Error: err.Error()}
	}
	if isNewerVersion(rel.TagName, appVersion) {
		return UpdateInfo{HasUpdate: true, Latest: rel.TagName, Current: appVersion}
	}
	return UpdateInfo{Current: appVersion}
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
	setSplashPhase("update")
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
	}
	go startUpdateApply(rel)
}

// CancelUpdate 取消进行中的更新（前端 splash 视图「取消更新」按钮）。
func (a *App) CancelUpdate() {
	cancelActiveUpdate()
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
	return p, nil
}

// StartExport 开始导出：勾选项打包为 dsh-systray-export-*.zip 保存到 Downloads。
// 进度与结果通过事件 export:progress / export:done 推送。
func (a *App) StartExport(includeSessions, includePlugins, includeFiles bool, dirs []string) {
	go func() {
		dest := filepath.Join(homeDownloads())
		if _, err := os.Stat(dest); err != nil {
			dest = os.TempDir()
		}
		final, err := buildExportZip(includeSessions, includePlugins, includeFiles, dirs, dest, func(t string, pct float64) {
			if appCtx != nil {
				wruntime.EventsEmit(appCtx, "export:progress", map[string]interface{}{"text": t, "pct": pct})
			}
		})
		if appCtx == nil {
			return
		}
		if err != nil {
			wruntime.EventsEmit(appCtx, "export:done", map[string]interface{}{"error": err.Error()})
			return
		}
		wruntime.EventsEmit(appCtx, "export:done", map[string]interface{}{"path": final})
	}()
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
)

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

// StartImport 恢复单个导入项；files 类目先让用户选择解压位置。
// 进度与结果通过事件 import:progress / import:done 推送。
func (a *App) StartImport(kind string, overwrite bool) {
	if importZipPath == "" {
		return
	}
	var target *importItem
	for i := range importItems {
		if importItems[i].Kind == kind {
			target = &importItems[i]
			break
		}
	}
	if target == nil {
		return
	}
	filesDest := ""
	if kind == "files" {
		p, err := wruntime.OpenDirectoryDialog(appCtx, wruntime.OpenDialogOptions{Title: "选择解压位置"})
		if err != nil || p == "" {
			return
		}
		filesDest = p
	}
	go func() {
		zipp := importZipPath
		_, err := restoreItem(kind, zipp, filesDest, overwrite, func(t string, pct float64) {
			if appCtx != nil {
				wruntime.EventsEmit(appCtx, "import:progress", map[string]interface{}{"kind": kind, "text": t, "pct": pct})
			}
		})
		if appCtx == nil {
			return
		}
		if err != nil {
			wruntime.EventsEmit(appCtx, "import:done", map[string]interface{}{"kind": kind, "error": err.Error()})
			return
		}
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
func (a *App) PickHarnessDir() string {
	return pickHarnessDir("选择 DeepSeek Harness 目录", harnessDir)
}

// OpenLogDir 打开日志目录（资源管理器/Finder）。
func (a *App) OpenLogDir() {
	if logDir == "" {
		return
	}
	openBrowser(logDir)
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
func homeDownloads() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	return filepath.Join(home, "Downloads")
}

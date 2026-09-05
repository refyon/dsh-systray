package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
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

// shotDemoLog 截图模式展示的中性演示日志（与真实环境完全隔离，避免泄露路径/主机名等信息；
// 行格式与统一日志一致：时间戳 [LEVEL] [module] message）。
var shotDemoLog = []string{
	"2026-08-11 09:12:01 [INFO] [app] dsh-systray v0.5.2 启动完成",
	"2026-08-11 09:12:03 [INFO] [app] 后台服务已就绪：http://127.0.0.1:3080/",
	"2026-08-11 09:12:05 [INFO] [app] 已检测到 3 个历史会话",
	"2026-08-11 09:12:07 [WARN] [app] 日志文件较大，已截断显示最近 4000 行",
	"2026-08-11 09:12:10 [INFO] [app] 导出完成：dsh-systray-export-20260811-091210-1a2b3c4d.zip",
	"2026-08-11 09:12:12 [INFO] [app] 已是最新版本（v0.5.2）",
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

// logUI 把设置页用户操作写入统一日志（模块 ui，等级按内容判定；便于检索与追溯）。
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
// RunningPort：服务当前实际运行端口（0 = 未在运行）；修改端口但未重启时，
// 该值与 ConfigInfo.Port（设置端口）不一致，前端据此显示「重启后生效」提示。
type ServiceState struct {
	State       string `json:"state"`
	Reason      string `json:"reason"`
	WebURL      string `json:"webURL"`
	RunningPort int    `json:"runningPort"`
}

func (a *App) GetServiceState() ServiceState {
	// 实际端口优先：配置端口或本进程最后启动的端口（端口已改未重启场景）
	if running, rp, live := resolveRunningService(); running {
		return ServiceState{State: "running", WebURL: live, RunningPort: rp}
	}
	// 未运行：RunningPort 回传「最后运行端口」（0=从未启动过），供前端判断端口是否需重启生效
	last := serverStartedPort
	if serviceFailed.Load() {
		s, _ := serviceFailReason.Load().(string)
		return ServiceState{State: "failed", Reason: s, WebURL: webURL, RunningPort: last}
	}
	if serverReady.Load() {
		return ServiceState{State: "stopped", WebURL: webURL, RunningPort: last}
	}
	return ServiceState{State: "starting", WebURL: webURL, RunningPort: last}
}

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

// logNameAllowed 校验日志文件名：仅允许 logDir 下单个 *.log 文件。
// 拒绝空名、".." 与任何路径分隔符（防路径穿越）。动态枚举与外部 name 参数共用此校验。
func logNameAllowed(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(name), ".log")
}

// logFilePath 返回 logDir 下校验通过的日志文件完整路径；name 非法时返回空串。
func logFilePath(name string) string {
	if !logNameAllowed(name) {
		return ""
	}
	return filepath.Join(logDir, name)
}

// GetLogFiles 返回日志页可查看的日志文件列表：统一日志后仅 dsh-systray.log 一项
// （所有行为与子进程输出合并写入该文件；行内带时间戳/等级/模块，前端按等级着色）。
func (a *App) GetLogFiles() []LogFile {
	p := unifiedLogPath()
	lf := LogFile{Name: unifiedLogName, Path: sanitizeShotPath(p), Exists: false}
	if fi, err := os.Stat(p); err == nil {
		lf.Exists = true
		lf.Size = fi.Size()
	}
	return []LogFile{lf}
}

func (a *App) GetLogPath(name string) string {
	p := logFilePath(name)
	if p == "" {
		return ""
	}
	return sanitizeShotPath(p)
}

// ReadLogTail 从 offset 起增量读取指定日志（前端定时轮询；offset 超出文件长度时重置为 0）。
// 截图模式下返回固定演示日志，与真实环境完全隔离（防止截图泄露路径/主机名等敏感信息）。
func (a *App) ReadLogTail(name string, offset int64) LogTail {
	if shotMode {
		return LogTail{Lines: shotDemoLog, NextOffset: int64(len(strings.Join(shotDemoLog, "\n")) + 1)}
	}
	p := logFilePath(name)
	if p == "" {
		return LogTail{}
	}
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
	if !logNameAllowed(name) {
		return
	}
	logUI("清空日志", name)
	_ = os.WriteFile(filepath.Join(logDir, name), nil, 0o644)
}

// ==================== 更新 ====================

// Versions 关于页版本信息。
type Versions struct {
	App     string `json:"app"`
	Harness string `json:"harness"`
}

func (a *App) GetVersions() Versions {
	hv := installedHarnessVersion()
	if shotMode && hv == "" {
		hv = "0.1.1" // 截图模式且本机无 harness 目录时给出中性演示版本
	}
	return Versions{
		App:     sanitizeShotVersion(withV(appVersion)),
		Harness: sanitizeShotLine(hv),
	}
}

// ModuleUpdate 单一模块（dsh-systray / DeepSeek Harness）手动检查更新的结果。
type ModuleUpdate struct {
	Current   string `json:"current"` // 当前已装版本（获取失败可能为空）
	Latest    string `json:"latest"`  // 远端最新可用版本
	HasUpdate bool   `json:"hasUpdate"`
	Note      string `json:"note"`  // 非网络失败场景的说明（如“仓库仅有预发布而通道未开”）；空为无
	Error     string `json:"error"` // 检查失败原因（网络等）
}

// CheckSystrayUpdate 检查 dsh-systray 自身是否有新版本（GitHub Release latest）。不弹原生对话框。
func (a *App) CheckSystrayUpdate() ModuleUpdate {
	info := ModuleUpdate{Current: appVersion}
	if appVersion == "" || appVersion == "dev" {
		info.Error = "当前为开发版本（dev），未启用版本检查。"
		return info
	}
	rel, err := fetchLatestRelease()
	if err != nil {
		info.Error = err.Error()
	} else if isNewerVersion(rel.TagName, appVersion) {
		info.HasUpdate = true
		info.Latest = rel.TagName
	} else {
		info.Latest = withV(appVersion)
	}
	logUI("检查 dsh-systray 更新", fmt.Sprintf("当前 %s | %s", withV(info.Current), moduleUpdateLogText(&info)))
	return info
}

// CheckHarnessUpdate 检查 DeepSeek Harness 是否有新版本（GitHub Release，按「预发布通道」开关联动）。
// 仅网络等真实失败才返回 Error；仓库只有预发布而通道关闭时返回 Note 说明（不再误报“无法获取”）。
func (a *App) CheckHarnessUpdate() ModuleUpdate {
	latest, cur, newer, note := queryHarnessUpdate()
	info := ModuleUpdate{Current: cur, Latest: latest, HasUpdate: newer, Note: note}
	if latest == "" && note == "" {
		info.Error = "无法获取 DeepSeek Harness 最新版本，请检查网络后重试。"
	}
	logUI("检查 Harness 更新", fmt.Sprintf("当前 %s | %s", orDash(cur), moduleUpdateLogText(&info)))
	return info
}

// moduleUpdateLogText 检查结果的日志摘要文本。
func moduleUpdateLogText(info *ModuleUpdate) string {
	switch {
	case info.Error != "":
		return "失败：" + info.Error
	case info.HasUpdate:
		return "有新版本 " + withV(info.Latest)
	case info.Note != "":
		return "已是最新（" + info.Note + "）"
	default:
		return "已是最新"
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// StartHarnessUpdate 按最新版更新 DeepSeek Harness（快照/回滚/重启校验，进度走 splash）。
func (a *App) StartHarnessUpdate() {
	latest, _, newer, _ := queryHarnessUpdate()
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
	startUpdateApplyWithUI(rel)
}

// startUpdateApplyWithUI 自动检查与手动路径共用的更新入口：显示主窗口并置前后，异步执行
// 下载安装。设置窗口通常 StartHidden 启动——自动检查路径若只调 startUpdateApply，进度事件
// 只发向隐藏的前端，用户点击「立即更新」后看不到任何 UI 变化（表现为"点击更新没有反应"）。
func startUpdateApplyWithUI(rel *latestRelease) {
	log.Printf("update apply confirmed, showing progress for %s", rel.TagName)
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
		ensureMainWindowForeground()
	}
	setSplashPhase("update")
	go startUpdateApply(rel)
}

// CancelUpdate 取消进行中的更新（前端 splash 视图「取消更新」按钮）。
func (a *App) CancelUpdate() {
	logUI("取消更新", "")
	cancelActiveUpdate()
}

// ==================== 插件检查 / 更新（关于页「插件」卡片，按插件单独操作） ====================

// GetInstalledPlugins 罗列用户通过 dsh add / dsh web add 安装的插件：
// 含当前已装版本、安装来源（npm/github/file/tarball）与可否更新。
// 截图模式返回演示清单（不暴露真实路径/来源）。
func (a *App) GetInstalledPlugins() []PluginRow {
	if shotMode {
		return shotPlugins()
	}
	return buildPluginRows()
}

// CheckPluginUpdate 检查单个插件是否有新版本（纯查询，结果交前端行内展示）：
// npm registry 安装查 dist-tag latest；github 安装按默认分支 package.json 版本判定。
func (a *App) CheckPluginUpdate(id string) PluginCheckResult {
	if shotMode {
		return shotPluginCheck(id)
	}
	row, ok := findPluginRowByID(id)
	if !ok {
		return PluginCheckResult{Name: id, Error: "未找到该插件，可能已被移除。"}
	}
	res := checkPluginUpdateByRow(row)
	state := "已是最新"
	if res.Error != "" {
		state = "失败：" + res.Error
	} else if res.HasUpdate {
		state = fmt.Sprintf("有新版本 %s", withV(res.Latest))
	}
	logUI("检查插件更新", fmt.Sprintf("%s（当前 %s）| %s", res.Name, orDash(res.Current), state))
	return res
}

// StartPluginUpdate 更新指定插件到最新版（splash 进度，完成/失败弹窗，失败自动回退）。
func (a *App) StartPluginUpdate(id string) {
	logUI("更新插件", id)
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
	}
	go runPluginUpdate(id)
}

// PickLocalPluginPath 本地插件「选择目录更新」第一步：目录选择框 → 校验为同一插件 →
// 返回所选目录版本与相对当前版本的关系（前端据此提示「覆盖更新」或「已是最新」）。
// 目录选择被取消返回 Canceled=true；本地来源以外的插件不提供该入口。
func (a *App) PickLocalPluginPath(id string) PluginLocalPick {
	if shotMode {
		return PluginLocalPick{Canceled: true}
	}
	row, ok := findPluginRowByID(id)
	if !ok {
		return PluginLocalPick{Error: "未找到该插件，可能已被移除。"}
	}
	p, err := wruntime.OpenDirectoryDialog(appCtx, wruntime.OpenDialogOptions{
		Title: "选择插件 " + row.Name + " 的新版本目录",
	})
	if err != nil || p == "" {
		return PluginLocalPick{Canceled: true}
	}
	name, ver, err := packageMeta(p)
	if err != nil {
		return PluginLocalPick{Path: p, Error: err.Error()}
	}
	if name != row.Name {
		return PluginLocalPick{Path: p, Error: fmt.Sprintf(
			"所选目录不是插件 %s（目录 package.json 的 name=%s）", row.Name, name)}
	}
	logUI("选择本地插件目录", fmt.Sprintf("%s（当前 v%s → 所选 v%s）", row.Name, orDash(row.Version), orDash(ver)))
	return PluginLocalPick{Path: p, Version: ver, Current: row.Version, Relation: localPickRelation(row.Version, ver)}
}

// ApplyLocalPluginUpdate 把本地插件覆盖更新为所选目录（前端确认后调用）。执行中服务短暂
// 重启，失败自动回退到更新前版本。
func (a *App) ApplyLocalPluginUpdate(id, dir string) {
	if shotMode {
		return
	}
	row, ok := findPluginRowByID(id)
	if !ok {
		showMessageBox("未找到该插件，可能已被移除。", appName)
		return
	}
	name, ver, err := packageMeta(dir)
	if err != nil {
		showMessageBox("无法更新本地插件：\n"+err.Error(), appName)
		return
	}
	if name != row.Name {
		showMessageBox(fmt.Sprintf("无法更新本地插件：\n所选目录不是插件 %s（目录 package.json 的 name=%s）。", row.Name, name), appName)
		return
	}
	logUI("更新本地插件", fmt.Sprintf("%s → %s（v%s）", row.Name, dir, orDash(ver)))
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
	}
	go runLocalPluginUpdate(row, dir)
}

// EnablePlugin 手动启用被禁用的插件（前端「启用」按钮）：
// 清除禁用记录并加回 bundles → 重启健康校验；若启用后仍不兼容（启动日志点名该插件），
// 自动重新禁用并重启服务——保证最终服务可启动。结果以弹窗提示。
func (a *App) EnablePlugin(id string) {
	if shotMode {
		return
	}
	row, ok := findPluginRowByID(id)
	if !ok {
		showMessageBox("未找到该插件，可能已被移除。", appName)
		return
	}
	if !row.Disabled {
		return
	}
	logUI("启用插件", row.Name)
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
	}
	go runPluginEnable(row)
}

// runPluginEnable 执行启用（见 enablePluginAndVerify）：失败自动重新禁用并重启，保证服务可启动。
func runPluginEnable(row PluginRow) {
	enabled, why := enablePluginAndVerify(row)
	if enabled {
		logUI("启用插件完成", row.Name)
		showMessageBox(fmt.Sprintf("插件 %s 已启用，服务已重启。", row.Name), appName)
	} else {
		logUI("启用插件失败，已自动重新禁用", fmt.Sprintf("%s：%s", row.Name, why))
		showMessageBox(fmt.Sprintf("插件 %s 启用失败（仍与当前版本不兼容），已自动重新禁用并重启服务。\n原因：%s\n\n"+
			"可先「检查更新」到兼容版本，或确认插件已修复后再尝试启用。", row.Name, why), appName)
	}
	if appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugins:changed", nil)
	}
}

// RemovePlugin 删除指定插件（前端确认后调用）：物理移除依赖与文件，失败自动回退。
// 覆盖该插件声明的全部环境/profile。截图模式不执行真实删除。
func (a *App) RemovePlugin(id string) {
	if shotMode {
		return
	}
	logUI("删除插件", id)
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
	}
	go runPluginRemove(id)
}

// ResetStats 重置弹窗展示的将清除内容数量（供用户勾选前参考）。
type ResetStats struct {
	SessionCount int `json:"sessionCount"` // 将清除的会话记录数
	PluginCount  int `json:"pluginCount"`  // 将清除的已安装插件数
}

// GetResetStats 返回重置弹窗所需的统计：会话记录数量、已安装插件数量。
func (a *App) GetResetStats() ResetStats {
	return ResetStats{
		SessionCount: countResetSessions(),
		PluginCount:  countInstalledPlugins(),
	}
}

// ResetHarness 重置 DeepSeek Harness（前端勾选弹窗确认后调用）：
// harness 版本回退始终执行（必选项）；targetVersion 为用户从「重置目标版本」下拉选择的、
// 早于当前运行版本的官方版本（空 = 边界降级放行，按官方默认目标执行）；
// clearSessions / clearPlugins 按勾选物理删除对应数据（会话记录 / 已安装插件）。
// 异步执行：进度走 splash 事件，完成/失败以弹窗提示。
func (a *App) ResetHarness(clearSessions, clearPlugins bool, targetVersion string) {
	logUI("重置服务", fmt.Sprintf("clearSessions=%v clearPlugins=%v target=%s", clearSessions, clearPlugins, orDash(targetVersion)))
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
	}
	go runHarnessReset(clearSessions, clearPlugins, targetVersion)
}

// GetResetVersions 返回重置弹窗「重置目标版本」下拉所需数据：当前已装版本、全部候选
// （仅早于当前版本的官方 npm 版本，按新→旧）、默认选中与边界说明。源码形态不支持重置。
// 查询失败（网络/registry）时 Options 为空、Note 携带原因，前端据此禁用确认并提示。
func (a *App) GetResetVersions() ResetVersionInfo {
	if isSourceHarnessDir() {
		return ResetVersionInfo{Form: "source",
			Note: "当前为源码 checkout 形态，暂不支持自动清空目录重装；请先在 Web UI 切换到 npm 预构建形态。"}
	}
	if shotMode {
		// 截图模式：不触网，给出静态中性候选（与真实版本完全隔离）
		return ResetVersionInfo{Form: "npm", Current: "0.1.1",
			Options: []ResetVersionOption{{Version: "0.1.0", Prerelease: false}}, Default: "0.1.0"}
	}
	cur := installedHarnessVersion()
	versions, err := npmHarnessPublishedVersions()
	if err != nil {
		return ResetVersionInfo{Form: "npm", Current: cur, Note: "查询 npm 已发布版本失败：" + err.Error()}
	}
	opts, def := buildResetVersionOptions(versions, cur)
	info := ResetVersionInfo{Form: "npm", Current: cur, Options: opts, Default: def}
	if cur == "" {
		info.Note = "未能识别当前已装版本，已列出全部官方版本供选择。"
	} else if len(opts) == 0 {
		info.Note = "当前已是最早的官方已发布版本（无更早目标），将按官方默认目标执行重置。"
	}
	return info
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
		// 每个打包阶段（会话/插件/文件/压缩）的首条进度也写入统一日志，便于事后排查
		lastPhase := ""
		final, err := buildExportZip(includeSessions, includePlugins, includeFiles, uniq, homeDownloads(), func(t string, pct float64) {
			if t != "" && t != lastPhase {
				lastPhase = t
				log.Printf("export phase: %s", t)
			}
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

// OpenExportDir 打开导出包所在目录并在资源管理器中选中该文件
// （需求：导出完成后便于核对落盘位置）。路径不存在时按目录尝试打开。
func (a *App) OpenExportDir(path string) {
	if path == "" {
		return
	}
	logUI("打开导出目录", filepath.Dir(path))
	revealFile(path)
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
	if shotMode {
		// 截图模式：直接返回演示可恢复项（不弹文件对话框、不暴露真实路径）
		return &ImportPickResult{
			Path: "dsh-systray-export-20260903-091210-1a2b3c4d.zip",
			Items: []ImportItem{
				{Kind: "sessions", Label: "历史会话记录", Size: 2482124},
				{Kind: "plugins", Label: "已安装插件", Size: 1892356},
				{Kind: "files", Label: "自选文件目录", Size: 128512000},
			},
		}, nil
	}
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
	log.Printf("import preview result: kind=%s conflicts=%d tops=%v err=%v", kind, n, tops, err)
	return pv
}

// ApplyRestore 执行恢复（须先 PreviewRestore 同 kind 成功）。
// overwrite=true 覆盖冲突项（冲突顶层先改名备份，失败/取消回滚）；false 跳过已有。
// sessions/plugins 恢复前暂停后台服务、完成后自动重新拉起并刷新状态；
// 插件恢复成功后把源 profile 的 dependencies/bundles 合并写回目标 profile，使插件被 harness 识别；
// 合并后对本地链接依赖做可用性改写、pnpm 对齐失败时按依赖摘除重试一次（跨机恢复硬失败自愈）。
// 任务期间占用恢复槽（其余「恢复」请求被跳过）；CancelRestore 可请求中断解压并自动回退。
// 进度与结果通过事件 import:progress / import:done 推送（done 含 ok/error/canceled/note）。
func (a *App) ApplyRestore(kind string, overwrite bool) {
	logUI("恢复导入项", fmt.Sprintf("kind=%s overwrite=%v", kind, overwrite))
	if ok, why := importEnqueue(kind, overwrite); !ok {
		logUI("恢复导入项被跳过", why)
		if appCtx != nil {
			wruntime.EventsEmit(appCtx, "import:done", map[string]interface{}{"kind": kind, "error": why})
		}
	}
}

// CancelRestore 取消指定 kind 的恢复任务（每行独立取消）。解压/对齐阶段在检查点响应
// （对齐中的 pnpm 立即终止进程树）；已进入（共享）自愈阶段时请求被忽略——不可中断。
// 返回：ok=已受理；healing=自愈阶段不可取消；idle=无该 kind 任务。
func (a *App) CancelRestore(kind string) string {
	return importCancelKind(kind)
}

// ==================== 杂项 ====================

// webTokenRe 匹配 dsh web 启动时打印的访问 URL（含最新鉴权 token）。
var webTokenRe = regexp.MustCompile(`(?m)dsh web:\s*(https?://\S+)`)

// webTokenURL 取 dsh web 当前（带最新 token 的）URL：dsh web 每次启动都会换新 token，
// 自愈/重启后旧 token 失效会报 "authentication required; reopen the URL printed by dsh web"。
// 按需尾部扫描统一日志（含 .1 轮转档，各最多读尾 512KB）取最新一条；取不到回退 webURL。
func webTokenURL() string {
	for _, p := range []string{unifiedLogPath(), unifiedLogPath() + ".1"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		start := int64(0)
		if st, serr := f.Stat(); serr == nil && st.Size() > 512*1024 {
			start = st.Size() - 512*1024
		}
		if _, serr := f.Seek(start, io.SeekStart); serr != nil {
			f.Close()
			continue
		}
		b, rerr := io.ReadAll(f)
		f.Close()
		if rerr != nil {
			continue
		}
		ms := webTokenRe.FindAllSubmatch(b, -1)
		if len(ms) > 0 {
			return string(ms[len(ms)-1][1])
		}
	}
	return webURL
}

// OpenWebUI 打开 harness Web 界面（带最新鉴权 token，兼容“修改端口未重启/自愈重启后
// token 轮换”两个窗口期）。
func (a *App) OpenWebUI() {
	if running, _, _ := resolveRunningService(); running {
		openBrowser(webTokenURL())
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

// GetShotScroll 调试用：DSH_SYSTRAY_SHOT_SCROLL 指定截图时内容区的滚动量
// （"bottom"=滚到底部；正整数=像素；空=不滚动）。
func (a *App) GetShotScroll() string {
	return os.Getenv("DSH_SYSTRAY_SHOT_SCROLL")
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

package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"dsh-systray/internal/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

// isWailsBindingsProcess 判断当前进程是否为 wails 绑定收集器（wailsbindings.exe）。
// wails build / wails generate module 会编译并运行它收集绑定，本程序 main() 会被执行，
// 但此时不允许任何真实副作用（见 main() 中 bindingsRun 分支）。
func isWailsBindingsProcess() bool {
	if exe, err := os.Executable(); err == nil {
		return strings.Contains(strings.ToLower(filepath.Base(exe)), "wailsbindings")
	}
	return false
}

// appCtx Wails 运行上下文（OnStartup 设置），供事件推送与窗口控制使用。
var appCtx context.Context

const (
	appName     = "DeepSeek Harness"
	defaultPort = 3080
	// 设置窗口固定尺寸（840×560，1.5:1 等比例，紧凑且无滚动条）
	winW = 840
	winH = 560
)

// shotWindowHeight 设置窗口高度：截图模式可用 DSH_SYSTRAY_SHOT_HEIGHT 加高
// （关于页加入插件列表后内容变长，完整展示需要更高窗口）；未设置时默认 winH。
func shotWindowHeight() int {
	h := winH
	if v := os.Getenv("DSH_SYSTRAY_SHOT_HEIGHT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= winH {
			h = n
		}
	}
	return h
}

var (
	logDir             string
	webURL             string
	harnessDir         string
	port               int
	startupTimeout     time.Duration
	quitting           atomic.Bool
	keepServerRunning  atomic.Bool
	harnessDirExplicit bool
	// serverStartedPort 本进程最近一次实际启动后台服务所用的端口（startServer 成功时写入，
	// killServer 后保留为「最后运行端口」供状态展示；0 = 本进程从未启动过服务）。
	// 用于“端口已修改、重启后生效”提示：配置端口 port ≠ 实际运行端口时前端持续提示。
	serverStartedPort int
	// quitRequested 托盘「退出」流程标记：Wails OnBeforeClose 据此放行应用退出（区别于窗口 X 关闭）
	quitRequested atomic.Bool
	// systemShuttingDown 系统关机/重启/注销已开始（macOS NSWorkspace 通知置位）：
	// 退出流程据此跳过「是否停止后台服务」询问，避免阻塞系统关机。
	systemShuttingDown atomic.Bool
	// singleInstanceRelease 单实例互斥体释放函数（更新重启前调用，避免新进程被误判重复运行）
	singleInstanceRelease func()
)

// resolveRunningService 解析后台服务当前实际运行状态：
// 优先按配置端口（webURL）探测；不响应时若本进程曾以其它端口启动服务且该端口仍存活
// （用户修改端口后尚未重启的场景）则返回该端口。返回 (是否在运行, 运行端口, 实际 URL)。
func resolveRunningService() (bool, int, string) {
	if serverResponding(webURL) {
		return true, port, webURL
	}
	if serverStartedPort != 0 && serverStartedPort != port {
		alt := fmt.Sprintf("http://127.0.0.1:%d/", serverStartedPort)
		if serverResponding(alt) {
			return true, serverStartedPort, alt
		}
	}
	return false, 0, webURL
}

type appConfig struct {
	Port              int    `json:"port"`
	HarnessDir        string `json:"harnessDir"`
	StartupTimeoutSec int    `json:"startupTimeoutSec"`
	UpdateMirror      string `json:"updateMirror"`
	// HarnessPrerelease 允许把 alpha/beta/rc 等预发布版视为 DeepSeek Harness 的可更新版本（默认关闭）。
	HarnessPrerelease bool `json:"harnessPrerelease"`
	// Language 界面语言偏好：auto（跟随系统）| zh | en；缺省 auto。运行时解析见 i18n.go。
	Language string `json:"language"`
	// PendingPluginOps 待应用的插件变更（更新/删除）：点击后只登记，等用户在关闭设置窗口时
	// 确认、或在关于页点「立即应用」才执行（整批一次重启）。跨托盘重启保留，见 plugin_batch.go。
	PendingPluginOps []pendingPluginOp `json:"pendingPluginOps,omitempty"`
}

// pendingPluginOp 一条待应用插件变更的持久化形态（config.json）。
type pendingPluginOp struct {
	ID string `json:"id"` // 插件行稳定标识（PluginRow.ID）
	Op string `json:"op"` // update | remove
}

// configFilePath 用户配置目录下的 config.json（Windows: %APPDATA%\dsh-systray；macOS: ~/Library/Application Support/dsh-systray）。
func configFilePath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "dsh-systray", "config.json")
	}
	return ""
}

// legacyConfigPath 旧版本保存在 exe 同目录的 config.json（仅作兼容读取）。
func legacyConfigPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "config.json")
	}
	return ""
}

func applyConfigFile(cfg *appConfig, path string) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var f appConfig
	if json.Unmarshal(data, &f) != nil {
		return
	}
	if f.Port != 0 {
		cfg.Port = f.Port
	}
	if f.HarnessDir != "" {
		cfg.HarnessDir = f.HarnessDir
		harnessDirExplicit = true
	}
	if f.StartupTimeoutSec != 0 {
		cfg.StartupTimeoutSec = f.StartupTimeoutSec
	}
	if f.UpdateMirror != "" {
		cfg.UpdateMirror = f.UpdateMirror
	}
	if f.HarnessPrerelease {
		cfg.HarnessPrerelease = true
	}
	if l := normalizeLang(f.Language); l != "auto" {
		cfg.Language = l
	}
	if len(f.PendingPluginOps) > 0 {
		cfg.PendingPluginOps = f.PendingPluginOps
	}
}

func loadConfig() appConfig {
	cfg := appConfig{Port: defaultPort, HarnessDir: defaultHarnessDir(), StartupTimeoutSec: 300}
	// 优先读用户配置目录；不存在时兼容读取 exe 同目录旧配置
	userPath := configFilePath()
	loaded := false
	if userPath != "" {
		if _, err := os.Stat(userPath); err == nil {
			applyConfigFile(&cfg, userPath)
			loaded = true
		}
	}
	if !loaded {
		applyConfigFile(&cfg, legacyConfigPath())
	}
	if p := os.Getenv("DSH_SYSTRAY_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			cfg.Port = n
		}
	}
	if d := os.Getenv("DSH_SYSTRAY_HARNESS_DIR"); d != "" {
		cfg.HarnessDir = d
		harnessDirExplicit = true
	}
	if t := os.Getenv("DSH_SYSTRAY_STARTUP_TIMEOUT"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			cfg.StartupTimeoutSec = n
		}
	}
	return cfg
}

// saveConfig 将配置写入用户配置目录的 config.json，便于记住用户选择的目录。
func saveConfig(cfg appConfig) {
	p := configFilePath()
	if p == "" {
		log.Printf("cannot resolve config path")
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		log.Printf("cannot create config dir: %v", err)
		return
	}
	if data, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		if err := os.WriteFile(p, data, 0o644); err != nil {
			log.Printf("cannot write config.json: %v", err)
		} else {
			log.Printf("wrote config.json: %s", p)
		}
	}
}

// autostartLaunch 是否为开机自启动（登录时）启动。此时完全静默：不显示启动进度窗口，也不弹任何
// 提示/询问。通过自动启动项注入的 --autostart 参数识别。
var autostartLaunch = func() bool {
	for _, a := range os.Args {
		if a == "--autostart" {
			return true
		}
	}
	return false
}()

// maybeStartSplash 启动阶段显示进度；开机自启动场景下返回空实现（不开窗、完全静默）。
func maybeStartSplash(text string) *SplashState {
	if autostartLaunch {
		return &SplashState{Update: func(string, float64) {}, Close: func() {}}
	}
	return startSplash(text)
}

// 后台服务状态（托盘菜单）：四态实时反映——运行中/已停止/启动失败/启动中。
var (
	serverReady       atomic.Bool
	serviceFailed     atomic.Bool
	serviceFailReason atomic.Value      // string
	menuOpen          *systray.MenuItem // “打开 Web UI”
	menuStatus        *systray.MenuItem // 状态说明行
	mSettings         *systray.MenuItem // “设置”
	mQuit             *systray.MenuItem // “退出”
)

// statusLineMaxRunes 托盘状态行（失败原因）的最大展示长度——菜单宽度随最长文本变化，
// 过长原因会把托盘菜单撑得很宽；完整原因保留在设置页服务副标题与日志里。
const statusLineMaxRunes = 36

// truncRunes 按字符截断（中英文都算 1 个展示位）并追加省略号。
func truncRunes(s string, n int) string {
	if n < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func serviceStatusText() string {
	if serviceFailed.Load() {
		if s, _ := serviceFailReason.Load().(string); s != "" {
			return truncRunes(s, statusLineMaxRunes)
		}
		return T("服务启动失败")
	}
	if serverReady.Load() {
		return T("服务已停止")
	}
	return T("服务启动中…")
}

// refreshServiceMenu 按服务实际运行状态刷新托盘菜单（可跨线程、可周期调用）。
// 就绪判定基于实际运行端口（配置端口或本进程最后启动端口），避免修改端口后、
// 重启前「打开 Web UI」被错误禁用/指向不可达地址。
func refreshServiceMenu() {
	if menuOpen == nil || menuStatus == nil {
		return
	}
	ready, _, _ := resolveRunningService()
	systray.RunOnLoop(func() {
		if menuOpen == nil || menuStatus == nil {
			return
		}
		if ready {
			// 就绪：隐藏状态行，显示并启用「打开 Web UI」
			menuStatus.Hide()
			menuOpen.Show()
			menuOpen.Enable()
			return
		}
		// 未就绪：隐藏「打开 Web UI」，显示状态原因行
		menuOpen.Hide()
		menuStatus.Show()
		menuStatus.SetTitle(serviceStatusText())
	})
}

// refreshTrayTexts 语言切换后把托盘菜单各条目更新为当前生效语言（菜单项运行时 SetTitle 即生效）。
// systray 操作须在消息循环线程执行，整体包进 RunOnLoop；refreshServiceMenu 再按真实服务状态
// 覆盖状态行文案与显隐（内部自行 RunOnLoop，队列式实现，嵌套调用安全）。
func refreshTrayTexts() {
	if menuOpen == nil || menuStatus == nil {
		return
	}
	systray.RunOnLoop(func() {
		if menuStatus != nil {
			menuStatus.SetTooltip(T("后台服务状态"))
		}
		if menuOpen != nil {
			menuOpen.SetTitle(T("打开 Web UI"))
			menuOpen.SetTooltip(T("打开网页端界面"))
		}
		if mSettings != nil {
			mSettings.SetTitle(T("设置"))
			mSettings.SetTooltip(T("打开设置窗口"))
		}
		if mQuit != nil {
			mQuit.SetTitle(T("退出"))
			mQuit.SetTooltip(T("退出并关闭后台服务器"))
		}
		refreshServiceMenu()
	})
}

// pollServiceMenu 周期性探测服务状态并刷新菜单，保证每次打开托盘菜单都反映实时状态。
func pollServiceMenu() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if quitting.Load() {
				return
			}
			refreshServiceMenu()
		}
	}
}

// ==================== 托盘单击/双击 ====================
// 行为约定（v2 简化）：单击/双击/右键均即时弹出菜单，无延迟、无双击打开 Web UI。
// 之前为区分“双击开 Web UI”而引入的系统双击间隔延迟（约 500ms）让单击弹菜单有明显延迟，
// 且双击语义收益低；已按用户确认去掉，左/右键点击均直接弹菜单。
// 所有菜单显示经 ShowMenuAsync 投递到托盘消息循环线程执行（跨线程 TrackPopupMenu 不可靠）。

// trayOnClick 托盘左键单击：即时弹菜单。
func trayOnClick(menu systray.IMenu) {
	log.Printf("[tray] left click")
	systray.ShowMenuAsync()
}

// trayOnDClick 托盘左键双击：双击不再打开 Web UI（用户确认去掉），等同弹菜单；
// fork 的 DBLCLK 仅在菜单未打开时到达，此回调保持兜底弹菜单。
func trayOnDClick(menu systray.IMenu) {
	log.Printf("[tray] double click")
	systray.ShowMenuAsync()
}

// trayOnRClick 托盘右键单击：即时弹菜单。
func trayOnRClick(menu systray.IMenu) {
	log.Printf("[tray] right click")
	systray.ShowMenuAsync()
}

func main() {
	// bindingsRun：wails build/generate 会编译并运行 wailsbindings.exe（-tags bindings）
	// 来收集绑定；它执行本 main()，但不得有真实副作用（日志/注册表自愈/托盘/单实例弹窗），
	// 否则 stderr 输出会让 CI/PowerShell 构建判定失败、或污染自启动注册表。
	bindingsRun := isWailsBindingsProcess()
	if bindingsRun {
		log.SetOutput(io.Discard)
	}

	cfg := loadConfig()
	updateMirrorOverride = cfg.UpdateMirror
	harnessPrereleaseOverride = cfg.HarnessPrerelease
	port = cfg.Port
	webURL = fmt.Sprintf("http://127.0.0.1:%d/", port)
	harnessDir = cfg.HarnessDir
	startupTimeout = time.Duration(cfg.StartupTimeoutSec) * time.Second
	// 语言：config 未写 language（旧版本升级 / 全新安装）默认简体中文，避免旧用户升级后
	// 因「跟随系统」检测到英文系统语言而整体变英文（0.8.0 升级反馈）；显式 auto/zh/en 按选择生效。
	langPref = "zh"
	if cfg.Language != "" {
		langPref = normalizeLang(cfg.Language)
	}
	curLang = resolveLang(langPref)
	log.Printf("[i18n] language pref=%q system=%s → curLang=%s", cfg.Language, detectSystemLang(), curLang)
	// 待应用的插件变更随 config 持久化：先存下，等插件列表可用（onStartup）时逐条校验载入。
	pendingPluginOpsFromConfig = cfg.PendingPluginOps

	// 自愈历史自启动项：旧版本注册的自启动条目未带 --autostart 参数，或残留
	// 「裸二进制直接 exec」形态（macOS 上因缺 bundle 上下文导致开机自启失效），
	// 启动时升级为 open .app 形态（仅写 plist 文件，不 bootout——当前进程可能正由
	// 该 launchd job 启动，bootout 会自杀；文件改动下次登录加载生效）。
	// 注意：bindings 生成进程（wailsbindings.exe，wails build/generate 运行）不执行任何真实副作用。
	if !bindingsRun && isAutostartEnabled() {
		if err := writeLaunchAgentPlist(); err != nil {
			log.Printf("autostart entry refresh failed: %v", err)
		} else {
			log.Printf("autostart entry refreshed with --autostart flag")
		}
	}

	cfgDir, err := os.UserConfigDir()
	if err != nil {
		cfgDir = os.TempDir()
	}
	logDir = filepath.Join(cfgDir, "dsh-systray", "logs")
	if !bindingsRun {
		// 统一日志：所有行为（自身/UI/托盘/子进程）写入 logDir/dsh-systray.log
		// （进程级单例句柄，行格式 ts [LEVEL] [module] message；见 logsetup.go）。
		// 默认目录创建/打开失败时回退系统临时目录，保证日志总有着落（此前失败完全
		// 静默丢弃，表现为"日志为空"且无从诊断）。
		if !initUnifiedLog() && !strings.HasPrefix(logDir, os.TempDir()) {
			logDir = filepath.Join(os.TempDir(), "dsh-systray", "logs")
			initUnifiedLog()
		}
		mergeLegacyLogs() // 升级迁移：合并旧多文件日志后删除源文件（须先于任何新日志写入）
		// stderr 双写：macOS 上从 Console/unified 日志也能看到应用日志（诊断兜底）
		log.SetOutput(io.MultiWriter(appLogWriter{}, os.Stderr))
	}
	log.SetFlags(log.LstdFlags)
	if !bindingsRun {
		// 启动首行：固定记录版本/pid/日志路径，便于对照「日志页显示路径」与实际落盘位置
		log.Printf("dsh-systray v%s starting (pid=%d), log file: %s", appVersion, os.Getpid(), unifiedLogPath())
	}

	release, acquired := acquireSingleInstance()
	singleInstanceRelease = release
	if !acquired {
		if bindingsRun {
			return // bindings 生成进程：直接退出，不弹窗
		}
		// 已在运行：弹窗提示后退出，不产生第二个托盘图标。
		showMessageBox(T("DeepSeek Harness 已在运行中，请使用系统托盘图标操作。"), appName)
		return
	}
	defer release()

	// 清理上次更新遗留的旧程序文件；后台自动检查新版本并提示更新
	if !bindingsRun {
		cleanupStaleUpdateFiles()
		startAutoUpdateCheck()
	}

	// Windows：托盘在独立 goroutine 自建窗口+消息循环，与 Wails 事件循环共存。
	// macOS：托盘通过 RunWithExternalLoop 集成（见 onStartup），不接管 NSApplication。
	if runtime.GOOS == "windows" && !bindingsRun {
		go systray.Run(onReady, onExit)
	}

	err = wails.Run(&options.App{
		Title:     "dsh-systray",
		Width:     winW,
		Height:    shotWindowHeight(),
		MinWidth:  winW,
		MaxWidth:  winW,
		MinHeight: shotWindowHeight(),
		MaxHeight: shotWindowHeight(),
		Assets:    assets,
		Bind: []interface{}{
			app,
		},
		OnStartup:     onStartup,
		OnDomReady:    onDomReady,
		OnShutdown:    onShutdown,
		OnBeforeClose: onBeforeClose,
		// macOS：窗口红点关闭只隐藏应用（不经 OnBeforeClose），Dock/⌘Q 的退出请求才走
		// onBeforeClose——两者由此可区分（此前无此开关时二者都汇入同一回调，
		// onBeforeClose 返回 true 会连 Dock 退出也吞掉，只能强制退出）。
		HideWindowOnClose: runtime.GOOS == "darwin",
		StartHidden:       autostartLaunch || (!shotMode && startupEnvReady()),
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			Theme:                windows.SystemDefault,
		},
	})
	if err != nil {
		log.Printf("wails run: %v", err)
	}
}

// onStartup Wails 应用启动回调：建立上下文、启动 macOS 托盘、开始后台服务编排。
func onStartup(ctx context.Context) {
	appCtx = ctx
	loadStartupPendingPluginOps() // 跨托盘重启保留「待应用变更未生效」提示（逐条校验后载入）
	if runtime.GOOS == "darwin" {
		// 系统关机/注销/重启回调须在托盘启动前注册，避免通知竞态丢失。
		// true=关机/注销开始（跳过停服询问直接放行）；false=会话恢复（FUS 切回，复位）。
		systray.NotifySystemPowerChange(func(shuttingDown bool) {
			if shuttingDown {
				systemShuttingDown.Store(true)
				log.Printf("system shutdown/logout detected, stop-server prompt will be skipped")
			} else {
				systemShuttingDown.Store(false)
				log.Printf("session became active again, stop-server prompt restored")
			}
		})
		startSignalHandling() // SIGTERM/SIGINT（launchd 注销/关机路径）→ 优雅退出不弹窗
		start, _ := systray.RunWithExternalLoop(onReady, onExit)
		start()
	}
		go bootstrapService()
}

// pendingPluginOpsFromConfig 启动时从 config.json 读到的待应用插件变更（onStartup 逐条校验载入）。
var pendingPluginOpsFromConfig []pendingPluginOp

// loadStartupPendingPluginOps 载入上次运行登记的待应用插件变更（跨托盘重启保留「变更未生效」提示）：
// 逐条按当前插件列表校验，失效条目录入后丢弃并回写配置。
func loadStartupPendingPluginOps() {
	if shotMode || len(pendingPluginOpsFromConfig) == 0 {
		return
	}
	if dropped := loadPendingPluginOps(pendingPluginOpsFromConfig); dropped > 0 {
		saveCurrentConfig()
		log.Printf("startup: dropped %d stale pending plugin op(s)", dropped)
	}
}

// signalShotReady 截图/预览模式：设置页切换完成后写入标记文件，供截图脚本同步等待，
// 避免脚本在 splash 阶段过早截屏（此前曾截到“近空白的启动视图”）。
func signalShotReady() {
	if p := os.Getenv("DSH_SYSTRAY_SHOT_READY_FILE"); p != "" {
		_ = os.WriteFile(p, []byte("ready"), 0o644)
	}
}

// onDomReady 前端就绪：非自启动场景通知前端进入 splash 视图。
func onDomReady(ctx context.Context) {
	// 截图/预览模式：窗口保持置顶（WebView2 偶发重绘/失焦会让一次性置顶失效，
	// 常驻心跳每 1.2s 重新置顶，保证 PrintWindow / 屏幕截取窗口始终最前且不被遮挡）。
	if os.Getenv("DSH_SYSTRAY_SHOW_WINDOW") == "1" {
		wruntime.WindowSetAlwaysOnTop(ctx, true)
		go func() {
			t := time.NewTicker(1200 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if quitting.Load() {
						return
					}
					wruntime.WindowSetAlwaysOnTop(ctx, true)
				}
			}
		}()
	}
	if !autostartLaunch {
		wruntime.EventsEmit(ctx, "ui:show-splash", nil)
	}
}

// onBeforeClose 窗口关闭回调（Wails 的 Quit 也经此拦截）：
//   - 托盘「退出」流程（quitRequested 已置位）：放行（返回 false），允许应用退出；
//   - Windows：窗口 X 仅隐藏窗口并阻止关闭（托盘常驻）；更新进行中先询问是否取消更新；
//   - macOS：红点关闭已由 HideWindowOnClose 直接隐藏（不到这里），到达此处即为真实退出
//     （Dock/⌘Q/托盘退出）→ 询问是否停止后台服务：确定/保留服务均放行退出，取消则留在前台。
func onBeforeClose(ctx context.Context) bool {
	if quitRequested.Load() {
		return false // 托盘退出：允许关闭并退出应用
	}
	if updateFlowBusy() {
		if fn := splashOnCloseFn(); fn != nil && fn() {
			cancelActiveUpdate()
			log.Printf("update cancelled on window close")
		}
	}
	if runtime.GOOS == "darwin" {
		// 系统关机/重启/注销（NSWorkspace 通知已置位）：跳过询问直接放行退出。
		// 保留后台服务不主动 kill——系统退出流程会自行回收全部进程，且避免 lsof/
		// pkill 等额外操作拖慢退出（表现为阻塞关机）。
		if systemShuttingDown.Load() {
			keepServerRunning.Store(true)
			quitRequested.Store(true)
			return false
		}
		// 真实退出请求：与托盘「退出」一致地询问停服策略（0=停止并退出 1=保留服务 -1=取消）
		choice := askStopServer()
		if choice < 0 {
			return true // 用户取消退出：留在前台，不关闭
		}
		keepServerRunning.Store(choice == 1)
		quitRequested.Store(true)
		return false
	}
	// 关闭设置窗口：存在待应用插件变更时先询问是否立即应用并重启（用户选「稍后」则照常隐藏，
	// 变更保留在待应用区、关于页持续提示；应用流程的 splash 收尾会自行隐藏窗口）。
	if askApplyPendingBeforeHide() {
		return true
	}
	wruntime.WindowHide(ctx)
	return true
}

// onShutdown 退出清理：终止进行中的更新、按 keepServerRunning 保留或停止后台服务。
func onShutdown(ctx context.Context) {
	quitting.Store(true)
	cancelActiveUpdate()
	if keepServerRunning.Load() {
		keepPID := 0
		if serverCmd != nil && serverCmd.Process != nil {
			keepPID = serverCmd.Process.Pid
		}
		killChildProcesses(keepPID)
		log.Printf("quit with backend server kept running")
	} else {
		killServer()
		killChildProcesses(0)
		log.Printf("quit with backend server stopped")
	}
}

// recoverInterruptedImport 启动自愈：上一次插件导入恢复任务被意外中断（进程退出/窗口被杀，
// 无法执行回退或清理）时的恢复。按事务日志（import-journal.json）分派：
//   - stage=healing：中断发生在收尾自愈中 → **续跑自愈**（finishPluginImport 重入），
//     确保 harness 服务按正常流程启动并到达确定结果（保留 / 禁用不兼容插件 / 回退）；
//   - stage=importing：中断发生在解压/对齐中 → 回退到导入前状态（.importbak 还原）；
//   - 无日志但有 .importbak 残留（旧版本遗留）→ 回退兜底。
//
// 返回处理过的目录数（0 = 无残留）。幂等：rollbackImportProfiles 与 clearImportJournal
// 会消费掉快照与日志，正常完成/取消的导入不会在后续启动被误处理。
func recoverInterruptedImport() int {
	if shotMode {
		return 0
	}
	stopOrphan := func() {
		// 停掉占用 profile 文件（尤其 node_modules）的旧服务进程——可能来自上次异常退出后
		// 残留的孤儿服务；否则文件锁会导致回退改名/删除失败。
		if serverResponding(webURL) {
			killServer()
			time.Sleep(500 * time.Millisecond)
		}
	}
	if j, err := readImportJournal(); err == nil && j != nil && len(j.Dirs) > 0 {
		if j.Stage == "healing" {
			// 自愈中被杀：续跑收尾自愈（不可中断语义跨重启保持）
			stopOrphan()
			note, ferr := finishPluginImport(j.Dirs, j.HadNM)
			clearImportJournal()
			if ferr == nil {
				if note != "" {
					note = "。" + note
				}
				logUI("恢复中断自愈", "已续上次未完成的自愈收尾，服务正常"+note)
			} else {
				logUI("恢复中断自愈", "续自愈未通过并已自动回退："+ferr.Error())
			}
			return len(j.Dirs)
		}
		// importing：回退到导入前状态
		stopOrphan()
		rollbackImportProfiles(j.Dirs, j.HadNM)
		clearImportJournal()
		logUI("恢复中断自愈", fmt.Sprintf("检测到上次未完成的导入恢复，已自动回退 %d 个环境", len(j.Dirs)))
		return len(j.Dirs)
	}
	// 无事务日志：按 .importbak 残留扫描回退兜底（旧版本遗留）
	profiles := enumeratePluginProfiles()
	if len(profiles) == 0 {
		return 0
	}
	var dirs []string
	var had []bool
	for _, pf := range profiles {
		dir := pf.dir
		pjBak := filepath.Join(dir, "package.json"+importBakSuffix)
		nmBak := filepath.Join(dir, "node_modules"+importBakSuffix)
		if _, err := os.Stat(pjBak); err != nil {
			if _, err2 := os.Stat(nmBak); err2 != nil {
				continue
			}
		}
		dirs = append(dirs, dir)
		if _, err := os.Stat(nmBak); err == nil {
			had = append(had, true)
		} else {
			had = append(had, false)
		}
	}
	if len(dirs) == 0 {
		return 0
	}
	stopOrphan()
	rollbackImportProfiles(dirs, had)
	logUI("恢复中断自愈", fmt.Sprintf("检测到上次未完成的导入恢复，已自动回退 %d 个环境", len(dirs)))
	return len(dirs)
}

// bootstrapService 后台服务编排（原 main 中的启动流程，改为事件驱动进度）：
// 运行环境 → harness 安装/构建 → 启动服务 → 就绪提示。
func bootstrapService() {
	// 未部署（无 package.json）时不询问用户指定目录：显式配置的目录失效时回退到
	// 官方默认目录（~ 下 deepseek-harness，与官方 npx/源码部署及 macOS 语义一致），
	// 交由下方自动探测/部署流程静默处理。
	if _, err := os.Stat(filepath.Join(harnessDir, "package.json")); err != nil && harnessDirExplicit {
		log.Printf("configured harness dir %s not found, falling back to default %s", harnessDir, defaultHarnessDir())
		harnessDir = defaultHarnessDir()
		harnessDirExplicit = false
		saveConfig(appConfig{Port: port, HarnessDir: harnessDir, StartupTimeoutSec: int(startupTimeout / time.Second), UpdateMirror: updateMirrorOverride, HarnessPrerelease: harnessPrereleaseOverride, Language: langPref})
	}

	// 未显式配置时：自动探测已存在的 harness 源码 checkout（如各盘符根目录下的 deepseek-harness）
	if !harnessDirExplicit {
		if found := findExistingHarnessDir(); found != "" {
			harnessDir = found
			saveConfig(appConfig{Port: port, HarnessDir: harnessDir, StartupTimeoutSec: int(startupTimeout / time.Second), UpdateMirror: updateMirrorOverride, HarnessPrerelease: harnessPrereleaseOverride, Language: langPref})
			log.Printf("detected existing harness at %s", found)
		}
	}

	splash := maybeStartSplash(T("正在准备运行环境…"))

	// 0.5) 解压工具：环境检查加入 7-Zip（优先下载使用，Windows/macOS 均可）；失败不阻塞启动（zip 有 Go 兜底）。
	splash.Update(T("正在检查解压工具…"), 0.07)
	ensureArchiveTool(func(t string, pct float64) { splash.Update(t, 0.07+0.01*pct) })

	// 1) 运行环境：优先便携 Node.js / pnpm（无管理员权限、无窗口、后台静默）
	if !runtimeOK() {
		splash.Update(T("正在下载 Node.js / pnpm 运行时（首次约 1-3 分钟）…"), 0.08)
		if err := ensureRuntime(splash); err != nil {
			splash.Close()
			showMessageBox("下载运行环境失败：\n"+err.Error()+"\n\n请检查网络后重试；日志："+unifiedLogPath(), appName)
			return
		}
	}
	// 便携 node/pnpm 就位后（本次安装或历史安装）持久化到用户 PATH：用户新开终端即可
	// 直接运行 npx / pnpm 官方 dsh 命令（如安装插件）；系统已有 node/pnpm 时为 no-op。
	refreshEnvPath()
	// git 检测：github: 形式的插件安装（dsh plugin add github:<owner>/<repo>）依赖系统 git。
	// 缺失不阻塞启动，仅记录引导（日志页可见；README「安装插件」章节有说明）。
	if _, err := exec.LookPath("git"); err != nil {
		log.Printf("git not found on PATH: installing plugins via 'dsh plugin add github:<owner>/<repo>' requires git — install Git for Windows (https://git-scm.com/download/win) and open a new terminal")
	}

	// 2) DeepSeek Harness 本体：源码 checkout 走 pnpm 构建；全新机器走 npm 预构建产物（免 git / 免构建）
	switch harnessMode() {
	case "source":
		if !sourceDepsInstalled() {
			splash.Update(T("正在安装 harness 依赖（首次约 2-5 分钟）…"), 0.35)
			if err := runSourceDepsInstall(); err != nil {
				splash.Close()
				showMessageBox("安装 harness 依赖失败：\n"+err.Error()+"\n\n日志："+unifiedLogPath(), appName)
				return
			}
		}
		if !harnessBuiltOK() {
			splash.Update(T("正在构建 harness 前端产物（首次约 1-3 分钟）…"), 0.55)
			if err := runHarnessBuild(); err != nil {
				splash.Close()
				showMessageBox("harness 构建失败：\n"+err.Error()+"\n\n日志："+unifiedLogPath(), appName)
				return
			}
		}
	case "missing":
		splash.Update(T("正在安装 DeepSeek Harness（首次约 2-5 分钟）…"), 0.35)
		if err := ensureNpmHarness(); err != nil {
			splash.Close()
			showMessageBox("安装 DeepSeek Harness 失败：\n"+err.Error()+"\n\n日志："+unifiedLogPath(), appName)
			return
		}
	}

	// 2.5) 上次导入恢复被意外中断（进程退出/窗口关闭）自愈：残留 .importbak 事务快照 →
	// 回退到导入前状态（服务随后按正常流程拉起并健康校验）
	if n := recoverInterruptedImport(); n > 0 {
		splash.Update(fmt.Sprintf("已检测到上次未完成的导入恢复，自动回退 %d 个环境…", n), 0.87)
	}

	// 3) 启动服务
	splash.Update(T("正在启动服务…"), 0.9)
	started := false
	startedByUs := false
	var serverExitCh <-chan error
	serverLogBefore := int64(0)
	if serverResponding(webURL) {
		log.Printf("server already running on %s, skipping spawn", webURL)
		started = true
	} else {
		// 本次启动的健康校验只扫描此后的追加日志段；先轮转留档，使本次冷启动现场独立成档
		serverLogBefore = rotateServerLog()
		started, serverExitCh = startServer()
		startedByUs = started
	}
	if !started {
		splash.Close()
		showMessageBox("启动 DeepSeek Harness 服务失败，请查看日志：\n"+unifiedLogPath(), appName)
		return
	}
	closeSplash := splash.Close

	go func() {
		ready, why := waitForServerReady(webURL, serverExitCh, startupTimeout)
		closeSplash()
		if quitting.Load() {
			return
		}
		// 本进程拉起的服务就绪后再做健康校验（就绪 ≠ 健康：版本混装/插件不兼容会报加载错误，
		// 且错误常迟于就绪数秒刷出——必须用覆盖窗口的 verifyServerBoot，否则会把异常当成功并误清 LKG）
		bootError := ""
		if ready && startedByUs {
			if !verifyServerBootOnColdStart(serverLogBefore, serverExitCh) {
				ready = false
				bootError = "启动日志存在加载错误（版本/插件不兼容）"
			}
		}
		if ready {
			if startedByUs {
				clearAllLkg() // 冷启动验证通过：当前状态即新的「已知良好」，旧 LKG 不再需要
			}
			serverReady.Store(true)
			serviceFailed.Store(false)
			refreshServiceMenu()
			notifySplashDone()
			signalShotReady()
			if !autostartLaunch {
				notifyReady()
			} else {
				log.Printf("autostart: service ready, staying silent")
			}
			return
		}
		// 启动失败：特征指向环境本身时（进程异常退出 / 加载错误）自动尝试回退到上次正常状态
		if bootError != "" || why == "exited" {
			// 无法解析 bundle 特例自愈：摘除残留的激活声明后重启校验，健康则直接进入就绪
			// （无需整体回退 LKG；本机删除 deepseek-idesign 后 bundles 残留导致启动失败实证）
			if names := unresolvedBundleNames(serverLogBefore); len(names) > 0 {
				if stripUnresolvedBundles(names) > 0 && restartAndVerifyServer() {
					serverReady.Store(true)
					serviceFailed.Store(false)
					refreshServiceMenu()
					notifySplashDone()
					signalShotReady()
					if !autostartLaunch {
						notifyReady()
					} else {
						log.Printf("autostart: service ready after stripping unresolved bundles, staying silent")
					}
					logUI("启动自愈", "已摘除无法解析的 bundle（"+strings.Join(names, "、")+"），服务已恢复")
					return
				}
			}
			reason := bootError
			if reason == "" {
				reason = "服务进程异常退出"
			}
			kept, rolled, prev, disabledNames := tryBootRollback(reason)
			if kept {
				reportBootKept(disabledNames, reason)
				notifySplashDone()
				signalShotReady()
				if !autostartLaunch {
					notifyReady()
				}
				return
			}
			reportBootRollback(rolled, prev, reason)
			if rolled {
				notifySplashDone()
				signalShotReady()
				if !autostartLaunch {
					notifyReady()
				}
				return
			}
			serviceFailed.Store(true)
			serviceFailReason.Store("服务启动失败（" + reason + "）")
			refreshServiceMenu()
			return
		}
		if autostartLaunch {
			log.Printf("autostart: service not ready yet (%s), continuing silently in background", why)
			return
		}
		// 超时但服务进程仍在运行：慢机器/首次启动常见，继续后台等待，就绪后再次提示
		showMessageBox("DeepSeek Harness 服务启动较慢（首次启动或机器性能较低时属正常现象），已继续在后台启动，就绪后会再次提示。\n日志："+unifiedLogPath(), appName)
		if ready2, _ := waitForServerReady(webURL, serverExitCh, 15*time.Minute); ready2 && !quitting.Load() {
			serverReady.Store(true)
			serviceFailed.Store(false)
			refreshServiceMenu()
			notifyReady()
		} else if !quitting.Load() {
			serviceFailed.Store(true)
			serviceFailReason.Store("服务启动失败（超时）")
			refreshServiceMenu()
			showMessageBox("DeepSeek Harness 服务最终未能就绪，请查看日志：\n"+unifiedLogPath(), appName)
		}
	}()
}

// showMainWindow 显示设置窗口（托盘“设置”点击）。
func showMainWindow() {
	if appCtx == nil {
		return
	}
	wruntime.WindowShow(appCtx)
	ensureMainWindowForeground() // 平台实现：把窗口真正置前（需求：所有弹窗/窗口自动前台）
	// 更新进行中（自身更新下载/安装、harness 更新/重置）：保持更新进度界面置顶，不切回设置页——
	// ui:show-settings 会让前端整块重载设置页内容（含刷新版本/插件），进度界面随之消失，
	// 用户看不到下载/安装阶段与「取消更新」入口。
	if updateProgressActive() {
		log.Printf("settings view suppressed: update in progress")
		return
	}
	wruntime.EventsEmit(appCtx, "ui:show-settings", nil)
}

// hideMainWindow 隐藏设置窗口（splash 完成/窗口关闭时）。
// 调试环境变量 DSH_SYSTRAY_SHOW_WINDOW=1 时不隐藏（用于截图/开发预览）。
func hideMainWindow() {
	if os.Getenv("DSH_SYSTRAY_SHOW_WINDOW") == "1" {
		return
	}
	if appCtx == nil {
		return
	}
	wruntime.WindowHide(appCtx)
}

// notifySplashDone 通知前端 splash 阶段完成（前端据此切换视图）。
func notifySplashDone() {
	if appCtx == nil {
		return
	}
	if os.Getenv("DSH_SYSTRAY_SHOT_SPLASH") == "1" {
		return // 截图模式：保持 splash 视图
	}
	wruntime.EventsEmit(appCtx, "splash:done", nil)
}

func onReady() {
	systray.SetIcon(trayIconData())
	setTemplateIcon()
	startIconThemeWatch()
	// 不设置托盘图标标题文字：菜单栏/托盘只显示图标（macOS 上 SetTitle 会把应用名显示在图标旁），
	// 名称信息放鼠标悬停 tooltip。
	systray.SetTooltip(appName)

	// 状态说明行：禁用样式（置灰、不可点击），仅作状态提示；「打开 Web UI」未就绪时隐藏、就绪时显示可点
	menuStatus = systray.AddMenuItem(T("服务启动中…"), T("后台服务状态"))
	menuStatus.Disable()
	menuOpen = systray.AddMenuItem(T("打开 Web UI"), T("打开网页端界面"))
	refreshServiceMenu()
	// 周期刷新，保证每次打开托盘菜单都反映服务实时状态
	go pollServiceMenu()
	systray.AddSeparator()
	mSettings = systray.AddMenuItem(T("设置"), T("打开设置窗口"))
	systray.AddSeparator()
	mQuit = systray.AddMenuItem(T("退出"), T("退出并关闭后台服务器"))

	menuOpen.Click(func() {
		if running, _, _ := resolveRunningService(); running {
			openBrowser(webTokenURL()) // 带最新 token，避免重启后旧 token 失效
		}
	})
	mSettings.Click(showMainWindow)
	mQuit.Click(func() {
		// 退出前询问是否停止后台 Web 服务：0=停止并退出 1=保留服务 -1=取消退出
		choice := askStopServer()
		if choice < 0 {
			return // 取消退出：托盘保持可用
		}
		keepServerRunning.Store(choice == 1)
		quitRequested.Store(true)
		if appCtx != nil {
			wruntime.Quit(appCtx)
		}
	})

	// 单击/双击/右键均即时弹菜单（经消息循环线程），双击不再打开 Web UI（用户确认去掉）。
	systray.SetOnClick(trayOnClick)
	systray.SetOnDClick(trayOnDClick)
	systray.SetOnRClick(trayOnRClick)
}

func onExit() {
	log.Printf("tray exiting")
}

// setAutostartOn 统一开关开机自启动（供设置窗口调用）。
func setAutostartOn(on bool) {
	var err error
	if on {
		err = enableAutostart()
	} else {
		err = disableAutostart()
	}
	if err != nil {
		log.Printf("set autostart %v failed: %v", on, err)
		showMessageBox("设置开机自启动失败：\n"+err.Error(), appName)
		return
	}
	if on {
		log.Printf("autostart enabled (settings)")
	} else {
		log.Printf("autostart disabled (settings)")
	}
}

func prereqsOK() bool {
	if _, err := exec.LookPath("node"); err != nil {
		log.Printf("node not found: %v", err)
		return false
	}
	if _, err := exec.LookPath("pnpm"); err != nil {
		log.Printf("pnpm not found: %v", err)
		return false
	}
	if _, err := os.Stat(filepath.Join(harnessDir, "package.json")); err != nil {
		log.Printf("harness not found at %s: %v", harnessDir, err)
		return false
	}
	return true
}

// harnessBuiltOK 判断 harness 是否已完成完整构建（web 前端 dist、client 构建记录、host 库产物齐全）。
func harnessBuiltOK() bool {
	checks := []string{
		filepath.Join(harnessDir, ".dsh-build", "client-build-environment.json"),
		filepath.Join(harnessDir, "apps", "web", "dist"),
		filepath.Join(harnessDir, "packages", "interaction", "commands", "lib", "typert.host.js"),
	}
	for _, p := range checks {
		if _, err := os.Stat(p); err != nil {
			log.Printf("harness build output missing: %s", p)
			return false
		}
	}
	return true
}

// runHarnessBuild 执行 pnpm run build（输出按行改写进统一日志，模块 build），超时 10 分钟。
func runHarnessBuild() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, pnpmCmd(), "run", "build")
	cmd.Dir = harnessDir
	cmd.Env = append(os.Environ(), pnpmTunedEnv()...)
	if _, err := os.Stat(filepath.Join(harnessDir, ".git")); err != nil {
		// 非 git 部署（如 zip 解压）：提供占位提交哈希，避免构建脚本依赖 git
		cmd.Env = append(cmd.Env, "DSH_CLIENT_COMMIT_HASH=0000000")
	}
	hideCmdWindow(cmd)
	w := newModuleLogWriter("build")
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		w.Flush()
		return fmt.Errorf("pnpm run build failed: %w（日志：%s）", err, unifiedLogPath())
	}
	w.Flush()
	log.Printf("harness build completed")
	return nil
}

// isNpmHarnessReady 是否为 npm 预构建产物形态（@deepseek-ai/dsh）。
func isNpmHarnessReady() bool {
	_, err := os.Stat(filepath.Join(harnessDir, "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"))
	return err == nil
}

// isSourceHarnessDir 是否为 harness 源码 checkout。
func isSourceHarnessDir() bool {
	if isNpmHarnessReady() {
		return false
	}
	for _, p := range []string{"apps", "packages", ".dsh-build"} {
		if _, err := os.Stat(filepath.Join(harnessDir, p)); err == nil {
			return true
		}
	}
	return false
}

// harnessMode 返回 "npm"（预构建产物）/ "source"（源码 checkout）/ "missing"（需安装）。
func harnessMode() string {
	if isNpmHarnessReady() {
		return "npm"
	}
	if isSourceHarnessDir() {
		return "source"
	}
	return "missing"
}

// startupEnvReady 运行环境是否已就绪（用于启动窗口策略）：
//   - node/pnpm 运行时可用；
//   - harness 已是可运行形态：npm 预构建产物就位，或源码 checkout 且依赖已装、前端已构建。
//
// 就绪 → 启动时窗口保持隐藏，服务在后台拉起，就绪后直接弹「是否打开 Web UI」；
// 未就绪 → 需要下载/安装/构建，显示 splash 进度窗口（用户可见等待过程）。
func startupEnvReady() bool {
	if !runtimeOK() {
		return false
	}
	switch harnessMode() {
	case "npm":
		return true
	case "source":
		return sourceDepsInstalled() && harnessBuiltOK()
	default:
		return false
	}
}

// findExistingHarnessDir 在常见位置探测已存在的 harness 源码 checkout。
func findExistingHarnessDir() string {
	for _, d := range candidateHarnessDirs() {
		if d == "" || d == harnessDir {
			continue
		}
		if _, err := os.Stat(filepath.Join(d, "package.json")); err != nil {
			continue
		}
		for _, p := range []string{"apps", "packages", ".dsh-build"} {
			if _, err := os.Stat(filepath.Join(d, p)); err == nil {
				return d
			}
		}
	}
	return ""
}

func sourceDepsInstalled() bool {
	_, err := os.Stat(filepath.Join(harnessDir, "node_modules"))
	return err == nil
}

// pnpmTunedEnv 慢机器调优：限制并发、克隆式安装（参考 dsh-desktop）；网络故障快速失败
// ——单请求 15s 超时、不重试（fetch_retries=0）：坏网络下 registry 拉取失败一次即放弃，
// 让上层立即进入收尾（自愈/自动禁用），避免每轮 install 卡 2-4 分钟（new_device.log 实证
// 「Will retry in 10 seconds…2 retries left」的多档重试循环）。registry 官方不可达自动切
// npmmirror（installRegistry 探测一次并缓存）。
func pnpmTunedEnv() []string {
	return []string{
		"PNPM_MAX_WORKERS=1",
		"npm_config_child_concurrency=1",
		"npm_config_package_import_method=clone-or-copy",
		"npm_config_side_effects_cache=false",
		"npm_config_fetch_retries=0",
		"npm_config_fetch_timeout=15000",
		"npm_config_registry=" + installRegistry(),
	}
}

// runSourceDepsInstall 源码模式：pnpm install（输出改写进统一日志，模块 install）。
func runSourceDepsInstall() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, pnpmCmd(), "install")
	cmd.Dir = harnessDir
	cmd.Env = append(os.Environ(), pnpmTunedEnv()...)
	hideCmdWindow(cmd)
	w := newModuleLogWriter("install")
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		w.Flush()
		return fmt.Errorf("pnpm install failed: %w（日志：%s）", err, unifiedLogPath())
	}
	w.Flush()
	return nil
}

// ensureNpmHarness 全新机器：安装 npm 预构建产物 @deepseek-ai/dsh（免 git / 免构建）。
// 默认版本钉在官方初始 rc（历史行为；显式最新版走 ensureNpmHarnessVersion）。
func ensureNpmHarness() error {
	return ensureNpmHarnessVersion("0.1.1-rc.2")
}

// ensureNpmHarnessVersion 在 harnessDir 安装 npm 预构建产物 @deepseek-ai/dsh@ver
// （脚手架与白名单复刻 ensureNpmHarness 的历史语义；用于「重置=清空目录后全新安装最新版」）。
func ensureNpmHarnessVersion(ver string) error {
	return ensureNpmHarnessVersionPinned(ver, nil)
}

// ensureNpmHarnessVersionPinned 同 ensureNpmHarnessVersion，另把 familyNames 里的家族包逐个钉到
// ver（见 setHarnessFamilyOverrides）：重置场景下包名取自被替换下来的原目录，把整族锁在目标
// 版本——否则 caret 范围（^0.1.5-rc.1 允许 0.1.5-rc.2）会在官方分批发布时选中半发布的新版本，
// 随后在其缺失依赖上 ERR_PNPM_NO_MATCHING_VERSION 整次安装失败（2026-09-10 实测）。
// 钉版导致解析失败（目标版本家族未发全 / 包已改名）时自动去掉钉版重试一次。
func ensureNpmHarnessVersionPinned(ver string, familyNames []string) error {
	if err := os.MkdirAll(harnessDir, 0o755); err != nil {
		return err
	}
	// package.json：声明需执行的构建脚本（原生依赖），避免 pnpm 默认忽略导致失败
	pkgPath := filepath.Join(harnessDir, "package.json")
	pkg := "{\n  \"name\": \"deepseek-harness\",\n  \"private\": true,\n  \"pnpm\": {\n    \"onlyBuiltDependencies\": [\n      \"@deepseek-ai/dsh-subprocess-local\",\n      \"@google/genai\",\n      \"koffi\",\n      \"node-pty\",\n      \"protobufjs\"\n    ]\n  }\n}\n"
	write := false
	if data, err := os.ReadFile(pkgPath); err != nil {
		write = true
	} else if !strings.Contains(string(data), "onlyBuiltDependencies") {
		write = true
	}
	if write {
		if err := os.WriteFile(pkgPath, []byte(pkg), 0o644); err != nil {
			return err
		}
	}
	// pnpm-workspace.yaml：pnpm 11 的 allowBuilds 白名单，消除 ERR_PNPM_IGNORED_BUILDS
	wsPath := filepath.Join(harnessDir, "pnpm-workspace.yaml")
	ws := "allowBuilds:\n  '@deepseek-ai/dsh-subprocess-local': true\n  '@google/genai': true\n  koffi: true\n  node-pty: true\n  protobufjs: true\n"
	writeWS := false
	if data, err := os.ReadFile(wsPath); err != nil {
		writeWS = true
	} else if strings.Contains(string(data), "set this to true or false") || !strings.Contains(string(data), ": true") {
		writeWS = true
	}
	if writeWS {
		if err := os.WriteFile(wsPath, []byte(ws), 0o644); err != nil {
			return err
		}
	}
	if ver != "latest" {
		if err := setHarnessFamilyOverrides(harnessDir, ver, familyNames); err != nil {
			// 钉版写失败不阻断：退化为普通安装（旧锁文件已被重置流程清空，仍能装出单一版本）
			log.Printf("install: write family overrides failed: %v", err)
		} else if len(familyNames) > 0 {
			log.Printf("install: pinned %d family packages to %s", len(familyNames), ver)
		}
	}
	out, err := runNpmHarnessAdd(ver)
	if err != nil {
		if len(familyNames) > 0 && strings.Contains(out, "ERR_PNPM_NO_MATCHING_VERSION") {
			// 钉版把某个家族包钉到了目标版本不存在的组合（目标版本家族未发全/包已改名）：
			// 去掉钉版原样重试一次，让 pnpm 自行解析。
			log.Printf("install: pinned resolution failed, retrying without family pin")
			if cerr := setHarnessFamilyOverrides(harnessDir, ver, nil); cerr == nil {
				out, err = runNpmHarnessAdd(ver)
			}
		}
	}
	if err != nil {
		if isNpmHarnessReady() {
			log.Printf("npm harness installed (pnpm reported: %v)", err)
		} else {
			msg := fmt.Sprintf("安装 @deepseek-ai/dsh@%s 失败：%v", ver, err)
			if hint := harnessInstallHint(out); hint != "" {
				msg += "\n" + hint
			}
			if tail := outputTail(out, 600); tail != "" {
				msg += "\n\n输出尾部：\n" + tail
			}
			return fmt.Errorf("%s\n\n日志：%s", msg, unifiedLogPath())
		}
	}
	if !isNpmHarnessReady() {
		return fmt.Errorf("安装后未找到 dsh 入口：%s", filepath.Join(harnessDir, "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"))
	}
	log.Printf("npm harness %s installed at %s", ver, harnessDir)
	return nil
}

// runNpmHarnessAdd 执行 pnpm add @deepseek-ai/dsh@ver --save-exact（输出进统一日志，同时保留
// 尾部供失败归类与弹窗展示）。
func runNpmHarnessAdd(ver string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, pnpmCmd(), "add", "@deepseek-ai/dsh@"+ver, "--save-exact")
	cmd.Dir = harnessDir
	cmd.Env = append(os.Environ(), pnpmTunedEnv()...)
	hideCmdWindow(cmd)
	var buf bytes.Buffer
	w := newModuleLogWriter("install")
	cmd.Stdout = io.MultiWriter(w, &buf)
	cmd.Stderr = io.MultiWriter(w, &buf)
	err := cmd.Run()
	w.Flush()
	return buf.String(), err
}

// waitForServerReady 等待服务就绪：ready=true 表示已响应；
// ready=false 时 why 为 "exited"（服务进程已退出，快速失败）或 "timeout"（超时但进程仍在运行）。
func waitForServerReady(url string, serverExited <-chan error, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-serverExited:
			return false, "exited"
		case <-ticker.C:
			if time.Now().After(deadline) {
				return false, "timeout"
			}
			if resp, err := client.Get(url); err == nil {
				resp.Body.Close()
				if resp.StatusCode < 500 {
					return true, ""
				}
			}
		}
	}
}

// serverResponding 快速探测服务是否已在运行（端口是否已被占用）。
func serverResponding(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

func notifyReady() {
	if os.Getenv("DSH_SYSTRAY_SHOW_WINDOW") == "1" {
		return // 截图/预览模式：不弹就绪提示
	}
	showReadyPrompt(webURL)
}

// extractLargestPNG 从 ICO 中提取最大尺寸的 PNG 条目（macOS 菜单栏需要 PNG 格式）。
func extractLargestPNG(ico []byte) []byte {
	if len(ico) < 6 {
		return ico
	}
	count := int(binary.LittleEndian.Uint16(ico[4:6]))
	var best []byte
	bestPixels := 0
	for i := 0; i < count; i++ {
		off := 6 + i*16
		if off+16 > len(ico) {
			break
		}
		w, h := int(ico[off]), int(ico[off+1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		size := int(binary.LittleEndian.Uint32(ico[off+8 : off+12]))
		dataOff := int(binary.LittleEndian.Uint32(ico[off+12 : off+16]))
		if dataOff+size > len(ico) {
			continue
		}
		if w*h > bestPixels {
			bestPixels = w * h
			best = ico[dataOff : dataOff+size]
		}
	}
	if best == nil {
		return ico
	}
	return best
}

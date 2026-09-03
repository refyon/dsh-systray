package main

import (
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
)

func serviceStatusText() string {
	if serviceFailed.Load() {
		if s, _ := serviceFailReason.Load().(string); s != "" {
			return s
		}
		return "服务启动失败"
	}
	if serverReady.Load() {
		return "服务已停止"
	}
	return "服务启动中…"
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

	// 自愈历史自启动项：旧版本注册的自启动条目未带 --autostart 参数，导致登录时被当作“手动双击”而弹提示。
	// 注意：bindings 生成进程（wailsbindings.exe，wails build/generate 运行）不执行任何真实副作用。
	if !bindingsRun && isAutostartEnabled() {
		enableAutostart() // 幂等：确保条目含 --autostart
		log.Printf("autostart entry refreshed with --autostart flag")
	}

	cfgDir, err := os.UserConfigDir()
	if err != nil {
		cfgDir = os.TempDir()
	}
	logDir = filepath.Join(cfgDir, "dsh-systray", "logs")
	if !bindingsRun {
		if err := os.MkdirAll(logDir, 0o755); err == nil {
			if f, ferr := os.OpenFile(filepath.Join(logDir, "app.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
				defer f.Close()
				log.SetOutput(f)
			}
		}
	}
	log.SetFlags(log.LstdFlags)

	release, acquired := acquireSingleInstance()
	singleInstanceRelease = release
	if !acquired {
		if bindingsRun {
			return // bindings 生成进程：直接退出，不弹窗
		}
		// 已在运行：弹窗提示后退出，不产生第二个托盘图标。
		showMessageBox("DeepSeek Harness 已在运行中，请使用系统托盘图标操作。", appName)
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
		StartHidden:   autostartLaunch || (!shotMode && startupEnvReady()),
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
	if runtime.GOOS == "darwin" {
		start, _ := systray.RunWithExternalLoop(onReady, onExit)
		start()
	}
	go bootstrapService()
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
	// 截图/预览模式：窗口自身置顶，确保屏幕截取不被其它窗口遮挡
	if os.Getenv("DSH_SYSTRAY_SHOW_WINDOW") == "1" {
		wruntime.WindowSetAlwaysOnTop(ctx, true)
	}
	if !autostartLaunch {
		wruntime.EventsEmit(ctx, "ui:show-splash", nil)
	}
}

// onBeforeClose 窗口关闭回调（Wails 的 Quit 也经此拦截）：
// - 托盘「退出」流程（quitRequested）：放行（返回 false），允许应用退出；
// - 窗口 X：隐藏窗口并阻止关闭（托盘常驻）；更新进行中先询问是否取消更新。
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
		saveConfig(appConfig{Port: port, HarnessDir: harnessDir, StartupTimeoutSec: int(startupTimeout / time.Second), UpdateMirror: updateMirrorOverride, HarnessPrerelease: harnessPrereleaseOverride})
	}

	// 未显式配置时：自动探测已存在的 harness 源码 checkout（如各盘符根目录下的 deepseek-harness）
	if !harnessDirExplicit {
		if found := findExistingHarnessDir(); found != "" {
			harnessDir = found
			saveConfig(appConfig{Port: port, HarnessDir: harnessDir, StartupTimeoutSec: int(startupTimeout / time.Second), UpdateMirror: updateMirrorOverride, HarnessPrerelease: harnessPrereleaseOverride})
			log.Printf("detected existing harness at %s", found)
		}
	}

	splash := maybeStartSplash("正在准备运行环境…")

	// 0.5) 解压工具：环境检查加入 7-Zip（优先下载使用，Windows/macOS 均可）；失败不阻塞启动（zip 有 Go 兜底）。
	splash.Update("正在检查解压工具…", 0.07)
	ensureArchiveTool(func(t string, pct float64) { splash.Update(t, 0.07+0.01*pct) })

	// 1) 运行环境：优先便携 Node.js / pnpm（无管理员权限、无窗口、后台静默）
	if !runtimeOK() {
		splash.Update("正在下载 Node.js / pnpm 运行时（首次约 1-3 分钟）…", 0.08)
		if err := ensureRuntime(splash); err != nil {
			splash.Close()
			showMessageBox("下载运行环境失败：\n"+err.Error()+"\n\n请检查网络后重试；日志："+filepath.Join(logDir, "app.log"), appName)
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
			splash.Update("正在安装 harness 依赖（首次约 2-5 分钟）…", 0.35)
			if err := runSourceDepsInstall(); err != nil {
				splash.Close()
				showMessageBox("安装 harness 依赖失败：\n"+err.Error()+"\n\n日志："+filepath.Join(logDir, "install.log"), appName)
				return
			}
		}
		if !harnessBuiltOK() {
			splash.Update("正在构建 harness 前端产物（首次约 1-3 分钟）…", 0.55)
			if err := runHarnessBuild(); err != nil {
				splash.Close()
				showMessageBox("harness 构建失败：\n"+err.Error()+"\n\n日志："+filepath.Join(logDir, "build.log"), appName)
				return
			}
		}
	case "missing":
		splash.Update("正在安装 DeepSeek Harness（首次约 2-5 分钟）…", 0.35)
		if err := ensureNpmHarness(); err != nil {
			splash.Close()
			showMessageBox("安装 DeepSeek Harness 失败：\n"+err.Error()+"\n\n日志："+filepath.Join(logDir, "install.log"), appName)
			return
		}
	}

	// 3) 启动服务
	splash.Update("正在启动服务…", 0.9)
	started := false
	startedByUs := false
	var serverExitCh <-chan error
	serverLogBefore := serverLogSize() // 本次启动的健康校验只扫描此后的追加日志段
	if serverResponding(webURL) {
		log.Printf("server already running on %s, skipping spawn", webURL)
		started = true
	} else {
		started, serverExitCh = startServer()
		startedByUs = started
	}
	if !started {
		splash.Close()
		showMessageBox("启动 DeepSeek Harness 服务失败，请查看日志：\n"+filepath.Join(logDir, "app.log"), appName)
		return
	}
	closeSplash := splash.Close

	go func() {
		ready, why := waitForServerReady(webURL, serverExitCh, startupTimeout)
		closeSplash()
		if quitting.Load() {
			return
		}
		// 本进程拉起的服务就绪后再扫启动日志（就绪 ≠ 健康：版本混装/插件不兼容会报加载错误）
		bootError := ""
		if ready && startedByUs {
			time.Sleep(2 * time.Second)
			if serverLogHasBootErrors(serverLogBefore) {
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
			reason := bootError
			if reason == "" {
				reason = "服务进程异常退出"
			}
			rolled, prev := tryBootRollback(reason)
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
		showMessageBox("DeepSeek Harness 服务启动较慢（首次启动或机器性能较低时属正常现象），已继续在后台启动，就绪后会再次提示。\n日志："+filepath.Join(logDir, "server.log"), appName)
		if ready2, _ := waitForServerReady(webURL, serverExitCh, 15*time.Minute); ready2 && !quitting.Load() {
			serverReady.Store(true)
			serviceFailed.Store(false)
			refreshServiceMenu()
			notifyReady()
		} else if !quitting.Load() {
			serviceFailed.Store(true)
			serviceFailReason.Store("服务启动失败（超时）")
			refreshServiceMenu()
			showMessageBox("DeepSeek Harness 服务最终未能就绪，请查看日志：\n"+filepath.Join(logDir, "server.log"), appName)
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
	systray.SetTitle(appName)
	systray.SetTooltip(appName)

	// 状态说明行：禁用样式（置灰、不可点击），仅作状态提示；「打开 Web UI」未就绪时隐藏、就绪时显示可点
	menuStatus = systray.AddMenuItem("服务启动中…", "后台服务状态")
	menuStatus.Disable()
	menuOpen = systray.AddMenuItem("打开 Web UI", "打开网页端界面")
	refreshServiceMenu()
	// 周期刷新，保证每次打开托盘菜单都反映服务实时状态
	go pollServiceMenu()
	systray.AddSeparator()
	mSettings := systray.AddMenuItem("设置", "打开设置窗口")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "退出并关闭后台服务器")

	menuOpen.Click(func() {
		if running, _, live := resolveRunningService(); running {
			openBrowser(live)
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
	if on {
		enableAutostart()
		log.Printf("autostart enabled (settings)")
	} else {
		disableAutostart()
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

// runHarnessBuild 执行 pnpm run build（输出写入 build.log），超时 10 分钟。
func runHarnessBuild() error {
	buildLogPath := filepath.Join(logDir, "build.log")
	f, err := os.OpenFile(buildLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		f = nil // 无法写日志不阻塞构建
	}
	if f != nil {
		defer f.Close()
	}

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
	if f != nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pnpm run build failed: %w（构建日志：%s）", err, buildLogPath)
	}
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

// pnpmTunedEnv 慢机器调优：限制并发、克隆式安装（参考 dsh-desktop）。
func pnpmTunedEnv() []string {
	return []string{
		"PNPM_MAX_WORKERS=1",
		"npm_config_child_concurrency=1",
		"npm_config_package_import_method=clone-or-copy",
		"npm_config_side_effects_cache=false",
	}
}

// runSourceDepsInstall 源码模式：pnpm install（输出写入 install.log）。
func runSourceDepsInstall() error {
	logPath := filepath.Join(logDir, "install.log")
	f, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if f != nil {
		defer f.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, pnpmCmd(), "install")
	cmd.Dir = harnessDir
	cmd.Env = append(os.Environ(), pnpmTunedEnv()...)
	hideCmdWindow(cmd)
	if f != nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pnpm install failed: %w（日志：%s）", err, logPath)
	}
	return nil
}

// ensureNpmHarness 全新机器：安装 npm 预构建产物 @deepseek-ai/dsh（免 git / 免构建）。
func ensureNpmHarness() error {
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
	logPath := filepath.Join(logDir, "install.log")
	f, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if f != nil {
		defer f.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, pnpmCmd(), "add", "@deepseek-ai/dsh@0.1.1-rc.2", "--save-exact")
	cmd.Dir = harnessDir
	cmd.Env = append(os.Environ(), pnpmTunedEnv()...)
	hideCmdWindow(cmd)
	if f != nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Run(); err != nil {
		// 构建脚本已白名单；若 dsh 入口已就绪，则即便 pnpm 因个别原生构建失败也视为安装完成
		if isNpmHarnessReady() {
			log.Printf("npm harness installed (pnpm reported: %v)", err)
		} else {
			return fmt.Errorf("安装 @deepseek-ai/dsh 失败：%w（日志：%s）", err, logPath)
		}
	}
	if !isNpmHarnessReady() {
		return fmt.Errorf("安装后未找到 dsh 入口：%s", filepath.Join(harnessDir, "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"))
	}
	log.Printf("npm harness installed at %s", harnessDir)
	return nil
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

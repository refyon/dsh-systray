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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dsh-systray/internal/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
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
	logDir     string
	webURL     string
	harnessDir string
	port       int
	// trustedHosts 配置里声明的额外信任地址，启动服务时透传给 dsh web（--trusted-host）。
	// dsh 的 /api 有 Host/Origin 栅栏：手机经端口转发/隧道访问时，必须声明"手机看到的地址"，
	// 否则界面能打开但对话 403。见 trustedHostFlags。
	trustedHosts       []string
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
		markServerResponsive()
		return true, port, webURL
	}
	if serverStartedPort != 0 && serverStartedPort != port {
		alt := fmt.Sprintf("http://127.0.0.1:%d/", serverStartedPort)
		if serverResponding(alt) {
			markServerResponsive()
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
	// MirrorBase 自建 GitHub 中转 Worker 的基址（如 https://dsh-mirror.example.com）。
	// 设置后：① 所有 GitHub 下载走 `<base>/gh/`（等价 updateMirror 前缀，自动排在最前）；
	// ② 私有仓（github: 来源）插件改用 `<base>/p/<owner>/<repo>/tar.gz/<sha>` 安装/更新——
	// 国内直连 codeload 实测 0.2 Mbps，经 Cloudflare 边缘 9-12 Mbps。
	MirrorBase string `json:"mirrorBase,omitempty"`
	// HarnessPrerelease 允许把 alpha/beta/rc 等预发布版视为 DeepSeek Harness 的可更新版本（默认关闭）。
	HarnessPrerelease bool `json:"harnessPrerelease"`
	// Language 界面语言偏好：auto（跟随系统）| zh | en；缺省 auto。运行时解析见 i18n.go。
	Language string `json:"language"`
	// TrustedHosts 额外信任的访问地址（host 或 host:port，如 192.168.1.5:8899）。
	// dsh web 的 /api 有 Host/Origin 栅栏，只认 loopback 与这里声明的地址：手机经端口转发
	// 或隧道访问时，必须把"手机看到的那个地址"声明进来，否则界面能打开但无法对话（403）。
	TrustedHosts []string `json:"trustedHosts,omitempty"`
	// AccountAPIBase 账号同步服务地址；空 = 内置正式域名（见 account.go 的 defaultAccountAPIBase）。
	AccountAPIBase string `json:"accountApiBase,omitempty"`
	// Proxy 出网代理（见 netproxy.go）：
	//   auto（默认）  自动：显式地址 > HTTP_PROXY/HTTPS_PROXY 环境变量 > Windows 系统代理 > 直连
	//   direct        强制直连，忽略环境变量与系统代理
	//   代理地址      如 http://127.0.0.1:10808 / socks5://127.0.0.1:10808（对全部外部地址生效）
	// 本机回环与私网地址恒不走代理（托盘自身服务探测）。环境变量 DSH_SYSTRAY_PROXY 优先级更高。
	Proxy string `json:"proxy,omitempty"`
	// PendingPluginOps 待应用的插件变更（更新/删除/启用）：点击后只登记，等用户在关闭设置窗口时
	// 确认、或在关于页点「立即应用」才执行（整批一次重启）。跨托盘重启保留，见 plugin_batch.go。
	PendingPluginOps []pendingPluginOp `json:"pendingPluginOps,omitempty"`
	// LaunchTarget 托盘「打开」的默认启动方式：auto（跟随检测，默认）| web | desktop。
	// auto 时「装了官方桌面端就用桌面端」，web/desktop 为用户的显式选择（桌面端缺失时
	// desktop 自动回退 web）。同时决定设置页「版本 / 检查更新 / 更新 / 重置」作用于哪个
	// harness 引擎：web = 本程序装在 harnessDir 的 dsh web；desktop = 官方桌面端内置引擎。
	// 见 desktop_app.go。
	LaunchTarget string `json:"launchTarget,omitempty"`
	// LaunchMismatchAck 用户就「启动方式偏好与实机不符」选择「保持现状」时记下的偏好值：
	// 同一偏好不再重复询问（用户显式改动启动方式时清除）。见 launchTargetMismatch。
	LaunchMismatchAck string `json:"launchMismatchAck,omitempty"`
}

// pendingPluginOp 一条待应用插件变更的持久化形态（config.json）。
type pendingPluginOp struct {
	ID string `json:"id"` // 插件行稳定标识（PluginRow.ID）
	Op string `json:"op"` // update | remove
	// Profile 登记该变更时的插件环境（web | desktop）：应用时据此只改那个环境。
	// 旧配置无此字段（空）= 按当前启动方式的环境处理。
	Profile string `json:"profile,omitempty"`
	// Risk 删除登记时检测到的会话数据风险（该插件写入的自定义事件）：跨重启保留警示与「修复」入口。
	Risk *pluginSessionRisk `json:"risk,omitempty"`
}

// configFilePath 用户配置目录下的 config.json（Windows: %APPDATA%\dsh-systray；macOS: ~/Library/Application Support/dsh-systray）。
func configFilePath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "dsh-systray", "config.json")
	}
	return ""
}

// trustedHostFlags 把配置里声明的额外信任地址转成 dsh web 的命令行参数。
// 过滤空白项并去重，避免空参数让 dsh 启动即报错。
func trustedHostFlags() []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range trustedHosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, "--trusted-host", h)
	}
	return out
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
	if v := strings.TrimRight(strings.TrimSpace(f.MirrorBase), "/"); v != "" {
		cfg.MirrorBase = v
	}
	if f.HarnessPrerelease {
		cfg.HarnessPrerelease = true
	}
	if l := normalizeLang(f.Language); l != "auto" {
		cfg.Language = l
	}
	if v := strings.TrimSpace(f.AccountAPIBase); v != "" {
		cfg.AccountAPIBase = v
	}
	// proxy 支持显式 "direct"/"auto"（不能被「空串即忽略」的写法吞掉，故不加非空判断）。
	if v := strings.TrimSpace(f.Proxy); v != "" {
		cfg.Proxy = v
	}
	if len(f.PendingPluginOps) > 0 {
		cfg.PendingPluginOps = f.PendingPluginOps
	}
	if len(f.TrustedHosts) > 0 {
		cfg.TrustedHosts = f.TrustedHosts
	}
	if v := strings.TrimSpace(f.LaunchTarget); v != "" {
		cfg.LaunchTarget = v
	}
	if v := strings.TrimSpace(f.LaunchMismatchAck); v != "" {
		cfg.LaunchMismatchAck = v
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
	// 启动方式：环境变量优先（调试与截图脚本可用它固定 web / desktop 形态，
	// 与 DSH_SYSTRAY_LANG 同类的临时覆盖，不写回 config.json）。
	if v := strings.TrimSpace(os.Getenv("DSH_SYSTRAY_LAUNCH_TARGET")); v != "" {
		cfg.LaunchTarget = v
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

// accountAPIBaseGlobal 运行时生效的账号同步服务地址（空 = 内置正式域名，见 account.go）。
// 保存配置时回写用——store 里的覆盖值由 setAccountAPIBase 写入，需与文件内容保持一致。
var accountAPIBaseGlobal string

// currentConfig 从当前运行时状态汇总 appConfig，供 saveConfig 落盘。
//
// 统一入口的理由：此前三个保存点各自手写结构体字面量，字段集合不一致——bootstrapService
// 的两处保存（回退默认 harness 目录 / 自动探测到既有目录）会静默丢掉 accountApiBase、
// trustedHosts 与待应用插件变更。集中一处后新增字段不会再漏（proxy 即借此接入）。
func currentConfig() appConfig {
	return appConfig{
		Port:              port,
		HarnessDir:        harnessDir,
		StartupTimeoutSec: int(startupTimeout / time.Second),
		UpdateMirror:      updateMirrorOverride,
		MirrorBase:        mirrorBase,
		HarnessPrerelease: harnessPrereleaseOverride,
		Language:          langPref,
		TrustedHosts:      trustedHosts,
		AccountAPIBase:    accountAPIBaseGlobal,
		Proxy:             proxyConfigValueOf(),
		PendingPluginOps:  pluginPendingOps(),
		LaunchTarget:      launchTargetPref,
		LaunchMismatchAck: launchMismatchAck,
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
	return splashForFlow(text, false)
}

// splashForFlow 显示进度视图。interactive = 由用户操作触发（切回 Web UI、按需启动服务）：
// 即使当前进程是开机自启启动的，也必须让用户看到进度与结果，不能走静默空实现。
func splashForFlow(text string, interactive bool) *SplashState {
	if autostartLaunch && !interactive {
		return &SplashState{Update: func(string, float64) {}, Close: func() {}}
	}
	return startSplash(text)
}

// 后台服务状态（托盘菜单）：四态实时反映——运行中/已停止/启动失败/启动中。
var (
	serverReady       atomic.Bool
	serviceFailed     atomic.Bool
	serviceFailReason atomic.Value      // string
	serviceFailKind   atomic.Value      // string：失败分类 ""|port-blocked|port-in-use（见 portcheck.go）
	menuOpen          *systray.MenuItem // “打开 Web UI”
	menuStatus        *systray.MenuItem // 状态说明行
	mSettings         *systray.MenuItem // “设置”
	mQuit             *systray.MenuItem // “退出”
	// lastTrayLaunchMode 上一次写入「打开」菜单文案的启动方式（web | desktop）：文案只在
	// refreshTrayTexts 与 refreshServiceMenu 的方式变化分支里写，据此判断是否已落后于解析结果。
	lastTrayLaunchMode atomic.Value // string
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

// trayOpenTitle / trayOpenTooltip 托盘「打开」菜单项文案：随启动方式切换
// （desktop → 「打开 Desktop UI」并拉起官方桌面端）。见 desktop_app.go。
func trayOpenTitle() string {
	if launchTargetIsDesktop() {
		return T("打开 Desktop UI")
	}
	return T("打开 Web UI")
}

func trayOpenTooltip() string {
	if launchTargetIsDesktop() {
		return T("打开官方桌面端")
	}
	return T("打开网页端界面")
}

// trayLaunchModeTag 当前解析结果对应的启动方式标签（web | desktop）。
func trayLaunchModeTag() string {
	if launchTargetIsDesktop() {
		return launchTargetDesktop
	}
	return launchTargetWeb
}

// trayTextsNeedSync 「打开」菜单文案是否已落后于当前解析结果（纯判据，便于测试）。
func trayTextsNeedSync() bool {
	last, _ := lastTrayLaunchMode.Load().(string)
	return last != trayLaunchModeTag()
}

// refreshServiceMenu 按服务实际运行状态刷新托盘菜单（可跨线程、可周期调用）。
// 就绪判定基于实际运行端口（配置端口或本进程最后启动端口），避免修改端口后、
// 重启前「打开 Web UI」被错误禁用/指向不可达地址。
//
// desktop 启动方式下「打开 Desktop UI」与后台 Web 服务状态无关（桌面端自带 Host），
// 只要装了桌面端就常显可点；服务状态不再占用菜单行，避免「服务没起来 → 桌面端入口消失」。
func refreshServiceMenu() {
	if menuOpen == nil || menuStatus == nil {
		return
	}
	ready, _, _ := resolveRunningService()
	desktopTarget := launchTargetIsDesktop()
	systray.RunOnLoop(func() {
		if menuOpen == nil || menuStatus == nil {
			return
		}
		// 文案随解析结果同步：自动检测（auto）会在运行期翻转启动方式（桌面端装上/卸掉、检测失效），
		// 那条路径不经过 refreshTrayTexts，不补这一步菜单会一直写着旧方式，而点击动作由
		// openDefaultUI 按**点击时**的解析结果分派——表现为「菜单写着 Web UI，点开却是桌面端」。
		// 判据与写入放在同一处，保证记下的方式与写进去的文案一致；只在方式确实变化时改写：
		// Windows 上每次 SetTitle 都会打一次 SetMenuItemInfo 并触发菜单宽度重算，2 秒轮询不该每轮都做。
		if trayTextsNeedSync() {
			menuOpen.SetTitle(trayOpenTitle())
			menuOpen.SetTooltip(trayOpenTooltip())
			lastTrayLaunchMode.Store(trayLaunchModeTag())
		}
		if desktopTarget {
			// desktop 启动方式：「打开 Desktop UI」常显可点（与后台服务状态解耦）。
			// 但服务失败时不静默——失败原因仍占一行，否则「Web UI 打不开」在托盘里无从排查。
			menuOpen.Show()
			menuOpen.Enable()
			if serviceFailed.Load() {
				menuStatus.Show()
				menuStatus.SetTitle(serviceStatusText())
				return
			}
			menuStatus.Hide()
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
			menuOpen.SetTitle(trayOpenTitle())
			menuOpen.SetTooltip(trayOpenTooltip())
			lastTrayLaunchMode.Store(trayLaunchModeTag()) // 记下文案对应方式，刷新后不必再同步
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
// 顺带做启动方式漂移检查（desktop → web，见 checkLaunchTargetDrift）。
func pollServiceMenu() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if quitting.Load() {
				return
			}
			checkLaunchTargetDrift()
			refreshServiceMenu()
		}
	}
}

// ==================== 启动方式漂移（双向） ====================
//
// 偏好没变而解析结果变了，就是实机漂移，两个方向都要收尾（托盘文案由 refreshServiceMenu 同步）：
//
//	desktop → web：官方桌面端被卸载（或检测失效），解析回退 Web UI，而 desktop 形态下后台服务
//	              从未启动——托盘「打开」此时没有可用目标。问用户是否现在启动后台服务；不静默
//	              拉起（desktop 形态下用户并没有在用网页端）。
//	web → desktop：装上了官方桌面端，解析切到桌面端（自带引擎）。此时端口上继续跑着的后台
//	              Web 服务只白占资源——显式切换由 setLaunchTarget 直接停服，漂移路径照同样语义
//	              询问，但不静默杀掉用户可能正在用的网页端会话。

// lastResolvedLaunch 上一次解析出的启动方式，lastResolvedPref 是当时的偏好——两者合起来构成
// 漂移检测基线：只有「偏好没变、解析结果却变了」才是实机漂移（桌面端被卸载/检测失效）；
// 用户显式改偏好走 setLaunchTarget 自己的启停流程，不在这里重复询问。
// launchDriftAsked 本次漂移是否已询问过（用户选择「暂不处理」后不再重复打扰）。
var (
	lastResolvedLaunch atomic.Value // string
	lastResolvedPref   atomic.Value // string
	launchDriftAsked   atomic.Bool
)

// askStartServiceFn / askStopServiceForDriftFn 两个漂移询问的实现（测试可替换，避免真实弹窗）。
var (
	askStartServiceFn        = askStartService
	askStopServiceForDriftFn = askStopServiceForDrift
)

// rememberLaunchTargetBaseline 把「当前偏好 + 当前解析结果」记为漂移检测基线。
// 显式切换启动方式（setLaunchTarget）与轮询发现变化（checkLaunchTargetDrift）都会调用，
// 使「用户改了偏好」与「实机漂移」可区分（见 checkLaunchTargetDrift 的 prevPref 判据）。
func rememberLaunchTargetBaseline() {
	lastResolvedPref.Store(normalizeLaunchTarget(launchTargetPref))
	lastResolvedLaunch.Store(resolvedLaunchTarget())
	launchDriftAsked.Store(false)
}

// checkLaunchTargetDrift 检测启动方式漂移（web ↔ desktop），并按用户选择收尾服务生命周期。
// 只由 pollServiceMenu 的周期 goroutine 调用（状态无并发写）。
func checkLaunchTargetDrift() {
	pref := normalizeLaunchTarget(launchTargetPref)
	cur := resolvedLaunchTarget()
	// 判定期间用户可能刚在设置页改了偏好（setLaunchTarget 在 UI 线程写 launchTargetPref）：
	// 偏好已变即本次判定作废，交由 setLaunchTarget 的启停流程收尾。
	if normalizeLaunchTarget(launchTargetPref) != pref {
		return
	}
	prev, _ := lastResolvedLaunch.Load().(string)
	prevPref, _ := lastResolvedPref.Load().(string)
	if prev == cur && prevPref == pref {
		return
	}
	lastResolvedLaunch.Store(cur)
	lastResolvedPref.Store(pref)
	launchDriftAsked.Store(false) // 进入新的启动方式：允许下一次漂移重新询问
	// 偏好被显式改动（用户自己选了 Web UI / Desktop UI / 自动检测）：不是实机漂移，不询问。
	if prevPref != pref {
		return
	}
	switch {
	case prev == launchTargetDesktop && cur == launchTargetWeb:
		if !launchDriftAsked.CompareAndSwap(false, true) {
			return
		}
		if running, _, _ := resolveRunningService(); running {
			return // 服务已在跑（用户在别处起的）：无需询问
		}
		log.Printf("[launch] desktop target unavailable, fell back to web ui: asking user to start service")
		go func() {
			if askStartServiceFn() {
				startServiceBootstrap(true)
			}
		}()
	case prev == launchTargetWeb && cur == launchTargetDesktop:
		if !launchDriftAsked.CompareAndSwap(false, true) {
			return
		}
		if running, _, _ := resolveRunningService(); !running {
			return // 服务没在跑：桌面端形态本就该如此，不打扰
		}
		log.Printf("[launch] desktop target available, web service still running: asking user to stop it")
		go func() {
			if askStopServiceForDriftFn() {
				stopServiceForDesktopTarget()
			}
		}()
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
	// 装配「迟到的启动错误」自愈动作：健康校验提前通过后由后台兜底监视调用（见 updater.go
	// lateBootSelfHeal 说明——这里装配而非直接调用，避免初始化依赖环）。
	lateBootSelfHeal = runLateBootSelfHeal

	cfg := loadConfig()
	updateMirrorOverride = cfg.UpdateMirror
	mirrorBase = strings.TrimRight(cfg.MirrorBase, "/")
	harnessPrereleaseOverride = cfg.HarnessPrerelease
	setAccountAPIBase(strings.TrimSpace(cfg.AccountAPIBase))
	accountAPIBaseGlobal = strings.TrimSpace(cfg.AccountAPIBase)
	// 出网代理：须早于任何网络请求生效（显式地址 / 环境变量 / Windows 系统代理）。
	applyProxyConfig(cfg.Proxy)
	port = cfg.Port
	trustedHosts = cfg.TrustedHosts // 供 startServer 透传给 dsh web（见 trustedHostFlags）
	webURL = fmt.Sprintf("http://127.0.0.1:%d/", port)
	harnessDir = cfg.HarnessDir
	startupTimeout = time.Duration(cfg.StartupTimeoutSec) * time.Second
	// 启动方式：auto（默认，按是否装了官方桌面端解析）/ web / desktop；见 desktop_app.go。
	launchTargetPref = normalizeLaunchTarget(cfg.LaunchTarget)
	launchMismatchAck = strings.TrimSpace(cfg.LaunchMismatchAck)
	// 语言：config 未写 language（旧版本升级 / 全新安装）默认简体中文，避免旧用户升级后
	// 因「跟随系统」检测到英文系统语言而整体变英文（0.8.0 升级反馈）；显式 auto/zh/en 按选择生效。
	langPref = "zh"
	if cfg.Language != "" {
		langPref = normalizeLang(cfg.Language)
	}
	// 截图/预览模式可用 DSH_SYSTRAY_LANG 覆盖界面语言：生成英文截图时不必改动用户配置
	// （脚本 scripts/render_shots.mjs 依赖它，见该文件头部说明）。
	if v := strings.TrimSpace(os.Getenv("DSH_SYSTRAY_LANG")); v != "" {
		if l := normalizeLang(v); l != "auto" {
			langPref = l
		}
	}
	curLang = resolveLang(langPref)
	log.Printf("[i18n] language pref=%q system=%s → curLang=%s", cfg.Language, detectSystemLang(), curLang)
	// 待应用的插件变更随 config 持久化：先存下，等插件列表可用（onStartup）时逐条校验载入。
	pendingPluginOpsFromConfig = cfg.PendingPluginOps
	// 登录态（account.json）与待上报操作记录：只读载入，不阻塞启动。
	initAccountState()
	// 文件同步清单（filesync.json）：同样只读载入，联网与扫描在后台循环里做。
	initFileSyncState()

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
	if v := strings.TrimSpace(os.Getenv("DSH_SYSTRAY_LOG_DIR")); v != "" {
		logDir = expandTildePath(v) // 显式覆盖（诊断/特殊部署用）
	}
	logFallbackNote := ""
	if !bindingsRun {
		// 统一日志：所有行为（自身/UI/托盘/子进程）写入 logDir/dsh-systray.log
		// （进程级单例句柄，行格式 ts [LEVEL] [module] message；见 logsetup.go）。
		// **优先保住主目录**：先直接打开，失败则 chmod / 改名让位后重试（自愈），只有自愈也
		// 不成功才回退系统临时目录，并把原因写进启动日志与 stderr——否则日志页（读主目录）
		// 看不到这次启动，表现为"日志时有时无"（2026-09-24 现场）。
		if ok, note := initUnifiedLogWithRepair(); !ok {
			logFallbackNote = note
			logDir = filepath.Join(os.TempDir(), "dsh-systray", "logs")
			if _, note2 := initUnifiedLogWithRepair(); note2 != "" {
				logFallbackNote += "；临时目录也不可用：" + note2
			}
		} else if note != "" {
			logFallbackNote = note
		}
		mergeLegacyLogs() // 升级迁移：合并旧多文件日志后删除源文件（须先于任何新日志写入）
		// stderr 双写：macOS 上从 Console/unified 日志也能看到应用日志（诊断兜底）
		log.SetOutput(io.MultiWriter(appLogWriter{}, os.Stderr))
	}
	log.SetFlags(log.LstdFlags)
	if !bindingsRun {
		if logFallbackNote != "" {
			log.Printf("[log] 日志落盘说明：%s", logFallbackNote)
		}
		// 启动首行：固定记录版本/pid/日志路径/执行上下文（完整性级别+会话号），
		// 便于对照「日志页显示路径」与实际落盘位置，并在托盘注册失败时可一眼看出
		// "能起来的那次"与"起不来的那次"是否处在不同执行上下文。
		log.Printf("dsh-systray v%s starting (pid=%d), log file: %s, %s",
			appVersion, os.Getpid(), unifiedLogPath(), processExecutionContext())
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
		// 托盘初始化失败必须让用户看见：本程序没有主窗口（StartHidden），失败时既无图标也
		// 无窗口，只能靠弹窗告知，否则表现为"双击没反应"（2026-09-24 现场：initInstance
		// 返回 Access is denied，只写日志就 return，用户与排查者都无从判断）。
		systray.SetOnFail(func(err error) {
			log.Printf("systray failed: %v", err)
			showMessageBox(TF("系统托盘未能启动，程序无法继续运行。\n\n%v\n\n日志：%s", err, unifiedLogPath()), appName)
			os.Exit(1)
		})
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
		// desktop 启动方式下没有启动进度要展示（不跑引导流程），与自启动一样直接隐藏窗口；
		// 截图/预览模式必须显示窗口（脚本依赖它截设置页）。
		StartHidden: autostartLaunch || (!shotMode && (launchTargetIsDesktop() || startupEnvReady())),
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			Theme:                windows.SystemDefault,
		},
		// macOS：关闭缩放（最大化）按钮——主窗口是固定尺寸的，缩放只会把它拉到屏幕左上角。
		Mac: &mac.Options{
			DisableZoom: true,
		},
	})
	if err != nil {
		log.Printf("wails run: %v", err)
	}
}

// onStartup Wails 应用启动回调：建立上下文、启动 macOS 托盘、开始后台服务编排。
func onStartup(ctx context.Context) {
	appCtx = ctx
	loadStartupPendingPluginOps()       // 跨托盘重启保留「待应用变更未生效」提示（逐条校验后载入）
	revalidatePendingApplyOnStartup()   // 同步待生效集合按本机现状重校验（上次应用中途退出的自愈）
	startAccountBackground(ctx)         // 启动自动登录校验 + 每 20 分钟一次的后台同步检查（需求④⑤）
	startFileSyncBackground(ctx)        // 文件同步：每 60 秒扫描本机，有变更或超时即上传/对账
	startServiceWatchdog(ctx, nil, nil) // 服务存活看门狗：静默死亡（如沿用的服务）自动拉起
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
		// 与 Windows 对称：托盘注册失败必须可见（本程序无主窗口，失败即"毫无反应"）
		systray.SetOnFail(func(err error) {
			log.Printf("systray failed: %v", err)
			showMessageBox(TF("系统托盘未能启动，程序无法继续运行。\n\n%v\n\n日志：%s", err, unifiedLogPath()), appName)
			os.Exit(1)
		})
		start, _ := systray.RunWithExternalLoop(onReady, onExit)
		start()
	}
	// 启动方式与实机不符（配置为 Desktop UI 但未装桌面端 / 配置为 Web UI 但装了桌面端）：
	// 先让用户决定，再按最终配置决定是否启动后台服务（见 promptLaunchTargetMismatch）。
	if kind, ok := launchTargetMismatch(); ok {
		go promptLaunchTargetMismatch(kind)
		return
	}
	// 后台服务：desktop 启动方式下不自动启动——其 harness 引擎由官方桌面端自带，托盘自带的
	// Web 服务只服务 Web UI 形态。用户把启动方式切回 Web UI 时才按需拉起（见 SetLaunchTarget）。
	if launchTargetIsDesktop() {
		serverReady.Store(true) // 未启动且非失败：设置页显示「后台服务：已停止」而不是一直「启动中」
		notifySplashDone()      // 本形态没有启动引导：前端不要停在 splash 视图（未就绪机器上窗口会显示）
		signalShotReady()
		log.Printf("[startup] launch target=desktop: background service not started")
		refreshServiceMenu()
		// profile 级事务自愈与服务无关，desktop 形态必须照样执行（见 recoverProfileTransactions）
		go recoverProfileTransactions(nil)
		return
	}
	startServiceBootstrap(false)
}

// ==================== 启动方式偏好与实机不符时的询问 ====================
//
// 配置里明确写了启动方式、但实机情况与之矛盾时静默按回落结果运行会让用户困惑：配置 Desktop UI
// 却没装桌面端（什么都打不开），或配置 Web UI 但装了桌面端（用不上桌面端）。启动时询问一次，
// 用户做出决定或选择「保持现状」后不再重复询问（launchMismatchAck）。

// launchTargetMismatch 启动方式偏好与实机是否不符（需要询问用户）。
// kind：desktop-missing（配置 desktop 但未装桌面端）/ desktop-installed（配置 web 但装了桌面端）。
// auto 偏好不存在「不符」；截图模式不弹窗；已就同一偏好确认过（launchMismatchAck）也不再询问。
func launchTargetMismatch() (string, bool) {
	if shotMode {
		return "", false
	}
	pref := normalizeLaunchTarget(launchTargetPref)
	if pref == launchTargetAuto {
		return "", false
	}
	if launchMismatchAck == pref {
		return "", false
	}
	installed := desktopApp().Installed
	switch {
	case pref == launchTargetDesktop && !installed:
		return "desktop-missing", true
	case pref == launchTargetWeb && installed:
		if autostartLaunch {
			// 开机自启下不打扰：Web 形态完全可用，这里只是「你也可以用桌面端」的提示
			log.Printf("[launch] desktop app installed while launch target=web: prompt skipped on autostart")
			return "", false
		}
		return "desktop-installed", true
	}
	return "", false
}

// promptLaunchTargetMismatch 询问用户如何处理启动方式与实机不符，并按选择收尾：确认服务生命周期
// （只有最终按 Web UI 运行且服务未启动时才拉起）与安装流程。
// 在后台 goroutine 中执行（原生弹窗阻塞），先等界面就绪再问。
func promptLaunchTargetMismatch(kind string) {
	// profile 级事务自愈先跑（「安装桌面端」「改用 Desktop UI」等分支不会走 bootstrapService）
	recoverProfileTransactions(nil)
	// 询问期间什么都不在启动：状态先落到「已停止」（选择「改用 Web UI / 保持现状」后会启动服务）
	serverReady.Store(true)
	serviceFailed.Store(false)
	refreshServiceMenu()
	time.Sleep(700 * time.Millisecond) // 等托盘图标与设置窗口就绪，避免弹窗盖在启动画面上
	switch kind {
	case "desktop-missing":
		switch askLaunchTargetDesktopMissing() {
		case "install":
			logUI("启动方式与实机不符", "选择安装官方桌面端")
			startDesktopInstallFlow() // 异步下载 + 启动安装向导（进度走 splash）
			serverReady.Store(true)
			serviceFailed.Store(false)
			refreshServiceMenu()
		case "web":
			logUI("启动方式与实机不符", "选择改用 Web UI")
			setLaunchTarget(launchTargetWeb) // 内含：启动后台服务
		default:
			logUI("启动方式与实机不符", "保持现状（配置仍为 Desktop UI，先按 Web UI 运行）")
			rememberLaunchMismatchAck()
			startServiceBootstrap(true)
		}
	case "desktop-installed":
		switch askLaunchTargetDesktopInstalled() {
		case "desktop":
			logUI("启动方式与实机不符", "选择改用 Desktop UI")
			setLaunchTarget(launchTargetDesktop) // 内含：停止后台服务
		default:
			logUI("启动方式与实机不符", "保持现状（继续使用 Web UI）")
			rememberLaunchMismatchAck()
			startServiceBootstrap(true)
		}
	}
}

// rememberLaunchMismatchAck 记下用户对当前偏好「与实机不符」的选择：同一偏好不再重复询问
// （偏好被显式改动时清除，见 setLaunchTarget）。
func rememberLaunchMismatchAck() {
	launchMismatchAck = normalizeLaunchTarget(launchTargetPref)
	saveCurrentConfig()
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
	// 固定尺寸窗口：禁用最大化按钮（Windows 改窗口样式，macOS 走 mac.Options.DisableZoom）
	startDisableWindowMaximize("dsh-systray")
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

// askStopServerForQuit 退出前的「是否保留后台服务」询问（托盘「退出」与 macOS 真实退出共用）。
//
// desktop 启动方式下后台服务本就不该在运行（启动闸门不拉起、切换启动方式时已停止），询问没有
// 意义：直接按「停止并退出」处理（若仍有残留服务进程，退出清理会顺带收掉）。
// 返回 0=停止并退出 / 1=保留服务 / -1=取消退出。
func askStopServerForQuit() int {
	if launchTargetIsDesktop() {
		log.Printf("[quit] launch target=desktop: skip keep-server prompt")
		return 0
	}
	return askStopServer()
}

// onBeforeClose 窗口关闭回调（Wails 的 Quit 也经此拦截）：
//   - 托盘「退出」流程（quitRequested 已置位）：放行（返回 false），允许应用退出；
//   - Windows：窗口 X 仅隐藏窗口并阻止关闭（托盘常驻）；更新进行中先询问是否取消更新；
//   - macOS：红点关闭已由 HideWindowOnClose 直接隐藏（不到这里），到达此处即为真实退出
//     （Dock/⌘Q/托盘退出）→ 询问是否停止后台服务：确定/保留服务均放行退出，取消则留在前台
//     （desktop 启动方式下没有可保留的服务，见 askStopServerForQuit）。
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
		choice := askStopServerForQuit()
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

// onShutdown 退出清理：终止进行中的更新/同步应用、按 keepServerRunning 保留或停止后台服务。
func onShutdown(ctx context.Context) {
	quitting.Store(true)
	cancelActiveUpdate()
	// 同步「重启生效」中途退出是允许的（用户决策）：取消应用循环并让已完成的项保持落盘，
	// 下次启动由 revalidatePendingApplyOnStartup 重校验待生效集合，不留半途假象。
	if accountApplyBusy() {
		log.Printf("sync apply in progress: cancelling before exit")
		cancelAccountApply()
	}
	if keepServerRunning.Load() {
		keepPID := 0
		if serverCmd != nil && serverCmd.Process != nil {
			keepPID = serverCmd.Process.Pid
		}
		killChildProcesses(keepPID)
		// 服务继续跑，但它的输出由本进程 tail：停机前把残留半行补进统一日志。
		// 注意：服务输出是**文件句柄**（不是管道），本进程退出不会影响它继续写
		// ——2026-10-06 的「服务静默终止」正是管道读端随托盘退出消失导致的。
		stopServerLogTail()
		log.Printf("quit with backend server kept running")
	} else {
		stopServerLogTail()
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

// recoverProfileTransactions 启动时的 profile 级事务自愈（与「是否启动后台服务」无关）：
//  1. 上次插件导入恢复被中断（进程退出/窗口被杀）→ 按事务日志回退或续跑收尾；
//  2. 上次插件操作（更新/删除/同步应用）在快照后被强杀 → 残骸还原到操作前状态。
//
// 这两步只动 profile 文件，**desktop 启动方式下也必须执行**：否则 node_modules 会停在
// 被改名的快照状态，官方桌面端加载直接失败（desktop 形态不走 bootstrapService，故在此显式调用）。
// profileRecoveryMu 串行化 profile 级事务自愈：多条启动路径（bootstrap / desktop 分支 /
// 启动方式询问）都可能调用，而快照还原是「删活体 + 改名回填」的非幂等操作——并发跑第二次
// 会把刚还原好的 node_modules 删掉。
var profileRecoveryMu sync.Mutex

// progress 可为 nil（无进度视图时只记日志）。
func recoverProfileTransactions(progress func(text string)) {
	profileRecoveryMu.Lock()
	defer profileRecoveryMu.Unlock()
	if n := recoverInterruptedImport(); n > 0 && progress != nil {
		progress(fmt.Sprintf("已检测到上次未完成的导入恢复，自动回退 %d 个环境…", n))
	}
	for _, pf := range enumeratePluginProfiles() {
		recoverInterruptedPluginSnapshot(pf.dir)
	}
}

// setServiceFailed 统一写入失败状态（分类 + 原因 + 托盘菜单）。kind 取 portcheck.go 的
// failKind* 常量，普通失败传 ""。分类只在 serviceFailed 为真时透出（见 GetServiceState），
// 因此成功路径无需清理它。
func setServiceFailed(kind, reason string) {
	serviceFailed.Store(true)
	serviceFailKind.Store(kind)
	serviceFailReason.Store(reason)
	refreshServiceMenu()
}

// portFailureKindAndReason 端口不可用时的失败分类与原因（状态行 / 设置页 / 弹窗共用）：
// 以当前端口实测为准，避免沿用已经不成立的旧结论。
func portFailureKindAndReason(p int) (string, string) {
	kind := failKindPortBlocked
	if k, _ := probePort(p); k == portInUse {
		kind = failKindPortInUse
	}
	return kind, portFailReason(kind, p)
}

// failServiceOnPort 端口不可用导致无法启动的收尾：置失败状态与分类（前端据此显示
// 「改用推荐端口」入口），返回用户可读原因供调用方展示。
func failServiceOnPort(p int) string {
	kind, reason := portFailureKindAndReason(p)
	setServiceFailed(kind, reason)
	logError("app", "后台服务端口不可用：%s", reason)
	return reason
}

// promptPortChange 端口不可用时询问用户处置，返回用户选择：
//
//	"switch" 已改用推荐端口（port/webURL 与 config.json 同步更新，调用方可直接继续启动）；
//	"retry"  用户已自行排除占用/保留，要求重新探测；
//	"logs"   已为用户打开日志目录，调用方应回到询问再让用户选一次；
//	"keep"   暂不启动（含直接关掉弹窗）。
//
// 文案与按钮标签都带端口号，故在这里拼好整条消息交给平台层弹窗（见 askPortBlocked）。
func promptPortChange(p int, kind string) string {
	suggest := pickFreePort(0)
	if suggest <= 0 {
		suggest = freePortBase
	}
	cause := T("被系统保留（Windows 排除端口段）")
	if kind == failKindPortInUse {
		cause = T("已被其它程序占用")
	}
	msg := TF("后台服务需要监听 127.0.0.1:%d，但该端口%s，服务无法启动。\n\n"+
		"改用端口 %d 可立即恢复；若手机等设备按旧端口访问过，请同步更新地址。", p, cause, suggest)
	switch askPortBlocked(suggest, msg) {
	case "switch":
		applyServerPort(suggest)
		logUI("改用服务端口", strconv.Itoa(suggest))
		return "switch"
	case "retry":
		logUI("重试启动后台服务", fmt.Sprintf("端口 %d", p))
		return "retry"
	case "logs":
		openLogDir()
		return "logs"
	}
	return "keep"
}

// ensureServicePortUsable 启动前的端口预检与用户处置（冷启动、切回 Web UI、设置页重启共用）：
//  1. 可绑定 → true；
//  2. 被其它程序占用 → 先按既有语义停掉占用者再探一次（killServer 连非本进程启动的同端口
//     监听者一并终止，见其实现）；仍不可用进入第 3 步；
//  3. 被系统保留（Windows 排除端口段）/ 仍被占用 → 交互场景弹窗询问（改用推荐端口 / 暂不启动）；
//     静默场景（开机自启）不打扰，直接 false。
//
// 返回 true 表示端口已可用（可能已被换成推荐端口），调用方可继续 spawn。
// 必须先于 startServer：端口不可用时 spawn 必然失败，且失败现场会被误判成插件问题（见 portcheck.go）。
func ensureServicePortUsable(interactive bool) bool {
	kind, err := probePort(port)
	if kind == portOK {
		return true
	}
	if kind == portInUse {
		log.Printf("[service] port %d preflight: in use, stopping the listener before spawn", port)
		logUI("后台服务端口被占用", fmt.Sprintf("端口 %d，正在停止占用进程", port))
		killServer()
		waitPortReleased(port, portReleaseTimeout)
		if k2, _ := probePort(port); k2 == portOK {
			return true
		}
	} else {
		log.Printf("[service] port %d preflight: %s (%v)", port, kind, err)
	}
	if !interactive {
		log.Printf("[service] port %d unavailable, silent launch: skip prompt", port)
		return false
	}
	// 交互处置：最多问两轮——用户选「打开日志」看完现场后还能回来再选一次（否则只能从头再来
	// 一遍切换）。轮数有界，避免任何形式的弹窗死循环。
	for ask := 0; ask < 2; ask++ {
		kind, _ = probePort(port)
		switch promptPortChange(port, failKindFor(kind)) {
		case "switch":
			return true // 新端口已由 pickFreePort 验证可绑定
		case "retry":
			if k, _ := probePort(port); k == portOK {
				log.Printf("[service] port %d available after user retry", port)
				return true
			}
			log.Printf("[service] port %d still unavailable after user retry", port)
		case "logs":
			continue // 已打开日志目录：回到询问
		default:
			return false // 暂不启动 / 关掉弹窗
		}
	}
	return false
}

// 后台服务编排的并发闸门：冷启动的自动引导与用户切回 Web UI 的按需引导可能先后/并发到来，
// 同一时刻只允许一次（否则会拉起两个服务进程互相抢端口）。
var serviceBootstrapRunning atomic.Bool

// startServiceBootstrap 按需启动后台服务编排（幂等，返回是否受理）。
func startServiceBootstrap(interactive bool) bool {
	if !serviceBootstrapRunning.CompareAndSwap(false, true) {
		log.Printf("[service] bootstrap already running, skip duplicate request (interactive=%v)", interactive)
		return false
	}
	// 进入启动流程：状态回到「启动中」（desktop 形态下可能被置为「已停止」；上次失败的标记也要清），
	// 就绪/失败由 bootstrap 自身收尾置位。
	serverReady.Store(false)
	serviceFailed.Store(false)
	go func() {
		defer serviceBootstrapRunning.Store(false)
		bootstrapService(interactive)
	}()
	return true
}

// bootstrapService 后台服务编排（原 main 中的启动流程，改为事件驱动进度）：
// 运行环境 → harness 安装/构建 → 启动服务 → 就绪提示。
// interactive = 用户操作触发（切回 Web UI / 漂移确认后启动），进度窗口必须可见。
//
// 幂等：各步骤自带前置检查（运行环境已就绪 / harness 已安装或已构建 / 服务已在响应），
// 因此 desktop 启动方式下跳过、之后用户切回 Web UI 再执行，与冷启动路径等价。
func bootstrapService(interactive bool) {
	// 未部署（无 package.json）时不询问用户指定目录：显式配置的目录失效时回退到
	// 官方默认目录（~ 下 deepseek-harness，与官方 npx/源码部署及 macOS 语义一致），
	// 交由下方自动探测/部署流程静默处理。
	if _, err := os.Stat(filepath.Join(harnessDir, "package.json")); err != nil && harnessDirExplicit {
		log.Printf("configured harness dir %s not found, falling back to default %s", harnessDir, defaultHarnessDir())
		harnessDir = defaultHarnessDir()
		harnessDirExplicit = false
		saveConfig(currentConfig())
	}

	// 未显式配置时：自动探测已存在的 harness 源码 checkout（如各盘符根目录下的 deepseek-harness）
	if !harnessDirExplicit {
		if found := findExistingHarnessDir(); found != "" {
			harnessDir = found
			saveConfig(currentConfig())
			log.Printf("detected existing harness at %s", found)
		}
	}

	splash := splashForFlow(T("正在准备运行环境…"), interactive)

	// 悬空 LKG 标记清理（须在冷启动健康校验之前）：标记在、备份全无（用户手工删过 harness /
	// .dsh 目录）时 LKG 已无法回退，留着只会让本次冷启动走「加长窗口且不做提前通过」白等 60s。
	clearDanglingLkgMarker()

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
		ver := freshHarnessInstallVersion()
		// 注意：这里**不**因首装为预发布而自动打开「预发布通道」——通道保持用户/默认状态
		// （config.json 缺省关闭）。首装只负责把版本落到最新稳定版/rc；之后 npm 出了稳定版，
		// 通道关闭状态下照样能收到该稳定版的更新提示（见 queryHarnessUpdate）。
		splash.Update(fmt.Sprintf(T("正在安装 DeepSeek Harness %s（首次约 2-5 分钟）…"), withV(ver)), 0.35)
		if err := ensureNpmHarnessVersion(ver); err != nil {
			splash.Close()
			showMessageBox("安装 DeepSeek Harness 失败：\n"+err.Error()+"\n\n日志："+unifiedLogPath(), appName)
			return
		}
	}

	// 2.5/2.6) profile 级事务自愈：上次导入恢复中断 → 回退/续跑；插件快照残骸 → 还原到操作前状态
	recoverProfileTransactions(func(text string) { splash.Update(text, 0.87) })
	// 2.7) codeload 凭据自愈：历史的「带参数」tokenHelper 会让 pnpm 10.34.5（托盘自带运行时）
	// 下**所有** pnpm 命令失败（启动即解析全部 tokenHelper），而那条配置只在下过私有仓插件的
	// 机器上存在、没有任何路径会自动触发修复——启动时重写为新格式（或清理旧行）兜住。
	go repairCodeloadTokenHelper()

	// 3) 启动服务
	// 先做端口预检：端口被系统保留（Windows 排除端口段）或被占用时 spawn 必然失败，且失败现场
	// 会被后续收尾误判成插件不兼容（见 portcheck.go 文件头）。交互场景里用户能直接改用推荐
	// 端口，改完继续本次启动；静默场景（开机自启）只置失败状态与原因。
	if !serverResponding(webURL) && !ensureServicePortUsable(interactive) {
		splash.Close()
		failServiceOnPort(port)
		return
	}
	splash.Update(T("正在启动服务…"), 0.9)
	started := false
	startedByUs := false
	var serverExitCh <-chan error
	serverLogBefore := int64(0)
	if serverResponding(webURL) {
		// 服务由先前进程拉起（上次退出保留了服务 / 重启电脑后仍在跑）：本进程没有它的句柄，
		// 但输出仍写在 server.log —— 从文件末尾开始 tail，把后续输出并回统一日志
		// （此前这段输出完全看不到，排障只能靠猜）。
		log.Printf("server already running on %s, skipping spawn", webURL)
		startServerLogTail(true)
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
		deferLkgClear := false // 存在 LKG 时：清除已委托给后台兜底（窗口走完且无错误才清），此处不得再清
		if ready && startedByUs {
			var res bootVerifyResult
			res, deferLkgClear = verifyServerBootOnColdStart(serverLogBefore, serverExitCh)
			switch res {
			case bootSuperseded:
				// 校验期间服务被其它操作主动停止并接管（同步应用 / 插件批处理 / 更新 / 重置 / 导入）：
				// 既不算失败（不报错、不回退），也不提升 LKG——服务生命周期已由该操作自己的启动校验
				// 负责，冷启动这一遍必须让位（2026-09-25 现场：同步应用停服装插件被误报启动失败）。
				log.Printf("boot verify superseded: server stopped by another operation, skip rollback")
				signalShotReady()
				return
			case bootFailed:
				ready = false
				bootError = coldStartBootFailureReason(serverLogBefore)
			}
		}
		if ready {
			if startedByUs && !deferLkgClear {
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
		// 端口不可用（被系统保留 / 被其它程序占用）与插件加载失败必须分开：前者的证据是 node 的
		// listen EACCES/EADDRINUSE，此时禁用插件与 LKG 回退都无意义，还会动用户的插件树
		//（2026-09-28 现场：3080 落在 Windows 排除端口段，三次启动把全部用户插件禁用又恢复）。
		if kind := portFailKindFromLog(serverLogBefore); kind != "" {
			reason := portFailReason(kind, port)
			setServiceFailed(kind, reason)
			logError("app", "启动失败：%s（已跳过插件自愈与版本回退）", reason)
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
// 窗口已开着：只置顶，**不重载内容**——reload 会把正在进行的进度界面（更新下载/安装、
// harness 更新/重置、导入恢复、GitHub 授权等）整块换成设置页，用户就看不到进度与「取消」入口了。
// 只有窗口未打开时才按正常启动加载设置内容（并刷新版本/插件清单）。
// showMainWindow 显示设置窗口（托盘“设置”点击）。
// 窗口已开着：只置顶，**不重载内容**——reload 会把正在进行的进度界面（更新下载/安装、
// harness 更新/重置、导入恢复、GitHub 授权等）整块换成设置页，用户就看不到进度与「取消」入口了。
// 窗口是被关掉后重开的：若还有进度流程在跑，先把进度视图按最近一次状态还原（用户关窗只是想
// 收起来，不是要放弃看进度）；没有进度流程才按正常启动加载设置内容（并刷新版本/插件清单）。
func showMainWindow() {
	if appCtx == nil {
		return
	}
	alreadyOpen := mainWindowVisible()
	// 窗口显示前再确保一次最大化被禁用：窗口可能是隐藏创建的（自启动/服务已就绪），
	// onDomReady 那一次若失败（窗口尚未创建），这里补上（Windows 改样式，其它平台空实现）。
	startDisableWindowMaximize("dsh-systray")
	wruntime.WindowShow(appCtx)
	ensureMainWindowForeground() // 平台实现：把窗口真正置前（需求：所有弹窗/窗口自动前台）
	if alreadyOpen {
		log.Printf("settings window already open: raised only (no content reload)")
		return
	}
	// 进度流程进行中（首次下载运行时/依赖、更新下载安装、harness 更新/重置、导入恢复、
	// GitHub 授权等）：补发最近的进度事件把进度视图还原，不切设置页。
	if replayActiveSplash() {
		log.Printf("settings view suppressed: progress flow active, progress view replayed")
		return
	}
	// 更新进行中但进度不在 splash 视图（自身更新的下载/安装阶段已登记取消句柄）：同样不切设置页。
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
	menuOpen = systray.AddMenuItem(trayOpenTitle(), trayOpenTooltip())
	refreshServiceMenu()
	// 周期刷新，保证每次打开托盘菜单都反映服务实时状态
	go pollServiceMenu()
	// 沿用已在运行的服务时（上次退出保留服务 / 重启电脑后服务仍在跑），日志里已没有令牌行：
	// 校验上次记录的访问链接，有效则复用（帮助页「复制访问链接」恢复可用），失效则清除。
	go adoptPersistedTokenURL()
	systray.AddSeparator()
	mSettings = systray.AddMenuItem(T("设置"), T("打开设置窗口"))
	systray.AddSeparator()
	mQuit = systray.AddMenuItem(T("退出"), T("退出并关闭后台服务器"))

	menuOpen.Click(func() {
		// 按当前启动方式打开：desktop → 官方桌面端；web → 带最新 token 的 Web UI
		// （见 desktop_app.go 的 openDefaultUI，已含桌面端缺失时回退 Web UI）。
		openDefaultUI()
	})
	mSettings.Click(showMainWindow)
	mQuit.Click(func() {
		// 退出前询问是否停止后台 Web 服务：0=停止并退出 1=保留服务 -1=取消退出
		// （desktop 启动方式下不询问——服务不在运行，见 askStopServerForQuit）
		choice := askStopServerForQuit()
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

// pnpmEnvVars 生成 pnpm 配置环境变量：**同一键同时写 npm_config_X 与 pnpm_config_X 两个前缀**。
// 依据（2026-09-24 本机实测，同一台机器、同一份 .npmrc，用 registry 指向不可达地址判定是否生效）：
//   - pnpm 10.34.5（托盘自带便携运行时的版本）只认 npm_config_*；
//   - pnpm 11.7.0（PATH 上的系统 pnpm）只认 pnpm_config_*——它的 config reader 前缀就是
//     "pnpm_config_"（@pnpm/config/reader/lib/env.js），npm_config_* 仅对少数键生效。
//
// 键名用下划线（child_concurrency → childConcurrency），连字符写法两边都不认（实测）。
// 两个前缀都写后，两种 pnpm 版本都能拿到 registry / fetch-retries / prefer-offline 等配置；
// 旧实现只写 npm_config_*，在 pnpm ≥ 11 上这些配置全部静默失效。
func pnpmEnvVars(kv ...string) []string {
	out := make([]string, 0, len(kv)*2)
	for _, e := range kv {
		out = append(out, "npm_config_"+e, "pnpm_config_"+e)
	}
	return out
}

// pnpmTunedEnv 慢机器调优：限制并发、克隆式安装（参考 dsh-desktop）；网络故障快速失败
// ——单请求 15s 超时、不重试（fetch_retries=0）：坏网络下 registry 拉取失败一次即放弃，
// 让上层立即进入收尾（自愈/自动禁用），避免每轮 install 卡 2-4 分钟（new_device.log 实证
// 「Will retry in 10 seconds…2 retries left」的多档重试循环）。registry 取首选源
// （installRegistry：npmmirror 优先、不可达回退官方，探测一次并缓存）。
func pnpmTunedEnv() []string {
	env := []string{"PNPM_MAX_WORKERS=1"}
	return append(env, pnpmEnvVars(
		"child_concurrency=1",
		"package_import_method=clone-or-copy",
		"side_effects_cache=false",
		"fetch_retries=0",
		"fetch_timeout=15000",
		// 离线优先：store 已有的包不再回源校验元数据（harness 更新后逐 profile 的对齐
		// 绝大多数依赖已命中 store，此前每个 profile 都要把整棵树的元数据重取一遍）。
		// 缺包/缺元数据时仍会正常下载，不影响首次解析。
		"prefer_offline=true",
		"registry="+installRegistry(),
	)...)
}

// pnpmTunedEnvWithRegistry 同 pnpmTunedEnv，但把 registry 换成 reg（空则保持首选 registry）。
func pnpmTunedEnvWithRegistry(reg string) []string {
	if reg == "" {
		return pnpmTunedEnv()
	}
	return replacePnpmEnvValue(pnpmTunedEnv(), "registry", reg)
}

// replacePnpmEnvValue 把 env 里 key 的值（npm_config_/pnpm_config_ 双前缀）替换为 val：
// 同名键重复出现时谁生效取决于子进程实现，必须替换而非追加。
func replacePnpmEnvValue(env []string, key, val string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "npm_config_"+key+"="), strings.HasPrefix(kv, "pnpm_config_"+key+"="):
			out = append(out, kv[:strings.IndexByte(kv, '=')+1]+val)
		default:
			out = append(out, kv)
		}
	}
	return out
}

// pnpmVersionQueryEnv 版本查询（pnpm view）专用环境：在 pnpmTunedEnv 基础上**关闭离线优先**。
//
// 依据（2026-10-09 本机现场定位）：pnpm 的 view 实际 fork 自带 npm 执行，而 npm 的
// prefer-offline 语义是「命中缓存即跳过新鲜度校验」——缓存一旦写入就永不复验。pnpmTunedEnv 为
// 安装提速全量注入 prefer_offline=true，于是 @deepseek-ai/dsh 的包元数据被钉死在旧快照上：
// 「重置服务」候选版本列表与「检查 Harness 更新」同时被截断（实测官方 registry 快照停在
// 22 个版本、最新 0.1.6-alpha.2 且无任何稳定版，界面据此把 0.2.0-rc.2 判为「不在 npm 已发布
// 列表」；同一条命令关闭该开关后返回 31 个版本、最新 0.2.1-alpha.2）。
// 版本查询必须看到真实已发布版本，故此处改为回源校验（命中 304 时开销极小）；
// 安装/依赖解析路径继续沿用离线优先以保留提速。
func pnpmVersionQueryEnv() []string {
	return replacePnpmEnvValue(pnpmTunedEnv(), "prefer_offline", "false")
}

// harnessRegistryOverride 本次 harness 安装应使用的 registry（空 = 首选 installRegistry()）。
// 由 harnessRegistryForVersion 按目标版本在镜像上的可见性决定（见 runHarnessUpdate 预检），
// 避免镜像同步滞后把刚发布的版本挡死（ERR_PNPM_NO_MATCHING_VERSION）。
var harnessRegistryOverride string

// pnpmHarnessEnv harness 目录 pnpm 命令的环境：只覆盖 registry 与离线优先，不带 profile 侧的
// 重度调优档（PNPM_MAX_WORKERS=1 / child_concurrency=1 等是为 Windows 文件占用与杀软干扰调的，
// harness 安装不需要；clone-or-copy 也会让大目录安装更慢）。
func pnpmHarnessEnv() []string {
	reg := harnessRegistryOverride
	if reg == "" {
		reg = installRegistry()
	}
	return pnpmEnvVars("registry="+reg, "prefer_offline=true")
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

// fallbackHarnessVersion 首次部署的兜底版本：仅在 npm 版本查询失败（离线/registry 异常）
// 或已发布版本里找不到 rc/稳定版时使用。正常路径由 freshHarnessInstallVersion 取现网最新候选版。
const fallbackHarnessVersion = "0.1.1-rc.2"

// freshHarnessInstallVersion 全新机器首次部署的版本：查询 npm 已发布版本，在「稳定版 ∪ rc 候选版」
// 里取最新（如 0.1.7-rc.2 > 0.1.5-rc.3 > 0.1.1-rc.2），新机器直接落到当前最新候选版。
// 修复：此前这里写死 ensureNpmHarness("0.1.1-rc.2")（2026-08-23 当时的 npm 最新版），
// 上游连发 0.1.2→0.1.7 后新机器仍装旧版，且因「预发布通道」默认关闭、npm 上从无稳定版
// 而永远收不到更新提示（见 queryHarnessUpdate/fetchNpmResetTarget）。
// 查询失败或无可选版本时回退 fallbackHarnessVersion，保证首次部署不因查询失败而中止。
func freshHarnessInstallVersion() string {
	// npmVersionsOn 在 harnessDir 下执行 pnpm view：全新机器该目录要到安装时才创建
	// （此前由 ensureNpmHarnessVersionPinned 建），先建目录，否则子进程 chdir 直接失败。
	if err := os.MkdirAll(harnessDir, 0o755); err != nil {
		log.Printf("first-run: create harness dir %s failed: %v (fallback %s)", harnessDir, err, fallbackHarnessVersion)
		return fallbackHarnessVersion
	}
	versions, err := npmHarnessPublishedVersions()
	if err != nil {
		log.Printf("first-run: query npm versions failed: %v (fallback %s)", err, fallbackHarnessVersion)
		return fallbackHarnessVersion
	}
	if best := pickFreshHarnessVersion(versions); best != "" {
		log.Printf("first-run: install harness %s (npm 已发布 %d 个版本，最高 rc/稳定版=%s)", best, len(versions), best)
		return best
	}
	log.Printf("first-run: npm 无 rc/稳定版可用，回退 %s", fallbackHarnessVersion)
	return fallbackHarnessVersion
}

// ensureNpmHarnessVersion 在 harnessDir 安装 npm 预构建产物 @deepseek-ai/dsh@ver
// （脚手架与白名单复刻 ensureNpmHarness 的历史语义；调用方：全新机器首次部署
// freshHarnessInstallVersion，以及「重置=清空目录后全新安装所选版本」）。
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
	// registry 按目标版本的镜像可见性选定：镜像有该版本用镜像（快），刚发布、镜像尚未同步
	// 的版本用官方（正确性优先，见 harnessRegistryForVersion）。
	reg, _ := harnessRegistryForVersion(ver)
	cmd.Env = append(os.Environ(), pnpmTunedEnvWithRegistry(reg)...)
	hideCmdWindow(cmd)
	var buf bytes.Buffer
	w := newModuleLogWriter("install")
	cmd.Stdout = io.MultiWriter(w, &buf)
	cmd.Stderr = io.MultiWriter(w, &buf)
	err := cmd.Run()
	w.Flush()
	return buf.String(), err
}

// 就绪探测节奏（2026-09-30 重启提速）：固定 1s 一跳在启动窗口内是纯白等——服务常在
// 1.5-2.5s 就绪，却要等到下一个整秒才被看见，每次重启白付 0.3-1.0s。改为启动窗口内
// 120ms 一跳（覆盖绝大多数启动），窗口过后退避到 500ms，长启动（首次装依赖等）不再空转。
const (
	readyProbeStepFast = 120 * time.Millisecond
	readyProbeStepSlow = 500 * time.Millisecond
	readyProbeFastSpan = 5 * time.Second
)

// waitForServerReady 等待服务就绪：ready=true 表示已响应；
// ready=false 时 why 为 "exited"（服务进程已退出，快速失败）或 "timeout"（超时但进程仍在运行）。
func waitForServerReady(url string, serverExited <-chan error, timeout time.Duration) (bool, string) {
	start := time.Now()
	deadline := start.Add(timeout)
	client := newHTTPClient(3 * time.Second)
	var lastErr string
	for {
		if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				markServerResponsive() // 已确认在响应：此刻才解除「停服意图」（见 markServerResponsive）
				return true, ""
			}
			lastErr = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = err.Error()
		}
		if time.Now().After(deadline) {
			// 失败原因落日志：此前被调用方丢弃，事后只剩「不兼容」这类与事实无关的文案
			//（2026-10-08 现场：就绪探测失败的原因无从判断，只能靠拼时间线推断）。
			log.Printf("[service] not ready within %s: %s (last probe: %s)", timeout, url, lastErr)
			return false, "timeout"
		}
		step := readyProbeStepSlow
		if time.Since(start) < readyProbeFastSpan {
			step = readyProbeStepFast
		}
		timer := time.NewTimer(step)
		select {
		case <-serverExited:
			timer.Stop()
			log.Printf("[service] not ready: process exited before responding (%s, last probe: %s)", url, lastErr)
			return false, "exited"
		case <-timer.C:
		}
	}
}

// serverResponding 快速探测服务是否已在运行（端口是否已被占用）。
func serverResponding(url string) bool {
	client := newHTTPClient(2 * time.Second)
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

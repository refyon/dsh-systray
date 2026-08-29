package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getlantern/systray"
)

const (
	appName     = "DeepSeek Harness"
	defaultPort = 3080
)

var (
	logDir             string
	webURL             string
	harnessDir         string
	port               int
	startupTimeout     time.Duration
	quitting           atomic.Bool
	keepServerRunning  atomic.Bool
	harnessDirExplicit bool
	// singleInstanceRelease 单实例互斥体释放函数（更新重启前调用，避免新进程被误判重复运行）
	singleInstanceRelease func()
)

type appConfig struct {
	Port              int    `json:"port"`
	HarnessDir        string `json:"harnessDir"`
	StartupTimeoutSec int    `json:"startupTimeoutSec"`
	UpdateMirror      string `json:"updateMirror"`
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

func main() {
	cfg := loadConfig()
	updateMirrorOverride = cfg.UpdateMirror
	port = cfg.Port
	webURL = fmt.Sprintf("http://127.0.0.1:%d/", port)
	harnessDir = cfg.HarnessDir
	startupTimeout = time.Duration(cfg.StartupTimeoutSec) * time.Second

	cfgDir, err := os.UserConfigDir()
	if err != nil {
		cfgDir = os.TempDir()
	}
	logDir = filepath.Join(cfgDir, "dsh-systray", "logs")
	if err := os.MkdirAll(logDir, 0o755); err == nil {
		if f, ferr := os.OpenFile(filepath.Join(logDir, "app.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
			defer f.Close()
			log.SetOutput(f)
		}
	}
	log.SetFlags(log.LstdFlags)

	release, acquired := acquireSingleInstance()
	singleInstanceRelease = release
	if !acquired {
		// 已在运行：弹窗提示后退出，不产生第二个托盘图标。
		showMessageBox("DeepSeek Harness 已在运行中，请使用系统托盘图标操作。", appName)
		return
	}
	defer release()

	// 清理上次更新遗留的旧程序文件；启动 30 秒后静默检查新版本并提示更新
	cleanupStaleUpdateFiles()
	startAutoUpdateCheck()

	// 显式配置的 harness 目录不可用时让用户重新选择；默认目录则静默自动部署
	if _, err := os.Stat(filepath.Join(harnessDir, "package.json")); err != nil && harnessDirExplicit {
		log.Printf("harness source not found at %s, prompting user to choose", harnessDir)
		if chosen := pickHarnessDir("未找到 DeepSeek Harness 源码目录。\n请选择已有的 harness 源码目录，或选择即将自动安装到的文件夹：", harnessDir); chosen != "" {
			harnessDir = chosen
			cfg.HarnessDir = chosen
			saveConfig(cfg)
			log.Printf("harness dir set to %s", harnessDir)
		}
	}

	// 未显式配置时：自动探测已存在的 harness 源码 checkout（如各盘符根目录下的 deepseek-harness），避免重复安装
	if !harnessDirExplicit {
		if found := findExistingHarnessDir(); found != "" {
			harnessDir = found
			cfg.HarnessDir = found
			saveConfig(cfg)
			log.Printf("detected existing harness at %s", found)
		}
	}

	splash := startSplash("正在准备运行环境…")

	// 1) 运行环境：优先便携 Node.js / pnpm（无管理员权限、无窗口、后台静默）
	if !runtimeOK() {
		splash.Update("正在下载 Node.js / pnpm 运行时（首次约 1-3 分钟）…", 0.08)
		if err := ensureRuntime(splash); err != nil {
			splash.Close()
			showMessageBox("下载运行环境失败：\n"+err.Error()+"\n\n请检查网络后重试；日志："+filepath.Join(logDir, "app.log"), appName)
			return
		}
		refreshEnvPath()
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
	var serverExitCh <-chan error
	if serverResponding(webURL) {
		log.Printf("server already running on %s, skipping spawn", webURL)
		started = true
	} else {
		started, serverExitCh = startServer()
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
		if ready {
			notifyReady()
			return
		}
		if why == "exited" {
			showMessageBox("DeepSeek Harness 服务启动失败（进程已退出），请查看日志：\n"+filepath.Join(logDir, "server.log"), appName)
			return
		}
		// 超时但服务进程仍在运行：慢机器/首次启动常见，继续后台等待，就绪后再次提示
		showMessageBox("DeepSeek Harness 服务启动较慢（首次启动或机器性能较低时属正常现象），已继续在后台启动，就绪后会再次提示。\n日志："+filepath.Join(logDir, "server.log"), appName)
		if ready2, _ := waitForServerReady(webURL, serverExitCh, 15*time.Minute); ready2 && !quitting.Load() {
			notifyReady()
		} else if !quitting.Load() {
			showMessageBox("DeepSeek Harness 服务最终未能就绪，请查看日志：\n"+filepath.Join(logDir, "server.log"), appName)
		}
	}()

	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(trayIconData())
	setTemplateIcon()
	startIconThemeWatch()
	systray.SetTitle(appName)
	systray.SetTooltip(appName)

	mOpen := systray.AddMenuItem("打开 Web UI", "打开网页端界面")
	systray.AddSeparator()
	mSettings := systray.AddMenuItem("设置", "打开设置窗口")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "退出并关闭后台服务器")

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				openBrowser(webURL)
			case <-mSettings.ClickedCh:
				openSettingsWindow()
			case <-mQuit.ClickedCh:
				// 退出前询问是否停止后台 Web 服务：0=停止并退出 1=保留服务 -1=取消退出
				choice := askStopServer()
				if choice < 0 {
					continue // 取消退出：菜单事件循环继续，托盘保持可用
				}
				keepServerRunning.Store(choice == 1)
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {
	quitting.Store(true)
	cancelActiveUpdate() // 终止进行中的更新下载/安装
	if !keepServerRunning.Load() {
		killServer()
	} else {
		log.Printf("quit with backend server kept running")
	}
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
	pkg := "{\n  \"name\": \"dsh-systray-harness\",\n  \"private\": true,\n  \"pnpm\": {\n    \"onlyBuiltDependencies\": [\n      \"@deepseek-ai/dsh-subprocess-local\",\n      \"@google/genai\",\n      \"koffi\",\n      \"node-pty\",\n      \"protobufjs\"\n    ]\n  }\n}\n"
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

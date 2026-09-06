//go:build darwin

package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dsh-systray/internal/systray"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed scripts/install-prereqs.sh
var installScript []byte

const launchAgentLabel = "com.deepseek.dsh-systray"

var serverCmd *exec.Cmd

// ensureMainWindowForeground macOS：把应用激活到前台（macOS 无 Windows 式强制置顶，
// Wails 的 WindowShow 已触发应用激活；此处兜底按 bundle id 再激活一次）。
// 开发构建（裸二进制、非 .app 内运行）定位不到 bundle 时静默忽略。
func ensureMainWindowForeground() {
	_ = exec.Command("osascript", "-e", `tell application id "`+launchAgentLabel+`" to activate`).Run()
}

// candidateHarnessDirs 探测候选：用户主目录 + 默认目录。
func candidateHarnessDirs() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, "deepseek-harness"))
	}
	out = append(out, defaultHarnessDir())
	return out
}

func defaultHarnessDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "deepseek-harness")
}

// pickHarnessDir 弹出目录选择对话框（macOS choose folder），返回用户选择的目录；取消返回 ""。
func pickHarnessDir(title, initial string) string {
	script := fmt.Sprintf(`POSIX path of (choose folder with prompt "%s")`, escapeAppleScript(title))
	out, err := runAppleScript(script)
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	return strings.TrimSuffix(p, "/")
}

// ---- 运行环境（macOS）：优先便携 Node/pnpm，缺省时才回退系统 PATH ----
// 与 Windows 便携运行时同一策略：应用自下载 node + 安装 pnpm 到用户目录，
// 不依赖 Homebrew/系统安装（原 brew 方案在无 brew 或网络受限时启动即失败）。

const (
	macNodeVersion = "v24.9.0" // 与 Windows 便携运行时保持一致
	macPnpmVersion = "10.34.5"
)

// runtimeDir 便携运行时根目录（macOS: ~/Library/Application Support/dsh-systray/runtime）。
func runtimeDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "dsh-systray", "runtime")
}

func nodeDir() string { return filepath.Join(runtimeDir(), "node") }
func nodeBin() string { return filepath.Join(nodeDir(), "bin", "node") }

// pnpmWrapper 便携 pnpm 包装脚本（绝对路径调用便携 node 执行 pnpm.cjs，规避 PATH/env 依赖）。
func pnpmWrapper() string { return filepath.Join(runtimeDir(), "pnpm") }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func nodeAvailable() bool {
	if fileExists(nodeBin()) {
		return true
	}
	_, err := exec.LookPath("node")
	return err == nil
}

func pnpmAvailable() bool {
	if fileExists(pnpmWrapper()) {
		return true
	}
	_, err := exec.LookPath("pnpm")
	return err == nil
}

// nodeCmd / pnpmCmd 优先返回便携运行时路径，其次系统 PATH。
func nodeCmd() string {
	if fileExists(nodeBin()) {
		return nodeBin()
	}
	return "node"
}

func pnpmCmd() string {
	if fileExists(pnpmWrapper()) {
		return pnpmWrapper()
	}
	return "pnpm"
}

func runtimeOK() bool { return nodeAvailable() && pnpmAvailable() }

// macNodeURLs 便携 Node 下载地址（多镜像，见 mirrors.go；DSH_NODE_MIRROR 可固定单一来源）。
func macNodeURLs() []string {
	file := fmt.Sprintf("%s/node-%s-darwin-%s.tar.gz", macNodeVersion, macNodeVersion, macArch())
	out := make([]string, 0, len(nodeDistBases()))
	for _, b := range nodeDistBases() {
		out = append(out, b+"/"+file)
	}
	return out
}

// ensureRuntime 下载便携 Node.js + 安装 pnpm（无 brew / 无需管理员，多镜像自动切换）。
// 失败返回错误，错误信息含失败来源与可用环境变量（DSH_NODE_MIRROR / DSH_NPM_REGISTRY）。
func ensureRuntime(splash *SplashState) error {
	refreshEnvPath()
	if runtimeOK() {
		return nil
	}
	if err := os.MkdirAll(runtimeDir(), 0o755); err != nil {
		return err
	}
	if !nodeAvailable() {
		tgz := filepath.Join(runtimeDir(), "node.tgz")
		urls := macNodeURLs()
		err := mirrorDownload(urls, tgz, "Node.js", 4*time.Minute, func(host string, pct float64) {
			splash.Update(fmt.Sprintf("正在下载 Node.js 运行时（来源：%s，%.0f%%）…", host, pct*100), 0.10)
		})
		if err != nil {
			return err
		}
		splash.Update("正在解压 Node.js 运行时…", 0.24)
		if err := os.MkdirAll(runtimeDir(), 0o755); err != nil {
			return err
		}
		if err := runTarXzf(tgz, runtimeDir()); err != nil {
			return fmt.Errorf("解压 Node.js 失败：%w", err)
		}
		_ = os.Remove(tgz)
		// 归档内为 node-vX.Y.Z-darwin-<arch>/ 单层目录 → 整理为 runtime/node
		src := filepath.Join(runtimeDir(), "node-"+macNodeVersion+"-darwin-"+macArch())
		if err := os.Rename(src, nodeDir()); err != nil && !fileExists(nodeBin()) {
			return fmt.Errorf("整理 Node.js 目录失败：%w", err)
		}
	}
	if !pnpmAvailable() {
		if err := installPnpmMirrors(splash); err != nil {
			return err
		}
		if err := writePnpmWrapper(); err != nil {
			return fmt.Errorf("生成 pnpm 启动包装失败：%w", err)
		}
	}
	refreshEnvPath()
	return nil
}

// installPnpmMirrors 用便携 npm 安装 pnpm：registry 多镜像自动切换（npmRegistryBases）；
// 收紧 npm 自身 fetch 超时/重试，让慢源快速失败并切换（npm 默认 5 分钟×多次重试会“假超时”）。
func installPnpmMirrors(splash *SplashState) error {
	npm := filepath.Join(nodeDir(), "bin", "npm")
	registries := npmRegistryBases()
	var errs []string
	for i, reg := range registries {
		splash.Update(fmt.Sprintf("正在安装 pnpm 包管理器（registry %d/%d：%s）…",
			i+1, len(registries), urlHost(reg)), 0.26)
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		cmd := exec.CommandContext(ctx, npm, "install", "-g", "pnpm@"+macPnpmVersion,
			"--prefix", runtimeDir(), "--loglevel", "error")
		cmd.Env = append(os.Environ(),
			"PATH="+filepath.Join(nodeDir(), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
			"npm_config_registry="+reg,
			"npm_config_fetch_timeout=20000", // 单次 fetch 20s 即失败（而非默认 5 分钟挂起）
			"npm_config_fetch_retries=1",
			"npm_config_fetch_retry_mintimeout=1000",
			"npm_config_fetch_retry_maxtimeout=10000")
		hideCmdWindow(cmd)
		w := newModuleLogWriter("install")
		cmd.Stdout = w
		cmd.Stderr = w
		err := cmd.Run()
		w.Flush()
		cancel()
		if err == nil {
			return nil
		}
		log.Printf("pnpm install via registry %q failed: %v", reg, err)
		errs = append(errs, fmt.Sprintf("%s: %v", reg, err))
	}
	return fmt.Errorf("安装 pnpm 失败（已尝试 %d 个 registry）：%s（日志：%s）",
		len(registries), strings.Join(errs, "；"), unifiedLogPath())
}

func macArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x64"
}

// runTarXzf 用系统 tar 解压（macOS 自带 bsdtar，支持 .tar.gz）。
func runTarXzf(tgz, dest string) error {
	cmd := exec.Command("tar", "-xzf", tgz, "-C", dest)
	hideCmdWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

// writePnpmWrapper npm -g --prefix 生成的 pnpm 是 node 脚本 shim，依赖 PATH 里的 node；
// 覆盖为固定调用便携 node 的 sh 包装，保证任意启动场景（含开机自启、无 PATH 刷新）都可执行。
func writePnpmWrapper() error {
	cjs := filepath.Join(runtimeDir(), "lib", "node_modules", "pnpm", "bin", "pnpm.cjs")
	if !fileExists(cjs) {
		return fmt.Errorf("未找到已安装的 pnpm：%s", cjs)
	}
	script := "#!/bin/sh\nexec %s %s \"$@\"\n"
	content := fmt.Sprintf(script, shellQuote(nodeBin()), shellQuote(cjs))
	p := pnpmWrapper()
	// npm -g 生成的 pnpm 是指向 pnpm.cjs 的符号链接：先删除，避免 WriteFile 沿链接覆写包文件
	_ = os.Remove(p)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		return err
	}
	return os.Chmod(p, 0o755)
}

// shellQuote 用 %q 生成 POSIX 双引号字面量（路径含空格时安全）。
func shellQuote(p string) string {
	return fmt.Sprintf("%q", p)
}

// refreshEnvPath 把便携 node/pnpm 目录加入当前进程 PATH（覆盖子进程；不做用户级持久化，
// node/pnpm 均以绝对路径/包装脚本调用，不依赖 PATH）。
func refreshEnvPath() {
	if !fileExists(nodeBin()) {
		return
	}
	dirs := []string{filepath.Join(nodeDir(), "bin"), runtimeDir()}
	cur := os.Getenv("PATH")
	for _, d := range dirs {
		if !strings.Contains(cur, d) {
			if cur == "" {
				cur = d
			} else {
				cur = d + string(os.PathListSeparator) + cur
			}
		}
	}
	os.Setenv("PATH", cur)
}

// hideCmdWindow macOS 无窗口概念，占位实现。
func hideCmdWindow(cmd *exec.Cmd) {}

func trayIconData() []byte {
	// macOS 菜单栏需要 PNG；从 ICO 中提取最大尺寸的 PNG 条目。
	return extractLargestPNG(iconData)
}

// setTemplateIcon macOS：菜单栏使用模板图标（自动适配深浅色）。
func setTemplateIcon() {
	systray.SetTemplateIcon(iconDataTemplate, extractLargestPNG(iconData))
}

// startIconThemeWatch macOS：菜单栏模板图标由系统自动适配深浅色，无需监听主题。
func startIconThemeWatch() {}

func startServer() (bool, <-chan error) {
	// 防御性检查：目录必须存在，否则 cmd.Dir 指向无效目录会导致 fork 失败
	if !isNpmHarnessReady() {
		if _, err := os.Stat(filepath.Join(harnessDir, "package.json")); err != nil {
			log.Printf("harness not found at %s: %v", harnessDir, err)
			return false, nil
		}
	}

	var cmd *exec.Cmd
	if isNpmHarnessReady() {
		// npm 预构建产物：直接用 node 启动 @deepseek-ai/dsh 入口
		bin := filepath.Join(harnessDir, "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js")
		cmd = exec.Command(nodeCmd(), bin, "web", "--no-open", "--host", "127.0.0.1", "--port", strconv.Itoa(port))
	} else {
		// 源码 checkout：pnpm dsh web
		cmd = exec.Command("sh", "-c", fmt.Sprintf("%s dsh web --port %d --no-open", pnpmCmd(), port))
	}
	cmd.Dir = harnessDir
	// 输出经统一日志句柄落盘（勿提前 Close，见 platform_windows startServer 注释）
	w := newModuleLogWriter("server")
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		log.Printf("failed to start server: %v", err)
		return false, nil
	}
	serverCmd = cmd
	serverStartedPort = port // 记录实际启动端口（端口修改提示与状态展示依据）
	trackChildProcess(cmd.Process)
	exitCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		w.Flush() // 进程退出后补出崩溃末行（无换行残留）
		exitCh <- err
	}()
	log.Printf("server started, pid=%d", cmd.Process.Pid)
	return true, exitCh
}

func killServer() {
	if serverCmd != nil && serverCmd.Process != nil {
		_ = serverCmd.Process.Kill()
		serverCmd = nil
	}
	// 终止监听本端口的 dsh web 进程（即使不是本应用启动的）
	if pid, err := findListenerPID(port); err == nil {
		log.Printf("killing listener pid=%d on port %d", pid, port)
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

func findListenerPID(port int) (int, error) {
	// lsof -ti :PORT 输出监听该端口的 PID（可能多行）
	out, err := exec.Command("lsof", "-ti", fmt.Sprintf(":%d", port)).Output()
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("no listener on port %d", port)
	}
	return pid, nil
}

// killProcessTreePID macOS 尽力终止指定 PID 及其子进程（pkill -P 终止直接子进程 + SIGTERM
// 自身；与 Windows 的 taskkill /T 对应，深度进程树按尽力而为）。
func killProcessTreePID(pid int) {
	if pid <= 0 {
		return
	}
	log.Printf("killing process tree pid=%d", pid)
	_ = exec.Command("pkill", "-TERM", "-P", strconv.Itoa(pid)).Run()
	_ = syscall.Kill(pid, syscall.SIGTERM)
}

func openBrowser(url string) {
	cmd := exec.Command("open", url)
	if err := cmd.Start(); err != nil {
		log.Printf("open browser failed: %v", err)
		return
	}
	trackChildProcess(cmd.Process)
}

// downloadsDir macOS：默认 ~/Downloads（无系统级重定向场景）。
func downloadsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Downloads")
}

// openDir macOS：用 Finder 打开目录（open <dir>）。
func openDir(dir string) {
	cmd := exec.Command("open", dir)
	if err := cmd.Start(); err != nil {
		log.Printf("open dir failed: %v", err)
		return
	}
	trackChildProcess(cmd.Process)
}

// revealFile macOS：Finder 中打开文件所在目录并选中该文件（open -R <path>）。
func revealFile(path string) {
	cmd := exec.Command("open", "-R", path)
	if err := cmd.Start(); err != nil {
		log.Printf("reveal failed: %v", err)
		openDir(filepath.Dir(path))
		return
	}
	trackChildProcess(cmd.Process)
}

func launchAgentPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

// autostartLaunchTarget 自启动目标：当前可执行文件所在 .app 包目录（发布形态）。
// 裸二进制（开发构建，不在 .app 内）没有 bundle/LSUIElement 上下文，经 launchd 直接
// 启动会异常（正是旧版"开机自启失效"根因），此时返回 "" 由调用方给出明确报错。
func autostartLaunchTarget() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return appBundleDir(exe)
}

// launchAgentLoaded 校验 launchd 是否已注册本登录项：
// plist 文件存在 ≠ 已注册（launchctl load 失败/被系统移除时会"假启用"）。
// macOS 10.10+ 的 gui/<uid> 域可用 launchctl print 校验。
func launchAgentLoaded() bool {
	err := exec.Command("launchctl", "print",
		fmt.Sprintf("gui/%d/%s", os.Getuid(), launchAgentLabel)).Run()
	return err == nil
}

func isAutostartEnabled() bool {
	if _, err := os.Stat(launchAgentPlistPath()); err != nil {
		return false
	}
	return launchAgentLoaded()
}

// autostartPlistContent 生成 LaunchAgent plist：
//   - ProgramArguments 用 /usr/bin/open 打开 .app（open 经 LaunchServices 正常启动应用，
//     LSUIElement/bundle 上下文完整），后续 --args --autostart 传给应用保持静默逻辑；
//   - LimitLoadToSessionType=Aqua：仅图形登录会话加载（避免 SSH 等非 GUI 域误启动）；
//   - RunAtLoad：登录即启动。
func autostartPlistContent(bundle string) string {
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		return r.Replace(s)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/bin/open</string>
		<string>%s</string>
		<string>--args</string>
		<string>--autostart</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>LimitLoadToSessionType</key><string>Aqua</string>
	<key>ProcessType</key><string>Interactive</string>
</dict>
</plist>
`, launchAgentLabel, esc(bundle))
}

// writeLaunchAgentPlist 幂等写入登录项 plist（内容未变时跳过写盘）。
// 供 main() 启动自愈调用：老用户残留的"裸二进制直接 exec"旧 plist 在此升级为
// open .app 形态——只写文件即可，下次登录 launchd 从磁盘加载即生效；
// 不能在自愈里 bootout（若当前进程正由该 launchd job 启动会被自杀）。
func writeLaunchAgentPlist() error {
	bundle := autostartLaunchTarget()
	if bundle == "" {
		return fmt.Errorf("当前为非 .app 开发构建，无法注册开机自启动（请使用发布的 dsh-systray.app）")
	}
	content := autostartPlistContent(bundle)
	if cur, err := os.ReadFile(launchAgentPlistPath()); err == nil && string(cur) == content {
		return nil
	}
	if err := os.WriteFile(launchAgentPlistPath(), []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入登录启动项失败：%w", err)
	}
	return nil
}

func enableAutostart() error {
	if err := writeLaunchAgentPlist(); err != nil {
		return err
	}
	// 立即注册生效：launchctl load 已废弃（且旧 job 内容在内存中不随文件刷新），
	// 现代 API 为 bootout（幂等忽略"未加载"）+ bootstrap 到 gui/<uid> 域。
	_ = exec.Command("launchctl", "bootout",
		fmt.Sprintf("gui/%d/%s", os.Getuid(), launchAgentLabel)).Run()
	if out, err := exec.Command("launchctl", "bootstrap",
		fmt.Sprintf("gui/%d", os.Getuid()), launchAgentPlistPath()).CombinedOutput(); err != nil {
		return fmt.Errorf("注册登录启动项失败：%v：%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func disableAutostart() error {
	_ = exec.Command("launchctl", "bootout",
		fmt.Sprintf("gui/%d/%s", os.Getuid(), launchAgentLabel)).Run()
	if err := os.Remove(launchAgentPlistPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除登录启动项失败：%w", err)
	}
	return nil
}

// startSignalHandling macOS：launchd 注销/系统关机路径会对进程发 SIGTERM（SIGINT 为兜底）。
// 捕获后保留后台服务并优雅退出：跳过交互询问、走 Wails onShutdown 清理路径；
// 应用退出早期（appCtx 未就绪）直接退出。Windows 无此需求（WM_QUERYENDSESSION 自理）。
func startSignalHandling() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-ch
		log.Printf("received signal %v: graceful quit, server kept running", s)
		keepServerRunning.Store(true)
		quitRequested.Store(true)
		if appCtx != nil {
			wruntime.Quit(appCtx)
		} else {
			os.Exit(0)
		}
	}()
}

func runInstaller() {
	tmp := filepath.Join(os.TempDir(), "dsh-systray-install-prereqs.sh")
	script := strings.ReplaceAll(string(installScript), "{{HARNESS_DIR}}", harnessDir)
	if err := os.WriteFile(tmp, []byte(script), 0o755); err != nil {
		log.Printf("write installer failed: %v", err)
		showMessageBox("检测到缺少运行依赖，但无法写入安装脚本。", appName)
		return
	}
	cmd := exec.Command("sh", tmp)
	if err := cmd.Run(); err != nil {
		log.Printf("installer failed: %v", err)
	}

	// 结果由 main() 重新检查 prereqsOK 后统一处理
	if prereqsOK() {
		log.Printf("prerequisites now satisfied")
	} else {
		log.Printf("prerequisites still missing after installer")
	}
}

func acquireSingleInstance() (func(), bool) {
	lockPath := filepath.Join(os.TempDir(), "dsh-systray.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, true
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return func() {}, false
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true
}

func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

func runAppleScript(script string) (string, error) {
	tmp := filepath.Join(os.TempDir(), "dsh-systray-osascript.scpt")
	if err := os.WriteFile(tmp, []byte(script), 0o644); err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	out, err := exec.Command("osascript", tmp).Output()
	return string(out), err
}

func showMessageBox(text, caption string) {
	script := fmt.Sprintf(`display dialog "%s" with title "%s" buttons {"确定"} default button "确定"`, escapeAppleScript(text), escapeAppleScript(caption))
	_, _ = runAppleScript(script)
}

// askStopServer 退出前询问是否停止后台 Web 服务：0=停止并退出，1=保留服务，-1=取消退出。
func askStopServer() int {
	script := fmt.Sprintf(`display dialog "%s" with title "%s" buttons {"确定", "保留后台服务"} default button "确定"`,
		escapeAppleScript("是否停止后台 Web 服务？确定将停止服务并退出；保留后台服务仅关闭托盘，服务继续运行。"), appName)
	out, err := runAppleScript(script)
	if err != nil {
		return -1
	}
	if strings.Contains(out, "确定") {
		return 0
	}
	if strings.Contains(out, "保留后台服务") {
		return 1
	}
	return -1
}

func showReadyPrompt(url string) {
	script := fmt.Sprintf(`display dialog "%s" with title "%s" buttons {"打开", "取消"} default button "打开"`, escapeAppleScript("DeepSeek Harness 服务已就绪。是否立即打开 Web UI？"), appName)
	out, err := runAppleScript(script)
	if err == nil && strings.Contains(out, "打开") {
		openBrowser(url)
	}
}

// askUpdateDialog 提示用户发现新版本：true=立即更新。
func askUpdateDialog(newVer string) bool {
	msg := fmt.Sprintf("发现新版本 %s（当前版本 %s）。\n是否立即下载并更新？", withV(newVer), withV(appVersion))
	script := fmt.Sprintf(`display dialog "%s" with title "%s" buttons {"稍后", "立即更新"} default button "立即更新"`,
		escapeAppleScript(msg), appName)
	out, err := runAppleScript(script)
	if err != nil {
		return false
	}
	return strings.Contains(out, "立即更新")
}

// askUpdateHarness 提示用户 DeepSeek Harness 有新版本：true=先更新 Harness。
func askUpdateHarness(newVer, curVer string) bool {
	msg := fmt.Sprintf("DeepSeek Harness 有新版本 %s（当前 %s）。\n是否先更新 Harness？", withV(newVer), withV(curVer))
	script := fmt.Sprintf(`display dialog "%s" with title "%s" buttons {"稍后", "更新 Harness"} default button "更新 Harness"`,
		escapeAppleScript(msg), appName)
	out, err := runAppleScript(script)
	if err != nil {
		return false
	}
	return strings.Contains(out, "更新 Harness")
}

// askRestartServiceMac 重启后台服务前确认（macOS）：true=确认重新启动。
func askRestartServiceMac() bool {
	msg := escapeAppleScript("是否重启后台 Web 服务？\n重启期间 Web UI 会短暂不可用。")
	script := fmt.Sprintf(`display dialog "%s" with title "%s" buttons {"取消", "重新启动"} default button "重新启动"`, msg, appName)
	out, err := runAppleScript(script)
	if err != nil {
		return false
	}
	return strings.Contains(out, "重新启动")
}

// replaceAndRelaunch 替换当前 .app 并重启（自定义更新方案的辅助工具思路）：
// 写入一个等待 2 秒的 shell 脚本，由它在本进程退出后 rm 旧 .app、mv 新 .app、open 重新打开。
func replaceAndRelaunch(newApp string) error {
	cur, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法定位当前程序路径：%w", err)
	}
	bundle := appBundleDir(cur)
	if bundle == "" {
		return fmt.Errorf("无法定位当前 .app 包路径")
	}
	if err := checkWritable(filepath.Dir(bundle)); err != nil {
		return fmt.Errorf("程序所在目录无写权限（%s）：%w", filepath.Dir(bundle), err)
	}
	script := "#!/bin/bash\nsleep 2\nrm -rf \"$2\"\nmv \"$1\" \"$2\"\nopen \"$2\"\n"
	scriptPath := filepath.Join(os.TempDir(), "dsh-systray-updater.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return err
	}
	cmd := exec.Command("sh", scriptPath, newApp, bundle)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动更新脚本失败：%w", err)
	}
	// 与 Windows 对称：重启前主动释放单实例锁，避免新实例被误判重复运行。
	if singleInstanceRelease != nil {
		singleInstanceRelease()
	}
	log.Printf("update applied via helper, relaunching %s", bundle)
	os.Exit(0)
	return nil
}

// appBundleDir 从可执行文件路径向上查找 .app 包目录；未处于 .app 内时返回 ""。
func appBundleDir(exe string) string {
	d := filepath.Dir(exe)
	for {
		if strings.HasSuffix(strings.ToLower(d), ".app") {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// checkWritable 探测目录可写性（用于更新前预检，避免替换静默失败）。
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".dsh-systray-write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// startUpdateApply macOS 保持进程内更新：下载 → 辅助脚本替换 .app → 自动重启。
func startUpdateApply(rel *latestRelease) {
	if err := downloadAndApplyUpdate(rel); err != nil {
		log.Printf("update failed: %v", err)
		showMessageBox("更新失败：\n"+err.Error()+"\n\n请稍后重试，或前往 GitHub Releases 手动下载。", appName)
	}
}

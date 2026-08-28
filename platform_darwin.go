//go:build darwin

package main

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/getlantern/systray"
)

//go:embed scripts/install-prereqs.sh
var installScript []byte

const launchAgentLabel = "com.deepseek.dsh-systray"

var serverCmd *exec.Cmd

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

// ---- 运行环境（macOS 依赖 Homebrew 安装的 node/pnpm） ----

func nodeCmd() string { return "node" }
func pnpmCmd() string { return "pnpm" }

func runtimeOK() bool {
	_, e1 := exec.LookPath("node")
	_, e2 := exec.LookPath("pnpm")
	return e1 == nil && e2 == nil
}

// ensureRuntime macOS：调用 brew 安装脚本（install-prereqs.sh）。
func ensureRuntime(splash *SplashState) error {
	runInstaller()
	if runtimeOK() {
		return nil
	}
	return fmt.Errorf("运行依赖（Node.js / pnpm）仍缺失，请手动安装后重试")
}

func refreshEnvPath() {}

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

	serverLogPath := filepath.Join(logDir, "server.log")
	f, err := os.OpenFile(serverLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("cannot open server log: %v", err)
	}
	if f != nil {
		defer f.Close()
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
	if f != nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		log.Printf("failed to start server: %v", err)
		return false, nil
	}
	serverCmd = cmd
	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()
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

func openBrowser(url string) {
	if err := exec.Command("open", url).Start(); err != nil {
		log.Printf("open browser failed: %v", err)
	}
}

func launchAgentPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

func isAutostartEnabled() bool {
	_, err := os.Stat(launchAgentPlistPath())
	return err == nil
}

func enableAutostart() {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("cannot resolve exe path: %v", err)
		return
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array><string>%s</string></array>
	<key>RunAtLoad</key><true/>
</dict>
</plist>
`, launchAgentLabel, exe)
	if err := os.WriteFile(launchAgentPlistPath(), []byte(plist), 0o644); err != nil {
		log.Printf("write launch agent failed: %v", err)
		return
	}
	_ = exec.Command("launchctl", "load", launchAgentPlistPath()).Run()
}

func disableAutostart() {
	_ = exec.Command("launchctl", "unload", launchAgentPlistPath()).Run()
	_ = os.Remove(launchAgentPlistPath())
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
	msg := fmt.Sprintf("发现新版本 %s（当前版本 %s）。\n是否立即下载并更新？", newVer, appVersion)
	script := fmt.Sprintf(`display dialog "%s" with title "%s" buttons {"稍后", "立即更新"} default button "立即更新"`,
		escapeAppleScript(msg), appName)
	out, err := runAppleScript(script)
	if err != nil {
		return false
	}
	return strings.Contains(out, "立即更新")
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

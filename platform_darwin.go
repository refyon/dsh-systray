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
)

//go:embed scripts/install-prereqs.sh
var installScript []byte

const launchAgentLabel = "com.deepseek.dsh-systray"

var serverCmd *exec.Cmd

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

func showReadyPrompt(url string) {
	script := fmt.Sprintf(`display dialog "%s" with title "%s" buttons {"打开", "取消"} default button "打开"`, escapeAppleScript("DeepSeek Harness 服务已就绪。是否立即打开 Web UI？"), appName)
	out, err := runAppleScript(script)
	if err == nil && strings.Contains(out, "打开") {
		openBrowser(url)
	}
}

//go:build windows

package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

//go:embed scripts/install-prereqs.ps1
var installScript []byte

const (
	registryPath = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	registryName = `DeepSeekHarness`
	mutexName    = `Local\dsh-systray-single-instance`
)

var serverCmd *exec.Cmd

// candidateHarnessDirs 探测候选：所有盘符的 \deepseek-harness + 用户主目录 + 默认目录。
func candidateHarnessDirs() []string {
	var out []string
	for c := 'A'; c <= 'Z'; c++ {
		root := string(c) + ":\\"
		if _, err := os.Stat(root); err != nil {
			continue
		}
		out = append(out, filepath.Join(root, "deepseek-harness"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, "deepseek-harness"))
	}
	out = append(out, defaultHarnessDir())
	return out
}

func defaultHarnessDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = home
		} else {
			base = os.TempDir()
		}
	}
	return filepath.Join(base, "Programs", "dsh-systray-harness")
}

const nodeDownloadURL = "https://nodejs.org/dist/v24.9.0/node-v24.9.0-win-x64.zip"

// ---- 便携运行环境（无管理员权限、无窗口、后台静默） ----

func runtimeDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "Programs", "dsh-systray-runtime")
}

func nodeDir() string { return filepath.Join(runtimeDir(), "node") }
func nodeExe() string { return filepath.Join(nodeDir(), "node.exe") }
func pnpmExe() string { return filepath.Join(runtimeDir(), "pnpm.cmd") }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func nodeAvailable() bool {
	if fileExists(nodeExe()) {
		return true
	}
	_, err := exec.LookPath("node")
	return err == nil
}

func pnpmAvailable() bool {
	if fileExists(pnpmExe()) {
		return true
	}
	_, err := exec.LookPath("pnpm")
	return err == nil
}

// nodeCmd / pnpmCmd 优先返回便携运行时路径，其次系统 PATH。
func nodeCmd() string {
	if fileExists(nodeExe()) {
		return nodeExe()
	}
	return "node"
}

func pnpmCmd() string {
	if fileExists(pnpmExe()) {
		return pnpmExe()
	}
	return "pnpm"
}

func runtimeOK() bool { return nodeAvailable() && pnpmAvailable() }

// hideCmdWindow 隐藏子进程窗口（win32 GUI 应用无控制台，避免闪窗）。
func hideCmdWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

// refreshEnvPath 把便携运行时加入当前进程 PATH 并持久化到用户 PATH（无需管理员）。
func refreshEnvPath() {
	dirs := []string{nodeDir(), runtimeDir()}
	cur := os.Getenv("PATH")
	lower := strings.ToLower(cur)
	for _, d := range dirs {
		if !strings.Contains(lower, strings.ToLower(d)) {
			cur = d + ";" + cur
		}
	}
	os.Setenv("PATH", cur)
	ps := fmt.Sprintf("$p=[Environment]::GetEnvironmentVariable('Path','User'); if($p -notlike '*%s*'){ [Environment]::SetEnvironmentVariable('Path','%s;'+$p,'User') }", runtimeDir(), runtimeDir())
	runHidden("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", ps)
}

// downloadFile 带进度回调的下载。
func downloadFile(url, dest string, splash *SplashState, from, to float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	buf := make([]byte, 256*1024)
	var done int64
	total := resp.ContentLength
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if splash != nil && total > 0 {
				pct := float64(done) / float64(total)
				splash.Update(fmt.Sprintf("正在下载 Node.js 运行时（%.0f%%）…", pct*100), from+(to-from)*pct)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

func extractZip(zipPath, destDir string) error {
	cmd := exec.Command("tar", "-xf", zipPath, "-C", destDir)
	hideCmdWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

// ensureRuntime 下载便携 Node.js + 安装 pnpm（后台静默，无 UAC）。
func ensureRuntime(splash *SplashState) error {
	if err := os.MkdirAll(runtimeDir(), 0o755); err != nil {
		return err
	}
	if !nodeAvailable() {
		splash.Update("正在下载 Node.js 运行时（约 30MB）…", 0.10)
		zipPath := filepath.Join(runtimeDir(), "node.zip")
		if err := downloadFile(nodeDownloadURL, zipPath, splash, 0.10, 0.22); err != nil {
			return fmt.Errorf("下载 Node.js 失败：%w", err)
		}
		splash.Update("正在解压 Node.js 运行时…", 0.24)
		if err := extractZip(zipPath, runtimeDir()); err != nil {
			return fmt.Errorf("解压 Node.js 失败：%w", err)
		}
		_ = os.Remove(zipPath)
		src := filepath.Join(runtimeDir(), "node-v24.9.0-win-x64")
		if err := os.Rename(src, nodeDir()); err != nil && !fileExists(nodeExe()) {
			return fmt.Errorf("整理 Node.js 目录失败：%w", err)
		}
	}
	if !pnpmAvailable() {
		splash.Update("正在安装 pnpm 包管理器…", 0.26)
		logPath := filepath.Join(logDir, "install.log")
		f, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		cmd := exec.Command(filepath.Join(nodeDir(), "npm.cmd"), "install", "-g", "pnpm@10.34.5", "--prefix", runtimeDir(), "--loglevel", "error")
		hideCmdWindow(cmd)
		if f != nil {
			cmd.Stdout = f
			cmd.Stderr = f
		}
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("安装 pnpm 失败：%w（日志：%s）", err, logPath)
		}
	}
	splash.Update("运行环境就绪", 0.30)
	return nil
}

type browseInfoW struct {
	hwndOwner      uintptr
	pidlRoot       uintptr
	pszDisplayName *uint16
	lpszTitle      *uint16
	ulFlags        uint32
	lpfn           uintptr
	lParam         uintptr
	iImage         int32
}

// pickHarnessDir 弹出目录选择对话框（经典 SHBrowseForFolder，无需 COM 初始化），返回用户选择的目录；取消返回 ""。
func pickHarnessDir(title, initial string) string {
	modShell32 := syscall.NewLazyDLL("shell32.dll")
	browse := modShell32.NewProc("SHBrowseForFolderW")
	getPath := modShell32.NewProc("SHGetPathFromIDListW")
	free := syscall.NewLazyDLL("ole32.dll").NewProc("CoTaskMemFree")

	titlePtr, _ := syscall.UTF16PtrFromString(title)
	var display [260]uint16
	bi := browseInfoW{
		hwndOwner:      0,
		lpszTitle:      titlePtr,
		pszDisplayName: &display[0],
		ulFlags:        0x0001, // BIF_RETURNONLYFSDIRS：仅返回文件系统目录
	}
	pidl, _, _ := browse.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	defer free.Call(pidl)

	var pathBuf [260]uint16
	if r, _, _ := getPath.Call(pidl, uintptr(unsafe.Pointer(&pathBuf[0]))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(pathBuf[:])
}

func trayIconData() []byte {
	return iconData
}

// setTemplateIcon Windows 无需模板图标，占位实现。
func setTemplateIcon() {}

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
		cmd = exec.Command(pnpmCmd(), "dsh", "web", "--port", strconv.Itoa(port), "--no-open")
	}
	cmd.Dir = harnessDir
	hideCmdWindow(cmd)
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
	// 终止本应用启动的服务器进程树
	if serverCmd != nil && serverCmd.Process != nil {
		exec.Command("taskkill", "/PID", strconv.Itoa(serverCmd.Process.Pid), "/T", "/F").Run()
		serverCmd = nil
	}
	// 终止监听本端口的 dsh web 进程（即使不是本应用启动的）
	if pid, err := findListenerPID(port); err == nil {
		log.Printf("killing listener pid=%d on port %d", pid, port)
		exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
	}
}

func findListenerPID(port int) (int, error) {
	out, err := exec.Command("netstat", "-ano", "-p", "TCP").Output()
	if err != nil {
		return 0, err
	}
	suffix := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && f[0] == "TCP" && strings.HasSuffix(f[1], suffix) && f[3] == "LISTENING" {
			if pid, err := strconv.Atoi(f[4]); err == nil {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("no listener on port %d", port)
}

func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		log.Printf("open browser failed: %v", err)
	}
}

func isAutostartEnabled() bool {
	cmd := exec.Command("reg", "query", registryPath, "/v", registryName)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run() == nil
}

func enableAutostart() {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("cannot resolve exe path: %v", err)
		return
	}
	val := `"` + exe + `"`
	runHidden("reg", "add", registryPath, "/v", registryName, "/t", "REG_SZ", "/d", val, "/f")
}

func disableAutostart() {
	runHidden("reg", "delete", registryPath, "/v", registryName, "/f")
}

func runHidden(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("cmd %s %v failed: %v: %s", name, args, err, string(out))
	}
}

func runInstaller() {
	tmp := filepath.Join(os.TempDir(), "dsh-systray-install-prereqs.ps1")
	script := strings.ReplaceAll(string(installScript), "{{HARNESS_DIR}}", harnessDir)
	if err := os.WriteFile(tmp, []byte(script), 0o644); err != nil {
		log.Printf("write installer failed: %v", err)
		showMessageBox("检测到缺少运行依赖，但无法写入安装脚本。", appName)
		return
	}

	ps := fmt.Sprintf(
		`Start-Process -Verb RunAs -Wait -FilePath 'powershell.exe' -ArgumentList '-NoProfile -ExecutionPolicy Bypass -File "%s"'`,
		tmp,
	)
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
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
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	createMutex := kernel32.NewProc("CreateMutexW")
	closeHandle := kernel32.NewProc("CloseHandle")

	name, err := syscall.UTF16PtrFromString(mutexName)
	if err != nil {
		return func() {}, true
	}
	h, _, callErr := createMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return func() {}, true
	}
	// 用 Call 返回的 err（GetLastError 的即时值）判断互斥体是否已存在。
	// 不要单独再调 GetLastError：期间运行时可能重置 last-error，导致误判。
	if callErr == syscall.ERROR_ALREADY_EXISTS {
		closeHandle.Call(h)
		return func() {}, false
	}
	return func() { closeHandle.Call(h) }, true
}

func messageBoxResult(text, caption string, flags uintptr) uintptr {
	user32 := syscall.NewLazyDLL("user32.dll")
	msgBox := user32.NewProc("MessageBoxW")
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(caption)
	ret, _, _ := msgBox.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags)
	return ret
}

func showMessageBox(text, caption string) {
	messageBoxResult(text, caption, 0x00000010) // MB_ICONERROR
}

func showReadyPrompt(url string) {
	ret := messageBoxResult("DeepSeek Harness 服务已就绪。\n是否立即打开 Web UI？", appName, 0x00000004|0x00000040|0x00010000|0x00040000)
	if ret == 6 { // IDYES
		openBrowser(url)
	}
}

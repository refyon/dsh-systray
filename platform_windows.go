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

	"github.com/getlantern/systray"
)

//go:embed scripts/install-prereqs.ps1
var installScript []byte

const (
	registryPath = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	registryName = `DeepSeekHarness`
	mutexName    = `Local\dsh-systray-single-instance`
	// postUpdateEnv 更新后重启的新进程环境变量标记：允许其接管（旧实例正退出）。
	postUpdateEnv = `DSH_SYSTRAY_POST_UPDATE`
	// registryPersonalizeKey 系统主题个性化键：SystemUsesLightTheme=1 浅色任务栏，0 深色任务栏。
	registryPersonalizeKey = `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`
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
	// 启动即适配当前主题：浅色任务栏用深色鲸鱼，深色任务栏用浅色鲸鱼。
	if taskbarLight() {
		return iconDataDark
	}
	return iconData
}

// setTemplateIcon Windows 无需模板图标，占位实现。
func setTemplateIcon() {}

// startIconThemeWatch 监听系统主题变化（RegNotifyChangeKeyValue 事件驱动），
// 主题翻转时切换托盘图标配色（浅色任务栏 ↔ 深色任务栏）。
func startIconThemeWatch() {
	go func() {
		for {
			if !waitThemeChange(3 * time.Second) {
				// 通知失败或超时：短暂退避后重试，避免热循环。
				time.Sleep(3 * time.Second)
				continue
			}
			systray.SetIcon(trayIconData())
		}
	}()
}

// themeDWORD 读取主题注册表 DWORD 值；读取失败返回 (0, false)。
func themeDWORD(name string) (uint32, bool) {
	modAdvapi := syscall.NewLazyDLL("advapi32.dll")
	regOpenKeyEx := modAdvapi.NewProc("RegOpenKeyExW")
	regQueryValueEx := modAdvapi.NewProc("RegQueryValueExW")
	regCloseKey := modAdvapi.NewProc("RegCloseKey")

	subKey, _ := syscall.UTF16PtrFromString(registryPersonalizeKey)
	var hkey uintptr
	ret, _, _ := regOpenKeyEx.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(subKey)), 0, 0x0001 /* KEY_QUERY_VALUE */, uintptr(unsafe.Pointer(&hkey)))
	if ret != 0 {
		return 0, false
	}
	defer regCloseKey.Call(hkey)

	valueName, _ := syscall.UTF16PtrFromString(name)
	var data uint32
	var size uint32 = 4
	ret, _, _ = regQueryValueEx.Call(hkey, uintptr(unsafe.Pointer(valueName)), 0, 0, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&size)))
	if ret != 0 {
		return 0, false
	}
	return data, true
}

// taskbarLight 判断任务栏是否为浅色：优先 SystemUsesLightTheme，失败回退 AppsUseLightTheme；仍失败默认浅色。
func taskbarLight() bool {
	if v, ok := themeDWORD("SystemUsesLightTheme"); ok {
		return v == 1
	}
	if v, ok := themeDWORD("AppsUseLightTheme"); ok {
		return v == 1
	}
	return true
}

// waitThemeChange 阻塞等待主题相关注册表值变化（异步通知 + 事件 + 超时）；
// 返回 true 表示检测到变化（主题可能已翻转）。
func waitThemeChange(timeout time.Duration) bool {
	modAdvapi := syscall.NewLazyDLL("advapi32.dll")
	modKernel := syscall.NewLazyDLL("kernel32.dll")
	regOpenKeyEx := modAdvapi.NewProc("RegOpenKeyExW")
	regNotify := modAdvapi.NewProc("RegNotifyChangeKeyValue")
	regCloseKey := modAdvapi.NewProc("RegCloseKey")
	createEvent := modKernel.NewProc("CreateEventW")
	waitForSingleObject := modKernel.NewProc("WaitForSingleObject")
	closeHandle := modKernel.NewProc("CloseHandle")

	subKey, _ := syscall.UTF16PtrFromString(registryPersonalizeKey)
	var hkey uintptr
	const keyNotify = 0x0010 // KEY_NOTIFY
	ret, _, _ := regOpenKeyEx.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(subKey)), 0, keyNotify, uintptr(unsafe.Pointer(&hkey)))
	if ret != 0 {
		return false
	}
	defer regCloseKey.Call(hkey)

	evt, _, _ := createEvent.Call(0, 0, 0, 0)
	if evt == 0 {
		return false
	}
	defer closeHandle.Call(evt)

	const regNotifyChangeLastSet = 0x00000004
	ret, _, _ = regNotify.Call(hkey, 0, regNotifyChangeLastSet, evt, 1) // fAsynchronous=TRUE
	if ret != 0 {
		return false
	}
	ms := uintptr(timeout / time.Millisecond)
	if ms <= 0 {
		ms = 3000
	}
	waitRet, _, _ := waitForSingleObject.Call(evt, ms)
	return waitRet == 0 // WAIT_OBJECT_0：事件已触发
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
	trackChildProcess(cmd.Process)
	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()
	log.Printf("server started, pid=%d", cmd.Process.Pid)
	return true, exitCh
}

func killServer() {
	// 终止本应用启动的服务器进程树
	if serverCmd != nil && serverCmd.Process != nil {
		cmd := exec.Command("taskkill", "/PID", strconv.Itoa(serverCmd.Process.Pid), "/T", "/F")
		hideCmdWindow(cmd)
		_ = cmd.Run()
		serverCmd = nil
	}
	// 终止监听本端口的 dsh web 进程（即使不是本应用启动的）
	if pid, err := findListenerPID(port); err == nil {
		log.Printf("killing listener pid=%d on port %d", pid, port)
		cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
		hideCmdWindow(cmd)
		_ = cmd.Run()
	}
}

func findListenerPID(port int) (int, error) {
	cmd := exec.Command("netstat", "-ano", "-p", "TCP")
	hideCmdWindow(cmd)
	out, err := cmd.Output()
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
		return
	}
	trackChildProcess(cmd.Process)
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
		// 更新后重启：旧实例正在退出，允许新实例接管（否则会被误判为重复运行而退场）。
		if os.Getenv(postUpdateEnv) == "1" {
			return func() { closeHandle.Call(h) }, true
		}
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
	runModernDialog(caption, text, []string{"确定"}, 0)
}

// askStopServer 退出前询问是否停止后台 Web 服务：0=停止并退出，1=保留服务，-1=取消退出。
func askStopServer() int {
	return runModernDialog(appName, "是否停止后台 Web 服务？\n\n「确定」将停止服务并退出；\n「保留后台服务」仅关闭托盘，服务继续运行。", []string{"确定", "保留后台服务"}, 0)
}

func showReadyPrompt(url string) {
	ret := runModernDialog(appName, "DeepSeek Harness 服务已就绪。\n是否立即打开 Web UI？", []string{"打开", "取消"}, 0)
	if ret == 0 {
		openBrowser(url)
	}
}

// askUpdateDialog 提示用户发现新版本：true=立即更新。
func askUpdateDialog(newVer string) bool {
	msg := fmt.Sprintf("发现新版本 %s（当前版本 %s）。\n是否立即下载并更新？", withV(newVer), withV(appVersion))
	return runModernDialog(appName, msg, []string{"立即更新", "稍后"}, 0) == 0
}

// askUpdateHarness 提示用户 DeepSeek Harness 有新版本：true=先更新 Harness。
func askUpdateHarness(newVer, curVer string) bool {
	msg := fmt.Sprintf("DeepSeek Harness 有新版本 %s（当前 %s）。\n是否先更新 Harness？", withV(newVer), withV(curVer))
	return runModernDialog(appName, msg, []string{"更新 Harness", "稍后"}, 0) == 0
}

// replaceAndRelaunch 替换当前 exe 并重启。Windows 不允许覆盖正在运行的 exe，
// 采用「改名旧程序 → 换入新程序 → 重启」方案（minio/selfupdate 同款思路）。
// 新 exe 可能位于其他盘符（临时目录在 C:、程序在 D:），os.Rename 无法跨盘移动，
// 因此先把新 exe 复制到自身目录的同盘临时文件（.new），再做同盘改名换入。
func replaceAndRelaunch(newExe string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法定位当前程序路径：%w", err)
	}

	// 复制到自身目录（与 exe 同盘）的临时文件，规避跨盘 rename 失败
	tmpTarget := exe + ".new"
	if err := copyFile(newExe, tmpTarget); err != nil {
		return fmt.Errorf("无法写入新程序：%w", err)
	}
	defer os.Remove(tmpTarget)

	oldPath := exe + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(exe, oldPath); err != nil {
		_ = os.Remove(tmpTarget)
		return fmt.Errorf("无法重命名当前程序（所在目录可能无写权限）：%w", err)
	}
	if err := os.Rename(tmpTarget, exe); err != nil {
		_ = os.Rename(oldPath, exe) // 回滚，恢复旧程序
		return fmt.Errorf("无法写入新程序：%w", err)
	}
	if singleInstanceRelease != nil {
		singleInstanceRelease() // 释放单实例互斥体，便于新进程立即接管
	}
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Env = append(os.Environ(), postUpdateEnv+"=1") // 标记为更新后重启，允许接管单实例
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("更新完成但重启失败：%w", err)
	}
	log.Printf("update applied, relaunching %s", exe)
	os.Exit(0)
	return nil
}

// copyFile 复制源文件到目标；目标先写临时再改名（避免复制中途损坏）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// startUpdateApply Windows：进程内执行更新——进度窗口下载 → 校验 → 解压 → 替换 → 自动重启。
// 进度窗口关闭时提示是否取消；确认取消则终止下载/更新（可取消 context）。
func startUpdateApply(rel *latestRelease) {
	assetName := updateAssetName()
	var zipURL, sumURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case assetName:
			zipURL = a.BrowserDownloadURL
		case "SHA256SUMS.txt":
			sumURL = a.BrowserDownloadURL
		}
	}
	if zipURL == "" {
		showMessageBox("未找到适用于当前系统的更新包。", appName)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registerActiveUpdate(cancel)
	defer clearActiveUpdate()

	splash := startSplash("正在准备更新…")
	splash.SetOnClose(func() bool {
		if askCancelUpdate() {
			log.Printf("update cancelled by user")
			cancel()
			return true // 关闭进度窗口并中止
		}
		return false // 继续更新，不关闭窗口
	})

	dir, err := os.MkdirTemp("", "dsh-systray-update-*")
	if err != nil {
		splash.Close()
		showMessageBox("创建临时目录失败：\n"+err.Error(), appName)
		return
	}
	defer os.RemoveAll(dir)

	zipPath := filepath.Join(dir, assetName)
	splash.Update("正在下载 "+assetName+"…", 0.08)
	if err := downloadFileWithProgress(ctx, zipURL, zipPath, func(pct float64) {
		splash.Update(fmt.Sprintf("正在下载 %s（%.0f%%）…", assetName, pct*100), 0.08+0.52*pct)
	}); err != nil {
		splash.Close()
		if ctx.Err() != nil {
			return // 用户取消，不做错误提示
		}
		showMessageBox("下载更新包失败：\n"+err.Error()+"\n\n请检查网络，或稍后重试。", appName)
		return
	}

	if sumURL != "" {
		sumPath := filepath.Join(dir, "SHA256SUMS.txt")
		if err := downloadFileTo(ctx, sumURL, sumPath); err != nil && ctx.Err() != nil {
			splash.Close()
			return // 用户取消
		} else if err == nil {
			if err := verifyChecksum(zipPath, assetName, sumPath); err != nil {
				splash.Close()
				showMessageBox("更新包校验失败：\n"+err.Error(), appName)
				return
			}
		} else {
			log.Printf("checksum file unavailable: %v", err)
		}
	}

	if ctx.Err() != nil {
		splash.Close()
		return
	}
	splash.Update("正在解压安装…", 0.65)
	extractDir := filepath.Join(dir, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		splash.Close()
		showMessageBox("解压失败：\n"+err.Error(), appName)
		return
	}
	if err := extractUpdateZip(zipPath, extractDir); err != nil {
		splash.Close()
		showMessageBox("解压更新包失败：\n"+err.Error(), appName)
		return
	}
	payload, err := updatePayloadPath(extractDir)
	if err != nil {
		splash.Close()
		showMessageBox("更新包内容异常：\n"+err.Error(), appName)
		return
	}
	if ctx.Err() != nil {
		splash.Close()
		return
	}
	splash.Update("正在更新程序…", 0.9)
	if err := replaceAndRelaunch(payload); err != nil {
		splash.Close()
		showMessageBox("重启失败：\n"+err.Error()+"\n\n程序已替换，请手动重启。", appName)
		return
	}
	// replaceAndRelaunch 成功后内部 os.Exit，不会走到这里
}

// askCancelUpdate 关闭下载进度窗口时询问是否取消更新：true=确认取消。
func askCancelUpdate() bool {
	return runModernDialog(appName, "是否取消更新？", []string{"取消更新", "继续更新"}, 0) == 0
}

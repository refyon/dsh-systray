//go:build windows

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

func defaultHarnessDir() string {
	return `I:\deepseek-harness`
}

func trayIconData() []byte {
	return iconData
}

func startServer() {
	serverLogPath := filepath.Join(logDir, "server.log")
	f, err := os.OpenFile(serverLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("cannot open server log: %v", err)
	}
	if f != nil {
		defer f.Close()
	}

	cmd := exec.Command("cmd", "/c", fmt.Sprintf("pnpm dsh web --port %d --no-open", port))
	cmd.Dir = harnessDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	if f != nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		log.Printf("failed to start server: %v", err)
		showMessageBox("启动 DeepSeek Harness 服务器失败："+err.Error(), appName)
		return
	}
	serverCmd = cmd
	log.Printf("server started, pid=%d", cmd.Process.Pid)
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
	if err := os.WriteFile(tmp, installScript, 0o644); err != nil {
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

	if !prereqsOK() {
		showMessageBox(
			"运行依赖安装未完成（Node.js / pnpm / harness 缺失）。\n服务器可能无法启动，详情见日志：\n"+filepath.Join(logDir, "app.log"),
			appName,
		)
	} else {
		log.Printf("prerequisites now satisfied")
	}
}

func acquireSingleInstance() (func(), bool) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	createMutex := kernel32.NewProc("CreateMutexW")
	getLastError := kernel32.NewProc("GetLastError")
	closeHandle := kernel32.NewProc("CloseHandle")

	name, err := syscall.UTF16PtrFromString(mutexName)
	if err != nil {
		return func() {}, true
	}
	h, _, _ := createMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return func() {}, true
	}
	errCode, _, _ := getLastError.Call()
	if errCode == 183 { // ERROR_ALREADY_EXISTS
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

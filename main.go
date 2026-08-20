package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/getlantern/systray"
)

//go:embed scripts/install-prereqs.ps1
var installScript []byte

// iconData 定义在 icon_gen.go（由 scripts/gen-icon.mjs 生成，base64 内嵌多尺寸 ICO）。

const (
	appName      = "DeepSeek Harness"
	defaultPort  = 3080
	registryPath = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	registryName = `DeepSeekHarness`
	mutexName    = `Local\dsh-systray-single-instance`
)

var (
	serverCmd  *exec.Cmd
	logDir     string
	webURL     string
	harnessDir string
	port       int
)

type appConfig struct {
	Port       int    `json:"port"`
	HarnessDir string `json:"harnessDir"`
}

func loadConfig() appConfig {
	cfg := appConfig{Port: defaultPort, HarnessDir: `I:\deepseek-harness`}
	if exe, err := os.Executable(); err == nil {
		if data, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "config.json")); err == nil {
			var f appConfig
			if json.Unmarshal(data, &f) == nil {
				if f.Port != 0 {
					cfg.Port = f.Port
				}
				if f.HarnessDir != "" {
					cfg.HarnessDir = f.HarnessDir
				}
			}
		}
	}
	if p := os.Getenv("DSH_SYSTRAY_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			cfg.Port = n
		}
	}
	return cfg
}

func main() {
	cfg := loadConfig()
	port = cfg.Port
	webURL = fmt.Sprintf("http://127.0.0.1:%d/", port)
	harnessDir = cfg.HarnessDir

	logDir = filepath.Join(os.Getenv("LOCALAPPDATA"), "dsh-systray", "logs")
	if err := os.MkdirAll(logDir, 0o755); err == nil {
		if f, ferr := os.OpenFile(filepath.Join(logDir, "app.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
			defer f.Close()
			log.SetOutput(f)
		}
	}
	log.SetFlags(log.LstdFlags)

	h, acquired := acquireSingleInstance()
	if !acquired {
		// 已在运行：直接打开 Web UI 后退出，不产生第二个托盘图标。
		openBrowser(webURL)
		return
	}
	defer releaseSingleInstance(h)

	if !prereqsOK() {
		log.Printf("missing prerequisites detected, running installer")
		runInstaller()
	}

	startServer()

	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(iconData)
	systray.SetTitle(appName)
	systray.SetTooltip(appName)

	mOpen := systray.AddMenuItem("打开 Web UI", "打开网页端界面")
	systray.AddSeparator()
	mAuto := systray.AddMenuItem("开机自启动", "登录 Windows 时自动启动")
	if isAutostartEnabled() {
		mAuto.Check()
	}
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "退出并关闭后台服务器")

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				openBrowser(webURL)
			case <-mAuto.ClickedCh:
				toggleAutostart(mAuto)
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {
	killServer()
	log.Printf("tray exiting")
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
		showMessageBox("启动 DeepSeek Harness 服务器失败："+err.Error(), appName, 0x00000010)
		return
	}
	serverCmd = cmd
	log.Printf("server started, pid=%d", cmd.Process.Pid)
}

func killServer() {
	if serverCmd == nil || serverCmd.Process == nil {
		return
	}
	pid := serverCmd.Process.Pid
	log.Printf("stopping server tree pid=%d", pid)
	exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
	_ = serverCmd.Process.Kill()
	serverCmd = nil
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

func toggleAutostart(item *systray.MenuItem) {
	if isAutostartEnabled() {
		disableAutostart()
		item.Uncheck()
		log.Printf("autostart disabled")
	} else {
		enableAutostart()
		item.Check()
		log.Printf("autostart enabled")
	}
}

func runHidden(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("cmd %s %v failed: %v: %s", name, args, err, string(out))
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

func runInstaller() {
	tmp := filepath.Join(os.TempDir(), "dsh-systray-install-prereqs.ps1")
	if err := os.WriteFile(tmp, installScript, 0o644); err != nil {
		log.Printf("write installer failed: %v", err)
		showMessageBox("检测到缺少运行依赖，但无法写入安装脚本。", appName, 0x00000010)
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
			appName, 0x00000010,
		)
	} else {
		log.Printf("prerequisites now satisfied")
	}
}

func acquireSingleInstance() (uintptr, bool) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	createMutex := kernel32.NewProc("CreateMutexW")
	getLastError := kernel32.NewProc("GetLastError")
	closeHandle := kernel32.NewProc("CloseHandle")

	name, err := syscall.UTF16PtrFromString(mutexName)
	if err != nil {
		return 0, true
	}
	h, _, _ := createMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return 0, true
	}
	errCode, _, _ := getLastError.Call()
	if errCode == 183 { // ERROR_ALREADY_EXISTS
		closeHandle.Call(h)
		return 0, false
	}
	return h, true
}

func releaseSingleInstance(h uintptr) {
	if h == 0 {
		return
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	closeHandle := kernel32.NewProc("CloseHandle")
	closeHandle.Call(h)
}

func showMessageBox(text, caption string, flags uintptr) {
	user32 := syscall.NewLazyDLL("user32.dll")
	msgBox := user32.NewProc("MessageBoxW")
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(caption)
	msgBox.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags)
}

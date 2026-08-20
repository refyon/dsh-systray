package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/getlantern/systray"
)

const (
	appName     = "DeepSeek Harness"
	defaultPort = 3080
)

var (
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
	cfg := appConfig{Port: defaultPort, HarnessDir: defaultHarnessDir()}
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
	if !acquired {
		// 已在运行：弹窗提示后退出，不产生第二个托盘图标。
		showMessageBox("DeepSeek Harness 已在运行中，请使用系统托盘图标操作。", appName)
		return
	}
	defer release()

	if !prereqsOK() {
		log.Printf("missing prerequisites detected, running installer")
		runInstaller()
	}

	closeSplash := startSplash("正在启动 DeepSeek Harness 服务，请稍候…")

	startServer()

	go func() {
		ready := waitForServerReady(webURL, 90*time.Second)
		closeSplash()
		if ready {
			notifyReady()
		} else {
			showMessageBox("DeepSeek Harness 服务启动超时或失败，请查看日志：\n"+filepath.Join(logDir, "server.log"), appName)
		}
	}()

	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(trayIconData())
	systray.SetTitle(appName)
	systray.SetTooltip(appName)

	mOpen := systray.AddMenuItem("打开 Web UI", "打开网页端界面")
	systray.AddSeparator()
	mAuto := systray.AddMenuItem("开机自启动", "登录系统时自动启动")
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

func waitForServerReady(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return true
			}
		}
		time.Sleep(1 * time.Second)
	}
	return false
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

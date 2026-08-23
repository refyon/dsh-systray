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
	if d := os.Getenv("DSH_SYSTRAY_HARNESS_DIR"); d != "" {
		cfg.HarnessDir = d
	}
	return cfg
}

// saveConfig 将配置写回 exe 同目录的 config.json，便于记住用户选择的目录。
func saveConfig(cfg appConfig) {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config.json")
		if data, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			if err := os.WriteFile(p, data, 0o644); err != nil {
				log.Printf("cannot write config.json: %v", err)
			} else {
				log.Printf("wrote config.json: %s", p)
			}
		}
	}
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

	// 兼容：默认/配置的 harness 目录不可用时，让用户选择工作目录（新机器默认路径可能不存在）
	if _, err := os.Stat(filepath.Join(harnessDir, "package.json")); err != nil {
		log.Printf("harness source not found at %s, prompting user to choose", harnessDir)
		if chosen := pickHarnessDir("未找到 DeepSeek Harness 源码目录。\n请选择已有的 harness 源码目录，或选择即将自动安装到的文件夹：", harnessDir); chosen != "" {
			harnessDir = chosen
			cfg.HarnessDir = chosen
			saveConfig(cfg)
			log.Printf("harness dir set to %s", harnessDir)
		}
	}

	if !prereqsOK() {
		log.Printf("missing prerequisites detected, running installer")
		runInstaller()
	}
	// 安装后仍缺少依赖，则不启动：避免在缺失目录上 fork 失败、loading 卡住
	if !prereqsOK() {
		showMessageBox("运行依赖（Node.js / pnpm / harness 源码）仍缺失，无法启动 DeepSeek Harness。\n请检查网络后重试，或查看日志：\n"+filepath.Join(logDir, "app.log"), appName)
		return
	}

	// 兼容：harness 已 clone 但未执行 pnpm run build（缺少前端/客户端产物）——自动补构建
	if !harnessBuiltOK() {
		log.Printf("harness build outputs missing, running pnpm run build")
		closeBuild := startSplash("检测到 deepseek-harness 尚未构建，正在自动执行 pnpm run build（首次约需 1-3 分钟）…")
		err := runHarnessBuild()
		closeBuild()
		if err != nil {
			showMessageBox("harness 自动构建失败：\n"+err.Error()+"\n\n构建日志："+filepath.Join(logDir, "build.log"), appName)
			return
		}
		if !harnessBuiltOK() {
			showMessageBox("harness 构建后产物仍缺失，请查看构建日志：\n"+filepath.Join(logDir, "build.log"), appName)
			return
		}
	}

	closeSplash := startSplash("正在启动 DeepSeek Harness 服务，请稍候…")

	// 若服务已在运行（端口被占用），不再重复启动，直接复用
	started := false
	if serverResponding(webURL) {
		log.Printf("server already running on %s, skipping spawn", webURL)
		started = true
	} else {
		started = startServer()
	}
	if !started {
		closeSplash()
		showMessageBox("启动 DeepSeek Harness 服务失败，请查看日志：\n"+filepath.Join(logDir, "app.log"), appName)
		return
	}

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
	cmd := exec.CommandContext(ctx, "pnpm", "run", "build")
	cmd.Dir = harnessDir
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

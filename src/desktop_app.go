package main

import (
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ==================== 官方桌面端（DeepSeek Harness Desktop） ====================
//
// 「启动方式」（配置项 `launchTarget`）决定托盘「打开」指向哪个界面，以及设置页「版本 / 检查更新 /
// 更新 / 重置」作用于哪个 harness 引擎：
//
//	web     —— 本程序用 node/pnpm 在 harnessDir 拉起的 dsh web（默认 3080 端口）。
//	           版本取自 <harnessDir>/node_modules/@deepseek-ai/dsh/package.json；
//	           更新走 npm 官方源（源码形态走 GitHub Releases）。
//	desktop —— 官方 Electron 桌面端：harness 随安装包内置，版本与桌面壳一致
//	           （官方发布规则：Electron 与 @deepseek-ai/dsh 始终使用同一精确版本，
//	           见 deepseek-harness/apps/desktop/README 的「发布身份」决策）；
//	           更新源由安装包自带的 resources/app-update.yml 声明，正式环境为
//	           https://download.deepseek.com/dsh-desk/feeds/<target>/，通道固定 Nightly。
//
// 两者是不同产品形态、各自独立更新，因此：
//   - 「重置」只对 web 形态成立（桌面端的数据目录 $DSH_HOME/profiles/desktop 由桌面端自己
//     拥有，本程序不代为清空），desktop 形态下该入口在设置页隐藏并在后端拒绝；
//   - 「更新」对桌面端只做「下载官方安装包 + 启动安装向导」：安装位置与卸载登记属于桌面端
//     自己的安装程序，本程序不代为替换文件，也不强制结束正在运行的桌面端（它的安装程序会
//     引导用户先退出，强制结束可能中断用户正在跑的任务）。

const (
	launchTargetAuto    = "auto"
	launchTargetWeb     = "web"
	launchTargetDesktop = "desktop"

	// desktopProductName 官方桌面端产品名（与卸载登记 DisplayName / macOS 应用包同名）。
	desktopProductName = "DeepSeek Harness"
	// desktopExeName Windows 安装目录内的主程序文件名。
	desktopExeName = "DeepSeek Harness.exe"
	// desktopHostPort 桌面端内置 Web Host 的默认端口（与 dsh web 的 3080 区分，见官方文档）。
	desktopHostPort = 19387

	// desktopFeedFallbackBase 读不到 app-update.yml 时的兜底更新源基址（官方正式环境）。
	desktopFeedFallbackBase = "https://download.deepseek.com/dsh-desk/feeds/"
	// desktopFeedFallbackChannel 兜底通道名：桌面端固定 Nightly，不提供通道切换。
	desktopFeedFallbackChannel = "nightly"
)

// desktopAppInfo 官方桌面端的本机安装信息（零值 = 未安装）。
//
// 由各平台的 detectDesktopApp 填充「安装事实」，随后由 finalizeDesktopApp 补齐更新源信息。
type desktopAppInfo struct {
	Installed    bool
	Version      string // 安装版本（Windows 取卸载登记 DisplayVersion；macOS 取 Info.plist）
	Exe          string // 主可执行文件完整路径（用于启动与进程识别）
	DisplayPath  string // 设置页展示的安装位置（Windows 安装目录；macOS .app 路径）
	ResourcesDir string // resources 目录（读 app-update.yml 用）
	Channel      string // 更新通道（app-update.yml 的 channel）
	FeedURL      string // 更新清单地址（<基址>/<通道>[-mac].yml）
}

// DesktopAppInfo 暴露给设置页的桌面端状态快照。
type DesktopAppInfo struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	Path      string `json:"path"`
	Running   bool   `json:"running"`
	Channel   string `json:"channel"`
	FeedURL   string `json:"feedURL"`
}

// desktopDetectTTL 安装检测缓存时长：托盘每 2 秒刷新一次菜单、设置页每 3 秒刷新一次状态，
// 每次都读注册表/文件系统没必要；5 秒足够跟手，又不会让「刚装完桌面端」久久不识别。
const desktopDetectTTL = 5 * time.Second

var (
	desktopDetectMu  sync.Mutex
	desktopDetectAt  time.Time
	desktopDetectVal desktopAppInfo
	desktopDetectOK  bool
)

// desktopApp 返回当前桌面端安装信息（带短缓存）。
func desktopApp() desktopAppInfo {
	desktopDetectMu.Lock()
	defer desktopDetectMu.Unlock()
	if desktopDetectOK && time.Since(desktopDetectAt) < desktopDetectTTL {
		return desktopDetectVal
	}
	info := finalizeDesktopApp(detectDesktopApp())
	desktopDetectVal, desktopDetectAt, desktopDetectOK = info, time.Now(), true
	return info
}

// invalidateDesktopAppCache 立即作废安装检测缓存（安装/更新动作后调用，避免 5 秒内的旧结论）。
func invalidateDesktopAppCache() {
	desktopDetectMu.Lock()
	desktopDetectOK = false
	desktopDetectMu.Unlock()
}

// ==================== 启动方式解析 ====================

// launchTargetPref 用户偏好（config.json 的 launchTarget 字段）：auto | web | desktop。
var launchTargetPref = launchTargetAuto

// launchMismatchAck 用户就「启动方式偏好与实机不符」选择「保持现状」时记下的偏好值：
// 同一偏好不再重复询问（见 launchTargetMismatch）；显式改动启动方式时清除。
var launchMismatchAck = ""

// normalizeLaunchTarget 规整偏好；非法值回退 auto。
func normalizeLaunchTarget(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case launchTargetWeb:
		return launchTargetWeb
	case launchTargetDesktop:
		return launchTargetDesktop
	default:
		return launchTargetAuto
	}
}

// resolveLaunchTargetPref 把偏好解析为实际启动方式：
//   - auto：装了官方桌面端就用桌面端，否则用 Web UI（首次升级的老用户即此路径，
//     无需手工设置就能用上桌面端）；
//   - desktop：桌面端缺失（被卸载 / 检测失败）时回退 Web UI，避免托盘「打开」变成无操作；
//   - web：始终 Web UI。
func resolveLaunchTargetPref(pref string, desktopInstalled bool) string {
	switch normalizeLaunchTarget(pref) {
	case launchTargetWeb:
		return launchTargetWeb
	case launchTargetDesktop:
		if desktopInstalled {
			return launchTargetDesktop
		}
		return launchTargetWeb
	default:
		if desktopInstalled {
			return launchTargetDesktop
		}
		return launchTargetWeb
	}
}

// resolvedLaunchTarget 当前生效的启动方式（web | desktop）。
func resolvedLaunchTarget() string {
	return resolveLaunchTargetPref(launchTargetPref, desktopApp().Installed)
}

// launchTargetIsDesktop 当前是否走桌面端（设置页/托盘分支判据）。
func launchTargetIsDesktop() bool { return resolvedLaunchTarget() == launchTargetDesktop }

// ==================== 更新源（app-update.yml + 清单） ====================

// desktopUpdateConfig resources/app-update.yml 中本程序关心的字段。
type desktopUpdateConfig struct {
	BaseURL string
	Channel string
}

// parseDesktopUpdateConfig 解析 app-update.yml（electron-builder 生成的简单 YAML）：
// 只取 `url:` 与 `channel:` 两个标量；publisherName 等其余字段（含列表）忽略。
func parseDesktopUpdateConfig(data string) desktopUpdateConfig {
	var out desktopUpdateConfig
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = unquoteYAMLScalar(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		switch k {
		case "url":
			if out.BaseURL == "" {
				out.BaseURL = v
			}
		case "channel":
			if out.Channel == "" {
				out.Channel = v
			}
		}
	}
	return out
}

// desktopFeedFileNameFor 更新清单文件名：electron-builder 的 macOS 清单带 -mac 后缀。
func desktopFeedFileNameFor(channel, goos string) string {
	ch := strings.TrimSpace(channel)
	if ch == "" {
		ch = desktopFeedFallbackChannel
	}
	if goos == "darwin" {
		return ch + "-mac.yml"
	}
	return ch + ".yml"
}

// defaultDesktopFeedBaseFor 兜底更新源基址（读不到 app-update.yml 时）：
// 按平台/架构拼出官方 CDN 的目标目录（Linux 不是官方桌面端发布目标，按 win-x64 处理不影响使用）。
func defaultDesktopFeedBaseFor(goos, goarch string) string {
	target := "win-x64"
	switch goos {
	case "darwin":
		if goarch == "amd64" {
			target = "mac-x64"
		} else {
			target = "mac-arm64"
		}
	}
	return desktopFeedFallbackBase + target + "/"
}

// desktopFeedURL 由更新源基址与通道拼出清单地址；基址为空时按平台/架构取官方兜底基址。
func desktopFeedURL(baseURL, channel, goos, goarch string) string {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = defaultDesktopFeedBaseFor(goos, goarch)
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + desktopFeedFileNameFor(channel, goos)
}

// readDesktopUpdateConfig 读取安装目录内的 app-update.yml（缺失返回零值，不视为错误）。
func readDesktopUpdateConfig(resourcesDir string) desktopUpdateConfig {
	if strings.TrimSpace(resourcesDir) == "" {
		return desktopUpdateConfig{}
	}
	data, err := os.ReadFile(filepath.Join(resourcesDir, "app-update.yml"))
	if err != nil {
		return desktopUpdateConfig{}
	}
	return parseDesktopUpdateConfig(string(data))
}

// finalizeDesktopApp 补齐更新源信息与展示路径（检测函数只负责「安装事实」）。
func finalizeDesktopApp(info desktopAppInfo) desktopAppInfo {
	if !info.Installed {
		return desktopAppInfo{}
	}
	if info.DisplayPath == "" {
		info.DisplayPath = info.Exe
	}
	cfg := readDesktopUpdateConfig(info.ResourcesDir)
	info.Channel = cfg.Channel
	info.FeedURL = desktopFeedURL(cfg.BaseURL, cfg.Channel, runtime.GOOS, runtime.GOARCH)
	return info
}

// desktopFeed 更新清单（nightly.yml / latest-mac.yml）中本程序关心的字段。
type desktopFeed struct {
	Version string
	URL     string // 安装包直链
	SHA512  string // 安装包 sha512（base64；macOS DMG 的清单项没有该字段）
	Size    int64
}

// parseDesktopFeed 解析 electron-builder 更新清单。
//
// 形如：
//
//	version: 0.1.7-rc.2
//	files:
//	  - url: >-
//	      https://.../deepseek-harness-0.1.7-rc.2-win-x64.exe
//	    sha512: >-
//	      AY7f...
//	    size: 288245480
//	path: >-
//	  https://.../deepseek-harness-0.1.7-rc.2-win-x64.exe
//	sha512: >-
//	  AY7f...
//
// 解析策略：按缩进分「根层」（path/sha512/size/version）与「嵌套层」（files[0] 的 url/sha512/size），
// 折叠标量（>- / |）取其后的缩进续行；根层优先（path 才是最终产物，url 仅作兜底）。
func parseDesktopFeed(data string) desktopFeed {
	root := map[string]string{}
	nested := map[string]string{}
	pendingKey := ""
	pendingRoot := false
	pendingIndent := -1
	put := func(m map[string]string, k, v string) {
		if v == "" {
			return
		}
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if pendingKey != "" && indent > pendingIndent {
			if pendingRoot {
				put(root, pendingKey, unquoteYAMLScalar(trimmed))
			} else {
				put(nested, pendingKey, unquoteYAMLScalar(trimmed))
			}
			pendingKey, pendingIndent = "", -1
			continue
		}
		pendingKey, pendingIndent = "", -1
		item := trimmed
		if strings.HasPrefix(item, "- ") {
			item = strings.TrimSpace(item[2:])
		}
		k, v, ok := strings.Cut(item, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		isRoot := indent == 0
		switch v {
		case ">-", "|", "|-", ">", "|+":
			pendingKey, pendingRoot, pendingIndent = k, isRoot, indent
			continue
		}
		if isRoot {
			put(root, k, unquoteYAMLScalar(v))
		} else {
			put(nested, k, unquoteYAMLScalar(v))
		}
	}
	feed := desktopFeed{Version: root["version"]}
	feed.URL = root["path"]
	if feed.URL == "" {
		feed.URL = nested["url"]
	}
	feed.SHA512 = root["sha512"]
	if feed.SHA512 == "" {
		feed.SHA512 = nested["sha512"]
	}
	sizeText := root["size"]
	if sizeText == "" {
		sizeText = nested["size"]
	}
	if n, err := strconv.ParseInt(sizeText, 10, 64); err == nil {
		feed.Size = n
	}
	return feed
}

// unquoteYAMLScalar 去掉 YAML 标量两侧的引号（清单里的 releaseDate 带单引号）。
func unquoteYAMLScalar(s string) string {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// fetchDesktopFeed 拉取并解析桌面端更新清单。
func fetchDesktopFeed(feedURL string) (desktopFeed, error) {
	feedURL = strings.TrimSpace(feedURL)
	if feedURL == "" {
		return desktopFeed{}, fmt.Errorf("未找到桌面端更新源地址")
	}
	client := newHTTPClient(updateAPITimeout)
	req, err := http.NewRequest("GET", feedURL, nil)
	if err != nil {
		return desktopFeed{}, err
	}
	req.Header.Set("User-Agent", "dsh-systray/"+appVersion)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return desktopFeed{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return desktopFeed{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, updateMaxBodySize))
	if err != nil {
		return desktopFeed{}, err
	}
	feed := parseDesktopFeed(string(body))
	if strings.TrimSpace(feed.Version) == "" {
		return desktopFeed{}, fmt.Errorf("更新清单缺少版本号")
	}
	return feed, nil
}

// desktopInstallerURL 由清单得出「交给用户安装」的安装包地址：
// Windows 直接用清单里的 exe；macOS 清单给的是自动更新用的 zip，用户安装应取同一目录下的
// dmg（官方每次发布同时产出 DMG 与 ZIP；DMG 具备签名+公证，且无需本程序替换 .app）。
// 返回的第二个值表示该地址是否带清单校验和（DMG 没有，跳过校验并在日志里说明）。
func desktopInstallerURL(feed desktopFeed, goos string) (string, bool) {
	u := strings.TrimSpace(feed.URL)
	if u == "" {
		return "", false
	}
	if goos == "darwin" && strings.HasSuffix(u, ".zip") {
		return strings.TrimSuffix(u, ".zip") + ".dmg", false
	}
	return u, strings.TrimSpace(feed.SHA512) != ""
}

// isDesktopUpdateAssetURL 判断地址是否指向官方桌面端安装包（下载前的最后一道白名单：
// 只允许 https + download.deepseek.com，避免清单被篡改后本程序去下载任意地址）。
func isDesktopUpdateAssetURL(u string) bool {
	pu, err := url.Parse(strings.TrimSpace(u))
	if err != nil {
		return false
	}
	if pu.Scheme != "https" {
		return false
	}
	host := strings.ToLower(pu.Host)
	return host == "download.deepseek.com" || strings.HasSuffix(host, ".deepseek.com")
}

// verifyFileSHA512 校验文件 sha512（清单里是 base64，electron-builder 约定）。
func verifyFileSHA512(path, wantB64 string) error {
	wantB64 = strings.TrimSpace(wantB64)
	if wantB64 == "" {
		return nil
	}
	want, err := base64.StdEncoding.DecodeString(wantB64)
	if err != nil {
		return fmt.Errorf("清单校验和格式无效：%w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := h.Sum(nil)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return fmt.Errorf("安装包校验和不匹配（下载可能被中断或损坏）")
	}
	return nil
}

// desktopInstallerPath 安装包落盘位置：用户下载目录（找不到时回退临时目录）。
func desktopInstallerPath(installerURL string) (string, error) {
	base := filepath.Base(strings.TrimSpace(installerURL))
	if i := strings.IndexByte(base, '?'); i >= 0 {
		base = base[:i]
	}
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "", fmt.Errorf("安装包地址无效")
	}
	dir := strings.TrimSpace(downloadsDir())
	if dir == "" {
		dir = os.TempDir()
	}
	out := filepath.Join(dir, base)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	return out, nil
}

// ==================== 检查 / 更新 桌面端 ====================

// checkDesktopUpdate 检查桌面端是否有新版本（比对安装包自带更新源的清单）。
// 与 web 形态的区别：桌面端通道固定（安装包声明的 Nightly），清单里的版本即可直接安装的
// 版本，因此不做「稳定版 / 预发布通道」过滤。
func checkDesktopUpdate() ModuleUpdate {
	info := desktopApp()
	out := ModuleUpdate{Current: info.Version}
	if !info.Installed {
		out.Error = "未检测到官方桌面端，无法检查桌面端更新。"
		return out
	}
	feed, err := fetchDesktopFeed(info.FeedURL)
	if err != nil {
		out.Error = "无法获取桌面端更新清单：" + err.Error()
		return out
	}
	out.Latest = feed.Version
	if isNewerVersion(feed.Version, info.Version) {
		out.HasUpdate = true
	} else {
		out.Latest = withV(info.Version)
	}
	return out
}

// StartDesktopUpdate 下载官方安装包并启动安装向导（desktop 启动方式下的「更新」）。
// 只做「下载 + 交给安装向导」：安装位置、卸载登记与退出时机由桌面端自己的安装程序负责，
// 本程序不替换其文件，也不强制结束正在运行的桌面端。
func (a *App) StartDesktopUpdate() {
	if pluginBatchRunning() {
		showMessageBox("正在批量处理插件（更新/删除），请等待完成后再更新桌面端。", appName)
		return
	}
	info := desktopApp()
	if !info.Installed {
		showMessageBox("未检测到官方桌面端，无法更新。", appName)
		return
	}
	feed, err := fetchDesktopFeed(info.FeedURL)
	if err != nil {
		showMessageBox("检查桌面端更新失败：\n"+err.Error(), appName)
		return
	}
	if !isNewerVersion(feed.Version, info.Version) {
		return
	}
	installer, hasSum := desktopInstallerURL(feed, runtime.GOOS)
	if installer == "" {
		showMessageBox("更新清单中没有可用的安装包地址。", appName)
		return
	}
	if !isDesktopUpdateAssetURL(installer) {
		showMessageBox("更新清单中的安装包地址不在官方下载域名内，已中止。", appName)
		return
	}
	logUI("开始更新桌面端", fmt.Sprintf("当前 %s | 目标 %s", orDash(info.Version), withV(feed.Version)))
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
		ensureMainWindowForeground()
	}
	go runDesktopAppUpdate(info, feed, installer, hasSum)
}

// startDesktopInstallFlow 「安装桌面端」（启动方式询问里选安装，或未安装时的手动安装入口）：
// 从官方更新源取最新安装包 → 下载 → 校验 → 启动安装向导。
//
// 与「更新桌面端」的区别只有更新源来源：未安装时读不到安装包自带的 app-update.yml，
// 因此直接用平台兜底基址（官方 CDN 的 <target>/nightly.yml）；下载/校验/进度/取消
// 全部复用同一条流程（见 runDesktopAppUpdate）。
func startDesktopInstallFlow() {
	if pluginBatchRunning() {
		showMessageBox("正在批量处理插件（更新/删除），请等待完成后再安装桌面端。", appName)
		return
	}
	info := desktopApp()
	if info.Installed {
		showMessageBox("已检测到官方桌面端，无需重复安装。", appName)
		return
	}
	feedURL := desktopFeedURL("", "", runtime.GOOS, runtime.GOARCH)
	feed, err := fetchDesktopFeed(feedURL)
	if err != nil {
		showMessageBox("获取官方桌面端安装包信息失败：\n"+err.Error()+"\n\n更新源："+feedURL, appName)
		return
	}
	installer, hasSum := desktopInstallerURL(feed, runtime.GOOS)
	if installer == "" || !isDesktopUpdateAssetURL(installer) {
		showMessageBox("官方更新源中没有可用的安装包地址，已中止。\n\n更新源："+feedURL, appName)
		return
	}
	logUI("安装官方桌面端", withV(feed.Version))
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
		ensureMainWindowForeground()
	}
	// 未安装：info 只用于日志与「桌面端是否在运行」判定（Exe 为空时按进程名匹配，仍安全）。
	go runDesktopAppUpdate(info, feed, installer, hasSum)
}

// InstallDesktopApp 设置页「安装桌面端」：下载官方安装包并启动安装向导（未安装时才受理）。
func (a *App) InstallDesktopApp() {
	logUI("安装官方桌面端", "设置页手动触发")
	startDesktopInstallFlow()
}

// runDesktopAppUpdate 下载安装包（进度走 splash，可取消）→ 校验 → 启动安装向导。
// harnessOpBusy 与自身更新/harness 更新一致地在本流程内自持（与插件批处理互斥）。
func runDesktopAppUpdate(info desktopAppInfo, feed desktopFeed, installerURL string, hasSum bool) {
	splash := startSplash(T("正在下载桌面端安装包…"))
	harnessOpBusy.Store(true)
	defer harnessOpBusy.Store(false)

	ctx, cancel := context.WithCancel(context.Background())
	registerActiveUpdate(cancel)
	defer registerActiveUpdate(nil)

	dest, err := desktopInstallerPath(installerURL)
	if err != nil {
		finishHarnessUpdate(splash, T("无法准备安装包存放目录：")+"\n"+err.Error(), false, "")
		return
	}
	log.Printf("desktop update: downloading %s -> %s", installerURL, dest)
	if err := downloadFileWithProgress(ctx, installerURL, dest, func(pct float64) {
		emitSplash("", pct)
	}); err != nil {
		if ctx.Err() != nil {
			// 用户取消：与自身更新一致——关进度视图、清忙标记、按「已取消」收尾（不弹错误提示）
			splash.Close()
			harnessOpBusy.Store(false)
			emitUpdateDone(false, true, "")
			return
		}
		finishHarnessUpdate(splash, T("下载桌面端安装包失败：")+"\n"+err.Error(), false, "")
		return
	}

	if hasSum {
		if err := verifyFileSHA512(dest, feed.SHA512); err != nil {
			_ = os.Remove(dest) // 校验失败不留残包，避免用户误点
			finishHarnessUpdate(splash, T("桌面端安装包校验失败：")+"\n"+err.Error(), false, "")
			return
		}
	} else {
		log.Printf("desktop update: 清单未提供校验和，跳过校验（%s）", filepath.Base(dest))
	}

	// 桌面端在运行会阻止安装（Windows 安装程序要求先退出；macOS 替换 .app 同样需要退出）。
	// 强制结束可能中断用户正在跑的任务，因此只提示，由用户在桌面端托盘里自行退出。
	running := desktopAppProcessRunning(info)
	if running && !askLaunchDesktopInstaller(true) {
		finishHarnessUpdate(splash, TF(T("安装包已下载：\n%s\n\n桌面端仍在运行，未启动安装程序。"), dest), true, "")
		return
	}
	emitSplash(T("正在启动安装程序…"), 1)
	if err := launchInstallerFile(dest); err != nil {
		finishHarnessUpdate(splash, T("启动安装程序失败：")+"\n"+err.Error(), false, "")
		return
	}
	invalidateDesktopAppCache()
	finishHarnessUpdate(splash, TF(T("安装包已下载并启动安装程序：\n%s"), dest), true, "")
}

// ==================== Wails Bindings ====================

// GetDesktopApp 返回官方桌面端的安装/运行状态（设置页「常规」的桌面端卡片）。
func (a *App) GetDesktopApp() DesktopAppInfo {
	info := desktopApp()
	return DesktopAppInfo{
		Installed: info.Installed,
		Version:   info.Version,
		Path:      sanitizeShotPath(info.DisplayPath),
		Running:   info.Installed && desktopAppProcessRunning(info),
		Channel:   info.Channel,
		FeedURL:   info.FeedURL,
	}
}

// LaunchDesktopApp 打开官方桌面端：未运行则拉起，已运行则把它唤到前台
// （桌面端是单实例应用，重复启动会显示既有窗口）。
func (a *App) LaunchDesktopApp() {
	info := desktopApp()
	if !info.Installed {
		showMessageBox("未检测到官方桌面端。", appName)
		return
	}
	logUI("打开桌面端", orDash(info.Version))
	if err := launchDesktopApp(info); err != nil {
		showMessageBox("打开桌面端失败：\n"+err.Error(), appName)
	}
}

// CheckDesktopUpdate 检查官方桌面端是否有新版本（设置页「关于」的检查按钮在 desktop
// 启动方式下走这里；不弹原生对话框）。
func (a *App) CheckDesktopUpdate() ModuleUpdate {
	info := checkDesktopUpdate()
	logUI("检查桌面端更新", fmt.Sprintf("当前 %s | %s", orDash(info.Current), moduleUpdateLogText(&info)))
	return info
}

// OpenDefaultUI 按当前启动方式打开界面（设置页与托盘共用）：
// desktop → 官方桌面端；web → 带最新 token 的 Web UI。
func (a *App) OpenDefaultUI() {
	openDefaultUI()
}

// openDefaultUI 启动方式分派实现（托盘与 Wails 绑定共用，无需 appCtx）。
func openDefaultUI() {
	if launchTargetIsDesktop() {
		info := desktopApp()
		if info.Installed {
			log.Printf("[open] desktop ui (%s)", info.Version)
			if err := launchDesktopApp(info); err != nil {
				log.Printf("open desktop app failed: %v", err)
			}
			return
		}
		// 桌面端在此期间被卸载/检测失效：回退 Web UI，避免「打开」变成无操作
		log.Printf("[open] desktop target unavailable, falling back to web ui")
	}
	if running, _, _ := resolveRunningService(); running {
		openBrowser(webTokenURL()) // 带最新 token，避免重启后旧 token 失效
	}
}

// desktopVersionForDisplay 当前启动方式下的 harness 版本号（设置页「关于」版本行）：
// desktop 形态下 harness 随桌面端内置、版本与桌面壳一致（官方发布规则），因此取桌面端版本。
func desktopVersionForDisplay() string {
	info := desktopApp()
	if !info.Installed {
		return ""
	}
	if shotMode && info.Version == "" {
		return "0.1.1" // 截图模式：与 harness 行一致的中性演示版本
	}
	return info.Version
}

// desktopAppVersionRegexp 从卸载登记 DisplayName（"DeepSeek Harness 0.1.7-rc.2"）里取版本。
var desktopAppVersionRegexp = regexp.MustCompile(`\d+\.\d+\.\d+[0-9A-Za-z.\-]*`)

// versionFromDesktopDisplayName 从产品名里提取版本号（DisplayVersion 缺失时的兜底）。
func versionFromDesktopDisplayName(display string) string {
	if m := desktopAppVersionRegexp.FindString(display); m != "" {
		return m
	}
	return ""
}

// isDesktopDisplayName 卸载登记/应用包名是否属于官方桌面端（排除本程序与其它产品）。
func isDesktopDisplayName(display string) bool {
	d := strings.TrimSpace(display)
	if d == "" {
		return false
	}
	if strings.Contains(strings.ToLower(d), "systray") {
		return false
	}
	return strings.HasPrefix(d, desktopProductName)
}

// parsePlistStringValue 从 XML plist 文本取指定 key 的字符串值（<key>K</key><string>V</string>）。
// 二进制 plist 解析不到内容，返回空由 macOS 侧调用方回退 plutil。
func parsePlistStringValue(plist, key string) string {
	needle := "<key>" + key + "</key>"
	i := strings.Index(plist, needle)
	if i < 0 {
		return ""
	}
	rest := plist[i+len(needle):]
	open := strings.Index(rest, "<string>")
	if open < 0 {
		return ""
	}
	rest = rest[open+len("<string>"):]
	end := strings.Index(rest, "</string>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// desktopExeFromIcon 解析 Windows 卸载登记的 DisplayIcon（形如
// `D:\...\DeepSeek Harness.exe,0`）为可执行文件路径；逗号后是图标索引，
// 路径本身含反斜杠，故只在逗号后缀不含分隔符时截断。
func desktopExeFromIcon(icon string) string {
	s := strings.TrimSpace(icon)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, ","); i > 0 && !strings.ContainsAny(s[i:], `\/`) {
		s = s[:i]
	}
	s = strings.Trim(strings.TrimSpace(s), `"`)
	if strings.EqualFold(filepath.Ext(s), ".exe") {
		return s
	}
	return ""
}

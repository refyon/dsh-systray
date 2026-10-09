// desktop_reset.go：官方桌面端（Desktop UI 启动方式）的「重置桌面端」。
//
// 与 web 的「重置服务」（harness_reset.go）语义对齐：重装是必选项，会话记录 / 已安装插件按勾选
// 清除，确认弹窗、进度视图与结果提示沿用同一套前端。差别只有两处（2026-10-09 用户约定）：
//   - 重装目标来自桌面端版本列表（官方更新源当前版 + 本机已有安装包 + 用户选择的本地安装包），
//     不是 npm 上的 harness 版本列表；
//   - 执行方式：harness 随桌面端安装包内置，托盘无法清空目录静默重装，只能准备好官方安装包并交给
//     官方安装向导（安装位置、卸载登记与替换时机由安装程序负责，与「更新桌面端」同一链路）。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	// desktopInstallerPrefix 官方安装包文件名前缀（electron-builder 的 artifactName 约定）。
	desktopInstallerPrefix = "deepseek-harness-"
	// desktopBinFallbackBase 安装包目录兜底基址（读不到更新源地址时按平台拼目标目录）。
	desktopBinFallbackBase = "https://download.deepseek.com/dsh-desk/bin/"
)

// desktopSilentInstallTimeout 静默覆盖安装的等待上限（解压几百 MB，给足余量）。
const desktopSilentInstallTimeout = 10 * time.Minute

// desktopInstallVerifyTimeout 覆盖安装后的版本验收等待上限（安装器收尾与注册表刷新要一点时间）。
const desktopInstallVerifyTimeout = 30 * time.Second

// desktopInstallerMinSize 本机安装包的最小可信体积：官方包约 275–290MB，明显更小说明下载未完成
// 或文件被截断——拿它安装必然失败，宁可提前按「安装包异常」提示。（var 而非 const：单测据此调小。）
var desktopInstallerMinSize int64 = 50 << 20

// ==================== 安装包命名与地址 ====================

// desktopReleaseTarget 当前平台的官方发布目标目录名（与 defaultDesktopFeedBaseFor 同口径：
// 官方桌面端只发布 win-x64 / mac-x64 / mac-arm64，其它平台按 win-x64 处理不影响使用）。
func desktopReleaseTarget(goos, goarch string) string {
	if goos == "darwin" {
		if goarch == "amd64" {
			return "mac-x64"
		}
		return "mac-arm64"
	}
	return "win-x64"
}

// desktopInstallerExt 平台安装包扩展名：Windows 交给 NSIS exe；macOS 用 DMG（与更新链路同口径）。
func desktopInstallerExt(goos string) string {
	if goos == "darwin" {
		return ".dmg"
	}
	return ".exe"
}

// desktopInstallerFileName 官方安装包文件名：deepseek-harness-<版本>-<目标平台><扩展名>。
func desktopInstallerFileName(version, goos, goarch string) string {
	target := desktopReleaseTarget(goos, goarch)
	return desktopInstallerPrefix + strings.TrimSpace(version) + "-" + target + desktopInstallerExt(goos)
}

// parseDesktopInstallerName 解析官方安装包文件名 → (版本, 是否合法)。命名不符合约定、平台不是本机
// 目标平台、或版本段非法时返回 false（「选择本地安装包」据此给出异常提示）。
func parseDesktopInstallerName(base, goos, goarch string) (string, bool) {
	name := filepath.Base(strings.TrimSpace(base))
	ext := desktopInstallerExt(goos)
	if len(name) <= len(ext) || !strings.EqualFold(name[len(name)-len(ext):], ext) {
		return "", false
	}
	name = name[:len(name)-len(ext)]
	if !strings.HasPrefix(name, desktopInstallerPrefix) {
		return "", false
	}
	rest := name[len(desktopInstallerPrefix):]
	suffix := "-" + desktopReleaseTarget(goos, goarch)
	if !strings.HasSuffix(rest, suffix) {
		return "", false
	}
	version := strings.TrimSuffix(rest, suffix)
	if !validResetTarget(version) {
		return "", false
	}
	return version, true
}

// desktopBinDirFor 安装包目录：优先由更新源地址（.../feeds/<目标平台>/）推出 .../bin/<目标平台>/，
// 推不出或推出来的不是官方域名时用兜底基址。
func desktopBinDirFor(feedURL, goos, goarch string) string {
	target := desktopReleaseTarget(goos, goarch)
	if i := strings.Index(strings.TrimSpace(feedURL), "/feeds/"); i > 0 {
		base := feedURL[:i] + "/bin/" + target + "/"
		if isDesktopUpdateAssetURL(base) {
			return base
		}
	}
	return desktopBinFallbackBase + target + "/"
}

// desktopInstallerURLFor 按官方命名规则拼出某个版本的安装包直链（官方源保留历史 rc 包）。
func desktopInstallerURLFor(feedURL, version, goos, goarch string) string {
	return desktopBinDirFor(feedURL, goos, goarch) + desktopInstallerFileName(version, goos, goarch)
}

// ==================== 本机已有的安装包 ====================

// ResetLocalInstaller 本机已有（或用户选择）的官方桌面端安装包：重置可直接用它重装，无需下载。
type ResetLocalInstaller struct {
	Version  string `json:"version"`  // 文件名解析出的版本（非法命名不会进入本结构）
	Path     string `json:"path"`     // 绝对路径
	FileName string `json:"fileName"` // 文件名（界面展示）
	Size     int64  `json:"size"`     // 字节数
	Source   string `json:"source"`   // downloads（托盘下载目录）| cache（桌面端更新缓存）| picked（用户选择）
}

// formatInstallerSize 安装包体积的人读文案。
func formatInstallerSize(n int64) string {
	switch {
	case n <= 0:
		return "0"
	case n < 1<<20:
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
}

// desktopUpdaterCacheDir 官方桌面端 electron-updater 的下载缓存目录（读不到缓存名返回空）：
// Windows 在 %LOCALAPPDATA%、macOS 在 ~/Library/Caches、其余按 ~/.cache 处理。
func desktopUpdaterCacheDir(cacheName string) string {
	cacheName = strings.TrimSpace(cacheName)
	if cacheName == "" {
		return ""
	}
	switch runtime.GOOS {
	case "windows":
		if base := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); base != "" {
			return filepath.Join(base, cacheName)
		}
		return ""
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, "Library", "Caches", cacheName)
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, ".cache", cacheName)
	}
}

// installersInDir 扫描单个目录里符合当前平台命名的官方安装包（不递归；只认体积达标的文件）。
func installersInDir(dir, source string) []ResetLocalInstaller {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []ResetLocalInstaller
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		version, ok := parseDesktopInstallerName(e.Name(), runtime.GOOS, runtime.GOARCH)
		if !ok {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.Size() < desktopInstallerMinSize {
			continue
		}
		out = append(out, ResetLocalInstaller{
			Version:  version,
			Path:     filepath.Join(dir, e.Name()),
			FileName: e.Name(),
			Size:     info.Size(),
			Source:   source,
		})
	}
	return out
}

// scanLocalDesktopInstallers 扫描本机已有的官方桌面端安装包：托盘下载目录 + 桌面端更新缓存目录。
func scanLocalDesktopInstallers() []ResetLocalInstaller {
	out := installersInDir(downloadsDir(), "downloads")
	cacheName := readDesktopUpdateConfig(desktopApp().ResourcesDir).CacheDir
	if dir := desktopUpdaterCacheDir(cacheName); dir != "" {
		out = append(out, installersInDir(dir, "cache")...)
	}
	return out
}

// validateDesktopInstaller 校验本机安装包（结构性校验，不读全文件）：存在、命名符合官方约定、
// 平台匹配、体积达标。错误文案面向用户，直接进「安装包异常」提示。
func validateDesktopInstaller(path, source string) (ResetLocalInstaller, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return ResetLocalInstaller{}, errors.New("未选择安装包")
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return ResetLocalInstaller{}, errors.New("文件不存在或不可读")
	}
	version, ok := parseDesktopInstallerName(filepath.Base(p), runtime.GOOS, runtime.GOARCH)
	if !ok {
		return ResetLocalInstaller{}, fmt.Errorf("文件名不是官方桌面端安装包命名（应形如 %s）",
			desktopInstallerFileName("<版本>", runtime.GOOS, runtime.GOARCH))
	}
	if st.Size() < desktopInstallerMinSize {
		return ResetLocalInstaller{}, fmt.Errorf("文件体积仅 %s，下载可能未完成", formatInstallerSize(st.Size()))
	}
	return ResetLocalInstaller{
		Version:  version,
		Path:     p,
		FileName: filepath.Base(p),
		Size:     st.Size(),
		Source:   source,
	}, nil
}

// ==================== 重置目标候选 ====================

// buildDesktopResetOptions 构建桌面端重置目标候选：官方更新源当前版本 + 本机已装版本 + 本机已有
// 安装包的版本，去重后按新→旧排序；本机已有安装包的版本带上 localPath（选中即无需下载）。
// 默认选中本机已装版本（同版本重装，与 web 同口径）；它不在候选里时取最新版本。
func buildDesktopResetOptions(feedVersion, current string, locals []ResetLocalInstaller) (opts []ResetVersionOption, def string) {
	best := map[string]ResetLocalInstaller{}
	for _, l := range locals {
		if l.Version == "" {
			continue
		}
		if prev, ok := best[l.Version]; !ok || l.Size > prev.Size {
			best[l.Version] = l
		}
	}
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		opt := ResetVersionOption{Version: v}
		if l, ok := best[v]; ok {
			opt.LocalPath, opt.Size = l.Path, l.Size
		}
		opts = append(opts, opt)
	}
	add(feedVersion)
	add(current)
	for v := range best {
		add(v)
	}
	sort.Slice(opts, func(i, j int) bool { return compareVersions(opts[i].Version, opts[j].Version) > 0 })
	if current != "" && seen[current] {
		def = current
	}
	if def == "" && len(opts) > 0 {
		def = opts[0].Version
	}
	return opts, def
}

// desktopInstallPath 已安装桌面端的落地路径：Windows = 安装目录，macOS = .app 包路径
// （安装登记里没给时按主程序路径反推；两者都拿不到返回空 = 无法覆盖，只能走安装向导）。
func desktopInstallPath(info desktopAppInfo) string {
	if p := strings.TrimSpace(info.DisplayPath); p != "" {
		return p
	}
	exe := strings.TrimSpace(info.Exe)
	if exe == "" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		for p := exe; ; {
			parent := filepath.Dir(p)
			if parent == p || parent == "." || parent == string(filepath.Separator) {
				return ""
			}
			if strings.HasSuffix(parent, ".app") {
				return parent
			}
			p = parent
		}
	}
	return filepath.Dir(exe)
}

// desktopSilentInstallArgs 覆盖安装参数：与桌面端**自带的更新器**（electron-updater 的
// NsisUpdater.doInstall）完全一致 ——
//
//		["--updated", "/S", "/D=<安装目录>"]
//
//	  - `--updated`：按「更新」而非首次安装处理（保留用户数据、跳过向导页；安装器据此得知不必
//	    再检查/提示退出正在运行的应用）；
//	  - `/S`：静默；
//	  - `/D=<目录>`：必须是**最后一个参数**且不加引号（NSIS 按原始命令行解析 /D= 之后的全部内容；
//	    electron-builder 的模板自己解析该开关，见 multiUser.nsh 的 GetDParameter 宏）。
func desktopSilentInstallArgs(dir string) []string {
	return []string{"--updated", "/S", "/D=" + strings.TrimSpace(dir)}
}

// versionCore 版本号的数值核心（去掉 -rc.1 / +build 之类后缀）。
func versionCore(v string) string {
	if i := strings.IndexAny(v, "-+"); i > 0 {
		return v[:i]
	}
	return v
}

// desktopInstalledVersionMatches 安装后的版本是否达到本次目标：
// Windows 的卸载登记 DisplayVersion 与安装包版本同源（含 -rc.N），按全量比较；
// macOS 的 CFBundleShortVersionString 不带预发布后缀，只比较数值核心，避免误判成失败。
func desktopInstalledVersionMatches(installed, want string) bool {
	a, b := normalizeVersionText(installed), normalizeVersionText(want)
	if a == b {
		return true
	}
	if runtime.GOOS != "darwin" {
		return false
	}
	return versionCore(a) != "" && versionCore(a) == versionCore(b)
}

// desktopResetInstallMode 本次重置的落地方式。
type desktopResetInstallMode int

const (
	// desktopResetInstallWizard 未安装（或原路径未知）：交给官方安装向导。
	desktopResetInstallWizard desktopResetInstallMode = iota
	// desktopResetInstallInPlace 已安装且原路径已知：静默覆盖原路径（不再从头走安装向导）。
	desktopResetInstallInPlace
)

// desktopResetModeFor 依据安装事实决定落地方式（用户约定 2026-10-09：装过的设备直接覆盖原路径）。
func desktopResetModeFor(info desktopAppInfo) (desktopResetInstallMode, string) {
	if !info.Installed {
		return desktopResetInstallWizard, ""
	}
	dir := desktopInstallPath(info)
	if dir == "" {
		return desktopResetInstallWizard, ""
	}
	return desktopResetInstallInPlace, dir
}

// waitDesktopInstalledVersion 覆盖安装后的验收：轮询安装登记里的版本直到达到目标（超时报错，
// 由调用方如实告知用户，而不是把没生效的安装报成成功）。
func waitDesktopInstalledVersion(want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		invalidateDesktopAppCache()
		cur := desktopApp()
		if desktopInstalledVersionMatches(cur.Version, want) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("安装后版本为 %s，未达到目标 %s", orDash(cur.Version), withV(want))
		}
		time.Sleep(time.Second)
	}
}

// ==================== 安装包准备 ====================

// desktopResetInstaller 本次重置要交给安装向导的安装包。
type desktopResetInstaller struct {
	ResetLocalInstaller
	Downloaded bool // 是否本次下载（结果文案据此区分「本机已有」与「已下载」）
}

// resolveDesktopResetInstaller 解析本次重置使用的安装包：
//  1. 用户选择的本地安装包（localInstaller 非空）→ 结构校验后直接使用；
//  2. 否则目标版本**一律从官方源重新下载**（同名文件已存在也覆盖）：重置要保证用户「有机会重新
//     下载」——上一轮的本地包可能已损坏或过期，不能默默复用（用户约定 2026-10-09）；想直接复用
//     本机文件就用「选择本地安装包」显式指定（也是本机安装包列表标记的实际用法）。
//     404 说明该版本已从官方源下架，提示改选其它版本或指定本机安装包。
//
// 目标版本为空时取本机已装版本（同版本重装，与 web 的默认语义一致）。
func resolveDesktopResetInstaller(info desktopAppInfo, targetVersion, localInstaller string, splash *SplashState) (desktopResetInstaller, error) {
	if p := strings.TrimSpace(localInstaller); p != "" {
		ins, err := validateDesktopInstaller(p, "picked")
		if err != nil {
			return desktopResetInstaller{}, fmt.Errorf("所选安装包不可用：%v", err)
		}
		return desktopResetInstaller{ResetLocalInstaller: ins}, nil
	}
	target := strings.TrimSpace(targetVersion)
	if target == "" {
		target = strings.TrimSpace(info.Version)
	}
	if target == "" {
		return desktopResetInstaller{}, errors.New("未确定重装目标版本，请重新打开弹窗选择")
	}
	if !validResetTarget(target) {
		return desktopResetInstaller{}, fmt.Errorf("目标版本 %q 格式非法，请重新打开弹窗选择", target)
	}

	// 官方源下载：版本与清单一致时用清单里的直链（含 macOS 的 dmg 推导），否则按命名规则拼。
	feed, feedErr := fetchDesktopFeed(info.FeedURL)
	installerURL := desktopInstallerURLFor(info.FeedURL, target, runtime.GOOS, runtime.GOARCH)
	hasSum := false
	if feedErr == nil && strings.TrimSpace(feed.Version) == target {
		if u, ok := desktopInstallerURL(feed, runtime.GOOS); ok && u != "" {
			installerURL, hasSum = u, ok
		}
	}
	if !isDesktopUpdateAssetURL(installerURL) {
		return desktopResetInstaller{}, errors.New("安装包地址不在官方下载域名内，已中止")
	}
	dest, err := desktopInstallerPathFn(installerURL)
	if err != nil {
		return desktopResetInstaller{}, fmt.Errorf("无法准备安装包存放目录：%v", err)
	}
	splash.Update(fmt.Sprintf(T("正在下载桌面端安装包 %s…"), withV(target)), 0.3)
	if derr := downloadFileWithProgressFn(context.Background(), installerURL, dest, func(pct float64) {
		splash.Update(fmt.Sprintf(T("正在下载桌面端安装包 %s…"), withV(target)), 0.3+0.4*pct)
	}); derr != nil {
		if strings.Contains(derr.Error(), "404") {
			return desktopResetInstaller{}, fmt.Errorf("官方更新源没有 %s 的安装包（该版本可能已下架），请改选其它版本或用「选择本地安装包」指定本机已有的安装包", withV(target))
		}
		return desktopResetInstaller{}, fmt.Errorf("下载桌面端安装包失败：%v", derr)
	}
	ins, verr := validateDesktopInstaller(dest, "downloads")
	if verr != nil {
		return desktopResetInstaller{}, verr
	}
	// 校验和只有官方清单给出的那一个版本有（历史版本没有清单），有则必须校验。
	if hasSum {
		if serr := verifyFileSHA512(dest, feed.SHA512); serr != nil {
			_ = os.Remove(dest) // 校验失败不留残包，避免用户误点
			return desktopResetInstaller{}, serr
		}
	} else {
		log.Printf("desktop reset: 官方清单未提供该版本校验和，跳过校验（%s）", ins.FileName)
	}
	return desktopResetInstaller{ResetLocalInstaller: ins, Downloaded: true}, nil
}

// ==================== 执行流程 ====================

// ResetDesktopApp 重置官方桌面端（desktop 启动方式下的「重置桌面端」按钮）：
// 清除所选数据 + 准备所选版本的官方安装包 + 启动官方安装向导。
// targetVersion 为「重置目标版本」下拉的值；localInstaller 非空表示用户用「选择本地安装包」
// 指定了本机安装包（两者同时存在时以本地安装包为准）。
func (a *App) ResetDesktopApp(clearSessions, clearPlugins bool, targetVersion, localInstaller string) {
	if !launchTargetIsDesktop() {
		// 防御性拦截：该入口只属于 desktop 形态（web 形态用重置服务，装的是 npm 版 harness）
		showMessageBox(T("当前启动方式为 Web UI，请使用「重置服务」。"), appName)
		return
	}
	if pluginBatchRunning() {
		showMessageBox(T("正在批量处理插件（更新/删除），请等待完成后再重置桌面端。"), appName)
		return
	}
	logUI("重置桌面端", fmt.Sprintf("clearSessions=%v clearPlugins=%v target=%s local=%s",
		clearSessions, clearPlugins, orDash(targetVersion), orDash(filepath.Base(localInstaller))))
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
	}
	go runDesktopResetFlow(clearSessions, clearPlugins, targetVersion, localInstaller, func(msg string) {
		showMessageBox(msg, appName)
	})
}

// PickDesktopInstaller 让用户选择本机已有的官方桌面端安装包（重置弹窗的「选择本地安装包」）：
// 只做结构性校验（命名 / 平台 / 体积），失败的说明交给前端以小字提示。
func (a *App) PickDesktopInstaller() DesktopInstallerPick {
	if appCtx == nil {
		return DesktopInstallerPick{Error: "窗口尚未就绪"}
	}
	if !launchTargetIsDesktop() {
		return DesktopInstallerPick{Error: "当前启动方式为 Web UI"}
	}
	filters := []wruntime.FileFilter{
		{DisplayName: fmt.Sprintf(T("官方桌面端安装包 (*%s)"), desktopInstallerExt(runtime.GOOS)), Pattern: "*" + desktopInstallerExt(runtime.GOOS)},
		{DisplayName: T("所有文件 (*.*)"), Pattern: "*.*"},
	}
	p, err := wruntime.OpenFileDialog(appCtx, wruntime.OpenDialogOptions{
		Title:   T("选择官方桌面端安装包"),
		Filters: filters,
	})
	if err != nil {
		return DesktopInstallerPick{Error: err.Error()}
	}
	if strings.TrimSpace(p) == "" {
		return DesktopInstallerPick{Canceled: true}
	}
	ins, verr := validateDesktopInstaller(p, "picked")
	if verr != nil {
		logInfo("desktop", "重置桌面端：所选安装包不可用（%s）：%v", p, verr)
		return DesktopInstallerPick{Error: verr.Error()}
	}
	logUI("选择桌面端安装包", fmt.Sprintf("%s（%s）", ins.FileName, withV(ins.Version)))
	return DesktopInstallerPick{
		OK:       true,
		Path:     ins.Path,
		FileName: ins.FileName,
		Version:  ins.Version,
		Size:     ins.Size,
	}
}

// DesktopInstallerPick 本机安装包选择结果（前端据此显示已选包或「安装包异常」小字提示）。
type DesktopInstallerPick struct {
	Canceled bool   `json:"canceled"` // 用户取消选择（不显示提示）
	OK       bool   `json:"ok"`
	Path     string `json:"path"`
	FileName string `json:"fileName"`
	Version  string `json:"version"`
	Size     int64  `json:"size"`
	Error    string `json:"error"` // 无效原因（前端提示：安装包异常…建议从列表选择重新下载）
}

// desktopKillWaitTimeout 结束桌面端进程后的等待上限（Electron 退出要收尾子进程，给足余量）。
const desktopKillWaitTimeout = 10 * time.Second

// desktopKillGraceTimeout 优雅退出（macOS 走 AppleScript quit）的等待上限，超时再强杀。
const desktopKillGraceTimeout = 5 * time.Second

// 可替换实现（单测用：避免真实结束进程、跑安装器、启动向导、联网下载）。
var (
	killDesktopAppProcessesFn     = killDesktopAppProcesses
	runDesktopInstallerSilentFn   = runDesktopInstallerSilent
	waitDesktopInstalledVersionFn = waitDesktopInstalledVersion
	launchInstallerFileFn         = launchInstallerFile
	desktopInstallerPathFn        = desktopInstallerPath
	downloadFileWithProgressFn    = downloadFileWithProgress
)

// desktopAppProcessRunningFn 桌面端进程存活判定（测试可替换，避免依赖真实进程）。
var desktopAppProcessRunningFn = desktopAppProcessRunning

// waitDesktopAppStopped 轮询等待桌面端退出；超时返回错误（调用方据此中止并提示手动处理）。
func waitDesktopAppStopped(info desktopAppInfo, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !desktopAppProcessRunningFn(info) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("官方桌面端仍在运行")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// stopDesktopAppForReset 重置前结束正在运行的桌面端（清数据不被文件占用、安装程序也要求它退出）：
// 托盘主动结束（用户约定 2026-10-09）。返回是否确实结束过一个运行中的实例；
// 失败时返回原因，由调用方中止本次重置。
func stopDesktopAppForReset(info desktopAppInfo) (bool, error) {
	if !desktopAppProcessRunningFn(info) {
		return false, nil
	}
	if err := killDesktopAppProcessesFn(info); err != nil {
		log.Printf("desktop reset: kill desktop app failed: %v", err)
	}
	if err := waitDesktopAppStopped(info, desktopKillWaitTimeout); err != nil {
		return true, err
	}
	return true, nil
}

// runDesktopResetFlow 桌面端重置主流程：准备安装包 → 结束运行中的桌面端 →（按需）清会话/清插件 →
// 覆盖安装到原路径（已安装）或启动官方安装向导（未安装）。
//
// popup 为弹窗通道（常规入口传 showMessageBox；nil = 静默，仅返回结果）。
func runDesktopResetFlow(clearSessions, clearPlugins bool, targetVersion, localInstaller string, popup func(string)) resetResult {
	splash, ownSplash := resetFlowSplashTitled(popup == nil, T("正在重置官方桌面端…"))
	if ownSplash {
		defer splash.Close()
	}
	prevBusy := harnessOpBusy.Swap(true)
	defer harnessOpBusy.Store(prevBusy)

	info := desktopApp()
	splash.Update(T("正在准备桌面端安装包…"), 0.05)
	pkg, err := resolveDesktopResetInstaller(info, targetVersion, localInstaller, splash)
	if err != nil {
		return failDesktopResetFlow(splash, T("重置桌面端失败：")+"\n"+err.Error()+"\n\n日志："+unifiedLogPath(), popup)
	}
	log.Printf("desktop reset: installer=%s version=%s size=%s downloaded=%v clearSessions=%v clearPlugins=%v",
		pkg.Path, orDash(pkg.Version), formatInstallerSize(pkg.Size), pkg.Downloaded, clearSessions, clearPlugins)

	// 结束运行中的桌面端：否则清数据会撞文件占用（只清一半），安装程序也会拒绝安装。
	killedNote := ""
	if desktopAppProcessRunningFn(info) {
		splash.Update(T("正在结束正在运行的官方桌面端…"), 0.7)
	}
	killed, serr := stopDesktopAppForReset(info)
	if serr != nil {
		return failDesktopResetFlow(splash, T("重置桌面端失败：")+"\n"+
			T("未能结束正在运行的官方桌面端，请手动退出后重试。")+"\n\n日志："+unifiedLogPath(), popup)
	}
	if killed {
		killedNote = T("\n· 已结束正在运行的官方桌面端")
	}

	// 可选清理：与 web 重置同口径（失败不中止整次重置，只记入收尾说明）
	cleanupNotes := ""
	if clearSessions {
		splash.Update(T("正在清除会话记录…"), 0.78)
		if cerr := removeSessions(); cerr != nil {
			log.Printf("desktop reset: clear sessions failed: %v", cerr)
			cleanupNotes += T("\n· 会话记录清理失败：") + cerr.Error()
		}
	}
	if clearPlugins {
		splash.Update(T("正在清除已安装的插件…"), 0.85)
		if _, cerr := removeInstalledPlugins(); cerr != nil {
			log.Printf("desktop reset: clear plugins failed: %v", cerr)
			cleanupNotes += T("\n· 已安装插件清理失败：") + cerr.Error()
		}
	}

	// 下载/清理期间用户可能又把桌面端打开了：安装程序要求它退出，这里再结束一次。
	if _, serr := stopDesktopAppForReset(info); serr != nil {
		log.Printf("desktop reset: desktop app still running before install: %v", serr)
	}

	// 落地方式：装过的设备直接把所选版本覆盖到**原安装路径**（静默，不再从头走安装向导）；
	// 未安装（或原路径读不到）时交给官方安装向导（安装位置、卸载登记由安装程序负责）。
	mode, installPath := desktopResetModeFor(info)
	if mode == desktopResetInstallInPlace {
		splash.Update(fmt.Sprintf(T("正在覆盖安装到原路径 %s…"), installPath), 0.95)
		if ierr := runDesktopInstallerSilentFn(pkg.Path, installPath, desktopSilentInstallTimeout); ierr != nil {
			return failDesktopResetFlow(splash, T("重置桌面端失败：")+"\n"+
				fmt.Sprintf(T("覆盖安装未完成：%v"), ierr)+"\n\n"+
				fmt.Sprintf(T("可手动运行安装包完成重装：\n%s"), pkg.Path)+"\n\n日志："+unifiedLogPath(), popup)
		}
		invalidateDesktopAppCache()
		if verr := waitDesktopInstalledVersionFn(pkg.Version, desktopInstallVerifyTimeout); verr != nil {
			return failDesktopResetFlow(splash, T("重置桌面端失败：")+"\n"+
				fmt.Sprintf(T("覆盖安装后的版本校验未通过：%v"), verr)+"\n\n"+
				fmt.Sprintf(T("可手动运行安装包完成重装：\n%s"), pkg.Path)+"\n\n日志："+unifiedLogPath(), popup)
		}
		// 覆盖安装后把先前结束的桌面端拉起来（与 web 重置「重置后服务已重启」同语义）
		if killed {
			if lerr := launchDesktopApp(desktopApp()); lerr != nil {
				log.Printf("desktop reset: relaunch desktop app failed: %v", lerr)
			} else {
				killedNote = T("\n· 已结束正在运行的官方桌面端（重装后已重新启动）")
			}
		}
		splash.Close()
		source := T("本机已有安装包")
		if pkg.Downloaded {
			source = T("本次下载")
		}
		if pkg.Source == "picked" {
			source = T("你选择的本地安装包")
		}
		detail := T("官方桌面端已重装完成：\n") +
			"· " + fmt.Sprintf(T("目标版本：%s"), withV(pkg.Version)) + "\n" +
			"· " + fmt.Sprintf(T("已覆盖安装到原路径：%s"), installPath) + "\n" +
			"· " + fmt.Sprintf(T("安装包（%s）：%s"), source, pkg.FileName) + "\n"
		if clearSessions {
			detail += "· " + T("会话记录已清除") + "\n"
		}
		if clearPlugins {
			detail += "· " + T("已安装插件已清除") + "\n"
		}
		detail += killedNote
		logUI("重置桌面端完成", fmt.Sprintf("v%s | 覆盖安装到 %s | 安装包 %s（%s）", pkg.Version, installPath, pkg.FileName, source))
		// 重置（可能清了插件）后同步状态立刻按本机现状刷新；桌面端重置不改 harness 版本，
		// 传当前版本即「版本无变化」→ 只做状态刷新（与 web 重置同一收尾口径）。
		refreshSyncAfterReset(clearPlugins, installedHarnessVersion())
		if popup != nil {
			popup(detail + cleanupNotes)
		}
		return resetResult{OK: true, Note: detail + cleanupNotes}
	}

	splash.Update(T("正在启动安装程序…"), 0.95)
	if lerr := launchInstallerFileFn(pkg.Path); lerr != nil {
		return failDesktopResetFlow(splash, T("启动安装程序失败：")+"\n"+lerr.Error()+"\n\n日志："+unifiedLogPath(), popup)
	}
	invalidateDesktopAppCache()
	splash.Close()

	source := T("本机已有安装包")
	if pkg.Downloaded {
		source = T("本次下载")
	}
	if pkg.Source == "picked" {
		source = T("你选择的本地安装包")
	}
	detail := T("官方桌面端已开始重装：\n") +
		"· " + fmt.Sprintf(T("目标版本：%s"), withV(pkg.Version)) + "\n" +
		"· " + fmt.Sprintf(T("安装包（%s）：%s"), source, pkg.FileName) + "\n"
	if clearSessions {
		detail += "· " + T("会话记录已清除") + "\n"
	}
	if clearPlugins {
		detail += "· " + T("已安装插件已清除") + "\n"
	}
	detail += killedNote
	detail += "\n" + T("按安装向导完成安装后，桌面端即为所选版本。")
	logUI("重置桌面端完成", fmt.Sprintf("v%s | 已启动安装向导 | 安装包 %s（%s）", pkg.Version, pkg.FileName, source))
	// 重置（可能清了插件）后同步状态立刻按本机现状刷新；桌面端重置不改 harness 版本，
	// 传当前版本即「版本无变化」→ 只做状态刷新（与 web 重置同一收尾口径）。
	refreshSyncAfterReset(clearPlugins, installedHarnessVersion())
	if popup != nil {
		popup(detail + cleanupNotes)
	}
	return resetResult{OK: true, Note: detail + cleanupNotes}
}

// failDesktopResetFlow 桌面端重置的失败收尾：只收进度视图并提示。
// 与 web 的 failResetFlow 的差别：不尝试拉起托盘自带的后台服务——desktop 形态下它本就不该在运行，
// 静默启动只会多出一个用户没要的服务进程。
func failDesktopResetFlow(splash *SplashState, msg string, popup func(string)) resetResult {
	splash.Close()
	if popup != nil {
		popup(msg)
	}
	return resetResult{Err: msg}
}

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// appVersion 当前程序版本，由构建注入：-X main.appVersion=X.Y.Z（可带 v，运行时统一去掉前导 v）。
// 本地开发 / CI 手动触发（非 tag）构建时为 "dev"，此时跳过自动更新检查。
var appVersion = "dev"

func init() {
	// 归一化版本号：无论构建注入的是 v0.3.1 还是 0.3.1，内部统一不带前导 v（展示时由 withV 补 v）。
	appVersion = strings.TrimPrefix(appVersion, "v")
}

// withV 版本号统一带一个前导 v 用于展示；已带 v 或为 dev/空则不再加，避免出现 vv0.3.1。
func withV(ver string) string {
	ver = strings.TrimPrefix(ver, "v")
	if ver == "" || ver == "dev" {
		return ver
	}
	return "v" + ver
}

const (
	updateRepoOwner      = "refyon"
	updateRepoName       = "dsh-systray"
	updateCheckDelay     = 30 * time.Second // 启动后 30 秒检查新版本
	updateCheckInterval  = 24 * time.Hour   // 之后每 24 小时检查一次
	updateAPITimeout     = 8 * time.Second  // 版本接口单候选超时（直连失败回退镜像）
	updateDLTimeout      = 5 * time.Minute  // 单镜像单次下载上限
	updateMaxBodySize    = 4 << 20          // 版本接口响应上限 4MB
)

// updateMirrors 下载地址前缀：先直连 GitHub，失败再依次回退镜像（国内网络友好）。
// 可在 config.json 的 updateMirror 指定一个可用镜像，会插到最前优先尝试。
var updateMirrors = []string{
	"",
	"https://ghfast.top/",
	"https://ghproxy.net/",
	"https://gh-proxy.com/",
	"https://gh.llkk.cc/",
	"https://github.moeyy.xyz/",
	"https://mirror.ghproxy.com/",
}

// updateMirrorOverride 用户在 config.json 配置的 updateMirror（可空）。
var updateMirrorOverride string

// 进行中更新的取消控制（托盘退出时调用 cancelActiveUpdate 终止下载/安装）。
var (
	updateMu     sync.Mutex
	activeCancel context.CancelFunc
)

// updateCheckWindows 进行中的“检查更新”流程数（手动检查的进度窗口/提示期间计数）。
// 启动 30 秒的自动检查发现新版本时，若已存在检查/更新窗口则不再重复弹窗。
var updateCheckWindows atomic.Int32

func openUpdateCheckFlow()  { updateCheckWindows.Add(1) }
func closeUpdateCheckFlow() { updateCheckWindows.Add(-1) }

// updateFlowBusy 是否有检查/更新窗口正在使用：手动检查流程中，或更新应用进行中（activeCancel 已登记）。
func updateFlowBusy() bool {
	if updateCheckWindows.Load() > 0 {
		return true
	}
	updateMu.Lock()
	defer updateMu.Unlock()
	return activeCancel != nil
}

// 派生子进程登记表：托盘退出时除保留的后台服务外一并终止，避免孤儿进程。
var (
	childProcsMu sync.Mutex
	childProcs   = map[int]*os.Process{}
)

// trackChildProcess 登记一个派生的子进程（退出时统一 Kill）。
func trackChildProcess(p *os.Process) {
	if p == nil {
		return
	}
	childProcsMu.Lock()
	childProcs[p.Pid] = p
	childProcsMu.Unlock()
}

// killChildProcesses 终止所有登记的派生子进程；skipPID 为要保留的进程（如保留的后台服务）。
func killChildProcesses(skipPID int) {
	childProcsMu.Lock()
	defer childProcsMu.Unlock()
	for pid, p := range childProcs {
		if pid == skipPID {
			continue
		}
		_ = p.Kill()
		delete(childProcs, pid)
	}
}

// cancelActiveUpdate 取消正在进行的更新（下载/安装）；无进行中更新则忽略。
func cancelActiveUpdate() {
	updateMu.Lock()
	if activeCancel != nil {
		activeCancel()
	}
	updateMu.Unlock()
}

// registerActiveUpdate 登记/取消登记当前更新取消句柄。
func registerActiveUpdate(cancel context.CancelFunc) {
	updateMu.Lock()
	activeCancel = cancel
	updateMu.Unlock()
}

// progress 安全地调用可选的进度回调（t 为空表示无字面文本更新；p 为 0~1 进度）。
func progress(onStatus func(string, float64), t string, p float64) {
	if onStatus != nil {
		onStatus(t, p)
	}
}

// notoSansSCFamily 首选 UI 字体：Google Noto Sans SC（中英文统一）。系统已装→直接用，
// 未装→依次尝试多个 CDN 下载并注册；全部失败则回退系统默认字体。
const notoSansSCFamily = "Noto Sans SC"

// notoSansSCURLs Noto Sans SC 可变字体（含全部字重）的多个 CDN 候选源，依次尝试直至成功。
// 下载时每个候选还会走 downloadFileWithProgress 的多镜像回退。
var notoSansSCURLs = []string{
	"https://github.com/googlefonts/noto-cjk/raw/main/Sans/Variable/TTF/NotoSansSC%5Bwght%5D.ttf",
	"https://raw.githubusercontent.com/googlefonts/noto-cjk/main/Sans/Variable/TTF/NotoSansSC%5Bwght%5D.ttf",
	"https://cdn.jsdelivr.net/gh/googlefonts/noto-cjk@main/Sans/Variable/TTF/NotoSansSC%5Bwght%5D.ttf",
}

// notoSansSCFontDir 存放已下载字体的目录（用户配置目录下，避免写入系统、无需管理员）。
func notoSansSCFontDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "dsh-systray", "fonts")
}

// downloadNotoSansSC 依次尝试多个 CDN 字体源下载到 dest；成功返回 nil，全部失败返回错误。
func downloadNotoSansSC(dest string, onProgress func(pct float64)) error {
	var lastErr error
	for _, u := range notoSansSCURLs {
		if err := downloadFileWithProgress(context.Background(), u, dest, onProgress); err != nil {
			log.Printf("noto sans source failed: %v (%s); trying next", err, u)
			lastErr = err
			_ = os.Remove(dest) // 清理半成品，避免下一个候选追加
			continue
		}
		return nil
	}
	return lastErr
}

func clearActiveUpdate() {
	updateMu.Lock()
	activeCancel = nil
	updateMu.Unlock()
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type latestRelease struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

// startAutoUpdateCheck 启动后台定时检查：
//  1. 启动后 30 秒检查一次新版本；
//  2. 从启动时间起每 24 小时再检查一次。
// 每次检查若有新版本且当前没有正在使用的检查/更新窗口（updateFlowBusy），才提示用户。
func startAutoUpdateCheck() {
	// 规则1：启动后 30 秒检查一次
	go func() {
		time.Sleep(updateCheckDelay)
		autoCheckUpdate()
	}()
	// 规则2：从启动时间起每 24 小时检查一次
	go func() {
		ticker := time.NewTicker(updateCheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			autoCheckUpdate()
		}
	}()
}

func autoCheckUpdate() {
	if appVersion == "" || appVersion == "dev" {
		return
	}
	rel, err := fetchLatestRelease()
	if err != nil {
		log.Printf("update check failed: %v", err)
		return
	}
	if !isNewerVersion(rel.TagName, appVersion) {
		log.Printf("update check: current %s is up to date (latest %s)", appVersion, rel.TagName)
		return
	}
	log.Printf("update available: %s (current %s)", rel.TagName, appVersion)
	if updateFlowBusy() {
		log.Printf("update check: 已存在检查/更新窗口，跳过自动更新提示")
		return
	}
	if !askUpdateDialog(strings.TrimPrefix(rel.TagName, "v")) {
		log.Printf("user declined update %s", rel.TagName)
		return
	}
	startUpdateApply(rel)
}

// startUpdateApply 应用更新：各平台实现。Windows 派生独立更新进程（退出主程序→进度窗口下载/安装→自动重启）；
// macOS 进程内下载并用辅助脚本替换 .app 后重启。声明于此，由 platform_*.go 实现。

// checkForUpdatesManual 手动检查更新（设置页触发）：点击后立即弹出进度窗口，
// 在窗口下完成 harness + dsh-systray 版本查询；harness 有新版则优先提示先更新 harness。
func checkForUpdatesManual() {
	if appVersion == "" || appVersion == "dev" {
		showMessageBox("当前为开发版本（dev），未启用自动更新。", appName)
		return
	}
	// 全程计数：进度窗口 + 结果提示期间都视为“检查更新窗口在开”，自动检查不再重复弹窗。
	openUpdateCheckFlow()
	defer closeUpdateCheckFlow()
	// 立即弹出进度窗口（不等待查询结果）
	splash := startSplash("正在查询最新版本…")
	splash.Update("正在查询最新版本…", 0.15)

	// 1) 查询 DeepSeek Harness 是否有新版本
	harnessLatest, harnessCur, harnessNewer := queryHarnessUpdate()
	// 2) 查询 dsh-systray 自身最新版本
	rel, err := fetchLatestRelease()
	splash.Close()
	if err != nil {
		showMessageBox("检查更新失败：\n"+err.Error()+"\n\n请检查网络后重试。", appName)
		return
	}
	// 3) harness 有新版本 → 优先提示先更新 harness
	if harnessNewer {
		if askUpdateHarness(harnessLatest, harnessCur) {
			go runHarnessUpdate(harnessLatest)
		}
		return
	}
	// 4) dsh-systray 自身
	if !isNewerVersion(rel.TagName, appVersion) {
		hvText := withV(harnessCur)
		if hvText == "" {
			hvText = "未检测到"
		}
		showMessageBox(fmt.Sprintf("当前已是最新版本（%s）。\n\nDeepSeek Harness 版本：%s", withV(appVersion), hvText), appName)
		return
	}
	if askUpdateDialog(strings.TrimPrefix(rel.TagName, "v")) {
		// 异步执行，避免阻塞设置页 UI 线程（设置窗口在下载期间保持可响应/可关闭）。
		go startUpdateApply(rel)
	}
}

// restartBackgroundService 重启后台 Web 服务：停止 → 拉起 → 等待就绪（带进度窗口）。
// Windows/macOS 共用；startServer/killServer 由各平台实现，其余为主程序通用逻辑。
// onState 可选：进程各阶段回调（"stopping" / "stopped" / "starting" / "running" / "error"），
// 供设置页实时刷新服务状态（如停止后标红“已停止”）。
func restartBackgroundService(onState func(stage string)) {
	splash := startSplash("正在重启后台服务…")
	defer splash.Close()
	splash.Update("正在停止后台服务…", 0.2)
	if onState != nil {
		onState("stopping")
	}
	killServer()
	if onState != nil {
		onState("stopped")
	}
	time.Sleep(1 * time.Second)
	splash.Update("正在启动后台服务…", 0.55)
	if onState != nil {
		onState("starting")
	}
	started, exitCh := startServer()
	if !started {
		if onState != nil {
			onState("error")
		}
		showMessageBox("重启失败：无法启动后台服务。", appName)
		return
	}
	splash.Update("正在等待服务就绪…", 0.85)
	if ok, msg := waitForServerReady(webURL, exitCh, startupTimeout); !ok {
		if onState != nil {
			onState("error")
		}
		showMessageBox("重启失败：\n"+msg, appName)
		return
	}
	if onState != nil {
		onState("running")
	}
}

// queryHarnessUpdate 查询 harness 是否有新版本：返回最新版、当前版、是否为新版。
func queryHarnessUpdate() (latest, cur string, newer bool) {
	latest, err := fetchLatestHarnessVersion()
	if err != nil {
		log.Printf("harness update check failed: %v", err)
		return "", "", false
	}
	cur = installedHarnessVersion()
	if cur == "" {
		return "", "", false // 无法确定已装 harness 版本
	}
	return latest, cur, isNewerVersion("v"+latest, "v"+cur)
}

// harnessRepoOwner / harnessRepoName DeepSeek Harness 本体 GitHub 仓库（其 Release 标签带 dsh- 前缀，如 dsh-v0.1.2-alpha.2）。
// 与 npm 包 @deepseek-ai/dsh（对应 apps/cli）同源；npm 发布往往滞后，故此处以 GitHub Release 为准。
const (
	harnessRepoOwner = "deepseek-ai"
	harnessRepoName  = "deepseek-harness"
)

// fetchLatestHarnessVersion 查询 DeepSeek Harness 在 GitHub 上的最新 Release 版本号（去掉 dsh- / v 前缀）。
// 与 fetchLatestRelease 相同：直连失败依次回退镜像前缀；取返回 Release 中版本号最大者。
func fetchLatestHarnessVersion() (string, error) {
	direct := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases?per_page=20", harnessRepoOwner, harnessRepoName)
	var candidates []string
	candidates = append(candidates, direct)
	for _, m := range buildMirrors() {
		if m != "" {
			candidates = append(candidates, m+direct)
		}
	}

	client := &http.Client{Timeout: updateAPITimeout}
	var lastErr error
	for _, u := range candidates {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "dsh-systray/"+appVersion)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		var rels []struct {
			TagName string `json:"tag_name"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, updateMaxBodySize)).Decode(&rels); err != nil {
			resp.Body.Close()
			lastErr = err
			continue
		}
		resp.Body.Close()
		best := ""
		for _, r := range rels {
			v := strings.TrimPrefix(strings.TrimPrefix(r.TagName, "dsh-"), "v")
			if v == "" {
				continue
			}
			if best == "" || compareVersions(v, best) > 0 {
				best = v
			}
		}
		if best != "" {
			return best, nil
		}
		lastErr = fmt.Errorf("harness 仓库未发现可用 Release")
	}
	return "", lastErr
}

// installedHarnessVersion 读取已安装 harness（@deepseek-ai/dsh 或源码 package.json）的版本号。
func installedHarnessVersion() string {
	paths := []string{
		filepath.Join(harnessDir, "node_modules", "@deepseek-ai", "dsh", "package.json"),
		filepath.Join(harnessDir, "package.json"),
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			var j struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(data, &j) == nil && j.Version != "" {
				return strings.TrimPrefix(j.Version, "v")
			}
		}
	}
	return ""
}

// runHarnessCmd 在 harness 目录执行命令，输出写到日志文件。
func runHarnessCmd(name string, args ...string) error {
	logPath := filepath.Join(logDir, "harness-update.log")
	f, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if f != nil {
		defer f.Close()
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = harnessDir
	hideCmdWindow(cmd)
	if f != nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	return cmd.Run()
}

// runHarnessUpdate 更新 DeepSeek Harness（npm 模式更新 @deepseek-ai/dsh；源码模式 git pull+install+build），
// 完成后重启服务。异步执行，带进度窗口。
func runHarnessUpdate(latest string) {
	splash := startSplash("正在更新 DeepSeek Harness…")
	restart := func() {
		killServer()
		time.Sleep(2 * time.Second)
		if !serverResponding(webURL) {
			if started, exitCh := startServer(); started {
				waitForServerReady(webURL, exitCh, startupTimeout)
			}
		}
	}

	var err error
	if isNpmHarnessReady() {
		splash.Update("正在更新 DeepSeek Harness 依赖…", 0.3)
		// 安装检查到的新版本而非 @latest：npm 的 prerelease（如 0.1.2-alpha.2）不会成为 latest 标签，
		// 用 @latest 会装回旧版导致“更新后仍是旧版本”。
		ver := latest
		if ver == "" {
			ver = "latest"
		}
		err = runHarnessCmd(pnpmCmd(), "add", "@deepseek-ai/dsh@"+ver, "--save-exact")
	} else {
		splash.Update("正在拉取 DeepSeek Harness 最新代码…", 0.2)
		err = runHarnessCmd("git", "pull")
		if err == nil {
			splash.Update("正在安装 harness 依赖…", 0.45)
			err = runHarnessCmd(pnpmCmd(), "install")
		}
		if err == nil {
			splash.Update("正在构建 harness 前端…", 0.7)
			err = runHarnessCmd(pnpmCmd(), "run", "build")
		}
	}
	if err != nil {
		splash.Close()
		showMessageBox("DeepSeek Harness 更新失败：\n"+err.Error()+"\n\n日志："+filepath.Join(logDir, "harness-update.log"), appName)
		return
	}
	splash.Update("正在重启服务…", 0.9)
	restart()
	splash.Close()
	showMessageBox(fmt.Sprintf("DeepSeek Harness 已更新到 v%s，服务已重启。", withV(latest)), appName)
}

// fetchLatestRelease 查询 GitHub Releases 最新版本；直连失败时依次回退镜像前缀（国内 DNS/网络不稳时更可靠）。
func fetchLatestRelease() (*latestRelease, error) {
	direct := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", updateRepoOwner, updateRepoName)
	var candidates []string
	candidates = append(candidates, direct)
	for _, m := range buildMirrors() {
		if m != "" {
			candidates = append(candidates, m+direct)
		}
	}

	client := &http.Client{Timeout: updateAPITimeout}
	var lastErr error
	for _, u := range candidates {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "dsh-systray/"+appVersion)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		var rel latestRelease
		if err := json.NewDecoder(io.LimitReader(resp.Body, updateMaxBodySize)).Decode(&rel); err != nil {
			resp.Body.Close()
			lastErr = err
			continue
		}
		resp.Body.Close()
		return &rel, nil
	}
	return nil, lastErr
}

// isNewerVersion 判断最新标签是否比当前版本新（忽略前导 v/dsh-）。
func isNewerVersion(latest, current string) bool {
	return compareVersions(latest, current) > 0
}

// compareVersions 语义化版本比较：按 "." 分段逐段比较数值部分；数值相同再比较预发布标识符
// （如 alpha.2 > alpha.1，稳定版 > 预发布版）。可容忍前导 v / dsh- 前缀。
func compareVersions(a, b string) int {
	pa, preA := splitVersionParts(a)
	pb, preB := splitVersionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var na, nb int
		if i < len(pa) {
			na, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			nb, _ = strconv.Atoi(pb[i])
		}
		if na < nb {
			return -1
		}
		if na > nb {
			return 1
		}
	}
	return comparePrerelease(preA, preB)
}

// splitVersionParts 拆出版本字符串的数值段与预发布段；容忍前导 v / dsh- / dsh-v 前缀。
func splitVersionParts(v string) (num, pre []string) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "dsh-")
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = strings.Split(v[i+1:], ".")
		v = v[:i]
	}
	num = strings.Split(v, ".")
	return num, pre
}

// comparePrerelease 预发布标识符比较：稳定版（无预发布）大于预发布版；
// 同为预发布时逐段比较，数值段按大小、非数值段按字典序，短列表（如 alpha < alpha.1）更小。
func comparePrerelease(a, b []string) int {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	if len(a) == 0 {
		return 1 // a 为稳定版，b 为预发布 → a 更新
	}
	if len(b) == 0 {
		return -1
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] == b[i] {
			continue
		}
		ai, aErr := strconv.Atoi(a[i])
		bi, bErr := strconv.Atoi(b[i])
		if aErr == nil && bErr == nil {
			if ai < bi {
				return -1
			}
			return 1
		}
		if aErr == nil {
			return -1 // 数值段 < 字母段（semver 规则）
		}
		if bErr == nil {
			return 1
		}
		if a[i] < b[i] {
			return -1
		}
		return 1
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}

// downloadAndApplyUpdate 下载更新包 → SHA256 校验 → 解压 → 替换并重启。
func downloadAndApplyUpdate(rel *latestRelease) error {
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
		return fmt.Errorf("未找到适用于当前系统的更新包（%s）", assetName)
	}

	tmp, err := os.MkdirTemp("", "dsh-systray-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	zipPath := filepath.Join(tmp, assetName)
	if err := downloadFileTo(context.Background(), zipURL, zipPath); err != nil {
		return fmt.Errorf("下载更新包失败：%w", err)
	}
	if sumURL != "" {
		sumPath := filepath.Join(tmp, "SHA256SUMS.txt")
		if err := downloadFileTo(context.Background(), sumURL, sumPath); err != nil {
			log.Printf("checksum file unavailable: %v", err)
		} else if err := verifyChecksum(zipPath, assetName, sumPath); err != nil {
			return err
		}
	}

	extractDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return err
	}
	if err := extractUpdateZip(zipPath, extractDir); err != nil {
		return fmt.Errorf("解压更新包失败：%w", err)
	}
	payload, err := updatePayloadPath(extractDir)
	if err != nil {
		return err
	}
	return replaceAndRelaunch(payload)
}

// downloadFileTo 下载到本地文件；直连失败时依次回退镜像前缀（无进度回调）。
func downloadFileTo(ctx context.Context, url, dest string) error {
	return downloadFileWithProgress(ctx, url, dest, nil)
}

// downloadFileWithProgress 下载到本地文件；直连 GitHub 失败时依次回退镜像前缀，
// 支持进度回调（pct 0~1，nil 表示不回调）与取消（ctx 取消即中断）。config.json 的 updateMirror 插到最前优先尝试。
func downloadFileWithProgress(ctx context.Context, url, dest string, onProgress func(pct float64)) error {
	var lastErr error
	for _, prefix := range buildMirrors() {
		if err := downloadOnce(ctx, prefix+url, dest, onProgress); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err() // 已取消，直接返回
		} else {
			lastErr = err
		}
	}
	return lastErr
}

// buildMirrors 返回镜像优先顺序：用户配置镜像 → 默认列表。
func buildMirrors() []string {
	if updateMirrorOverride == "" {
		return updateMirrors
	}
	out := []string{updateMirrorOverride}
	for _, m := range updateMirrors {
		if m != updateMirrorOverride {
			out = append(out, m)
		}
	}
	return out
}

func downloadOnce(ctx context.Context, url, dest string, onProgress func(pct float64)) error {
	client := &http.Client{Timeout: updateDLTimeout}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "dsh-systray/"+appVersion)
	resp, err := client.Do(req)
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
			if onProgress != nil && total > 0 {
				onProgress(float64(done) / float64(total))
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

// verifyChecksum 用 Release 附带的 SHA256SUMS.txt 校验更新包。
func verifyChecksum(zipPath, assetName, sumsPath string) error {
	data, err := os.ReadFile(sumsPath)
	if err != nil {
		return err
	}
	expect := ""
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == assetName {
			expect = strings.ToLower(f[0])
			break
		}
	}
	if expect == "" {
		return fmt.Errorf("校验文件中未找到 %s 的校验和", assetName)
	}
	f, err := os.Open(zipPath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != expect {
		return fmt.Errorf("更新包校验和不匹配（期望 %s…，实际 %s…）", expect[:16], got[:16])
	}
	return nil
}

// updateAssetName 当前平台对应的 Release 资产名。
func updateAssetName() string {
	if runtime.GOOS == "windows" {
		return "dsh-systray-windows-x64.zip"
	}
	return "dsh-systray-macos-universal.zip"
}

// extractUpdateZip 解压更新包：Windows 用 bsdtar，macOS 用 ditto（保留权限/符号链接）。
func extractUpdateZip(zipPath, destDir string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("tar", "-xf", zipPath, "-C", destDir)
		hideCmdWindow(cmd)
	} else {
		cmd = exec.Command("ditto", "-x", "-k", zipPath, destDir)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// updatePayloadPath 解压目录中待替换的程序主体：Windows 为 exe，macOS 为 .app 包。
func updatePayloadPath(extractDir string) (string, error) {
	if runtime.GOOS == "windows" {
		p := filepath.Join(extractDir, "dsh-systray.exe")
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("更新包中缺少 dsh-systray.exe")
		}
		return p, nil
	}
	p := filepath.Join(extractDir, "dsh-systray.app")
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("更新包中缺少 dsh-systray.app")
	}
	return p, nil
}

// cleanupStaleUpdateFiles 清理上次更新遗留的旧程序文件（Windows：exe.old）。
func cleanupStaleUpdateFiles() {
	if runtime.GOOS == "windows" {
		if exe, err := os.Executable(); err == nil {
			_ = os.Remove(exe + ".old")
		}
	}
	// 清理更新中断（如退出托盘/强制结束）遗留的临时更新目录
	if dirs, err := filepath.Glob(filepath.Join(os.TempDir(), "dsh-systray-update-*")); err == nil {
		for _, d := range dirs {
			_ = os.RemoveAll(d)
		}
	}
}

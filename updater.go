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
	updateRepoOwner     = "refyon"
	updateRepoName      = "dsh-systray"
	updateCheckDelay    = 30 * time.Second // 启动后 30 秒检查新版本
	updateCheckInterval = 24 * time.Hour   // 之后每 24 小时检查一次
	updateAPITimeout    = 8 * time.Second  // 版本接口单候选超时（直连失败回退镜像）
	updateDLTimeout     = 5 * time.Minute  // 单镜像单次下载上限
	updateMaxBodySize   = 4 << 20          // 版本接口响应上限 4MB
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

// harnessPrereleaseOverride 是否允许把 harness 预发布版（alpha/beta/rc）作为可更新版本（config.json 的 harnessPrerelease）。
var harnessPrereleaseOverride bool

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
//
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
	harnessLatest, harnessCur, harnessNewer, _ := queryHarnessUpdate()
	// 2) 查询 dsh-systray 自身最新版本
	rel, err := fetchLatestRelease()
	splash.Close()
	if err != nil {
		showMessageBox("检查更新失败：\n"+err.Error()+"\n\n请检查网络后重试。", appName)
		return
	}
	// 3) harness 有新版本 → 优先提示先更新 harness；用户选「稍后」则继续完成 dsh-systray 自身更新检查
	if harnessNewer {
		if askUpdateHarness(harnessLatest, harnessCur) {
			go runHarnessUpdate(harnessLatest)
			return
		}
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
// 重启成功后（非开机自启动场景，该场景不会走到此函数）弹窗询问是否立即打开 Web UI；返回是否成功。
func restartBackgroundService(onState func(stage string)) bool {
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
		return false
	}
	splash.Update("正在等待服务就绪…", 0.85)
	if ok, msg := waitForServerReady(webURL, exitCh, startupTimeout); !ok {
		if onState != nil {
			onState("error")
		}
		showMessageBox("重启失败：\n"+msg, appName)
		return false
	}
	if onState != nil {
		onState("running")
	}
	// 需求：设置中重启服务成功后弹窗询问是否打开 Web UI；
	// 保留开机自启动的静默逻辑（autostartLaunch 场景不询问；此函数也仅在设置页触发）。
	if !autostartLaunch {
		showReadyPrompt(webURL)
	}
	return true
}

// queryHarnessUpdate 查询 harness 是否有新版本。返回：
//   - latest：按当前「预发布通道」开关应安装的最新版本（空 = 无可更新目标）；
//   - cur：当前已装版本（尽力获取；即使远端查询失败也回填，不再显示“当前 —”）；
//   - newer：latest 是否比已装版本新；
//   - note：非网络失败的面向用户说明（如“仓库仅有预发布而通道未开”），空表示无说明。
//
// 修复点：deepseek-harness 仓库目前只发布预发布 Release，预发布通道关闭时按稳定版过滤
// 会得到空集——旧实现因此把“无可用稳定版”误报为“无法获取（网络问题）”。此处改为返回
// 说明文案，由上层展示；仅真正的网络失败才由上层报“无法获取”。
func queryHarnessUpdate() (latest, cur string, newer bool, note string) {
	cur = installedHarnessVersion()
	tags, err := fetchHarnessLatestTags()
	if err != nil {
		log.Printf("harness update check failed: %v", err)
		return "", cur, false, ""
	}
	latest, _, note = resolveHarnessLatest(tags, harnessPrereleaseOverride)
	if latest == "" {
		return "", cur, false, note
	}
	if cur == "" {
		return latest, "", false, "未检测到已安装的 Harness 版本，无法判断是否为最新"
	}
	return latest, cur, isNewerVersion("v"+latest, "v"+cur), ""
}

// resolveHarnessLatest 由 Release 标签集解析应更新版本与说明（纯函数，便于单测）：
// 返回 latest（按开关应安装，可能空）、newest（仓库实际最新发布，含预发布）与 note。
// note 非空仅当：latest 为空、newest 非空且通道关闭——即“仓库只有预发布、用户未开通道”。
func resolveHarnessLatest(tags []string, allowPrerelease bool) (latest, newest, note string) {
	latest = pickHarnessVersion(tags, allowPrerelease)
	newest = pickHarnessVersion(tags, true)
	if latest == "" && newest != "" && !allowPrerelease {
		note = harnessPreOnlyNote(newest)
	}
	return latest, newest, note
}

// harnessPreOnlyNote 组装「仓库仅预发布而通道关闭」的说明文案。
func harnessPreOnlyNote(newest string) string {
	return fmt.Sprintf("仓库暂无稳定 Release，最新可用为 %s（预发布）；开启「预发布通道」后可更新", withV(newest))
}

// harnessRepoOwner / harnessRepoName DeepSeek Harness 本体 GitHub 仓库（其 Release 标签带 dsh- 前缀，如 dsh-v0.1.2-alpha.2）。
// 与 npm 包 @deepseek-ai/dsh（对应 apps/cli）同源；npm 发布往往滞后，故此处以 GitHub Release 为准。
const (
	harnessRepoOwner = "deepseek-ai"
	harnessRepoName  = "deepseek-harness"
)

// fetchHarnessLatestTags 查询 DeepSeek Harness 在 GitHub 上的最新 Release 标签列表
// （去掉 dsh- / v 前缀前的原始 tag，如 dsh-v0.1.2）。与 fetchLatestRelease 相同：
// 直连失败依次回退镜像前缀。
func fetchHarnessLatestTags() ([]string, error) {
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
		var tags []string
		for _, r := range rels {
			tags = append(tags, r.TagName)
		}
		if len(tags) == 0 {
			lastErr = fmt.Errorf("harness 仓库未发现可用 Release")
			continue
		}
		return tags, nil
	}
	return nil, lastErr
}

// fetchHarnessResetTarget 返回「重置 DeepSeek Harness」的回退目标版本：优先官方最后发布的稳定版；
// 仓库尚无稳定 Release 时回退到最新发布（预发布）并给出说明——否则与检查更新同样的问题：
// 预发布通道关闭时“重置”会因无稳定版而“无法获取”不可用。
// note 为空表示目标即官方最新稳定版；非空时为面向用户的回退说明。
func fetchHarnessResetTarget() (version, note string, err error) {
	tags, err := fetchHarnessLatestTags()
	if err != nil {
		return "", "", err
	}
	if best := pickHarnessVersion(tags, false); best != "" {
		return best, "", nil
	}
	if best := pickHarnessVersion(tags, true); best != "" {
		return best, "（仓库暂无稳定 Release，回退目标为最新发布 " + withV(best) + "）", nil
	}
	return "", "", fmt.Errorf("harness 仓库未发现可用 Release")
}

// isStableVersion 判断版本是否为稳定版（无 -alpha/-beta/-rc 等预发布后缀）。
func isStableVersion(v string) bool {
	return !strings.Contains(v, "-")
}

// pickHarnessVersion 从 Release 标签列表中选出应安装的版本号：
// 默认仅考虑稳定版（避免预发布与已装插件不兼容导致服务启动失败）；harnessPrereleaseOverride 开启时才包含预发布，
// 并在其中取版本号最大者（如 0.1.2-alpha.2 > 0.1.1-rc.2）。
func pickHarnessVersion(tags []string, allowPrerelease bool) string {
	best := ""
	for _, tag := range tags {
		v := strings.TrimPrefix(strings.TrimPrefix(tag, "dsh-"), "v")
		if v == "" {
			continue
		}
		if !allowPrerelease && !isStableVersion(v) {
			continue
		}
		if best == "" || compareVersions(v, best) > 0 {
			best = v
		}
	}
	return best
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

// runHarnessCmd 在 harness 目录执行命令，输出写到日志文件（每行带时间戳前缀）。
// 每次执行前写入一条命令分隔头，便于日后从日志直接归因失败命令。
func runHarnessCmd(name string, args ...string) error {
	logPath := filepath.Join(logDir, "harness-update.log")
	f, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	cmd := exec.Command(name, args...)
	cmd.Dir = harnessDir
	hideCmdWindow(cmd)
	if f == nil {
		return cmd.Run()
	}
	defer f.Close()
	w := newTimePrefixWriter(f)
	_, _ = fmt.Fprintf(w, "\n===== %s %s =====\n", name, strings.Join(args, " "))
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	w.Flush()
	return err
}

// isGitHarnessDir harness 目录是否为 git 仓库（源码形态更新/回退的前置条件；
// npm 预构建形态目录没有 .git，必须阻止 git 命令进入，否则就是“fatal: not a git repository”）。
func isGitHarnessDir() bool {
	_, err := os.Stat(filepath.Join(harnessDir, ".git"))
	return err == nil
}

// npmHarnessVersionAvailable 查询 npm registry 是否已发布该精确版本（pnpm view）。
// 供 npm 形态更新前预检：GitHub Release 常先于 npm 发布，直接 pnpm add 会失败且原因晦涩。
// 返回 false 表示未找到该版本或查询失败（网络/registry 异常，调用方给用户可理解文案）。
func npmHarnessVersionAvailable(version string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, pnpmCmd(), "view", "@deepseek-ai/dsh@"+version, "version")
	cmd.Dir = harnessDir
	cmd.Env = append(os.Environ(), pnpmTunedEnv()...)
	hideCmdWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// setHarnessOverrides 向 harness 目录的 package.json 注入
// pnpm.overrides["@deepseek-ai/*"] = <ver>（合并保留既有字段），使 pnpm install 把
// 全部 @deepseek-ai/* 家族强制到同一版本。修复“只 pnpm add @deepseek-ai/dsh 升级根包、
// 其余 @deepseek-ai/* 停在锁文件旧版 → 新旧混装 ESM 加载失败”的根因（0.1.2-rc.1 曾因此炸）。
// ver 为空时移除该 override。返回写回是否成功。
func setHarnessOverrides(dir, ver string) error {
	p := filepath.Join(dir, "package.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	pnpm, _ := root["pnpm"].(map[string]interface{})
	if pnpm == nil {
		pnpm = map[string]interface{}{}
	}
	ov, _ := pnpm["overrides"].(map[string]interface{})
	if ov == nil {
		ov = map[string]interface{}{}
	}
	if ver == "" {
		delete(ov, "@deepseek-ai/*")
	} else {
		ov["@deepseek-ai/*"] = ver
	}
	pnpm["overrides"] = ov
	root["pnpm"] = pnpm
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(out, '\n'), 0o644)
}

// harnessBackupSuffix 更新前快照文件后缀（更新失败时用于回退到上一可运行版本）。
const harnessBackupSuffix = ".dshbak"

// snapshotHarness 快照当前可运行版本：备份 package.json / pnpm-lock.yaml，并把 node_modules 整体改名备份
// （同盘 rename，秒级完成；更新失败可本地直接移回，不依赖网络）。返回是否成功备份了 node_modules。
// 注意：调用前必须先 killServer()，否则运行中的服务会占用 node_modules 内文件导致改名失败。
func snapshotHarness() (nodeModulesBacked bool) {
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		src := filepath.Join(harnessDir, name)
		if data, err := os.ReadFile(src); err == nil {
			_ = os.WriteFile(src+harnessBackupSuffix, data, 0o644)
		}
	}
	nm := filepath.Join(harnessDir, "node_modules")
	if _, err := os.Stat(nm); err != nil {
		return false
	}
	bak := nm + harnessBackupSuffix
	_ = os.RemoveAll(bak)
	return os.Rename(nm, bak) == nil
}

// restoreHarnessSnapshot 回退到快照版本：还原 package.json / pnpm-lock.yaml；
// 有 node_modules 备份直接移回（秒级），否则按还原后的锁文件重装。
func restoreHarnessSnapshot(hadNodeModulesBackup bool) {
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		src := filepath.Join(harnessDir, name+harnessBackupSuffix)
		if data, err := os.ReadFile(src); err == nil {
			_ = os.WriteFile(filepath.Join(harnessDir, name), data, 0o644)
		}
	}
	nm := filepath.Join(harnessDir, "node_modules")
	if hadNodeModulesBackup {
		_ = os.RemoveAll(nm)
		_ = os.Rename(nm+harnessBackupSuffix, nm)
		return
	}
	if err := runHarnessCmd(pnpmCmd(), "install", "--frozen-lockfile"); err != nil {
		log.Printf("rollback reinstall failed: %v", err)
	}
}

// cleanupHarnessSnapshot 更新成功：删除更新前的快照备份。
func cleanupHarnessSnapshot() {
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		_ = os.Remove(filepath.Join(harnessDir, name+harnessBackupSuffix))
	}
	_ = os.RemoveAll(filepath.Join(harnessDir, "node_modules"+harnessBackupSuffix))
}

// runHarnessCmdCapture 在 harness 目录执行命令并返回去除首尾空白的 stdout（不回显到日志文件）。
func runHarnessCmdCapture(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Dir = harnessDir
	hideCmdWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// harnessBootErrorMarkers 服务启动失败特征：版本混装 / 插件与 harness API 不兼容导致的 ESM 加载错误。
var harnessBootErrorMarkers = []string{
	"does not provide an export",
	"failed to import loader entry",
	"SyntaxError:",
	"ERR_MODULE_NOT_FOUND",
	"Cannot find package",
	"ERR_REQUIRE_ESM",
}

// serverLogSize 当前 server.log 字节数（健康校验按追加段扫描，避免把历史日志的报错误判进来）。
func serverLogSize() int64 {
	fi, err := os.Stat(filepath.Join(logDir, "server.log"))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// serverLogHasBootErrors 扫描 server.log 从 offset 起的追加段，判断本次启动是否出现加载报错。
func serverLogHasBootErrors(offset int64) bool {
	f, err := os.Open(filepath.Join(logDir, "server.log"))
	if err != nil {
		return false
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			offset = 0
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	s := string(data)
	for _, m := range harnessBootErrorMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// verifyServerBoot 就绪后的健康校验：周期扫描 server.log 从 before 起的追加段，并监听进程退出。
// 覆盖“HTTP 已就绪但插件/依赖加载错误更晚刷出”的漏判（错误常迟于就绪数秒出现，曾导致
// 混装版本的异常启动被当作成功、甚至把 LKG 误清）。任一命中立即判失败；窗口结束仍未命中为健康。
func verifyServerBoot(before int64, exited <-chan error) bool {
	const settle = 10 * time.Second // 总等待窗口：给慢刷错误留足时间
	deadline := time.Now().Add(settle)
	for {
		if serverLogHasBootErrors(before) {
			return false
		}
		if exited != nil {
			select {
			case <-exited:
				return false // 进程提前退出（启动后崩溃）
			default:
			}
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return true
		}
		step := 5 * time.Second
		if remain < step {
			step = remain
		}
		time.Sleep(step)
	}
}

// restartAndVerifyServer 重启后台服务并做健康校验：先停服务，再拉起并等待就绪（HTTP 响应），
// 就绪后进入 verifyServerBoot 健康窗口（追加段扫描 + 进程退出侦听）。全部通过返回 true。
func restartAndVerifyServer() bool {
	killServer()
	time.Sleep(1 * time.Second)
	if serverResponding(webURL) {
		return true // 端口已有可用服务（异常残留场景），视为可用
	}
	before := serverLogSize()
	started, exitCh := startServer()
	if !started {
		return false
	}
	if ok, _ := waitForServerReady(webURL, exitCh, startupTimeout); !ok {
		return false
	}
	return verifyServerBoot(before, exitCh)
}

// rollbackUpdate 更新失败处理：停止服务 → 回退快照 → 重启校验 → 弹窗报告。
func rollbackUpdate(splash *SplashState, prev string, hadNMBackup bool, reason string) {
	splash.Update("更新失败，正在回退到上一可用版本…", 0.6)
	killServer()
	restoreHarnessSnapshot(hadNMBackup)
	splash.Update("正在重启服务…", 0.85)
	restartAndVerifyServer()
	splash.Close()
	msg := "DeepSeek Harness 更新失败（" + reason + "），已回退到"
	if prev != "" {
		msg += " v" + prev + "。"
	} else {
		msg += "上一可用版本。"
	}
	msg += "\n\n日志：" + filepath.Join(logDir, "harness-update.log")
	showMessageBox(msg, appName)
}

// runHarnessUpdate 更新 DeepSeek Harness（npm 模式更新 @deepseek-ai/dsh；源码模式 git pull+install+build），
// 完成后重启服务并校验；失败自动回退到上一可运行版本。异步执行，带进度窗口。
func runHarnessUpdate(latest string) {
	splash := startSplash("正在更新 DeepSeek Harness…")
	prev := installedHarnessVersion()

	// 0) 先判定安装形态——必须在快照之前：快照会把 node_modules 改名备份，而 npm 形态判定
	//    依赖 node_modules/@deepseek-ai/dsh 存在性；若先快照再判定，npm 形态会被误判为源码
	//    形态而误跑 git（此前升级失败“fatal: not a git repository ×4”的根因之一）。
	npmMode := isNpmHarnessReady()
	sourceMode := !npmMode && isSourceHarnessDir()
	switch {
	case npmMode:
		// 预检目标版本已发布到 npm：GitHub Release 常先于 npm 发布，目标可能装不了——
		// 尽早给出明确原因，避免停服务后才失败回退。
		ver := latest
		if ver == "" {
			ver = "latest"
		}
		if ver != "latest" && !npmHarnessVersionAvailable(ver) {
			splash.Close()
			showMessageBox("无法更新 DeepSeek Harness：\n\nnpm registry 上未找到 @deepseek-ai/dsh@"+ver+
				"（该版本 GitHub 已发布但可能尚未同步到 npm，或 registry/网络异常）。\n未对当前版本做任何改动。\n\n"+
				"日志："+filepath.Join(logDir, "harness-update.log"), appName)
			return
		}
	case sourceMode:
		if !isGitHarnessDir() {
			splash.Close()
			showMessageBox("无法更新 DeepSeek Harness：\n\n该 Harness 目录不是 git 仓库（可能是 zip 解压或整目录复制而来），无法走源码更新。\n"+
				"请使用 git clone 的 Harness 源码目录，或恢复 npm 预构建形态（当前目录缺少 @deepseek-ai/dsh 入口）。\n\n目录："+harnessDir, appName)
			return
		}
	default:
		splash.Close()
		showMessageBox("无法识别 DeepSeek Harness 安装形态（npm 预构建或 git 源码 checkout）。\n\n目录："+harnessDir, appName)
		return
	}

	// 1) 先停止服务（否则运行中的 node 进程会占用 node_modules 文件，快照/回退改名会失败）
	killServer()
	time.Sleep(1 * time.Second)

	// 2) 快照当前可运行版本（本地回退用）
	splash.Update("正在备份当前版本…", 0.15)
	hadNMBackup := snapshotHarness()

	// 3) 安装新版本（失败原因按分支细化，供回退弹窗明确展示）
	var err error
	reason := "安装失败"
	if npmMode {
		splash.Update("正在更新 DeepSeek Harness 依赖…", 0.35)
		// 安装检查到的新版本而非 @latest：npm 的 prerelease（如 0.1.2-alpha.2）不会成为 latest 标签，
		// 用 @latest 会装回旧版导致“更新后仍是旧版本”。
		ver := latest
		if ver == "" {
			ver = "latest"
		}
		// 全家族 overrides 钉到目标版本：仅 pnpm add dsh 会把其余 @deepseek-ai/* 留在锁文件旧版，
		// 造成新旧混装（ESM 加载失败、服务“假启动”）。overrides 让 install 把整个家族拉齐。
		err = setHarnessOverrides(harnessDir, ver)
		if err == nil {
			err = runHarnessCmd(pnpmCmd(), "add", "@deepseek-ai/dsh@"+ver, "--save-exact")
		}
		if err == nil {
			// 全量 install 重新 reconcile 整个依赖树，避免只改根依赖导致的新旧版本混装
			splash.Update("正在安装依赖…", 0.55)
			err = runHarnessCmd(pnpmCmd(), "install")
			if err != nil {
				reason = "依赖安装失败（详见日志末尾）"
			}
		} else {
			reason = "安装指定版本失败（详见日志末尾）"
		}
	} else {
		prevHead := runHarnessCmdCapture("git", "rev-parse", "HEAD")
		splash.Update("正在拉取 DeepSeek Harness 最新代码…", 0.3)
		err = runHarnessCmd("git", "pull")
		if err == nil {
			splash.Update("正在安装 harness 依赖…", 0.5)
			err = runHarnessCmd(pnpmCmd(), "install")
			if err == nil {
				splash.Update("正在构建 harness 前端…", 0.7)
				err = runHarnessCmd(pnpmCmd(), "run", "build")
				if err != nil {
					reason = "前端构建失败（详见日志末尾）"
				}
			} else {
				reason = "依赖安装失败（详见日志末尾）"
			}
		} else {
			reason = "git pull 失败（详见日志末尾）"
		}
		if err != nil && prevHead != "" {
			// 源码模式回退：回到更新前 HEAD 并重装
			splash.Update("正在回退代码…", 0.6)
			_ = runHarnessCmd("git", "reset", "--hard", prevHead)
			_ = runHarnessCmd(pnpmCmd(), "install")
			_ = runHarnessCmd(pnpmCmd(), "run", "build")
		}
	}
	if err != nil {
		rollbackUpdate(splash, prev, hadNMBackup, reason)
		return
	}

	// 4) 重启并健康校验（就绪 + 启动日志无报错）
	splash.Update("正在重启服务…", 0.85)
	if !restartAndVerifyServer() {
		rollbackUpdate(splash, prev, hadNMBackup, "新版本启动失败")
		return
	}

	// 5) 成功：快照提升为 LKG（保留到下次冷启动验证通过后再清理——启动失败时可自动回退到该状态）
	promoteHarnessLkg(prev)
	splash.Close()
	msg := fmt.Sprintf("DeepSeek Harness 已更新到 %s，服务已重启。", withV(latest))
	if !isStableVersion(strings.TrimPrefix(latest, "v")) {
		msg += "\n\n提示：预发布版本可能与已装插件不兼容；如遇异常，可用「重置服务」回退到上一个正常运行的版本。"
	}
	showMessageBox(msg, appName)
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

package main

import (
	"bytes"
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
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
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

// updateFinalizing 是否已进入「替换并重启」不可取消阶段（此后再点取消不应中断替换，
// 否则会产生半更新状态）。由各平台在调用 replaceAndRelaunch 前置位。
var updateFinalizing atomic.Bool

// emitUpdateDone 通知前端更新流程结束（成功/取消/失败均发出，前端据此复位更新按钮并回到设置页）。
// 同时把 splash 阶段复位为 startup：更新期间的 phase=update 不泄漏到后续
// 插件/harness 更新与重启服务的进度视图（那些流程不注册「取消更新」句柄）。
func emitUpdateDone(ok, canceled bool, note string) {
	setSplashPhase("startup")
	if appCtx == nil {
		return
	}
	wruntime.EventsEmit(appCtx, "update:done", map[string]interface{}{
		"ok":       ok,
		"canceled": canceled,
		"error":    note,
	})
}

// 进行中更新的取消控制（托盘退出时调用 cancelActiveUpdate 终止下载/安装）。
var (
	updateMu     sync.Mutex
	activeCancel context.CancelFunc
)

// updateCheckWindows 进行中的“检查更新”流程数（手动检查的进度窗口/提示期间计数）。
// 启动 30 秒的自动检查发现新版本时，若已存在检查/更新窗口则不再重复弹窗。
var updateCheckWindows atomic.Int32

// harnessOpBusy 是否有 harness 更新/重置流程在跑：与插件操作批处理互斥——两者都会
// killServer + 在各自目录跑 pnpm，并发执行会互相踩踏（停服中断对方、pnpm 抢占同一 profile）。
var harnessOpBusy atomic.Bool

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
// 「替换并重启」阶段（updateFinalizing）不可取消——此时取消会产生半更新状态。
func cancelActiveUpdate() {
	if updateFinalizing.Load() {
		return
	}
	updateMu.Lock()
	if activeCancel != nil {
		activeCancel()
	}
	updateMu.Unlock()
}

// registerActiveUpdate 登记/取消登记当前更新取消句柄。
func registerActiveUpdate(cancel context.CancelFunc) {
	updateFinalizing.Store(false)
	updateMu.Lock()
	activeCancel = cancel
	updateMu.Unlock()
}

// setUpdateFinalizing 标记进入不可取消阶段（替换并重启前调用）。
func setUpdateFinalizing() {
	updateFinalizing.Store(true)
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
	startUpdateApplyWithUI(rel)
}

// startUpdateApply 应用更新：各平台实现。进度走前端 splash 视图（进程内下载/校验/替换），
// Windows 替换 exe 后自动重启，macOS 用辅助脚本替换 .app 后重启。声明于此，由 platform_*.go 实现。
// 注意：调用前须已显示主窗口（用 startUpdateApplyWithUI），否则隐藏窗口下用户看不到进度。

// checkForUpdatesManual 手动检查更新（设置页触发）：点击后立即弹出进度窗口，
// 在窗口下完成 harness + dsh-systray 版本查询；harness 有新版则优先提示先更新 harness。
func checkForUpdatesManual() {
	if appVersion == "" || appVersion == "dev" {
		showMessageBox(T("当前为开发版本（dev），未启用自动更新。"), appName)
		return
	}
	// 全程计数：进度窗口 + 结果提示期间都视为“检查更新窗口在开”，自动检查不再重复弹窗。
	openUpdateCheckFlow()
	defer closeUpdateCheckFlow()
	// 立即弹出进度窗口（不等待查询结果）
	splash := startSplash(T("正在查询最新版本…"))
	splash.Update(T("正在查询最新版本…"), 0.15)

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

// restartBackgroundService 重启后台 Web 服务：停止 → 拉起 → 就绪（带进度窗口），并做
// 健康校验与自动修复。onState 可选：进程各阶段回调（供设置页实时刷新服务状态文案）；
// 阶段字符串直接作为界面文案展示（不依赖前端翻译）。
//
// 容错（插件导入 / 变更后无法启动的自动修复）：
//  1. 启动就绪后进入 verifyServerBoot 健康窗口，扫出迟于 HTTP 就绪出现的加载错误
//     （版本混装 / 插件不兼容），不再把“假就绪”当成功；
//  2. 失败时自动对所有 profile 执行一次 pnpm install 对齐（等价用户手动
//     `pnpm dsh plugin remove/add` 触发的全树 reconcile），再重试一次启动；
//  3. 仍失败时从 server.log 定位疑似导致启动失败的插件名并展示给用户。
//
// 重启成功后（非开机自启动场景，该场景不会走到此函数）弹窗询问是否立即打开 Web UI；返回是否成功。
func restartBackgroundService(onState func(stage string)) bool {
	splash := startSplash(T("正在重启后台服务…"))
	defer splash.Close()
	splash.Update(T("正在停止后台服务…"), 0.2)
	if onState != nil {
		onState("正在停止后台服务…")
	}
	killServer()
	if onState != nil {
		onState("服务已停止")
	}
	time.Sleep(1 * time.Second)

	// 第一轮：拉起 → 就绪 → 健康窗口
	splash.Update(T("正在启动后台服务…"), 0.55)
	if onState != nil {
		onState("正在启动后台服务…")
	}
	ok, msg := startAndVerifyOnce()
	if !ok {
		// 自愈：对全部 profile 做一次 pnpm 对齐（健康校验失败多由依赖树不一致引起），再重试一次
		splash.Update(T("启动未通过健康校验，正在修复插件依赖并重试…"), 0.65)
		if onState != nil {
			onState("正在修复插件依赖并重试…")
		}
		for _, pf := range enumeratePluginProfiles() {
			if err := reconcileProfileDeps(pf.dir); err != nil {
				log.Printf("restart heal: %v", err)
			}
		}
		killServer()
		time.Sleep(1 * time.Second)
		splash.Update(T("正在重试启动后台服务…"), 0.8)
		ok, msg = startAndVerifyOnce()
	}
	if !ok {
		if onState != nil {
			onState("重启失败")
		}
		detail := msg
		if suspects := parseBootLogSuspects(0); len(suspects) > 0 {
			detail += "\n\n疑似导致启动失败的插件：" + strings.Join(suspects, "、") +
				"\n可到「插件管理」中移除可疑插件后重试；或再次点击「重启」让程序自动修复。"
		}
		logError("app", "重启后台服务失败：%s", strings.ReplaceAll(detail, "\n", " "))
		showMessageBox("重启失败：\n"+detail+"\n\n日志："+unifiedLogPath(), appName)
		return false
	}
	if onState != nil {
		onState("服务运行中")
	}
	// 需求：设置中重启服务成功后弹窗询问是否打开 Web UI；
	// 保留开机自启动的静默逻辑（autostartLaunch 场景不询问；此函数也仅在设置页触发）。
	if !autostartLaunch {
		showReadyPrompt(webURL)
	}
	return true
}

// queryHarnessUpdate 查询 harness 是否有新版本。返回：
//   - latest：按当前安装形态与「预发布通道」开关应安装的最新版本（空 = 无可更新目标）；
//   - cur：当前已装版本（尽力获取；即使远端查询失败也回填，不再显示“当前 —”）；
//   - newer：latest 是否比已装版本新；
//   - note：非网络失败的面向用户说明（如“仓库仅有预发布而通道未开”），空表示无说明。
//
// 版本源与「重置服务」保持一致（修复：0.1.2-rc.1 已发 npm 但 GitHub Release 列表缺失时，
// 重置显示 0.1.2-rc.1、检查更新却停在 0.1.1-rc.2 的不一致）：
//   - npm 预构建形态 → npm registry 已发布版本（GitHub Release 常领先/缺失，而安装走 npm，
//     必须以 npm 真实存在的版本为准）。目标按「预发布通道」开关选取：开启取版本号最大
//     （与源码形态 resolveHarnessLatest 一致）；关闭仅取稳定版，npm 无稳定版时不提供更新
//     目标、只返回说明（修复：未开通道仍“检测到”npm 最新预发布 0.1.3-alpha.2 的误报）；
//   - 源码 checkout 形态 → GitHub Release（源码更新切 git tag，以 Release 为准）。
func queryHarnessUpdate() (latest, cur string, newer bool, note string) {
	cur = installedHarnessVersion()
	if isNpmHarnessReady() {
		best, n, err := fetchNpmResetTarget(harnessPrereleaseOverride)
		if err != nil {
			log.Printf("harness update check (npm) failed: %v", err)
			return "", cur, false, ""
		}
		if best == "" {
			// 通道关闭且 npm 仅有预发布：无更新目标，n 为面向用户说明（前端显示 note，不误报“检查失败”）
			return "", cur, false, n
		}
		if n != "" {
			note = "npm 上当前仅有预发布版本，已按最新发布 " + withV(best) + " 检查"
		}
		if cur == "" {
			return best, "", false, "未检测到已安装的 Harness 版本，无法判断是否为最新"
		}
		return best, cur, isNewerVersion("v"+best, "v"+cur), note
	}
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
	direct := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases?per_page=100", harnessRepoOwner, harnessRepoName)
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

// runHarnessCmd 在 harness 目录执行命令，输出按行改写进统一日志（模块 harness）。
// 每次执行前写入一条命令分隔头，便于日后从日志直接归因失败命令。
func runHarnessCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = harnessDir
	hideCmdWindow(cmd)
	w := newModuleLogWriter("harness")
	_, _ = fmt.Fprintf(w, "\n===== %s %s =====\n", name, strings.Join(args, " "))
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	w.Flush()
	return err
}

// runHarnessCmdTail 同 runHarnessCmd，另返回输出尾部（供失败归类——例如
// ERR_PNPM_NO_MATCHING_VERSION 需区别「上游分批发布」与普通网络失败）。日志仍完整落盘。
func runHarnessCmdTail(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = harnessDir
	hideCmdWindow(cmd)
	var buf bytes.Buffer
	w := newModuleLogWriter("harness")
	_, _ = fmt.Fprintf(w, "\n===== %s %s =====\n", name, strings.Join(args, " "))
	cmd.Stdout = io.MultiWriter(w, &buf)
	cmd.Stderr = io.MultiWriter(w, &buf)
	err := cmd.Run()
	w.Flush()
	return buf.String(), err
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

// npmHarnessPublishedVersions 列出 npm registry 上 @deepseek-ai/dsh 的全部已发布版本号。
// 供 npm 预构建形态的「重置回退目标」解析使用：npm 形态安装走 npm，目标必须是 npm 已发布
// 版本——GitHub Release tag 常领先于 npm（如 0.1.3-alpha.1 有 tag 但未发 npm），直接采用
// GitHub tag 会 pnpm add "No matching version found"（实测 ERR_PNPM_NO_MATCHING_VERSION）。
// 解析容忍 pnpm 输出形态差异：优先 JSON 数组，失败按行剥离引号/括号兜底。
func npmHarnessPublishedVersions() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, pnpmCmd(), "view", "@deepseek-ai/dsh", "versions", "--json")
	cmd.Dir = harnessDir
	cmd.Env = append(os.Environ(), pnpmTunedEnv()...)
	hideCmdWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("查询 npm 已发布版本失败：%w", err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, fmt.Errorf("npm registry 未返回 @deepseek-ai/dsh 的已发布版本")
	}
	var versions []string
	if json.Unmarshal([]byte(s), &versions) == nil {
		return versions, nil
	}
	// 兜底：非 JSON（如每行一个版本）时逐行清洗
	seen := map[string]bool{}
	var outV []string
	for _, ln := range strings.Split(s, "\n") {
		v := strings.Trim(strings.TrimSpace(ln), `"[], `)
		v = strings.TrimPrefix(v, "dsh-")
		v = strings.TrimPrefix(v, "v")
		if v != "" && !seen[v] {
			seen[v] = true
			outV = append(outV, v)
		}
	}
	if len(outV) == 0 {
		return nil, fmt.Errorf("npm registry 返回的版本列表为空")
	}
	return outV, nil
}

// fetchNpmResetTarget 返回 npm 预构建形态应检测/回退的版本（现仅被 queryHarnessUpdate 调用；
// 重置下拉目标由 GetResetVersions 独立解析）。目标必须真实存在于 npm（pnpm add 精确版本
// 才装得上），按「预发布通道」开关解析：
//   - allowPrerelease=false：取最新稳定版；npm 无稳定版时不返回目标、只返回 note 说明；
//   - allowPrerelease=true：取版本号最大者（含预发布，与源码形态 resolveHarnessLatest 一致）；
//     全部为预发布时 note 说明按最新发布检测。
func fetchNpmResetTarget(allowPrerelease bool) (version, note string, err error) {
	versions, err := npmHarnessPublishedVersions()
	if err != nil {
		return "", "", err
	}
	return resolveNpmUpdateTarget(versions, allowPrerelease)
}

// resolveNpmUpdateTarget 由 npm 已发布版本表按通道开关解析应检测目标（纯函数，便于单测）。
func resolveNpmUpdateTarget(versions []string, allowPrerelease bool) (version, note string, err error) {
	stable := pickHarnessVersion(versions, false)
	if !allowPrerelease {
		if stable != "" {
			return stable, "", nil
		}
		if newest := pickHarnessVersion(versions, true); newest != "" {
			return "", harnessNpmPreOnlyNote(newest), nil
		}
		return "", "", fmt.Errorf("npm registry 未发现可用的 @deepseek-ai/dsh 版本")
	}
	best := pickHarnessVersion(versions, true)
	if best == "" {
		return "", "", fmt.Errorf("npm registry 未发现可用的 @deepseek-ai/dsh 版本")
	}
	if stable == "" {
		return best, "（npm 已发布版本均为预发布，最新可用 " + withV(best) + "）", nil
	}
	return best, "", nil
}

// harnessNpmPreOnlyNote 组装「npm 仅有预发布而通道关闭」的说明文案（npm 数据源措辞，
// 与源码形态 harnessPreOnlyNote 对齐但区分数据源）。
func harnessNpmPreOnlyNote(newest string) string {
	return fmt.Sprintf("npm 上暂无稳定版本，最新发布为 %s（预发布）；开启「预发布通道」后可更新", withV(newest))
}

// harnessFamilyPrefix harness 家族包名前缀。只钉 @deepseek-ai/dsh*：@deepseek-ai/cordis(4.x)、
// @deepseek-ai/schemastery(3.x)、@deepseek-ai/cordis-plugin-*(1.x) 与 harness 同 scope 却是
// 各自独立的版本线，钉到 harness 版本会让 pnpm 直接 ERR_PNPM_NO_MATCHING_VERSION。
const harnessFamilyPrefix = "@deepseek-ai/dsh"

// harnessPkgNameRe 从 pnpm-lock.yaml 抓家族包名（快照键形如 '@deepseek-ai/dsh-llm@0.1.5-rc.1':）。
var harnessPkgNameRe = regexp.MustCompile(`@deepseek-ai/(dsh[a-z0-9.-]*)@`)

// harnessFamilyNames 枚举目录现状里的 harness 家族包名（@deepseek-ai/dsh*），供整族钉版使用。
// 数据源优先 pnpm-lock.yaml（连只作为 peer 出现、未在根 package.json 声明的核心包一并覆盖），
// 回退 node_modules/.pnpm 目录名；两处都读不到时返回 nil（调用方退化为「不钉版」）。
func harnessFamilyNames(dir string) []string {
	seen := map[string]bool{}
	if data, err := os.ReadFile(filepath.Join(dir, "pnpm-lock.yaml")); err == nil {
		for _, m := range harnessPkgNameRe.FindAllStringSubmatch(string(data), -1) {
			name := "@deepseek-ai/" + m[1]
			if strings.HasPrefix(name, harnessFamilyPrefix) {
				seen[name] = true
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "node_modules", ".pnpm")); err == nil {
		for _, e := range entries {
			// 目录名形如 @deepseek-ai+dsh-llm@0.1.5-rc.1_@deepseek-ai+cordis@4.0.2
			rest, ok := strings.CutPrefix(e.Name(), "@deepseek-ai+")
			if !ok {
				continue
			}
			i := strings.IndexByte(rest, '@')
			if i <= 0 {
				continue
			}
			name := "@deepseek-ai/" + rest[:i]
			if strings.HasPrefix(name, harnessFamilyPrefix) {
				seen[name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// setHarnessFamilyOverrides 把 harness 家族包**逐个**钉到 ver，使 pnpm 解析出的家族版本单一
// （修复“只 pnpm add @deepseek-ai/dsh 升级根包、其余家族包停在锁文件旧版 → 新旧混装 ESM 加载
// 失败”，0.1.2-rc.1 与 0.1.5-rc.1 都因此炸过）。两个通道各写一份：package.json 的
// "pnpm.overrides"（旧版 pnpm 读）与 pnpm-workspace.yaml 顶层 overrides（pnpm ≥ v10 读）。
//
// 为什么不再用 "@deepseek-ai/*" 名字通配（上一版实现的写法）：
//  1. 2026-09-10 本机实测（pnpm 10.34.5，同一份 package.json + pnpm-workspace.yaml）：通配
//     不生效——家族仍解析到 0.1.1-rc.2（混装）与半发布的 0.1.5-rc.2；换成精确包名立即生效。
//     即“防混装补丁”此前一直空转，混装树照旧被装出来。
//  2. 通配即便生效也不安全：会连 @deepseek-ai/cordis(4.x)、schemastery(3.x)、
//     cordis-plugin-*(1.x) 一起钉成 harness 版本（那些包没有该版本）→ 安装直接失败。
//
// 逐包钉版还兜住「上游分批发布」窗口：家族依赖是 caret 范围（^0.1.5-rc.1 允许 0.1.5-rc.2），
// 上游先发一部分包时，全新解析会选中半发布的新版本、随后在其缺失依赖上
// ERR_PNPM_NO_MATCHING_VERSION 整次失败（2026-09-10 22:49 实测）；钉死后整族锁在目标版本。
// names 为空或 ver 为空时只清理历史条目、不写新条目。返回写回是否成功。
func setHarnessFamilyOverrides(dir, ver string, names []string) error {
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
	ov := map[string]interface{}{}
	if ver != "" {
		for _, n := range names {
			ov[n] = ver
		}
	}
	if len(ov) == 0 {
		delete(pnpm, "overrides") // 整体重建：历史 "@deepseek-ai/*" 通配条目一并清除
	} else {
		pnpm["overrides"] = ov
	}
	root["pnpm"] = pnpm
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, append(out, '\n'), 0o644); err != nil {
		return err
	}
	return writeHarnessWorkspaceOverrides(dir, ov)
}

// writeHarnessWorkspaceOverrides 重建 pnpm-workspace.yaml 顶层 overrides 块，内容完全由 ov
// 决定（顺带清掉历史通配与已失效包名）；文件内其它键原样保留，无内容则删除文件。
func writeHarnessWorkspaceOverrides(dir string, ov map[string]interface{}) error {
	p := filepath.Join(dir, "pnpm-workspace.yaml")
	raw := ""
	if data, err := os.ReadFile(p); err == nil {
		raw = strings.ReplaceAll(string(data), "\r\n", "\n")
	}
	lines := strings.Split(raw, "\n")
	var out []string
	removed := false
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimRight(lines[i], " \t")
		if strings.TrimSpace(trimmed) == "" {
			continue // 空行在写回时统一折叠
		}
		if trimmed == "overrides:" && !strings.HasPrefix(lines[i], " ") && !strings.HasPrefix(lines[i], "\t") {
			// 移除旧的 overrides 块（含其缩进子行）
			removed = true
			i++
			for i < len(lines) {
				l := lines[i]
				if l == "" || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
					i++
					continue
				}
				break
			}
			i--
			continue
		}
		out = append(out, strings.TrimRight(lines[i], " \t"))
	}
	if len(ov) > 0 {
		keys := make([]string, 0, len(ov))
		for k := range ov {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out = append(out, "overrides:")
		for _, k := range keys {
			out = append(out, fmt.Sprintf("  %q: %q", k, ov[k]))
		}
	}
	body := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if strings.TrimSpace(body) == "" {
		if removed || len(ov) == 0 {
			_ = os.Remove(p)
		}
		return nil
	}
	return os.WriteFile(p, []byte(body+"\n"), 0o644)
}

// harnessBackupSuffix 更新前快照文件后缀（更新失败时用于回退到上一可运行版本）。
const harnessBackupSuffix = ".dshbak"

// snapshotHarness 快照当前可运行版本：备份 package.json / pnpm-lock.yaml / pnpm-workspace.yaml，
// 并把 node_modules 整体改名备份（同盘 rename，秒级完成；更新失败可本地直接移回，不依赖网络）。
// 返回是否成功备份了 node_modules。
// 注意：pnpm-workspace.yaml 必须纳入备份——pnpm ≥ v10 的 setHarnessFamilyOverrides 在更新期把
// 家族逐包 overrides 与 minimumReleaseAgeExclude 写入该文件，快照不覆盖它则回滚后
// 残留坏版本的 overrides，下次任何 pnpm install 会把可用树再次拉向失败版本。
// 调用前必须先 killServer()，否则运行中的服务会占用 node_modules 内文件导致改名失败。
func snapshotHarness() (nodeModulesBacked bool) {
	for _, name := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"} {
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

// dropHarnessLockfile 删除 harness 目录的 pnpm-lock.yaml（回退路径由 snapshotHarness 留下的
// .dshbak 副本还原），使随后的 pnpm add/install 不再复用旧解析。
//
// 旧锁文件是「新旧混装」的另一半根因：家族核心包只是插件的 peer、未在根 package.json 声明，
// pnpm 会沿用锁文件里已钉死的旧条目（2026-09-10 实证：node_modules 已被改名备份、锁文件仍在，
// pnpm add 0.1.5-rc.1 之后家族核心包仍是 0.1.1-rc.2 → 启动时插件树 ESM 缺导出）。
func dropHarnessLockfile() {
	p := filepath.Join(harnessDir, "pnpm-lock.yaml")
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		log.Printf("harness update: remove stale pnpm-lock.yaml failed: %v", err)
		return
	}
	log.Printf("harness update: dropped stale pnpm-lock.yaml (force fresh resolution)")
}

// harnessInstallHint 把 pnpm 安装失败输出归类为可执行的中文提示（未命中返回空串）。
// 依据 2026-09-10 实测：上游 @deepseek-ai 家族分批发布时，全新解析会选中半发布的新版本、
// 再在其缺失依赖上报 ERR_PNPM_NO_MATCHING_VERSION（^0.1.5-rc.2 尚无对应包）；
// registry 抖动则表现为 Socket timeout / ECONNRESET（本机 pnpm 设了 fetch-retries=0，
// 坏网络下一次失败即放弃）。
func harnessInstallHint(out string) string {
	switch {
	case strings.Contains(out, "ERR_PNPM_NO_MATCHING_VERSION"):
		return "原因判断：registry 上该版本家族尚未发布完整（或目标版本不存在）。官方分批发布时会出现，稍后重试即可。"
	case strings.Contains(out, "ERR_PNPM_META_FETCH_FAIL"),
		strings.Contains(out, "ETIMEDOUT"),
		strings.Contains(out, "ECONNRESET"),
		strings.Contains(out, "Socket timeout"):
		return "原因判断：registry 请求超时/连接被重置（网络或官方源不稳），稍后重试即可。"
	}
	return ""
}

// harnessFamilyMismatches 列出 node_modules 顶层中版本与 ver 不一致的 harness 家族包
// （@deepseek-ai/dsh*），用于 pnpm install 步骤失败时判定「树是否真的不可用」。
//
// 只把「顶层已安装」的包当作不一致证据：锁文件里出现但未在顶层安装的家族包（纯传递依赖）
// 属正常形态，不计入。@deepseek-ai/dsh 入口包例外——它必须存在，否则树不可用。
// ver 非具体版本（空 / "latest"）时返回 nil：@latest 由各包自行解析，无可比对目标。
func harnessFamilyMismatches(dir, ver string) []string {
	ver = strings.TrimPrefix(strings.TrimSpace(ver), "v")
	if ver == "" || ver == "latest" {
		return nil
	}
	names := append([]string{"@deepseek-ai/dsh"}, harnessFamilyNames(dir)...)
	// 顶层实际安装的家族包同样纳入（锁文件/`.pnpm` 可能缺失或不完整——如安装中断、
	// 或包只作为 peer 被提升到顶层；一致性判定必须覆盖看得见的每一个）。
	if entries, err := os.ReadDir(filepath.Join(dir, "node_modules", "@deepseek-ai")); err == nil {
		for _, e := range entries {
			name := "@deepseek-ai/" + e.Name()
			if strings.HasPrefix(name, harnessFamilyPrefix) {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	var bad []string
	prev := ""
	for _, name := range names {
		if name == prev {
			continue
		}
		prev = name
		data, err := os.ReadFile(filepath.Join(dir, "node_modules", filepath.FromSlash(name), "package.json"))
		if err != nil {
			if name == "@deepseek-ai/dsh" {
				bad = append(bad, name+"（未安装）")
			}
			continue
		}
		var m struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(data, &m) != nil {
			bad = append(bad, name+"（版本不可读）")
			continue
		}
		if got := strings.TrimPrefix(strings.TrimSpace(m.Version), "v"); got != ver {
			bad = append(bad, fmt.Sprintf("%s@%s", name, got))
		}
	}
	return bad
}

// outputTail 取命令输出尾部（最多 n 字节，超出部分以 … 标记），供失败弹窗直接给出根因。
func outputTail(out string, n int) string {
	tail := strings.TrimSpace(out)
	if len(tail) > n {
		tail = "…" + tail[len(tail)-n:]
	}
	return tail
}

// restoreHarnessSnapshot 回退到快照版本：还原 package.json / pnpm-lock.yaml / pnpm-workspace.yaml；
// 有 node_modules 备份直接移回（秒级），否则按还原后的锁文件重装。
// pnpm-workspace.yaml 无快照时（更新前不存在、更新期由 setHarnessFamilyOverrides 新建）删除其残留——
// 否则失败版本写入的 overrides / minimumReleaseAgeExclude 会污染回退后的可用树。
func restoreHarnessSnapshot(hadNodeModulesBackup bool) {
	for _, name := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"} {
		src := filepath.Join(harnessDir, name+harnessBackupSuffix)
		if data, err := os.ReadFile(src); err == nil {
			_ = os.WriteFile(filepath.Join(harnessDir, name), data, 0o644)
		} else if name == "pnpm-workspace.yaml" {
			_ = os.Remove(filepath.Join(harnessDir, name))
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
	for _, name := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"} {
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

// unifiedLogSize 当前统一日志字节数（健康校验按追加段扫描，避免把历史日志的报错误判进来）。
func unifiedLogSize() int64 {
	fi, err := os.Stat(unifiedLogPath())
	if err != nil {
		return 0
	}
	return fi.Size()
}

// serverLogHasBootErrors 扫描统一日志从 offset 起的追加段（仅 [server] 模块行），
// 判断本次服务启动是否出现加载报错——只扫 server 行避免把 pnpm 输出（ERR_PNPM_* 等）
// 误判为服务启动失败。
func serverLogHasBootErrors(offset int64) bool {
	s := serverLogLines(offset)
	for _, m := range harnessBootErrorMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// 健康校验窗口。常规重启 10s 足够（加载错误通常数秒内刷出）；但**改版后**（harness 更新/
// 重置/回退，或上一次改版尚未经冷启动验证）留 60s：0.1.5-rc.1 混装树实测 22:38:13 启动、
// 22:38:32 被 10s 窗口判成功并提升 LKG、22:38:58（启动后 45s）才刷出 plugin tree failed to
// load —— 短窗口不仅误判成功，还把「唯一可回退的 LKG」当成已验证状态，事后服务直接停摆。
const (
	bootVerifySettle                   = 10 * time.Second
	bootVerifySettleAfterHarnessChange = 60 * time.Second
)

// verifyServerBoot 就绪后的健康校验：周期扫描 server.log 从 before 起的追加段，并监听进程退出。
// 覆盖“HTTP 已就绪但插件/依赖加载错误更晚刷出”的漏判（错误常迟于就绪数秒出现，曾导致
// 混装版本的异常启动被当作成功、甚至把 LKG 误清）。任一命中立即判失败；窗口结束仍未命中为健康。
func verifyServerBoot(before int64, exited <-chan error) bool {
	return verifyServerBootWithin(before, exited, bootVerifySettle)
}

// verifyServerBootAfterChange 改版路径（harness 更新/重置/回退）的健康校验：用加长窗口。
func verifyServerBootAfterChange(before int64, exited <-chan error) bool {
	return verifyServerBootWithin(before, exited, bootVerifySettleAfterHarnessChange)
}

// verifyServerBootOnColdStart 冷启动（双击拉起）健康校验：仅当存在 LKG（= 上次改版尚未经
// 冷启动验证通过）时用加长窗口，常规启动仍走短窗口——不为此让每次启动多等一分钟。
func verifyServerBootOnColdStart(before int64, exited <-chan error) bool {
	if hasAnyLkg() {
		return verifyServerBootWithin(before, exited, bootVerifySettleAfterHarnessChange)
	}
	return verifyServerBootWithin(before, exited, bootVerifySettle)
}

// verifyServerBootWithin 健康校验实现：窗口内轮询「追加段加载错误 / 进程退出」，任一命中即失败。
func verifyServerBootWithin(before int64, exited <-chan error, settle time.Duration) bool {
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
// 拉起前先轮转 server.log（rotateServerLog），保证本次启动的现场独立成档、校验只扫本次段。
func restartAndVerifyServer() bool {
	return restartAndVerifyServerWithin(bootVerifySettle)
}

// restartAndVerifyServerAfterChange 改版路径（harness 更新/重置/回退）的重启校验：用加长窗口，
// 覆盖迟至启动后 45s 才刷出的加载错误（见 bootVerifySettleAfterHarnessChange 说明）。
func restartAndVerifyServerAfterChange() bool {
	return restartAndVerifyServerWithin(bootVerifySettleAfterHarnessChange)
}

// restartAndVerifyServerWithin 重启并健康校验（窗口由调用方指定）。
func restartAndVerifyServerWithin(settle time.Duration) bool {
	killServer()
	time.Sleep(1 * time.Second)
	if serverResponding(webURL) {
		return true // 端口已有可用服务（异常残留场景），视为可用
	}
	before := rotateServerLog()
	started, exitCh := startServer()
	if !started {
		return false
	}
	if ok, _ := waitForServerReady(webURL, exitCh, startupTimeout); !ok {
		return false
	}
	return verifyServerBootWithin(before, exitCh, settle)
}

// rollbackUpdate 更新失败处理：停止服务 → 回退快照 → 重启校验 → 弹窗报告。
func rollbackUpdate(splash *SplashState, prev string, hadNMBackup bool, reason string) {
	splash.Update(T("更新失败，正在回退到上一可用版本…"), 0.6)
	killServer()
	restoreHarnessSnapshot(hadNMBackup)
	splash.Update(T("正在重启服务…"), 0.85)
	restartAndVerifyServerAfterChange()
	splash.Close()
	msg := "DeepSeek Harness 更新失败（" + reason + "），已回退到"
	if prev != "" {
		msg += " v" + prev + "。"
	} else {
		msg += "上一可用版本。"
	}
	msg += "\n\n日志：" + unifiedLogPath()
	showMessageBox(msg, appName)
}

// runHarnessUpdate 更新 DeepSeek Harness（npm 模式更新 @deepseek-ai/dsh；源码模式 git pull+install+build），
// 完成后重启服务并校验；失败自动回退到上一可运行版本。异步执行，带进度窗口。
func runHarnessUpdate(latest string) {
	splash := startSplash(T("正在更新 DeepSeek Harness…"))
	harnessOpBusy.Store(true) // 与插件操作批处理互斥（两者都会停服 + 跑 pnpm）
	defer harnessOpBusy.Store(false)
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
				"日志："+unifiedLogPath(), appName)
			emitUpdateDone(false, false, "npm registry 上未找到目标版本")
			return
		}
	case sourceMode:
		if !isGitHarnessDir() {
			splash.Close()
			showMessageBox("无法更新 DeepSeek Harness：\n\n该 Harness 目录不是 git 仓库（可能是 zip 解压或整目录复制而来），无法走源码更新。\n"+
				"请使用 git clone 的 Harness 源码目录，或恢复 npm 预构建形态（当前目录缺少 @deepseek-ai/dsh 入口）。\n\n目录："+harnessDir, appName)
			emitUpdateDone(false, false, "Harness 目录不是 git 仓库")
			return
		}
	default:
		splash.Close()
		showMessageBox("无法识别 DeepSeek Harness 安装形态（npm 预构建或 git 源码 checkout）。\n\n目录："+harnessDir, appName)
		emitUpdateDone(false, false, "无法识别 Harness 安装形态")
		return
	}

	// 1) 先停止服务（否则运行中的 node 进程会占用 node_modules 文件，快照/回退改名会失败）
	killServer()
	time.Sleep(1 * time.Second)

	// 2) 快照当前可运行版本（本地回退用）
	splash.Update(T("正在备份当前版本…"), 0.15)
	hadNMBackup := snapshotHarness()

	// 3) 安装新版本（失败原因按分支细化，供回退弹窗明确展示）
	var err error
	reason := "安装失败"
	installNote := "" // 安装阶段的非致命说明（供应链策略复核等），并入成功文案
	if npmMode {
		splash.Update(T("正在更新 DeepSeek Harness 依赖…"), 0.35)
		// 安装检查到的新版本而非 @latest：npm 的 prerelease（如 0.1.2-alpha.2）不会成为 latest 标签，
		// 用 @latest 会装回旧版导致“更新后仍是旧版本”。
		ver := latest
		if ver == "" {
			ver = "latest"
		}
		// 整族钉到目标版本 + 丢掉旧锁文件，二者缺一都会留下「新版插件 + 旧版核心包」的混装树：
		//   - 家族核心包只是插件的 peer、未在根 package.json 声明，旧锁文件会把它们钉在旧版；
		//   - 只 pnpm add 根包时 pnpm 复用旧解析，同样停在旧版（2026-09-10 实证 0.1.1-rc.2 混装）。
		// 精确包名钉版同时兜住「上游分批发布」窗口（caret 范围会选中半发布的新版本）。ver 为
		// 非具体版本（"latest"）时不钉版——"latest" 会指向家族里各包自己的 latest 标签。
		family := []string(nil)
		if ver != "latest" {
			family = harnessFamilyNames(harnessDir)
		}
		dropHarnessLockfile()
		log.Printf("harness update: pin family to %s (%d packages)", ver, len(family))
		err = setHarnessFamilyOverrides(harnessDir, ver, family)
		var addOut string
		if err == nil {
			addOut, err = runHarnessCmdTail(pnpmCmd(), "add", "@deepseek-ai/dsh@"+ver, "--save-exact")
			if err != nil && len(family) > 0 && strings.Contains(addOut, "ERR_PNPM_NO_MATCHING_VERSION") {
				// 钉版把某个家族包钉到目标版本不存在的组合（目标版本家族未发全/包已改名）：
				// 去掉钉版重试一次，让 pnpm 自行解析。
				log.Printf("harness update: pinned resolution failed, retrying without family pin")
				if cerr := setHarnessFamilyOverrides(harnessDir, ver, nil); cerr == nil {
					addOut, err = runHarnessCmdTail(pnpmCmd(), "add", "@deepseek-ai/dsh@"+ver, "--save-exact")
				}
			}
		}
		if err == nil {
			// 全量 install 重新 reconcile 整个依赖树，避免只改根依赖导致的新旧版本混装。
			// 注意：本步失败不必然等于「树不可用」——先看是否为供应链策略复核拦截（下方分支）。
			splash.Update(T("正在安装依赖…"), 0.55)
			var instOut string
			instOut, err = runHarnessCmdTail(pnpmCmd(), "install")
			if err != nil && supplyChainViolation(instOut) {
				// 供应链发布年龄校验（minimumReleaseAge）：pnpm add 在解析期已把整族写入
				// minimumReleaseAgeExclude 并装好（实测 add 成功装 497 包），随后的 install
				// 复核仍会拒绝这些 lockfile 条目——是「策略复核」失败，不是「安装」失败。
				// 2026-09-11 实证：旧代码据 exit status 判定依赖安装失败并整体回退，而「重置」
				// 路径只跑 add、不跑 install，所以重置能成功、检查更新不能——用户只能绕道重置。
				log.Printf("harness update: install blocked by supply-chain age policy, retrying with bypass")
				splash.Update(T("依赖校验被供应链策略拦截，正在跳过校验重试…"), 0.6)
				instOut, err = runHarnessCmdTail(pnpmCmd(), "install", "--config.minimumReleaseAge=0")
			}
			if err != nil {
				// 仍失败：判定以「家族版本一致性」为准，而非 exit status——策略复核失败但整族
				// 已装到目标版本的树可用（随后的启动健康校验才是真正的验收关口，失败仍会回退）。
				bad := harnessFamilyMismatches(harnessDir, ver)
				if supplyChainViolation(instOut) && len(bad) == 0 && isNpmHarnessReady() {
					log.Printf("harness update: install policy check failed but family is consistent at %s, continue", ver)
					installNote = "（依赖复核被供应链策略拦截，已按家族版本一致性确认安装结果）"
					err = nil
				} else {
					reason = "依赖安装失败（详见日志末尾）"
					if hint := harnessInstallHint(instOut); hint != "" {
						reason = hint
					}
					if len(bad) > 0 {
						if len(bad) > 6 {
							bad = append(bad[:6], fmt.Sprintf("等 %d 个", len(bad)))
						}
						reason += "；家族版本不一致：" + strings.Join(bad, "、")
					}
				}
			}
		} else {
			reason = "安装指定版本失败（详见日志末尾）"
			if hint := harnessInstallHint(addOut); hint != "" {
				reason = hint
			}
		}
	} else {
		prevHead := runHarnessCmdCapture("git", "rev-parse", "HEAD")
		splash.Update(T("正在拉取 DeepSeek Harness 最新代码…"), 0.3)
		err = runHarnessCmd("git", "pull")
		if err == nil {
			splash.Update(T("正在安装 harness 依赖…"), 0.5)
			err = runHarnessCmd(pnpmCmd(), "install")
			if err == nil {
				splash.Update(T("正在构建 harness 前端…"), 0.7)
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
			splash.Update(T("正在回退代码…"), 0.6)
			_ = runHarnessCmd("git", "reset", "--hard", prevHead)
			_ = runHarnessCmd(pnpmCmd(), "install")
			_ = runHarnessCmd(pnpmCmd(), "run", "build")
		}
	}
	if err != nil {
		rollbackUpdate(splash, prev, hadNMBackup, reason)
		emitUpdateDone(false, false, reason)
		return
	}

	// 3.5) 与「重置后重新导入插件」等价的一步：harness 换版后，各 profile 的插件树仍按旧
	//      harness 解析（pnpm 的 .modules.yaml / junction / 虚拟商店都是旧一代），必须重新
	//      pnpm install 对齐——这正是「重新导入插件」在批末做的事（reconcileProfileDeps）。
	//      用户实证：更新后直接启动会失败，重置+清插件+重新导入才能跑起来；差异就在这一步。
	//      对齐失败不阻断更新：记录后交由启动健康校验与插件自愈兜底（离线 / 本地链接失效时
	//      不让整个更新白跑）。
	alignNote := ""
	if npmMode {
		dirs := pluginProfileDirsWithDeps()
		for i, dir := range dirs {
			splash.Update(fmt.Sprintf("正在对齐插件依赖（%d/%d）…", i+1, len(dirs)), 0.78)
			if derr := reconcileProfileDeps(dir); derr != nil {
				log.Printf("harness update: profile reconcile failed (%s): %v", dir, derr)
				alignNote += "\n· 环境 " + filepath.Base(dir) + " 的插件依赖未对齐（已由启动校验兜底）"
			}
		}
	}

	// 4) 重启并健康校验（就绪 + 启动日志无报错）。
	//    失败时先尝试「禁用启动日志点名的用户插件」换取新版本可启动（不兼容自愈）；
	//    点名禁用未奏效或无点名嫌疑时，按用户决策（尽量保留新版本、不回退）禁用全部
	//    已激活的用户插件再试；仍失败（核心故障）→ 整体回退到上一版本。
	splash.Update(T("正在重启服务…"), 0.85)
	if !restartAndVerifyServerAfterChange() {
		splash.Update(T("启动校验失败，正在排查不兼容插件…"), 0.9)
		var profileDirs []string
		for _, pf := range enumeratePluginProfiles() {
			profileDirs = append(profileDirs, pf.dir)
		}
		disabled, ok := disableBootSuspects(profileDirs)
		allDisabled := false
		if !ok {
			splash.Update(T("服务启动受阻，正在尝试禁用部分插件…"), 0.93)
			disabled, ok = disableAllUserPlugins(profileDirs)
			allDisabled = ok
		}
		if ok {
			// 保留新版本：harness LKG 提升到更新前快照（未来失败回退旧版时插件完整可用的状态）
			promoteHarnessLkg(prev)
			splash.Close()
			names := make([]string, 0, len(disabled))
			for _, d := range disabled {
				names = append(names, d.Name)
			}
			logUI("更新 Harness 完成（含不兼容插件禁用）",
				fmt.Sprintf("v%s | 禁用 %s", latest, strings.Join(names, "、")))
			msg := fmt.Sprintf("DeepSeek Harness 已更新到 %s，服务已重启。\n\n以下插件与新版不兼容，已自动禁用"+
				"（保留记录，可在「关于页 → 已安装插件」中检查更新后重新启用）：\n· %s",
				withV(latest), strings.Join(names, "、"))
			if allDisabled {
				msg = fmt.Sprintf("DeepSeek Harness 已更新到 %s，服务已重启。\n\n未能定位到具体的不兼容插件，"+
					"已禁用全部已激活的用户插件以保证新版启动（保留记录，可在「关于页 → 已安装插件」中逐个重新启用）：\n· %s",
					withV(latest), strings.Join(names, "、"))
			}
			msg += installNote + alignNote
			showMessageBox(msg, appName)
			emitUpdateDone(true, false, "")
			return
		}
		// 两级禁用均未能换取启动（核心故障）→ 整体回退，原因需写明已尝试禁用插件，
		// 避免用户误以为「直接回退、未尝试保留新版本」。
		rbReason := "新版本启动失败（已尝试排查并禁用不兼容插件，仍无法启动——疑为核心故障）"
		rollbackUpdate(splash, prev, hadNMBackup, rbReason)
		emitUpdateDone(false, false, rbReason+"，已回退")
		return
	}

	// 5) 成功：快照提升为 LKG（保留到下次冷启动验证通过后再清理——启动失败时可自动回退到该状态）
	promoteHarnessLkg(prev)
	splash.Close()
	msg := fmt.Sprintf("DeepSeek Harness 已更新到 %s，服务已重启。", withV(latest))
	if !isStableVersion(strings.TrimPrefix(latest, "v")) {
		msg += "\n\n提示：预发布版本可能与已装插件不兼容；如遇异常，可用「重置服务」回退到上一个正常运行的版本。"
	}
	showMessageBox(msg+installNote+alignNote, appName)
	emitUpdateDone(true, false, "")
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
// macOS 共用入口：登记取消句柄（前端 splash「取消更新」→ cancelActiveUpdate 中断下载）；
// 进入「替换并重启」前置位 updateFinalizing，取消在该阶段不再生效。
// onProgress 进度回调（文本 + 0~1 分度；nil 表示不回调），供 splash 视图逐段刷新——
// 此前 macOS 更新全程无回调，下载耗时 1-2 分钟界面停在「正在准备更新」不动。
func downloadAndApplyUpdate(rel *latestRelease, onProgress func(text string, pct float64)) error {
	progress := func(text string, pct float64) {
		if onProgress != nil {
			onProgress(text, pct)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registerActiveUpdate(cancel)
	defer clearActiveUpdate()

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
	progress(TF("正在下载 %s…", assetName), 0.08)
	if err := downloadFileWithProgress(ctx, zipURL, zipPath, func(pct float64) {
		progress(fmt.Sprintf("正在下载 %s（%.0f%%）…", assetName, pct*100), 0.08+0.52*pct)
	}); err != nil {
		return fmt.Errorf("下载更新包失败：%w", err)
	}
	if sumURL != "" {
		progress(T("正在校验更新包…"), 0.62)
		sumPath := filepath.Join(tmp, "SHA256SUMS.txt")
		if err := downloadFileTo(ctx, sumURL, sumPath); err != nil {
			log.Printf("checksum file unavailable: %v", err)
		} else if err := verifyChecksum(zipPath, assetName, sumPath); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	progress(T("正在解压安装…"), 0.68)
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
	if ctx.Err() != nil {
		return ctx.Err()
	}
	setUpdateFinalizing() // 进入替换阶段：不再接受取消
	progress(T("正在更新程序…"), 0.9)
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
// 先查根级；再兼容解压目录带一层子目录（如历史包 `dist/` 前缀布局）的形态，防打包回归。
func updatePayloadPath(extractDir string) (string, error) {
	if runtime.GOOS == "windows" {
		p := filepath.Join(extractDir, "dsh-systray.exe")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		if m, _ := filepath.Glob(filepath.Join(extractDir, "*", "dsh-systray.exe")); len(m) > 0 {
			if _, err := os.Stat(m[0]); err == nil {
				return m[0], nil
			}
		}
		return "", fmt.Errorf("更新包中缺少 dsh-systray.exe")
	}
	p := filepath.Join(extractDir, "dsh-systray.app")
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return p, nil
	}
	if m, _ := filepath.Glob(filepath.Join(extractDir, "*", "dsh-systray.app")); len(m) > 0 {
		if fi, err := os.Stat(m[0]); err == nil && fi.IsDir() {
			return m[0], nil
		}
	}
	return "", fmt.Errorf("更新包中缺少 dsh-systray.app")
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

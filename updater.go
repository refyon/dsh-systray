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
	updateRepoOwner   = "refyon"
	updateRepoName    = "dsh-systray"
	updateCheckDelay  = 30 * time.Second // 启动后 30 秒静默检查新版本
	updateAPITimeout  = 8 * time.Second  // 版本接口单候选超时（直连失败回退镜像）
	updateDLTimeout   = 5 * time.Minute  // 单镜像单次下载上限
	updateMaxBodySize = 4 << 20          // 版本接口响应上限 4MB
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

// startAutoUpdateCheck 启动后台 goroutine：30 秒后静默检查新版本，发现新版本时提示用户更新。
func startAutoUpdateCheck() {
	go func() {
		time.Sleep(updateCheckDelay)
		autoCheckUpdate()
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
	if !askUpdateDialog(strings.TrimPrefix(rel.TagName, "v")) {
		log.Printf("user declined update %s", rel.TagName)
		return
	}
	startUpdateApply(rel)
}

// startUpdateApply 应用更新：各平台实现。Windows 派生独立更新进程（退出主程序→进度窗口下载/安装→自动重启）；
// macOS 进程内下载并用辅助脚本替换 .app 后重启。声明于此，由 platform_*.go 实现。

// checkForUpdatesManual 手动检查更新（设置页触发）：查询期间显示进度反馈，有新版弹确认并更新，无新版提示已是最新。
func checkForUpdatesManual() {
	if appVersion == "" || appVersion == "dev" {
		showMessageBox("当前为开发版本（dev），未启用自动更新。", appName)
		return
	}
	// 版本查询期间弹出进度窗口显示「正在查询最新版本」（Windows 进度窗口 / macOS 系统通知）
	splash := startSplash("正在查询最新版本…")
	splash.Update("正在查询最新版本…", 0.3)
	rel, err := fetchLatestRelease()
	if err != nil {
		splash.Close()
		showMessageBox("检查更新失败：\n"+err.Error()+"\n\n请检查网络后重试。", appName)
		return
	}
	splash.Close()
	if !isNewerVersion(rel.TagName, appVersion) {
		showMessageBox(fmt.Sprintf("当前已是最新版本（%s）。", withV(appVersion)), appName)
		return
	}
	if askUpdateDialog(strings.TrimPrefix(rel.TagName, "v")) {
		// 异步执行，避免阻塞设置页 UI 线程（设置窗口在下载期间保持可响应/可关闭）。
		go startUpdateApply(rel)
	}
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

// isNewerVersion 判断最新标签是否比当前版本新（忽略标签前导 v）。
func isNewerVersion(latest, current string) bool {
	return compareVersions(strings.TrimPrefix(latest, "v"), strings.TrimPrefix(current, "v")) > 0
}

// compareVersions 简单数字版本比较：按 "." 分段逐段比较，忽略预发布后缀（如 -rc.1）。
func compareVersions(a, b string) int {
	pa, pb := splitVersion(a), splitVersion(b)
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
	return 0
}

func splitVersion(v string) []string {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	return strings.Split(v, ".")
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

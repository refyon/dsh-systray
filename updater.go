package main

import (
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
	"time"
)

// appVersion 当前程序版本，由构建注入：-X main.appVersion=vX.Y.Z。
// 本地开发 / CI 手动触发（非 tag）构建时为 "dev"，此时跳过自动更新检查。
var appVersion = "dev"

const (
	updateRepoOwner   = "refyon"
	updateRepoName    = "dsh-systray"
	updateCheckDelay  = 30 * time.Second // 启动后 30 秒静默检查新版本
	updateAPITimeout  = 20 * time.Second
	updateDLTimeout   = 10 * time.Minute
	updateMaxBodySize = 4 << 20 // 版本接口响应上限 4MB
)

// updateMirrors 下载地址前缀：先直连 GitHub，失败再依次回退镜像（国内网络友好）。
var updateMirrors = []string{"", "https://mirror.ghproxy.com/"}

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
		log.Printf("update check: current v%s is up to date (latest %s)", appVersion, rel.TagName)
		return
	}
	log.Printf("update available: %s (current %s)", rel.TagName, appVersion)
	if !askUpdateDialog(strings.TrimPrefix(rel.TagName, "v")) {
		log.Printf("user declined update %s", rel.TagName)
		return
	}
	if err := downloadAndApplyUpdate(rel); err != nil {
		log.Printf("update failed: %v", err)
		showMessageBox("更新失败：\n"+err.Error()+"\n\n请稍后重试，或前往 GitHub Releases 手动下载。", appName)
	}
}

// fetchLatestRelease 查询 GitHub Releases 最新版本。
func fetchLatestRelease() (*latestRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", updateRepoOwner, updateRepoName)
	client := &http.Client{Timeout: updateAPITimeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "dsh-systray/"+appVersion)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("版本接口返回 HTTP %d", resp.StatusCode)
	}
	var rel latestRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, updateMaxBodySize)).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
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
	if err := downloadFileTo(zipURL, zipPath); err != nil {
		return fmt.Errorf("下载更新包失败：%w", err)
	}
	if sumURL != "" {
		sumPath := filepath.Join(tmp, "SHA256SUMS.txt")
		if err := downloadFileTo(sumURL, sumPath); err != nil {
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

// downloadFileTo 下载到本地文件；直连失败时依次回退镜像前缀。
func downloadFileTo(url, dest string) error {
	var lastErr error
	for _, prefix := range updateMirrors {
		if err := downloadOnce(prefix+url, dest); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func downloadOnce(url, dest string) error {
	client := &http.Client{Timeout: updateDLTimeout}
	req, err := http.NewRequest("GET", url, nil)
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
	_, err = io.Copy(out, resp.Body)
	return err
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
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := os.Executable(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

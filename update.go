package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// version 由构建注入（-ldflags "-X main.version=vX.Y.Z"，见 build.ps1 / release.yml）；
// 未注入时为 "dev"，dev 构建跳过更新检查（本地开发构建不误提示升级）。
var version = "dev"

// updateSwapFlag 自替换模式参数：<新exe> --update-swap <旧进程PID> <目标exe路径>。
const updateSwapFlag = "--update-swap"

// updateAPI 查询最新 Release（GitHub Releases latest）。
const updateAPI = "https://api.github.com/repos/refyon/dsh-systray/releases/latest"

// updateMirrors 下载地址前缀：先直连 GitHub，失败再依次回退镜像（国内网络友好）。
var updateMirrors = []string{"", "https://mirror.ghproxy.com/"}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type releaseInfo struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
	windows releaseAsset // 匹配到的 windows zip 资源（非 JSON 字段）
}

// checkForUpdate 查询最新版本；返回 nil 表示无更新或检查失败（失败不阻塞启动）。
func checkForUpdate() *releaseInfo {
	if version == "" || version == "dev" {
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(updateAPI)
	if err != nil {
		log.Printf("update check failed: %v", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("update check: HTTP %d", resp.StatusCode)
		return nil
	}
	var rel releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		log.Printf("update check: decode failed: %v", err)
		return nil
	}
	if !isNewerVersion(rel.TagName, version) {
		return nil
	}
	for _, a := range rel.Assets {
		if strings.HasPrefix(a.Name, "dsh-systray-windows") && strings.HasSuffix(a.Name, ".zip") {
			rel.windows = a
			return &rel
		}
	}
	log.Printf("update check: no windows asset in %s", rel.TagName)
	return nil
}

// runUpdateFlow 启动时检查新版本；用户确认升级则中断正常启动流程，
// 下载新版本 → 自替换 → 重启（spawnSwapper 后 os.Exit，正常不会返回）。
func runUpdateFlow(splash *SplashState) bool {
	if !updateSupported() {
		return false
	}
	splash.Update("正在检查更新…", 0.01)
	rel := checkForUpdate()
	if rel == nil {
		return false
	}
	if !confirmUpdate(rel.TagName) {
		log.Printf("update %s skipped by user", rel.TagName)
		return false
	}
	splash.Update("正在下载新版本…", 0.03)
	newExe, err := downloadUpdate(rel, splash)
	if err != nil {
		splash.Close()
		showMessageBox("更新失败："+err.Error()+"\n\n将继续以当前版本启动。", appName)
		return false
	}
	log.Printf("self-update to %s ready, swapping and restarting", rel.TagName)
	splash.Update("更新已就绪，正在重启…", 0.97)
	spawnSwapper(newExe)
	os.Exit(0)
	return true
}

// downloadUpdate 下载 Release 资产（直连 + 镜像回退），解压出新 exe 并返回其路径。
func downloadUpdate(rel *releaseInfo, splash *SplashState) (string, error) {
	dir, err := os.MkdirTemp("", "dsh-systray-update")
	if err != nil {
		return "", err
	}
	zipPath := filepath.Join(dir, "update.zip")
	var lastErr error
	for _, prefix := range updateMirrors {
		if err := downloadToFile(prefix+rel.windows.URL, zipPath, rel.windows.Size, splash); err == nil {
			lastErr = nil
			break
		}
		lastErr = err
	}
	if lastErr != nil {
		return "", fmt.Errorf("下载失败：%w", lastErr)
	}
	cmd := exec.Command("tar", "-xf", zipPath, "-C", dir)
	hideCmdWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("解压失败：%v %s", err, string(out))
	}
	exePath := filepath.Join(dir, "dsh-systray.exe")
	if _, err := os.Stat(exePath); err != nil {
		return "", fmt.Errorf("更新包中未找到 dsh-systray.exe")
	}
	return exePath, nil
}

// downloadToFile 下载到本地文件，带进度回调。
func downloadToFile(url, dest string, total int64, splash *SplashState) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
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
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if splash != nil && total > 0 {
				pct := float64(done) / float64(total)
				splash.Update(fmt.Sprintf("正在下载新版本（%.0f%%）…", pct*100), 0.03+0.9*pct)
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

// isNewerVersion a（如 v1.2.3）是否比 b 新；仅比较数字段，解析失败按 0 处理。
func isNewerVersion(a, b string) bool {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseVersion(s string) [3]int {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	var out [3]int
	for i, part := range strings.SplitN(s, ".", 3) {
		out[i], _ = strconv.Atoi(part)
	}
	return out
}

package main

// download_candidates_test.go：下载候选与失败原因的回归测试。
//
// 背景（2026-09-27 现场问题②）：镜像前缀（ghfast.top 等）与自建中转 Worker 只代理 GitHub 路径，
// 却被无条件拼到所有下载地址前——桌面端安装包地址是官方 CDN（download.deepseek.com），
// 于是六次尝试全部打在代理上、直连从未被使用，用户看到的是「下载桌面端安装包失败：HTTP 403」。
// 本文件守住「按 host 分流」与「失败原因带上试过的源」两条不变量。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// withDownloadMirrorConfig 临时设置镜像配置并在测试后还原。
func withDownloadMirrorConfig(t *testing.T, mirrorBaseValue, override string) {
	t.Helper()
	prevBase, prevOverride := mirrorBase, updateMirrorOverride
	mirrorBase, updateMirrorOverride = mirrorBaseValue, override
	t.Cleanup(func() { mirrorBase, updateMirrorOverride = prevBase, prevOverride })
}

// withDefaultMirrorList 只保留出厂镜像链（隔离用户 config.json 的影响）。
func withDefaultMirrorList(t *testing.T) {
	t.Helper()
	prev := updateMirrors
	updateMirrors = []string{"", "https://ghfast.top/"}
	t.Cleanup(func() { updateMirrors = prev })
}

// TestDownloadCandidatesNonGitHubStaysDirect 非 GitHub 地址只用自身：
// 不得带任何镜像前缀（前缀只会 403，白耗预算且泄漏地址给第三方）。
func TestDownloadCandidatesNonGitHubStaysDirect(t *testing.T) {
	withDownloadMirrorConfig(t, "https://mirror.example.com", "https://override.example.com/")
	withDefaultMirrorList(t)

	for _, u := range []string{
		"https://download.deepseek.com/dsh-desk/bin/win-x64/deepseek-harness-0.1.7-rc.2-win-x64.exe",
		"https://cdn.jsdelivr.net/gh/googlefonts/noto-cjk@main/Sans/Variable/TTF/Subset/NotoSansSC-VF.ttf",
		"http://127.0.0.1:8080/local.zip",
	} {
		got := downloadCandidates(u)
		if len(got) != 1 || got[0] != u {
			t.Fatalf("非 GitHub 地址应只保留直连：%q → %v", u, got)
		}
	}
}

// TestDownloadCandidatesGitHubKeepsMirrorChain GitHub 来源保留完整镜像回退链（含直连）。
func TestDownloadCandidatesGitHubKeepsMirrorChain(t *testing.T) {
	withDownloadMirrorConfig(t, "https://mirror.example.com", "")
	withDefaultMirrorList(t)

	u := "https://github.com/refyon/dsh-systray/releases/download/v1.0.0/dsh-systray-windows-x64.zip"
	got := downloadCandidates(u)
	if len(got) != 3 {
		t.Fatalf("GitHub 地址应有 3 个候选（自建中转 + 直连 + ghfast），实际 %v", got)
	}
	if got[0] != "https://mirror.example.com/gh/"+u {
		t.Fatalf("自建中转应排最前：%v", got)
	}
	if got[1] != u {
		t.Fatalf("直连应作为回退保留：%v", got)
	}
	if got[2] != "https://ghfast.top/"+u {
		t.Fatalf("第三方镜像应排在直连之后：%v", got)
	}
}

// TestDownloadCandidatesEmpty 空地址不产生候选（调用方据此不用发请求）。
func TestDownloadCandidatesEmpty(t *testing.T) {
	if got := downloadCandidates("   "); len(got) != 0 {
		t.Fatalf("空地址不应有候选：%v", got)
	}
}

// TestDownloadFileNonGitHubUsesDirectOnly 端到端：配了镜像的机器下载官方 CDN 地址时，
// 只允许请求直连——镜像候选一次都不该被尝试（现场问题②的直接成因）。
func TestDownloadFileNonGitHubUsesDirectOnly(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "installer-bytes")
	}))
	defer srv.Close()

	withDownloadMirrorConfig(t, srv.URL+"/gh/", srv.URL+"/override/")
	withDefaultMirrorList(t)

	dest := filepath.Join(t.TempDir(), "installer.bin")
	if err := downloadFileWithProgress(context.Background(), srv.URL+"/bin/installer.exe", dest, nil); err != nil {
		t.Fatalf("直连下载应成功: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "installer-bytes" {
		t.Fatalf("落盘内容错误：%q err=%v", data, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/bin/installer.exe" {
		t.Fatalf("非 GitHub 地址只应请求直连一次，实际 %v", paths)
	}
}

// TestDownloadCandidatesErrorListsSources 失败原因必须带上「试过哪些源」：
// 旧实现只报最后一个镜像的错误（用户看到 HTTP 403 却不知是哪个代理），无从判断真实原因。
func TestDownloadCandidatesErrorListsSources(t *testing.T) {
	last := fmt.Errorf("HTTP %d", http.StatusForbidden)
	tried := []string{
		"https://mirror.example.com/gh/https://download.deepseek.com/x.exe",
		"https://ghfast.top/https://download.deepseek.com/x.exe",
	}
	err := downloadCandidatesError(tried, last)
	if err == nil {
		t.Fatal("应返回错误")
	}
	msg := err.Error()
	for _, want := range []string{"HTTP 403", "已尝试 2 个下载源", "mirror.example.com", "ghfast.top"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("失败原因缺少 %q：%s", want, msg)
		}
	}
	// 单一候选保持原样（对接既有文案与单测断言）
	single := downloadCandidatesError([]string{"https://a.example.com/x"}, last)
	if single.Error() != last.Error() {
		t.Fatalf("单候选不应改写原因：%q", single.Error())
	}
	if !strings.Contains(single.Error(), "HTTP 403") {
		t.Fatalf("单候选应保留原始错误文本：%q", single.Error())
	}
	if got := downloadCandidatesError(nil, nil); got == nil {
		t.Fatal("无候选也应返回错误（不得返回 nil 被当成成功）")
	}
}

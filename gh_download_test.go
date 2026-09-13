package main

import (
	"archive/zip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setMirrorOverride 临时把用户镜像配置指向指定前缀（空串=只用默认列表），测试结束还原。
func setMirrorOverride(t *testing.T, m string) {
	t.Helper()
	prev := updateMirrorOverride
	updateMirrorOverride = m
	t.Cleanup(func() { updateMirrorOverride = prev })
}

// stallingServer 先吐 n 个字节再挂住不结束（模拟镜像中途挂死）。
func stallingServer(t *testing.T, n int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		_, _ = w.Write(make([]byte, n))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // 客户端取消/断开后才返回
	}))
	t.Cleanup(srv.Close)
	return srv
}

// makeGHZip 造一个形如官方发布包的 zip：给定条目名写入内容，返回文件路径。
func makeGHZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestUnpackGHToolEntryNames 解包必须认得官方包里的 `bin/gh(.exe)` 条目，
// 以及带前导斜杠 / 直接平铺两种打包形态。
// 回归（2026-09-13 现场实证）：原实现用 strings.HasSuffix(name, "/bin/gh.exe") 匹配，
// 而官方条目名是相对的 "bin/gh.exe"（无前导斜杠）——永远匹配不上，包下载完成后
// 报「GitHub CLI 包中未找到 gh.exe」，授权流程到此中断。
func TestUnpackGHToolEntryNames(t *testing.T) {
	cases := []struct {
		desc    string
		entries map[string]string
		wantBin bool
	}{
		{"官方布局 bin/gh.exe", map[string]string{"LICENSE": "mit", "bin/" + ghBinName(): "MZ-fake"}, true},
		{"带前导斜杠 /bin/gh.exe", map[string]string{"/bin/" + ghBinName(): "MZ-fake"}, true},
		{"直接平铺 gh.exe", map[string]string{ghBinName(): "MZ-fake"}, true},
		{"只有 LICENSE", map[string]string{"LICENSE": "mit"}, false},
	}
	for _, c := range cases {
		destDir := t.TempDir()
		got, err := unpackGHTool(makeGHZip(t, c.entries), destDir)
		if !c.wantBin {
			if err == nil {
				t.Errorf("%s：应报错，却返回 %q", c.desc, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s：解包失败 %v", c.desc, err)
			continue
		}
		if got != filepath.Join(destDir, ghBinName()) {
			t.Errorf("%s：路径 = %q", c.desc, got)
			continue
		}
		if b, err := os.ReadFile(got); err != nil || string(b) != "MZ-fake" {
			t.Errorf("%s：内容 = %q err=%v", c.desc, string(b), err)
		}
	}
}

// TestDownloadWithRetrySwitchesOnStall 直连候选中途停滞时，看门狗中断本次尝试并换镜像，
// 最终把完整内容落盘（回归：一次镜像挂死不再让整个下载失败）。
func TestDownloadWithRetrySwitchesOnStall(t *testing.T) {
	stall := stallingServer(t, 4096)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "gh-cli-zip-bytes")
	}))
	t.Cleanup(good.Close)
	setMirrorOverride(t, good.URL+"/")

	dest := filepath.Join(t.TempDir(), "gh.zip")
	start := time.Now()
	if err := downloadWithRetry(context.Background(), stall.URL+"/gh.zip", dest, 600*time.Millisecond, nil); err != nil {
		t.Fatalf("应在换镜像后成功，却失败：%v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("换镜像耗时过长：%v", took)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "gh-cli-zip-bytes" {
		t.Errorf("落盘内容 = %q", got)
	}
}

// TestDownloadWithRetryAllStalledFails 全部候选都停滞时按停滞上限收敛报错，不无限挂住。
func TestDownloadWithRetryAllStalledFails(t *testing.T) {
	stall := stallingServer(t, 1024)
	setMirrorOverride(t, "") // 默认镜像列表；直连候选即 stall 服务器

	dest := filepath.Join(t.TempDir(), "gh.zip")
	start := time.Now()
	err := downloadWithRetry(context.Background(), stall.URL+"/gh.zip", dest, 300*time.Millisecond, nil)
	if err == nil {
		t.Fatal("全部候选停滞时应报错")
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("停滞收敛过慢：%v", took)
	}
}

package main

import (
	"os"
	"testing"
)

// TestDownloadsResolution 验证 homeDownloads 解析到真实存在的目录
// （Windows 优先 SHGetKnownFolderPath 的真实「下载」位置，避免重定向/本地化目录名导致落盘位置异常）。
func TestDownloadsResolution(t *testing.T) {
	d := homeDownloads()
	if d == "" {
		t.Fatal("homeDownloads returned empty")
	}
	st, err := os.Stat(d)
	if err != nil || !st.IsDir() {
		t.Fatalf("homeDownloads %q is not an existing dir: %v", d, err)
	}
	t.Logf("homeDownloads -> %s", d)
	if dd := downloadsDir(); dd != "" {
		t.Logf("downloadsDir  -> %s", dd)
	}
}

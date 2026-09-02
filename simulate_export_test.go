//go:build simulateexport

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSimulatedExportToRealDownloads 一次性模拟「点击导出」：临时 DSH_HOME + 假会话目录 → buildExportZip
// 写入真实下载目录，验证最终 zip 确实落盘且内容完整（manifest.json + sessions.zip）。
// 仅 -tags simulateexport 时运行，避免常驻测试向真实下载目录写文件。
func TestSimulatedExportToRealDownloads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	sess := filepath.Join(home, "sessions", "--S1--", "session-1")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess, "session.jsonl.zstd"), []byte("demo"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := homeDownloads()
	final, err := buildExportZip(true, false, false, nil, dest, nil, "")
	if err != nil {
		t.Fatalf("buildExportZip: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(final), "dsh-systray-export-") || !strings.HasSuffix(final, ".zip") {
		t.Fatalf("unexpected export name: %s", final)
	}
	names, err := zipListNames(final)
	if err != nil {
		t.Fatalf("zipListNames: %v", err)
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["manifest.json"] || !found["sessions.zip"] {
		t.Fatalf("zip contents missing manifest.json/sessions.zip: %v", names)
	}
	t.Logf("SIMULATED EXPORT OK -> %s (%d zip entries)", final, len(names))
}

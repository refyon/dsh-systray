package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportImportPipeline(t *testing.T) {
	root := t.TempDir()
	homeA := filepath.Join(root, "homeA")
	// 源环境：sessions（两个 scope 各一个会话）+ 命名 profile（web）的 plugins + 一个用户目录
	if err := os.MkdirAll(filepath.Join(homeA, "sessions", "--S1--", "session-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeA, "sessions", "--S1--", "session-1", "session.jsonl.zstd"), []byte("data1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(homeA, "sessions", "--S2--", "session-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeA, "sessions", "--S2--", "session-2", "session.jsonl.zstd"), []byte("data2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(homeA, "profiles", "web", "node_modules", "pkg-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeA, "profiles", "web", "node_modules", "pkg-a", "index.js"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeA, "profiles", "web", "package.json"), []byte(`{"dependencies":{"pkg-a":"1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(root, "mydocs")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "note.txt"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DSH_HOME", homeA)

	destDir := filepath.Join(root, "exports")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := buildExportZip(true, true, true, []string{userDir}, destDir, nil)
	if err != nil {
		t.Fatalf("buildExportZip: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(p), "dsh-systray-export-") || !strings.HasSuffix(p, ".zip") {
		t.Fatalf("bad export name: %s", p)
	}

	items, err := parseExportZip(p)
	if err != nil {
		t.Fatalf("parseExportZip: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %v", items)
	}
	kinds := map[string]bool{}
	for _, it := range items {
		kinds[it.Kind] = true
	}
	for _, k := range []string{"sessions", "plugins", "files"} {
		if !kinds[k] {
			t.Fatalf("missing kind %s in %v", k, kinds)
		}
	}

	// 恢复到全新的 homeB
	homeB := filepath.Join(root, "homeB")
	t.Setenv("DSH_HOME", homeB)
	if n, err := countRestoreConflicts("sessions", mustInner(t, p, exportZipSessions)); err != nil || n != 0 {
		t.Fatalf("conflicts in fresh home: n=%d err=%v", n, err)
	}
	if _, err := restoreItem("sessions", mustInner(t, p, exportZipSessions), "", true, nil); err != nil {
		t.Fatalf("restore sessions: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeB, "sessions", "--S1--", "session-1", "session.jsonl.zstd")); err != nil {
		t.Fatalf("restored session missing: %v", err)
	}
	if _, err := restoreItem("plugins", mustInner(t, p, exportZipPlugins), "", true, nil); err != nil {
		t.Fatalf("restore plugins: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeB, "profiles", "web", "node_modules", "pkg-a", "index.js")); err != nil {
		t.Fatalf("restored plugin missing: %v", err)
	}
	filesDest := filepath.Join(root, "files-restored")
	if _, err := restoreItem("files", mustInner(t, p, exportZipFiles), filesDest, true, nil); err != nil {
		t.Fatalf("restore files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filesDest, "mydocs", "note.txt")); err != nil {
		t.Fatalf("restored file missing: %v", err)
	}

	// 冲突检测：homeB 已有数据，再次统计应 >0
	if n, err := countRestoreConflicts("sessions", mustInner(t, p, exportZipSessions)); err != nil || n == 0 {
		t.Fatalf("expected conflicts, n=%d err=%v", n, err)
	}
	// 跳过已有：不覆盖
	if _, err := restoreItem("sessions", mustInner(t, p, exportZipSessions), "", false, nil); err != nil {
		t.Fatalf("restore sessions skip: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(homeB, "sessions", "--S1--", "session-1", "session.jsonl.zstd"))
	if string(data) != "data1" {
		t.Fatalf("skip-existing overwrote data: %q", data)
	}
	// 覆盖更新：内容替换
	if _, err := restoreItem("sessions", mustInner(t, p, exportZipSessions), "", true, nil); err != nil {
		t.Fatalf("restore sessions overwrite: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(homeB, "sessions", "--S1--", "session-1", "session.jsonl.zstd"))
	if string(data) != "data1" { // 内容相同，但路径完整即可
		t.Fatalf("overwrite data mismatch: %q", data)
	}

	// 解析异常：非导出包
	if _, err := parseExportZip(filepath.Join(root, "mydocs", "note.txt")); err == nil {
		t.Fatal("expected parse error for non-zip")
	}
}

func mustInner(t *testing.T, master, name string) string {
	t.Helper()
	p, cleanup, err := extractInnerZip(master, name)
	if err != nil {
		t.Fatalf("extractInnerZip %s: %v", name, err)
	}
	t.Cleanup(cleanup)
	return p
}

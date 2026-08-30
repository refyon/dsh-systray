package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestZipRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	// 构造源目录树（含子目录与文件）
	src := filepath.Join(tmp, "src")
	sub := filepath.Join(src, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "root.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "deep.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(tmp, "test.zip")
	if err := zipCreate(zipPath, map[string]string{"sessions": src}, nil); err != nil {
		t.Fatalf("zipCreate: %v", err)
	}
	names, err := zipListNames(zipPath)
	if err != nil {
		t.Fatalf("zipListNames: %v", err)
	}
	found := false
	for _, n := range names {
		if n == "sessions/a/b/deep.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected sessions/a/b/deep.txt in %v", names)
	}

	out := filepath.Join(tmp, "out")
	if err := zipExtract(zipPath, out, true); err != nil {
		t.Fatalf("zipExtract: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "sessions", "a", "b", "deep.txt"))
	if err != nil || string(data) != "world" {
		t.Fatalf("extract content mismatch: %v %q", err, data)
	}
}

func TestValidateZipSafeTraversal(t *testing.T) {
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "bad.zip")
	if err := os.WriteFile(bad, []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 非 zip 文件：应报“无法打开压缩包”
	if err := validateZipSafe(bad); err == nil {
		t.Fatal("expected error for non-zip")
	}
}

func TestConflictTops(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "s")
	if err := os.MkdirAll(filepath.Join(src, "sessions", "scopeA", "s1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sessions", "scopeA", "s1", "x"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sessions", "scopeB"), 0o755); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(tmp, "s.zip")
	if err := zipCreate(zipPath, map[string]string{"sessions": filepath.Join(src, "sessions")}, nil); err != nil {
		t.Fatal(err)
	}
	tops, err := conflictTops("sessions", zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(tops) != 2 || tops[0] != "scopeA" || tops[1] != "scopeB" {
		t.Fatalf("unexpected tops: %v", tops)
	}
}

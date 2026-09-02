package main

import (
	"os"
	"path/filepath"
	"testing"
)

// buildPluginExport 构造带命名 profile（web）与一个插件包 pkg-a 的源 DSH_HOME，导出仅插件子包的总 zip。
func buildPluginExport(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	profDir := filepath.Join(home, "profiles", "web")
	pkgDir := filepath.Join(profDir, "node_modules", "pkg-a")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profDir, "package.json"),
		[]byte(`{"name":"dsh-profile-web","private":true,"dependencies":{"pkg-a":"1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"),
		[]byte(`{"name":"pkg-a","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "index.js"), []byte("module.exports=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	destDir := filepath.Join(home, "exports")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := buildExportZip(false, true, false, nil, destDir, nil, "")
	if err != nil {
		t.Fatalf("buildExportZip: %v", err)
	}
	return p
}

// TestRestorePluginPrefixComesFromZip 回归：恢复侧插件前缀必须从 zip 内容（源 profile web）推导，
// 而非目标机当前布局。此前 pluginsRelPrefix 在目标机无 profile 时回退 "profiles/node_modules/"，
// 与 zip 内 "profiles/web/node_modules/" 不匹配，导致恢复文件落位/冲突检测错位。
func TestRestorePluginPrefixComesFromZip(t *testing.T) {
	master := buildPluginExport(t)
	homeB := t.TempDir()
	t.Setenv("DSH_HOME", homeB)

	inner := mustInner(t, master, exportZipPlugins)
	prefix := innerZipContentPrefix("plugins", inner)
	if prefix != "profiles/web/node_modules/" {
		t.Fatalf("prefix must derive from zip (profiles/web/node_modules/), got %q", prefix)
	}
	if n, err := countRestoreConflicts("plugins", inner); err != nil || n != 0 {
		t.Fatalf("fresh home conflicts: n=%d err=%v", n, err)
	}
	if _, err := restoreItem("plugins", inner, "", true, nil); err != nil {
		t.Fatalf("restore plugins: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeB, "profiles", "web", "node_modules", "pkg-a", "index.js")); err != nil {
		t.Fatalf("plugin not restored to zip-layout path: %v", err)
	}
	if n, _ := countRestoreConflicts("plugins", inner); n != 1 {
		t.Fatalf("expected 1 conflict after restore, got %d", n)
	}
}

// TestRestorePluginConflictIgnoresTargetProfile 回归：目标机存在不同名 profile（default）且其下有同名包时，
// 冲突检测不得按目标机布局误判——zip 内容属于源 profile web，与 default 无交集。
func TestRestorePluginConflictIgnoresTargetProfile(t *testing.T) {
	master := buildPluginExport(t)
	homeB := t.TempDir()
	t.Setenv("DSH_HOME", homeB)
	otherPkg := filepath.Join(homeB, "profiles", "default", "node_modules", "pkg-a", "package.json")
	if err := os.MkdirAll(filepath.Dir(otherPkg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherPkg, []byte(`{"name":"pkg-a","version":"9.9.9"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	inner := mustInner(t, master, exportZipPlugins)
	if prefix := innerZipContentPrefix("plugins", inner); prefix != "profiles/web/node_modules/" {
		t.Fatalf("unexpected prefix %q", prefix)
	}
	if n, err := countRestoreConflicts("plugins", inner); err != nil || n != 0 {
		t.Fatalf("conflicts must ignore target default profile: n=%d err=%v", n, err)
	}
	if _, err := restoreItem("plugins", inner, "", true, nil); err != nil {
		t.Fatalf("restore plugins: %v", err)
	}
	data, _ := os.ReadFile(otherPkg)
	if string(data) != `{"name":"pkg-a","version":"9.9.9"}` {
		t.Fatalf("target default profile pkg must stay untouched, got %s", data)
	}
}

// TestTopDirConflicts files 类目冲突：用户选定解压位置后，统计 zip 顶层目录是否已存在。
func TestTopDirConflicts(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mydocs")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	zipP := filepath.Join(dir, "files.zip")
	if err := zipCreate(zipP, map[string]string{"mydocs": src}, nil); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if n, tops, err := topDirConflicts(zipP, dest); err != nil || n != 0 {
		t.Fatalf("fresh dest conflicts: n=%d tops=%v err=%v", n, tops, err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "mydocs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n, tops, err := topDirConflicts(zipP, dest); err != nil || n != 1 || len(tops) != 1 || tops[0] != "mydocs" {
		t.Fatalf("expected 1 conflict mydocs, got n=%d tops=%v err=%v", n, tops, err)
	}
}

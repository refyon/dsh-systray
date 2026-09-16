package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// mkImportProfile 构造一个待导入 profile 目录：package.json / pnpm-lock.yaml / node_modules 内容。
func mkImportProfile(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "profiles", "web")
	mustMkdirAll(t, dir)
	mustMkdirAll(t, filepath.Join(dir, "node_modules", "keep-pkg"))
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"dsh-profile-web","private":true}`)
	writeFile(t, filepath.Join(dir, "pnpm-lock.yaml"), "lockfile-version: '9.0'\n")
	writeFile(t, filepath.Join(dir, "node_modules", "keep-pkg", "index.js"), "module.exports = 1;\n")
	return dir
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestImportSnapshotRollback(t *testing.T) {
	root := t.TempDir()
	dir := mkImportProfile(t, root)
	origPJ := readFile(t, filepath.Join(dir, "package.json"))

	had := snapshotImportProfiles([]string{dir})
	if len(had) != 1 || !had[0] {
		t.Fatalf("had = %v, want [true]", had)
	}
	// node_modules 已整目录暂存
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("node_modules should be parked, err=%v", err)
	}
	if !exists(filepath.Join(dir, "node_modules"+importBakSuffix, "keep-pkg", "index.js")) {
		t.Fatalf("parked node_modules missing original content")
	}
	if !exists(filepath.Join(dir, "package.json"+importBakSuffix)) {
		t.Fatalf("package.json importbak missing")
	}

	// 模拟导入写入新内容（解压落在空 node_modules）
	mustMkdirAll(t, filepath.Join(dir, "node_modules", "new-pkg"))
	writeFile(t, filepath.Join(dir, "node_modules", "new-pkg", "index.js"), "module.exports = 2;\n")
	writeFile(t, filepath.Join(dir, "package.json"), `{"dependencies":{"new-pkg":"^1.0.0"}}`)

	// 回退：应还原 package.json 与原 node_modules，新内容被清掉
	rollbackImportProfiles([]string{dir}, had)
	if got := readFile(t, filepath.Join(dir, "package.json")); got != origPJ {
		t.Fatalf("package.json after rollback = %q, want %q", got, origPJ)
	}
	if !exists(filepath.Join(dir, "node_modules", "keep-pkg", "index.js")) {
		t.Fatalf("original node_modules not restored")
	}
	if exists(filepath.Join(dir, "node_modules", "new-pkg")) {
		t.Fatalf("new node_modules content should be gone after rollback")
	}
	if exists(filepath.Join(dir, "node_modules"+importBakSuffix)) {
		t.Fatalf("importbak should be consumed by rollback")
	}
}

func TestImportPromoteToLkg(t *testing.T) {
	root := t.TempDir()
	dir := mkImportProfile(t, root)

	had := snapshotImportProfiles([]string{dir})
	if !had[0] {
		t.Fatal("snapshot should park node_modules")
	}
	// 模拟导入后的新树已通过校验
	mustMkdirAll(t, filepath.Join(dir, "node_modules", "new-pkg"))
	promoteImportProfilesToLkg([]string{dir})

	// 导入前快照提升为 LKG：package.json.lkgbak / node_modules.lkgbak 存在且内容为导入前状态
	if !exists(filepath.Join(dir, "package.json"+lkgSuffix)) {
		t.Fatalf("package.json.lkgbak missing after promote")
	}
	if !exists(filepath.Join(dir, "node_modules"+lkgSuffix, "keep-pkg", "index.js")) {
		t.Fatalf("node_modules.lkgbak missing original content")
	}
	// 新树保留在 node_modules（校验通过的当前状态）
	if !exists(filepath.Join(dir, "node_modules", "new-pkg")) {
		t.Fatalf("verified new node_modules should remain in place")
	}
	// importbak 已消费，残留应被 cleanup 清掉
	cleanupImportProfiles([]string{dir})
	if exists(filepath.Join(dir, "package.json"+importBakSuffix)) {
		t.Fatalf("package.json.importbak should be cleaned after promote+cleanup")
	}
}

func TestImportSnapshotNoNodeModules(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profiles", "web")
	mustMkdirAll(t, dir)
	writeFile(t, filepath.Join(dir, "package.json"), `{}`)
	had := snapshotImportProfiles([]string{dir})
	if len(had) != 1 || had[0] {
		t.Fatalf("had = %v, want [false] (no node_modules)", had)
	}
	// 无 node_modules 时回退也不应报错
	rollbackImportProfiles([]string{dir}, had)
	if !exists(filepath.Join(dir, "package.json")) {
		t.Fatalf("package.json should still exist")
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRestoredPluginProfileDirsNilOnNoManifest(t *testing.T) {
	// 无 manifest 的 zip（临时构造一个空 zip）→ nil
	zipPath := filepath.Join(t.TempDir(), "empty.zip")
	if err := zipCreate(zipPath, map[string]string{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := restoredPluginProfileDirs(zipPath); got != nil {
		t.Fatalf("dirs = %v, want nil", got)
	}
}

func TestRestoredPluginProfileDirsDedupSort(t *testing.T) {
	// 用临时 DSH_HOME + manifest 验证：目录并集去重排序
	prevHome := os.Getenv("DSH_HOME")
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	defer os.Setenv("DSH_HOME", prevHome)

	// 当前激活 profile（旧布局：profiles/package.json 也可——profilePlugins 优先命名 profile）
	mustMkdirAll(t, filepath.Join(home, "profiles", "web"))
	writeFile(t, filepath.Join(home, "profiles", "web", "package.json"), `{"name":"dsh-profile-web","dependencies":{"a":"1"}}`)

	manifest := `{"format":"dsh-systray-export","version":1,"plugins":{"profile":"web","dependencies":{"b":"2"}}}`
	zipPath := filepath.Join(t.TempDir(), "m.zip")
	if err := zipCreate(zipPath, map[string]string{"manifest.json": writeTemp(t, manifest)}, nil); err != nil {
		t.Fatal(err)
	}
	got := restoredPluginProfileDirs(zipPath)
	want := []string{filepath.Join(home, "profiles", "web")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dirs = %v, want %v", got, want)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tmp.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

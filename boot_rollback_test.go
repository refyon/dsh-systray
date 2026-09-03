package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPromoteRestoreClearLkg LKG 状态机（目录级，不触达真实配置/网络）：
// 更新快照(.dshbak) → 成功提升为 LKG(.lkgbak) → 冷启动失败恢复 → 验证通过后清理。
func TestPromoteRestoreClearLkg(t *testing.T) {
	dir := t.TempDir()
	pj := filepath.Join(dir, "package.json")
	origPj := `{"dependencies":{"pkg":"^1.0.0"}}`
	if err := os.WriteFile(pj, []byte(origPj), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte("lock: 1.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(dir, "node_modules", "pkg", "index.js")
	if err := os.MkdirAll(filepath.Dir(oldFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldFile, []byte("// v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1) 更新前快照（模拟 update 流程）
	if !snapshotPluginProfile(dir) {
		t.Fatal("snapshot should succeed")
	}
	// 2) 模拟“安装成功”并改动现场：package.json 更新、node_modules 重建
	if err := os.WriteFile(pj, []byte(`{"dependencies":{"pkg":"^2.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(oldFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldFile, []byte("// v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 3) 更新成功：快照提升为 LKG（.dshbak → .lkgbak）
	promoteDirToLkg(dir)
	if _, err := os.Stat(filepath.Join(dir, "package.json"+lkgSuffix)); err != nil {
		t.Error("package.json.lkgbak should exist after promote")
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json.dshbak")); err == nil {
		t.Error("package.json.dshbak should be renamed away after promote")
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules"+lkgSuffix)); err != nil {
		t.Error("node_modules.lkgbak should exist after promote")
	}
	if !hasLkgInDir(dir) {
		t.Error("hasLkgInDir should be true after promote")
	}

	// 4) 冷启动失败：恢复 LKG → 旧 package.json / 旧 node_modules 回来
	if !restoreLkgInDir(dir) {
		t.Fatal("restoreLkgInDir should restore something")
	}
	data, err := os.ReadFile(pj)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != origPj {
		t.Errorf("package.json not restored to LKG: %s", string(data))
	}
	if got, err := os.ReadFile(oldFile); err != nil || string(got) != "// v1" {
		t.Errorf("node_modules not restored to LKG: %v %q", err, string(got))
	}

	// 5) 回退验证通过：清理 LKG（当前状态即新良好态）
	clearLkgInDir(dir)
	if hasLkgInDir(dir) {
		t.Error("hasLkgInDir should be false after clear")
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules"+lkgSuffix)); err == nil {
		t.Error("node_modules.lkgbak should be removed after clear")
	}
}

// TestRestoreLkgWithoutNodeModules 无 node_modules 备份（仅锁文件）时恢复路径不 panic 且锁文件还原。
// 该场景会尝试 pnpm frozen 重装——测试仅验证包文件还原与返回结果，不依赖网络（nm 备份存在时不会触发）。
func TestRestoreLkgWithoutNodeModules(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"a":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"+lkgSuffix), []byte(`{"dependencies":{"a":"0.9"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !restoreLkgInDir(dir) {
		t.Fatal("restore should report restored (package.json)")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	if string(data) != `{"dependencies":{"a":"0.9"}}` {
		t.Errorf("package.json should be restored from lkgbak, got %s", string(data))
	}
}

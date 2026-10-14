package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRetryPlainStartBeforeDisable 回归 2026-10-14：启动日志没点名任何插件（慢启动/被看门狗误杀/
// 瞬时故障）时必须先用原状态重试一次，避免把一次超时放大成「禁用全部用户插件 + 逐个复验重新启用」；
// 日志点名了插件（真正的加载错误）则不做无谓重试，直接走「先禁用肇事插件」。
func TestRetryPlainStartBeforeDisable(t *testing.T) {
	calls := 0
	ok := func() bool { calls++; return true }
	fail := func() bool { calls++; return false }

	if !retryPlainStartBeforeDisable(nil, ok) {
		t.Fatal("无点名嫌疑且重试成功时应返回 true")
	}
	if calls != 1 {
		t.Fatalf("重试校验应恰好执行一次，实际 %d", calls)
	}
	if retryPlainStartBeforeDisable([]string{"dsh-x"}, ok) {
		t.Fatal("点名了插件时不该做无谓重试")
	}
	if calls != 1 {
		t.Fatalf("点名插件时不应调用重试校验，实际 %d 次", calls)
	}
	if retryPlainStartBeforeDisable(nil, fail) {
		t.Fatal("重试仍失败时应返回 false（交回禁用全部插件/LKG 回退）")
	}
	if retryPlainStartBeforeDisable(nil, nil) {
		t.Fatal("校验缝未装配时应安全返回 false")
	}
}

// TestBootKeptMessage 未禁用任何插件（原状态重试成功）时不出现空名单，文案与禁用路径可区分。
func TestBootKeptMessage(t *testing.T) {
	plain := bootKeptMessage(nil, "服务进程异常退出")
	if strings.Contains(plain, "（）") || strings.Contains(plain, "禁用不兼容插件") {
		t.Fatalf("无禁用名单时不该出现禁用措辞：%s", plain)
	}
	if !strings.Contains(plain, "重试") || !strings.Contains(plain, "服务进程异常退出") {
		t.Fatalf("文案应说明重试成功与失败原因：%s", plain)
	}
	withNames := bootKeptMessage([]string{"dsh-a", "dsh-b"}, "启动日志存在加载错误")
	if !strings.Contains(withNames, "dsh-a、dsh-b") || !strings.Contains(withNames, "禁用不兼容插件") {
		t.Fatalf("有禁用名单时应点名列出的插件：%s", withNames)
	}
}

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

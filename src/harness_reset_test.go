package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveInstalledPlugins 回归：重置时删除用户插件（顶层目录 + .pnpm 非官方条目），
// 但保留 @deepseek-ai 官方依赖、bundle 注册与其 .pnpm 实体（防官方符号链接悬空 → client.js 加载失败）。
func TestRemoveInstalledPlugins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	profDir := filepath.Join(home, "profiles", "web")
	nm := filepath.Join(profDir, "node_modules")
	for _, dir := range []string{
		filepath.Join(nm, "deepseek-idesign"),
		filepath.Join(nm, "restrict-discipline"),
		filepath.Join(nm, "@deepseek-ai", "dsh-base"),
		filepath.Join(nm, ".pnpm", "@deepseek-ai+dsh-base@1.0.0", "node_modules", "@deepseek-ai", "dsh-base"),
		filepath.Join(nm, ".pnpm", "deepseek-idesign@0.2.2"),
		filepath.Join(nm, ".pnpm", "restrict-discipline@1.0.0"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(nm, ".pnpm", ".modules.yaml"), []byte("modules"), 0o644); err != nil {
		t.Fatal(err)
	}
	pj := filepath.Join(profDir, "package.json")
	initial := `{
  "name": "dsh-profile-web",
  "private": true,
  "dependencies": {
    "deepseek-idesign": "^0.2.2",
    "restrict-discipline": "file:../restrict-discipline",
    "@deepseek-ai/dsh-base": "^1.0.0"
  },
  "dsh": {"profile": {"bundles": ["@deepseek-ai/dsh-base", "deepseek-idesign", "restrict-discipline"]}}
}`
	if err := os.WriteFile(pj, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := removeInstalledPlugins()
	if err != nil {
		t.Fatalf("removeInstalledPlugins: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 profile cleaned, got %d", n)
	}
	// 官方 @deepseek-ai 顶层目录与 .pnpm 官方实体保留；用户插件与 .pnpm 非官方条目删除
	for _, keep := range []string{
		filepath.Join("@deepseek-ai", "dsh-base"),
		filepath.Join(".pnpm", "@deepseek-ai+dsh-base@1.0.0"),
		filepath.Join(".pnpm", ".modules.yaml"),
	} {
		if _, err := os.Stat(filepath.Join(nm, filepath.FromSlash(keep))); err != nil {
			t.Fatalf("official pkg should be kept: %s (%v)", keep, err)
		}
	}
	for _, gone := range []string{
		"deepseek-idesign",
		"restrict-discipline",
		filepath.Join(".pnpm", "deepseek-idesign@0.2.2"),
		filepath.Join(".pnpm", "restrict-discipline@1.0.0"),
	} {
		if _, err := os.Stat(filepath.Join(nm, gone)); err == nil {
			t.Fatalf("user plugin should be removed: %s", gone)
		}
	}
	// package.json：依赖只留官方；bundles 只留官方
	var root map[string]json.RawMessage
	data, _ := os.ReadFile(pj)
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var deps map[string]string
	_ = json.Unmarshal(root["dependencies"], &deps)
	if len(deps) != 1 || deps["@deepseek-ai/dsh-base"] == "" {
		t.Fatalf("dependencies should keep only official pkg, got %v", deps)
	}
	var dsh struct {
		Profile struct {
			Bundles []string `json:"bundles"`
		} `json:"profile"`
	}
	_ = json.Unmarshal(root["dsh"], &dsh)
	if len(dsh.Profile.Bundles) != 1 || dsh.Profile.Bundles[0] != "@deepseek-ai/dsh-base" {
		t.Fatalf("bundles should keep only official, got %v", dsh.Profile.Bundles)
	}
}

// TestRemoveInstalledPluginsEmptyHome 无 profile 时静默成功。
func TestRemoveInstalledPluginsEmptyHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	n, err := removeInstalledPlugins()
	if err != nil || n != 0 {
		t.Fatalf("empty home: n=%d err=%v", n, err)
	}
}

// TestResetStats 重置弹窗数量统计：两级会话布局计数、插件数与非官方依赖口径一致。
func TestResetStats(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	mk := func(rel ...string) {
		if err := os.MkdirAll(filepath.Join(append([]string{home}, rel...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("sessions", "--S1--", "session-a")
	mk("sessions", "--S1--", "session-b")
	mk("sessions", "--S2--", "session-c")
	mk("sessions", "legacy-single") // 单级目录：视为 1 个会话
	if n := countResetSessions(); n != 4 {
		t.Fatalf("expected 4 sessions (3 scope + 1 legacy), got %d", n)
	}
	// 插件统计：web profile 两个用户插件 + 官方包
	profDir := filepath.Join(home, "profiles", "web")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profDir, "package.json"),
		[]byte(`{"dependencies":{"deepseek-idesign":"^0.2.2","restrict-discipline":"file:x","@deepseek-ai/dsh-base":"^1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := countInstalledPlugins(); n != 2 {
		t.Fatalf("expected 2 user plugins, got %d", n)
	}
	// removeSessions：删除整个 sessions 根
	if err := removeSessions(); err != nil {
		t.Fatalf("removeSessions: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions")); err == nil {
		t.Fatal("sessions root should be removed")
	}
	// 再次统计归零
	if n := countResetSessions(); n != 0 {
		t.Fatalf("expected 0 sessions after removal, got %d", n)
	}
}

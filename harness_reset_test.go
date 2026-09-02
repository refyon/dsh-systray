package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveInstalledPlugins 回归：重置时物理删除用户插件，但保留 @deepseek-ai 官方依赖与 bundle 注册。
func TestRemoveInstalledPlugins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	profDir := filepath.Join(home, "profiles", "web")
	nm := filepath.Join(profDir, "node_modules")
	for _, dir := range []string{
		filepath.Join(nm, "deepseek-idesign"),
		filepath.Join(nm, "restrict-discipline"),
		filepath.Join(nm, "@deepseek-ai", "dsh-base"),
		filepath.Join(nm, ".pnpm", "virtual-store"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
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
	// 官方 @deepseek-ai 目录保留；用户插件与 .pnpm 删除
	for _, keep := range []string{filepath.Join("@deepseek-ai", "dsh-base")} {
		if _, err := os.Stat(filepath.Join(nm, filepath.FromSlash(keep))); err != nil {
			t.Fatalf("official pkg should be kept: %v", err)
		}
	}
	for _, gone := range []string{"deepseek-idesign", "restrict-discipline", ".pnpm"} {
		if _, err := os.Stat(filepath.Join(nm, gone)); err == nil {
			t.Fatalf("user plugin dir should be removed: %s", gone)
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

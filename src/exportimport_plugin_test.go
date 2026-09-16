package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDshHomeDirHonorsDSH_HOME(t *testing.T) {
	// DSH_HOME 未设置 → 默认 ~/.dsh
	t.Setenv("DSH_HOME", "")
	home := dshHomeDir()
	t.Logf("default home = %q", home)
	if home == "" || filepath.Base(home) != ".dsh" {
		t.Fatalf("default home should end with .dsh, got %q", home)
	}
	// DSH_HOME 设置为自定义路径
	user, _ := os.UserHomeDir()
	custom := filepath.Join(user, "my-dsh")
	t.Setenv("DSH_HOME", custom)
	if got := dshHomeDir(); got != custom {
		t.Fatalf("expected %q, got %q", custom, got)
	}
	// 空白 DSH_HOME 视为未设置
	t.Setenv("DSH_HOME", "   ")
	if got := dshHomeDir(); got == custom {
		t.Fatalf("blank DSH_HOME should fall back, got %q", got)
	}
}

func TestExpandTildePath(t *testing.T) {
	user, _ := os.UserHomeDir()
	if got := expandTildePath("~/foo"); got != filepath.Join(user, "foo") {
		t.Fatalf("~ expansion got %q", got)
	}
	if got := expandTildePath("~"); got != user {
		t.Fatalf("~ expansion got %q", got)
	}
	if got := expandTildePath("/abs/path"); got != "/abs/path" {
		t.Fatalf("absolute path should be unchanged, got %q", got)
	}
}

func TestMergePluginConfigIntoProfile(t *testing.T) {
	dir := t.TempDir()
	initial := `{
  "name": "dsh-profile-web",
  "private": true,
  "dsh": {"profile": {"bundles": ["@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app"]}},
  "dependencies": {"existing-plugin": "^1.0.0"}
}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := exportPlugins{
		Dependencies: map[string]string{
			"deepseek-idesign": "^0.2.2",
			"dsh-cost-meter":   "^1.5.42",
		},
		Bundles: []string{"deepseek-idesign", "dsh-cost-meter"},
	}
	if err := mergePluginConfigIntoProfile(dir, cfg); err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var deps map[string]string
	_ = json.Unmarshal(root["dependencies"], &deps)
	for _, k := range []string{"existing-plugin", "deepseek-idesign", "dsh-cost-meter"} {
		if deps[k] == "" {
			t.Fatalf("missing dependency %s in %v", k, deps)
		}
	}
	var dsh struct {
		Profile struct {
			Bundles []string `json:"bundles"`
		} `json:"profile"`
	}
	_ = json.Unmarshal(root["dsh"], &dsh)
	got := dsh.Profile.Bundles
	for _, b := range []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app", "deepseek-idesign", "dsh-cost-meter"} {
		found := false
		for _, g := range got {
			if g == b {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing bundle %s in %v", b, got)
		}
	}
}

func TestRegisterRestoredPlugins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	// 源 profile
	profDir := filepath.Join(home, "profiles", "web")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	initial := `{"name":"dsh-profile-web","private":true,"dsh":{"profile":{"bundles":["@deepseek-ai/dsh-base"]}},"dependencies":{"old":"^1.0.0"}}`
	if err := os.WriteFile(filepath.Join(profDir, "package.json"), []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	// 构造 manifest.json 并打成总 zip
	manifest := exportManifest{
		Format:  exportFormatName,
		Version: exportFormatVersion,
		Items: []exportItemInfo{
			{Kind: "plugins", Label: "已安装的插件", Zip: exportZipPlugins},
		},
		Plugins: exportPlugins{
			Profile: "web",
			Dependencies: map[string]string{
				"deepseek-idesign": "^0.2.2",
				"dsh-cost-meter":   "^1.5.42",
			},
			Bundles: []string{"deepseek-idesign", "dsh-cost-meter"},
		},
	}
	mb, _ := json.Marshal(manifest)
	mfPath := filepath.Join(home, "manifest.json")
	if err := os.WriteFile(mfPath, mb, 0o644); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(home, "export.zip")
	if err := zipCreate(zipPath, map[string]string{"manifest.json": mfPath}, nil); err != nil {
		t.Fatal(err)
	}

	if err := registerRestoredPlugins(zipPath); err != nil {
		t.Fatal(err)
	}
	// 校验 profile package.json 已合并
	var root map[string]json.RawMessage
	data, _ := os.ReadFile(filepath.Join(profDir, "package.json"))
	_ = json.Unmarshal(data, &root)
	var deps map[string]string
	_ = json.Unmarshal(root["dependencies"], &deps)
	for _, k := range []string{"old", "deepseek-idesign", "dsh-cost-meter"} {
		if deps[k] == "" {
			t.Fatalf("missing dep %s in %v", k, deps)
		}
	}
}

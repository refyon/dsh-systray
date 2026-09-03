package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClassifyPluginSpec 依赖 spec → 来源分类（npm/github/file/tarball/unknown）与可否更新。
func TestClassifyPluginSpec(t *testing.T) {
	cases := []struct {
		spec    string
		source  string
		canUp   bool
		wantErr bool // reason 非空即视为断言不可更新原因存在
	}{
		{"^0.2.2", "npm", true, false},
		{"1.5.x", "npm", true, false},
		{"latest", "npm", true, false},
		{"*", "npm", true, false},
		{"~1.2", "npm", true, false},
		{"0.1.1-rc.2", "npm", true, false},
		{"@scope/pkg", "npm", true, false},
		{"github:refyon/restrict-discipline", "github", true, false},
		{"github:user/repo#main", "github", true, false},
		{"git+https://github.com/user/repo.git", "github", true, false},
		{"git+ssh://git@github.com:user/repo.git", "github", true, false},
		{"owner/repo", "github", true, false},
		{"file:./local-plugin", "file", false, true},
		{"file:D:/agent-env/qtz/plugins/x", "file", false, true},
		{"link:../plugin", "file", false, true},
		{"https://example.com/pack-1.2.0.tgz", "tarball", false, true},
		{"https://example.com/pack.zip", "tarball", false, true},
		{"npm:real-pkg@^1.0.0", "npm", false, true},
		{"git+ssh://git@gitlab.com/team/x.git", "unknown", false, true},
		{"https://example.com/plain", "unknown", false, true},
		{"", "unknown", false, true},
	}
	for _, c := range cases {
		src, canUp, reason := classifyPluginSpec(c.spec)
		if src != c.source || canUp != c.canUp {
			t.Errorf("spec %q: got (%s, canUpdate=%v), want (%s, %v)", c.spec, src, canUp, c.source, c.canUp)
		}
		if c.wantErr && reason == "" {
			t.Errorf("spec %q: expected non-empty reason, got none", c.spec)
		}
		if !c.wantErr && reason != "" {
			t.Errorf("spec %q: unexpected reason %q", c.spec, reason)
		}
	}
}

// TestGithubSpecParts github spec 提取 owner/repo（忽略 #branch）。
func TestGithubSpecParts(t *testing.T) {
	cases := []struct {
		spec, owner, repo string
		ok                bool
	}{
		{"github:refyon/x", "refyon", "x", true},
		{"github:user/repo#dev", "user", "repo", true},
		{"git+https://github.com/user/repo.git", "user", "repo", true},
		{"git+ssh://git@github.com:user/repo.git#main", "user", "repo", true},
		{"owner/repo", "owner", "repo", true},
		{"file:x", "", "", false},
	}
	for _, c := range cases {
		o, r, ok := githubSpecParts(c.spec)
		if ok != c.ok || o != c.owner || r != c.repo {
			t.Errorf("spec %q: got (%s,%s,%v), want (%s,%s,%v)", c.spec, o, r, ok, c.owner, c.repo, c.ok)
		}
	}
}

// writeTestProfile 构造临时 DSH_HOME 下的命名 profile（含依赖声明与已装 node_modules 版本）。
func writeTestProfile(t *testing.T, home, profile string, deps map[string]string, versions map[string]string) {
	t.Helper()
	dir := filepath.Join(home, "profiles", profile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"dsh-profile-` + profile + `","private":true,"dependencies":{`
	first := true
	for name, spec := range deps {
		if !first {
			body += ","
		}
		first = false
		b, _ := json.Marshal(name)
		s, _ := json.Marshal(spec)
		body += string(b) + ":" + string(s)
	}
	body += `}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, ver := range versions {
		p := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "package.json"),
			[]byte(`{"name":"`+name+`","version":"`+ver+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestBuildPluginRows 插件清单：来源分类、已装版本读取、官方包排除、多 profile 同名合并。
func TestBuildPluginRows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)

	writeTestProfile(t, home, "web", map[string]string{
		"npm-a":                 "^1.2.3",
		"git-b":                 "github:user/repo-b",
		"file-c":                "file:/abs/path/c",
		"@deepseek-ai/dsh-base": "0.1.1", // 官方自带：应被排除
	}, map[string]string{
		"npm-a":  "1.2.3",
		"git-b":  "0.9.0",
		"file-c": "0.4.0",
	})
	// 多 profile 同 spec：web 与 default 都声明 npm-a
	writeTestProfile(t, home, "default", map[string]string{
		"npm-a": "^1.2.3",
	}, map[string]string{"npm-a": "1.2.3"})

	rows := buildPluginRows()
	if len(rows) != 3 {
		t.Fatalf("expected 3 user plugins (npm-a/git-b/file-c), got %d: %+v", len(rows), rows)
	}
	byName := map[string]PluginRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	npmA := byName["npm-a"]
	if npmA.Source != "npm" || !npmA.CanUpdate {
		t.Errorf("npm-a: source/canUpdate wrong: %+v", npmA)
	}
	if npmA.Version != "1.2.3" {
		t.Errorf("npm-a version: got %q", npmA.Version)
	}
	if len(npmA.Locs) != 2 {
		t.Errorf("npm-a should merge 2 profiles, locs=%v", npmA.Locs)
	}
	if !strings.Contains(npmA.Profile, "default") || !strings.Contains(npmA.Profile, "web") {
		t.Errorf("npm-a profile label should list both: %q", npmA.Profile)
	}
	gitB := byName["git-b"]
	if gitB.Source != "github" || !gitB.CanUpdate || gitB.Spec != "github:user/repo-b" || gitB.Version != "0.9.0" {
		t.Errorf("git-b wrong: %+v", gitB)
	}
	fileC := byName["file-c"]
	if fileC.Source != "file" || fileC.CanUpdate || fileC.Reason == "" {
		t.Errorf("file-c should be non-updatable with reason: %+v", fileC)
	}
}

// TestSnapshotRestorePluginProfile 更新快照 → 模拟安装改动 → 回退还原（package.json 与 node_modules）。
func TestSnapshotRestorePluginProfile(t *testing.T) {
	dir := t.TempDir()
	pj := filepath.Join(dir, "package.json")
	lock := filepath.Join(dir, "pnpm-lock.yaml")
	origPj := `{"dependencies":{"npm-a":"^1.2.3"}}`
	if err := os.WriteFile(pj, []byte(origPj), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("lockfileVersion: '9.0'"), 0o644); err != nil {
		t.Fatal(err)
	}
	nmFile := filepath.Join(dir, "node_modules", "npm-a", "index.js")
	if err := os.MkdirAll(filepath.Dir(nmFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nmFile, []byte("// a"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !snapshotPluginProfile(dir) {
		t.Fatal("snapshot should rename node_modules")
	}
	if _, err := os.Stat(nmFile); err == nil {
		t.Fatal("node_modules should be moved to .dshbak after snapshot")
	}
	// 模拟更新把 package.json 改掉、node_modules 重建
	if err := os.WriteFile(pj, []byte(`{"dependencies":{"npm-a":"^2.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(nmFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nmFile, []byte("// v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 失败回退：应还原 package.json 内容并移回原 node_modules
	restorePluginProfileSnapshot(dir, true)
	data, err := os.ReadFile(pj)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != origPj {
		t.Errorf("package.json not restored: %s", string(data))
	}
	if got, err := os.ReadFile(nmFile); err != nil || string(got) != "// a" {
		t.Errorf("node_modules not restored: %v %q", err, string(got))
	}

	// 成功后清理快照残留
	cleanupPluginProfileSnapshot(dir)
	for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name+".dshbak")); err == nil {
			t.Errorf("%s.dshbak should be cleaned", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules.dshbak")); err == nil {
		t.Error("node_modules.dshbak should be cleaned")
	}
}

// TestInstalledPluginVersionScoped scoped 包版本读取（node_modules/@scope/name）。
func TestInstalledPluginVersionScoped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "node_modules", "@scope", "plug")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "package.json"), []byte(`{"version":"v1.2.3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := installedPluginVersion(dir, "@scope/plug"); got != "1.2.3" {
		t.Errorf("expected 1.2.3, got %q", got)
	}
	if got := installedPluginVersion(dir, "missing"); got != "" {
		t.Errorf("expected empty for missing pkg, got %q", got)
	}
}

// TestPluginUpdateArgs npm 钉精确版本 + 显式 registry；github 用 pnpm update 重解析。
func TestPluginUpdateArgs(t *testing.T) {
	// npm：必须携带精确目标版本与同源 registry（修复 @latest 元数据缓存漂移）
	args, err := pluginUpdateArgs(PluginRow{Name: "dsh-cost-meter", Source: "npm", Spec: "^1.7.6"},
		"1.7.10", "https://registry.npmjs.org")
	if err != nil {
		t.Fatalf("npm args err: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "dsh-cost-meter@1.7.10") ||
		!strings.Contains(joined, "--registry https://registry.npmjs.org") ||
		strings.Contains(joined, "@latest") {
		t.Errorf("npm args should pin exact version + registry, got %v", args)
	}
	// npm 缺少目标版本应报错
	if _, err := pluginUpdateArgs(PluginRow{Name: "x", Source: "npm"}, "", ""); err == nil {
		t.Error("npm args without target should error")
	}
	// github：pnpm update 重解析（add 同 spec 会复用锁文件固定提交）
	args, err = pluginUpdateArgs(PluginRow{Name: "restrict-discipline", Source: "github", Spec: "github:refyon/restrict-discipline"},
		"0.6.2", "")
	if err != nil {
		t.Fatalf("github args err: %v", err)
	}
	if len(args) != 2 || args[0] != "update" || args[1] != "restrict-discipline" {
		t.Errorf("github args should be [update name], got %v", args)
	}
	// 不可更新来源应报错
	if _, err := pluginUpdateArgs(PluginRow{Name: "local", Source: "file", Reason: "本地"}, "", ""); err == nil {
		t.Error("file source args should error")
	}
}

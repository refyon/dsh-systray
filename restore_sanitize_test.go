package main

// 本地依赖消毒 / reconcile 依赖摘除 / 本地插件选目录 的单元测试（临时目录自包含，不触碰真实 profile）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestJSON(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func readTestJSON(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(data)
}

func TestSanitizeKeepsExistingLocalTarget(t *testing.T) {
	dir := t.TempDir()
	// 源目录存在（同机开发态）：保留原 link spec，不改写
	src := filepath.Join(t.TempDir(), "my-local")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, filepath.Join(dir, "package.json"),
		`{"name":"web","dependencies":{"my-local":"link:`+filepath.ToSlash(src)+`"}}`)
	if notes := sanitizeProfileLocalDeps(dir); len(notes) != 0 {
		t.Fatalf("want no notes, got %v", notes)
	}
	got := readTestJSON(t, filepath.Join(dir, "package.json"))
	if !strings.Contains(got, "link:") {
		t.Fatalf("spec rewritten unexpectedly: %s", got)
	}
}

func TestSanitizeMissingTargetAdoptsRestoredCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "my-local") // 源机路径在本机不存在
	writeTestJSON(t, filepath.Join(dir, "package.json"),
		`{"dependencies":{"my-local":"link:`+filepath.ToSlash(missing)+`"}}`)
	// 导入包恢复出来的 node_modules 副本：应迁到 <home>/local-plugins 并继续加载，而非挂起
	writeTestJSON(t, filepath.Join(dir, "node_modules", "my-local", "package.json"),
		`{"name":"my-local","version":"0.3.1"}`)
	notes := sanitizeProfileLocalDeps(dir)
	if len(notes) != 1 {
		t.Fatalf("want 1 note, got %v", notes)
	}
	if !strings.Contains(notes[0], "继续加载") {
		t.Fatalf("note should mention adopted copy: %v", notes)
	}
	got := readTestJSON(t, filepath.Join(dir, "package.json"))
	if strings.Contains(got, "npm:my-local") {
		t.Fatalf("must not rewrite to npm: %s", got)
	}
	if strings.Contains(got, "pendingLocalPlugins") {
		t.Fatalf("must not park pending when restored copy exists: %s", got)
	}
	canon := filepath.Join(home, "local-plugins", "my-local")
	if !strings.Contains(got, localLinkSpec(canon)) {
		t.Fatalf("spec not rewritten to canonical copy: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "my-local")); err == nil {
		t.Fatalf("restored copy should be moved out of node_modules")
	}
	if _, err := os.Stat(filepath.Join(canon, "package.json")); err != nil {
		t.Fatalf("canonical copy missing: %v", err)
	}
}

func TestSanitizeAdoptKeepsBundleActive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "my-local")
	writeTestJSON(t, filepath.Join(dir, "package.json"),
		`{"dependencies":{"my-local":"link:`+filepath.ToSlash(missing)+`"},
		  "dsh":{"profile":{"bundles":["my-local","other"]}}}`)
	writeTestJSON(t, filepath.Join(dir, "node_modules", "my-local", "package.json"),
		`{"name":"my-local","version":"0.3.1"}`)
	if notes := sanitizeProfileLocalDeps(dir); len(notes) != 1 {
		t.Fatalf("want 1 note, got %v", notes)
	}
	var root struct {
		Dsh struct {
			Profile struct {
				Bundles []string `json:"bundles"`
			} `json:"profile"`
		} `json:"dsh"`
	}
	if err := json.Unmarshal([]byte(readTestJSON(t, filepath.Join(dir, "package.json"))), &root); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"my-local": true, "other": true}
	for _, b := range root.Dsh.Profile.Bundles {
		delete(want, b)
	}
	if len(want) != 0 {
		t.Fatalf("bundle entries changed by adopt: %+v", root.Dsh.Profile.Bundles)
	}
}

func TestSanitizeAdoptSharedAcrossProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	// 源 profile（zip 解压落点，含 node_modules 副本）+ 当前激活 profile（名字不同、无副本）
	srcProf := filepath.Join(home, "profiles", "web")
	actProf := filepath.Join(home, "profiles", "default")
	missing := filepath.Join(t.TempDir(), "my-local")
	pkg := `{"dependencies":{"my-local":"link:` + filepath.ToSlash(missing) + `"},
	  "dsh":{"profile":{"bundles":["my-local"]}}}`
	writeTestJSON(t, filepath.Join(srcProf, "package.json"), pkg)
	writeTestJSON(t, filepath.Join(actProf, "package.json"), pkg)
	writeTestJSON(t, filepath.Join(srcProf, "node_modules", "my-local", "package.json"),
		`{"name":"my-local","version":"0.3.1"}`)
	notes := sanitizeProfileLocalDepsAll([]string{srcProf, actProf})
	if len(notes) != 1 {
		t.Fatalf("want 1 deduped note, got %v", notes)
	}
	canonSpec := localLinkSpec(filepath.Join(home, "local-plugins", "my-local"))
	for _, dir := range []string{srcProf, actProf} {
		got := readTestJSON(t, filepath.Join(dir, "package.json"))
		if !strings.Contains(got, canonSpec) {
			t.Fatalf("%s spec not rewritten to canonical copy: %s", dir, got)
		}
		if strings.Contains(got, "pendingLocalPlugins") {
			t.Fatalf("%s must not park pending when copy adopted: %s", dir, got)
		}
	}
}

func TestSanitizeMissingTargetNoCopyParksPendingNotNpm(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "my-local") // 不存在
	writeTestJSON(t, filepath.Join(dir, "package.json"),
		`{"dependencies":{"my-local":"link:`+filepath.ToSlash(missing)+`"}}`)
	// 导入包内也无副本：不得改写 npm:（registry 版本可能与目标机核心不兼容，导致恢复后启动失败）
	notes := sanitizeProfileLocalDeps(dir)
	if len(notes) != 1 {
		t.Fatalf("want 1 note, got %v", notes)
	}
	got := readTestJSON(t, filepath.Join(dir, "package.json"))
	if strings.Contains(got, "npm:my-local") {
		t.Fatalf("must not rewrite to npm: %s", got)
	}
	if strings.Contains(got, `"my-local": "link:`) {
		t.Fatalf("dep must move out of dependencies: %s", got)
	}
	if !strings.Contains(got, `"pendingLocalPlugins"`) {
		t.Fatalf("pending record missing: %s", got)
	}
	var root struct {
		Dsh struct {
			Profile struct {
				PendingLocalPlugins map[string]pendingLocalFixture `json:"pendingLocalPlugins"`
			} `json:"profile"`
		} `json:"dsh"`
	}
	if err := json.Unmarshal([]byte(got), &root); err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, ok := root.Dsh.Profile.PendingLocalPlugins["my-local"]
	if !ok || !strings.Contains(p.Spec, "my-local") || p.Bundled {
		t.Fatalf("pending record wrong: %+v", root.Dsh.Profile.PendingLocalPlugins)
	}
}

func TestSanitizeMovesPendingStripsBundleKeepsOthers(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "dev-only") // 不存在
	writeTestJSON(t, filepath.Join(dir, "package.json"),
		`{"dependencies":{"dev-only":"link:`+filepath.ToSlash(missing)+`","keep":"^1.0.0"},
		  "dsh":{"profile":{"bundles":["other","dev-only@0.1.0"]}}}`)
	notes := sanitizeProfileLocalDeps(dir)
	if len(notes) != 1 {
		t.Fatalf("want 1 note, got %v", notes)
	}
	got := readTestJSON(t, filepath.Join(dir, "package.json"))
	if strings.Contains(got, `"dev-only": "link:`) {
		t.Fatalf("dep still in dependencies: %s", got)
	}
	if strings.Contains(got, `"dev-only@0.1.0"`) {
		t.Fatalf("bundle entry not stripped: %s", got)
	}
	if !strings.Contains(got, `"keep": "^1.0.0"`) {
		t.Fatalf("other dep touched: %s", got)
	}
	if !strings.Contains(got, `"other"`) {
		t.Fatalf("unrelated bundle touched: %s", got)
	}
	// 曾激活（bundled=true）需记录在待重指定条目里，供重指定时恢复 bundle 激活
	var root struct {
		Dsh struct {
			Profile struct {
				PendingLocalPlugins map[string]pendingLocalFixture `json:"pendingLocalPlugins"`
			} `json:"profile"`
		} `json:"dsh"`
	}
	if err := json.Unmarshal([]byte(got), &root); err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, ok := root.Dsh.Profile.PendingLocalPlugins["dev-only"]
	if !ok || !strings.Contains(p.Spec, "dev-only") || !p.Bundled {
		t.Fatalf("pending record wrong: %+v", root.Dsh.Profile.PendingLocalPlugins)
	}
}

func TestFailingPnpmDepNoMatchingVersion(t *testing.T) {
	out := "ERR_PNPM_NO_MATCHING_VERSION No matching version found for old-thing@9.9.9\n..."
	if got := failingPnpmDep(out, t.TempDir()); got != "old-thing" {
		t.Fatalf("want old-thing, got %q", got)
	}
}

func TestFailingPnpmDepLinkedDir(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "package.json"),
		`{"dependencies":{"normal":"^1.0.0","broken":"link:C:/nope/not-there"}}`)
	out := "[ERR_PNPM_LINKED_PKG_DIR_NOT_FOUND] Could not install from \"C:/nope/not-there\" as it does not exist."
	if got := failingPnpmDep(out, dir); got != "broken" {
		t.Fatalf("want broken, got %q", got)
	}
}

func TestFailingPnpmDepDownloadURL(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "package.json"),
		`{"dependencies":{"good":"^1","deepseek-idesign":"^0.2.2"}}`)
	out := "2026/09/05 15:32:01 ERROR [profile] [WARN] GET https://registry.npmjs.org/deepseek-idesign/-/deepseek-idesign-0.2.2.tgz error (23). Will retry in 10 seconds. 2 retries left"
	if got := failingPnpmDep(out, dir); got != "deepseek-idesign" {
		t.Fatalf("want deepseek-idesign, got %q", got)
	}
	if got := failingPnpmDep("pnpm ERR! some unrelated failure\n...", dir); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestDropProfileDependencyStripsBundle(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "package.json"),
		`{"dependencies":{"gone":"link:C:/nope","stay":"1.0.0"},
		  "dsh":{"profile":{"bundles":["gone","stay"]}}}`)
	if !dropProfileDependency(dir, "gone") {
		t.Fatal("drop returned false")
	}
	got := readTestJSON(t, filepath.Join(dir, "package.json"))
	if strings.Contains(got, "gone") {
		t.Fatalf("gone still present: %s", got)
	}
	if !strings.Contains(got, "stay") {
		t.Fatalf("stay removed unexpectedly: %s", got)
	}
	if !dropProfileDependency(dir, "missing-dep") {
		// 二次移除不存在的依赖：应返回 false
	} else {
		t.Fatal("drop of nonexistent dep should return false")
	}
}

func TestLocalPickRelation(t *testing.T) {
	cases := []struct{ cur, picked, want string }{
		{"1.0.0", "1.0.0", "same"},
		{"1.0.0", "1.2.0", "newer"},
		{"1.2.0", "1.0.0", "older"},
		{"", "1.0.0", "unknown"},
		{"1.0.0", "", "unknown"},
		{"", "", "unknown"},
	}
	for _, c := range cases {
		if got := localPickRelation(c.cur, c.picked); got != c.want {
			t.Fatalf("localPickRelation(%q,%q)=%q want %q", c.cur, c.picked, got, c.want)
		}
	}
}

func TestLocalLinkSpecNormalizesSlashes(t *testing.T) {
	spec := localLinkSpec(filepath.Join("C:", "dev", "plugin-x"))
	if !strings.HasPrefix(spec, "link:") {
		t.Fatalf("spec missing link: prefix: %s", spec)
	}
	if strings.Contains(spec, `\`) {
		t.Fatalf("spec contains backslashes: %s", spec)
	}
}

func TestLocalSpecPathForms(t *testing.T) {
	for _, s := range []string{"link:C:/a/b", "file:../dev/x", "workspace:y", "./rel", `D:\abs\pkg`} {
		if p, ok := localSpecPath(s); !ok || p == "" {
			t.Fatalf("localSpecPath(%q) not detected", s)
		}
	}
	for _, s := range []string{"^1.2.3", "github:user/repo", "latest"} {
		if _, ok := localSpecPath(s); ok {
			t.Fatalf("localSpecPath(%q) misclassified as local", s)
		}
	}
}

func TestPendingLocalHelpers(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "package.json"), `{"name":"web","dependencies":{}}`)
	if list := listPendingLocalEntries(dir); len(list) != 0 {
		t.Fatalf("unexpected pending: %v", list)
	}
	if _, ok := readPendingLocal(dir, "x"); ok {
		t.Fatal("pending x should not exist")
	}
	root := readProfileRoot(dir)
	setPendingLocalEntry(root, "x", "link:C:/dev/x", true)
	if err := writeProfileRoot(dir, root); err != nil {
		t.Fatal(err)
	}
	if list := listPendingLocalEntries(dir); len(list) != 1 ||
		list["x"].spec != "link:C:/dev/x" || !list["x"].bundled {
		t.Fatalf("pending list wrong: %v", list)
	}
	if d, ok := readPendingLocal(dir, "x"); !ok || d.bundled != true {
		t.Fatalf("readPendingLocal wrong: %+v", d)
	}
	// setPendingLocalEntry 不覆盖既有记录
	root2 := readProfileRoot(dir)
	setPendingLocalEntry(root2, "x", "link:C:/else", false)
	if err := writeProfileRoot(dir, root2); err != nil {
		t.Fatal(err)
	}
	if d, _ := readPendingLocal(dir, "x"); d.spec != "link:C:/dev/x" {
		t.Fatalf("set must not overwrite existing: %+v", d)
	}
	// clearPendingLocal 幂等；空表连同键一起移除
	if !clearPendingLocal(dir, "x") {
		t.Fatal("clear should report removed")
	}
	if clearPendingLocal(dir, "x") {
		t.Fatal("second clear should report nothing")
	}
	got := readTestJSON(t, filepath.Join(dir, "package.json"))
	if strings.Contains(got, "pendingLocalPlugins") {
		t.Fatalf("empty pending key left in file: %s", got)
	}
}

func TestRelinkPendingRestoresBundleAndClearsRecord(t *testing.T) {
	// bundled=true：重指定时恢复 bundle 激活并清除挂起记录
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "x")
	writeTestJSON(t, filepath.Join(dir, "package.json"),
		`{"dependencies":{},"dsh":{"profile":{"pendingLocalPlugins":{"x":{"spec":"link:`+
			filepath.ToSlash(missing)+`","bundled":true}}}}}`)
	relinkPendingLocal(dir, "x")
	f := readPkgFixture(t, dir)
	if _, ok := f.Dsh.Profile.PendingLocalPlugins["x"]; ok {
		t.Fatalf("pending record not cleared: %+v", f.Dsh.Profile.PendingLocalPlugins)
	}
	if len(f.Dsh.Profile.Bundles) != 1 || f.Dsh.Profile.Bundles[0] != "x" {
		t.Fatalf("bundle activation not restored: %+v", f.Dsh.Profile.Bundles)
	}
	// bundled=false：历史记录未激活，但「重选=显式激活」语义下同样补进 bundles——
	// 用户重选目录即期望该插件加载进 harness，历史 bundled 只记录源机状态、不决定本次激活
	//（旧语义「只清记录、不激活」会导致重选后 spec/node_modules 就位但 harness 永不加载）。
	dir2 := t.TempDir()
	writeTestJSON(t, filepath.Join(dir2, "package.json"),
		`{"dependencies":{},"dsh":{"profile":{"pendingLocalPlugins":{"y":{"spec":"link:C:/nope","bundled":false}}}}}`)
	relinkPendingLocal(dir2, "y")
	f2 := readPkgFixture(t, dir2)
	if _, ok := f2.Dsh.Profile.PendingLocalPlugins["y"]; ok {
		t.Fatalf("pending not cleared: %+v", f2.Dsh.Profile.PendingLocalPlugins)
	}
	if len(f2.Dsh.Profile.Bundles) != 1 || f2.Dsh.Profile.Bundles[0] != "y" {
		t.Fatalf("relink must always activate (re-select = explicit activation): %+v", f2.Dsh.Profile.Bundles)
	}
}

func TestBuildPluginRowsSynthesizesPending(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	prof := filepath.Join(home, "profiles", "web")
	missing := filepath.Join(t.TempDir(), "my-dev-tool")
	writeTestJSON(t, filepath.Join(prof, "package.json"),
		`{"name":"web","dependencies":{"normal":"^1.0.0"},
		  "dsh":{"profile":{"bundles":["normal"],"pendingLocalPlugins":{
		     "my-dev-tool":{"spec":"link:`+filepath.ToSlash(missing)+`","bundled":true}}}}}`)
	rows := buildPluginRows()
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %+v", rows)
	}
	var pend *PluginRow
	for i := range rows {
		if rows[i].Name == "my-dev-tool" {
			pend = &rows[i]
		}
	}
	if pend == nil {
		t.Fatalf("pending row missing: %+v", rows)
	}
	if !pend.PendingLocal || pend.Source != "file" || pend.CanUpdate {
		t.Fatalf("pending row flags wrong: %+v", pend)
	}
	if !strings.Contains(pend.Reason, "重新选择本地目录") {
		t.Fatalf("pending reason missing guidance: %q", pend.Reason)
	}
	if len(pend.Locs) != 1 || !strings.HasSuffix(pend.Locs[0], "web") {
		t.Fatalf("pending locs wrong: %v", pend.Locs)
	}
	if pend.Version != "" {
		t.Fatalf("pending version should be empty (not installed): %q", pend.Version)
	}
}

func TestImportJournalRoundtrip(t *testing.T) {
	dir := t.TempDir()
	old := importJournalDirOverride
	importJournalDirOverride = dir
	defer func() { importJournalDirOverride = old }()

	if _, err := readImportJournal(); err == nil {
		t.Fatal("journal should not exist initially")
	}
	j := importJournal{Stage: "healing", Kind: "plugins",
		Dirs: []string{"C:/a/web"}, HadNM: []bool{true}}
	if err := writeImportJournal(j); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readImportJournal()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Stage != "healing" || len(got.Dirs) != 1 || !got.HadNM[0] {
		t.Fatalf("journal mismatch: %+v", got)
	}
	// 覆盖写
	j2 := importJournal{Stage: "importing", Kind: "plugins", Dirs: []string{"C:/b"}, HadNM: []bool{false}}
	if err := writeImportJournal(j2); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got2, _ := readImportJournal()
	if got2.Stage != "importing" {
		t.Fatalf("overwrite not applied: %+v", got2)
	}
	clearImportJournal()
	if _, err := readImportJournal(); err == nil {
		t.Fatal("journal should be cleared")
	}
	if _, err := os.Stat(importJournalPath() + ".tmp"); err == nil {
		t.Fatal("tmp file left behind")
	}
}

func TestPreflightSpecClassifiers(t *testing.T) {
	plainCases := map[string]bool{
		"1.2.3": true, "v1.2.3": true, "0.2.2": true, "0.2.2-beta.1": true,
		"^0.2.2": false, "~1.2.3": false, "latest": false, "*": false, "^0.2": false,
		"1.2.3+build": false, // 带 build 元数据不做精确端点校验（registry 常规无此形态）
	}
	for spec, want := range plainCases {
		if got := plainVerRe.MatchString(spec); got != want {
			t.Fatalf("plainVerRe(%q)=%v want %v", spec, got, want)
		}
	}
	rangeCases := map[string]bool{
		"^0.2.2": true, "~1.2.3": true, "1.2.3": false, "^0.2": false, "latest": false,
	}
	for spec, want := range rangeCases {
		if got := npmRangeExactRe.MatchString(spec); got != want {
			t.Fatalf("npmRangeExactRe(%q)=%v want %v", spec, got, want)
		}
	}
	if !preflightNpmDepSpec("my-plug", "^1.0.0") {
		t.Fatal("registry dep should be preflighted")
	}
	for _, skip := range []struct{ n, s string }{
		{"@deepseek-ai/official", "^1.0.0"},
		{"my-plug", "file:../x"},
		{"my-plug", "github:owner/repo"},
		{"my-plug", "npm:other@1"},
		{"my-plug", "https://example.com/a.tgz"},
	} {
		if preflightNpmDepSpec(skip.n, skip.s) {
			t.Fatalf("preflightNpmDepSpec should skip (%q, %q)", skip.n, skip.s)
		}
	}
}

func TestNpmDocVersions(t *testing.T) {
	body := `{"name":"x","versions":{"0.2.1":{},"0.2.2":{},"v0.3.0":{}}}`
	v := npmDocVersions([]byte(body))
	if len(v) != 3 || !v["0.2.2"] || !v["0.3.0"] || v["0.2.2x"] {
		t.Fatalf("versions wrong: %v", v)
	}
}

package main

// 插件树确定性预检 / 悬空本地依赖守卫 的单元测试（临时目录自包含，不触碰真实 profile；
// 需要 node 的用例在运行时不可用时跳过）。

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundleNameBase(t *testing.T) {
	cases := map[string]string{
		"restrict-discipline":       "restrict-discipline",
		"restrict-discipline@0.9.0": "restrict-discipline",
		"@scope/pkg":                "@scope/pkg",
		"":                          "",
	}
	for in, want := range cases {
		if got := bundleNameBase(in); got != want {
			t.Errorf("bundleNameBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePluginCheckOutput(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		wantKind string
		wantMsg  string
	}{
		{"通过", "DSHPREFLIGHT\tOK\n", "", ""},
		{"无 apply", "DSHPREFLIGHT\tNOAPPLY\n", "no-apply", ""},
		{
			"缺具名导出（assertNever 同类）",
			"DSHPREFLIGHT\tERR\tThe requested module 'x' does not provide an export named 'assertNever'\n",
			"import-error", "The requested module 'x' does not provide an export named 'assertNever'",
		},
		{"ERR 无原因", "DSHPREFLIGHT\tERR\n", "import-error", "插件入口加载失败"},
		{"无结果行", "some unrelated noise\n", "no-result", ""},
		{"混在日志里也能命中", "noise\nDSHPREFLIGHT\tOK\nmore noise\n", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, msg := parsePluginCheckOutput(c.out)
			if kind != c.wantKind || msg != c.wantMsg {
				t.Fatalf("got (%q,%q), want (%q,%q)", kind, msg, c.wantKind, c.wantMsg)
			}
		})
	}
}

func TestProfileBundleNamesSkipsOfficialAndVariants(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "package.json"), `{
	  "dependencies": {"restrict-discipline":"github:refyon/restrict-discipline"},
	  "dsh": {"profile": {"bundles": [
	    "@deepseek-ai/dsh-base",
	    "@deepseek-ai/dsh-web-app",
	    "dsh-ui-taste@0.2.1",
	    "restrict-discipline",
	    "dsh-ui-taste",
	    "restrict-discipline"
	  ]}}
	}`)
	got := profileBundleNames(dir)
	want := []string{"dsh-ui-taste", "restrict-discipline"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// 2026-09-10 实录：删除 dsh-codegraph 时 pnpm 因**另一个**插件的本地路径失效而整条失败。
// 这里验证「scandir 报错 → 归因到正确的依赖名」这一环。
func TestFailingPnpmDepScandirLocalDep(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "moved-plugin") // 本机不存在
	writeTestJSON(t, filepath.Join(dir, "package.json"),
		`{"dependencies":{"dsh-ui-taste":"file:`+filepath.ToSlash(missing)+`","other":"^1.0.0"}}`)

	out := "[ERROR] [profile] [ENOENT] ENOENT: no such file or directory, scandir '" + missing + "'"
	if got := failingPnpmDep(out, dir); got != "dsh-ui-taste" {
		t.Fatalf("want dsh-ui-taste, got %q", got)
	}

	// 与任何本地依赖无关的 scandir：不误归因
	other := "[ENOENT] ENOENT: no such file or directory, scandir '" + filepath.Join(t.TempDir(), "unrelated") + "'"
	if got := failingPnpmDep(other, dir); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

// guardProfileLocalDeps：悬空 file: 依赖被摘除并记为待重指定，无关依赖与 bundle 不受影响；
// 重复调用幂等（第二次无事可做）。
func TestGuardProfileLocalDepsDetachesDanglingFileDep(t *testing.T) {
	t.Setenv("DSH_HOME", t.TempDir())
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")
	writeTestJSON(t, filepath.Join(dir, "package.json"), `{
	  "dependencies": {
	    "dsh-ui-taste": "file:`+filepath.ToSlash(missing)+`",
	    "restrict-discipline": "github:refyon/restrict-discipline"
	  },
	  "dsh": {"profile": {"bundles": [
	    "@deepseek-ai/dsh-base", "dsh-ui-taste", "restrict-discipline"
	  ]}}
	}`)

	notes := guardProfileLocalDeps(dir)
	if len(notes) != 1 {
		t.Fatalf("want 1 note, got %v", notes)
	}
	root := map[string]interface{}{}
	if err := json.Unmarshal([]byte(readTestJSON(t, filepath.Join(dir, "package.json"))), &root); err != nil {
		t.Fatal(err)
	}
	deps, _ := root["dependencies"].(map[string]interface{})
	if _, ok := deps["dsh-ui-taste"]; ok {
		t.Fatal("悬空本地依赖应被摘除")
	}
	if _, ok := deps["restrict-discipline"]; !ok {
		t.Fatal("无关依赖不应被改动")
	}
	if !strings.Contains(readTestJSON(t, filepath.Join(dir, "package.json")), pendingLocalKey) {
		t.Fatal("应写入待重指定记录")
	}
	dsh, _ := root["dsh"].(map[string]interface{})
	prof, _ := dsh["profile"].(map[string]interface{})
	bundles, _ := prof["bundles"].([]interface{})
	for _, b := range bundles {
		if s, _ := b.(string); s == "dsh-ui-taste" {
			t.Fatal("悬空插件应从 bundles 摘除（残留会导致 cannot resolve profile bundle）")
		}
	}
	// 幂等：已消毒过，再调用不应再产生说明
	if again := guardProfileLocalDeps(dir); len(again) != 0 {
		t.Fatalf("want no notes on second call, got %v", again)
	}
}

// end-to-end（需 node）：坏插件入口 import 失败 → 精确点名并自动禁用；好插件不动；
// 只缺 apply 的插件只提示不禁用（避免误伤）。
func TestPreflightTreeCompatibility(t *testing.T) {
	if !nodeAvailable() {
		t.Skip("node 不可用：跳过 exec 类预检用例")
	}
	t.Setenv("DSH_HOME", t.TempDir())
	dir := t.TempDir()

	// 坏插件：引用不存在的具名导出（与 assertNever 事故同类，import 期即失败）
	writeTestJSON(t, filepath.Join(dir, "node_modules", "bad-plugin", "package.json"),
		`{"name":"bad-plugin","version":"1.0.0","type":"module","main":"index.js"}`)
	writeTestJSON(t, filepath.Join(dir, "node_modules", "bad-plugin", "index.js"),
		"import { notARealExport } from 'node:fs';\nexport const apply = () => {};\n")

	// 好插件：正常 host 插件形状
	writeTestJSON(t, filepath.Join(dir, "node_modules", "good-plugin", "package.json"),
		`{"name":"good-plugin","version":"1.0.0","type":"module","main":"index.js"}`)
	writeTestJSON(t, filepath.Join(dir, "node_modules", "good-plugin", "index.js"),
		"export const name = 'good-plugin';\nexport const apply = () => {};\n")

	// 只缺 apply：能加载但形状可疑 → 仅提示
	writeTestJSON(t, filepath.Join(dir, "node_modules", "noapply-plugin", "package.json"),
		`{"name":"noapply-plugin","version":"1.0.0","type":"module","main":"index.js"}`)
	writeTestJSON(t, filepath.Join(dir, "node_modules", "noapply-plugin", "index.js"),
		"export const name = 'noapply-plugin';\n")

	writeTestJSON(t, filepath.Join(dir, "package.json"), `{
	  "dependencies": {"bad-plugin":"^1.0.0","good-plugin":"^1.0.0","noapply-plugin":"^1.0.0"},
	  "dsh": {"profile": {"bundles": [
	    "@deepseek-ai/dsh-base", "bad-plugin", "good-plugin", "noapply-plugin"
	  ]}}
	}`)

	disabled, notes := preflightTreeCompatibility([]string{dir})
	if len(disabled) != 1 || disabled[0] != "bad-plugin" {
		t.Fatalf("want [bad-plugin] disabled, got %v (notes=%v)", disabled, notes)
	}
	joined := strings.Join(notes, "；")
	if !strings.Contains(joined, "bad-plugin") || !strings.Contains(joined, "不兼容") {
		t.Fatalf("说明应点名不兼容插件，got %v", notes)
	}
	if !strings.Contains(joined, "noapply-plugin") {
		t.Fatalf("缺 apply 的插件应被提示，got %v", notes)
	}

	root := map[string]interface{}{}
	if err := json.Unmarshal([]byte(readTestJSON(t, filepath.Join(dir, "package.json"))), &root); err != nil {
		t.Fatal(err)
	}
	dsh, _ := root["dsh"].(map[string]interface{})
	prof, _ := dsh["profile"].(map[string]interface{})
	dm, _ := prof["disabledPlugins"].(map[string]interface{})
	if _, ok := dm["bad-plugin"]; !ok {
		t.Fatal("坏插件应写入禁用记录（关于页可见、可重新启用）")
	}
	if _, ok := dm["good-plugin"]; ok {
		t.Fatal("好插件不应被禁用")
	}
	if _, ok := dm["noapply-plugin"]; ok {
		t.Fatal("仅缺 apply 的插件不应被禁用")
	}
	bundles, _ := prof["bundles"].([]interface{})
	var haveGood, haveBad bool
	for _, b := range bundles {
		switch s, _ := b.(string); s {
		case "good-plugin":
			haveGood = true
		case "bad-plugin":
			haveBad = true
		}
	}
	if !haveGood || haveBad {
		t.Fatalf("bundles 应保留好插件、摘除坏插件，got %v", bundles)
	}

	// 第二次预检：坏插件已在 bundles 之外，不再命中
	if d2, _ := preflightTreeCompatibility([]string{dir}); len(d2) != 0 {
		t.Fatalf("重复预检不应再禁用，got %v", d2)
	}
}

// 入口文件缺失（ERR_MODULE_NOT_FOUND 类）：同样应被点名禁用。
func TestPreflightDetectsMissingEntry(t *testing.T) {
	if !nodeAvailable() {
		t.Skip("node 不可用：跳过 exec 类预检用例")
	}
	t.Setenv("DSH_HOME", t.TempDir())
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "node_modules", "ghost-plugin", "package.json"),
		`{"name":"ghost-plugin","version":"1.0.0","type":"module","main":"lib/index.js"}`)
	writeTestJSON(t, filepath.Join(dir, "package.json"), `{
	  "dependencies": {"ghost-plugin":"^1.0.0"},
	  "dsh": {"profile": {"bundles": ["ghost-plugin"]}}
	}`)

	disabled, _ := preflightTreeCompatibility([]string{dir})
	if len(disabled) != 1 || disabled[0] != "ghost-plugin" {
		t.Fatalf("want [ghost-plugin] disabled, got %v", disabled)
	}
}

// 端到端回归（需 node+pnpm，纯本地依赖、不访问网络）：复现 2026-09-10「删除插件被无关
// 插件的悬空本地依赖打挂」，再验证 guardProfileLocalDeps 之后同一条命令成功。
func TestGuardUnblocksPnpmRemoveWithDanglingLocalDep(t *testing.T) {
	if !nodeAvailable() || !pnpmAvailable() {
		t.Skip("node/pnpm 不可用：跳过端到端 pnpm 用例")
	}
	t.Setenv("DSH_HOME", t.TempDir())
	dir := t.TempDir()
	present := filepath.Join(dir, "src-present")
	writeTestJSON(t, filepath.Join(present, "package.json"), `{"name":"present","version":"1.0.0","main":"index.js"}`)
	writeTestJSON(t, filepath.Join(present, "index.js"), "export const apply = () => {};\n")
	writeTestJSON(t, filepath.Join(dir, "pnpm-workspace.yaml"),
		"packages:\n  - .\n\nnodeLinker: hoisted\nautoInstallPeers: false\n")
	writeTestJSON(t, filepath.Join(dir, "package.json"), `{
	  "name": "e2e", "private": true,
	  "dependencies": {
	    "present": "file:`+filepath.ToSlash(present)+`",
	    "dangling": "file:`+filepath.ToSlash(filepath.Join(dir, "missing-dir"))+`"
	  }
	}`)

	// 1) 悬空依赖在：与被删插件无关，但整条 pnpm remove 失败（既有故障形态）
	if out, err := runProfileCmdCapture(dir, pnpmCmd(), "remove", "present"); err == nil {
		t.Skipf("本机 pnpm 未复现该故障（可能版本行为不同），跳过：%s", out)
	}

	// 2) 守卫消毒悬空依赖
	notes := guardProfileLocalDeps(dir)
	if len(notes) != 1 || !strings.Contains(notes[0], "dangling") {
		t.Fatalf("want 1 note naming dangling, got %v", notes)
	}

	// 3) 同一条命令现在成功（操作落到目标插件本身）
	if out, err := runProfileCmdCapture(dir, pnpmCmd(), "remove", "present"); err != nil {
		t.Fatalf("消毒后 pnpm remove 仍失败：%v\n%s", err, out)
	}
}

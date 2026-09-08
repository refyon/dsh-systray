package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestUpdatePayloadPathNested 更新包兼容一层子目录（历史 `dist/` 前缀布局）：
// 无包报错 → 嵌套命中兜底 → 根级与嵌套并存时根级优先。
func TestUpdatePayloadPathNested(t *testing.T) {
	payload := "dsh-systray.exe"
	if runtime.GOOS != "windows" {
		payload = "dsh-systray.app"
	}
	dir := t.TempDir()
	if _, err := updatePayloadPath(dir); err == nil {
		t.Fatal("expected error on empty extract dir")
	}
	nested := filepath.Join(dir, "dist", payload)
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := updatePayloadPath(dir)
	if err != nil {
		t.Fatalf("nested fallback failed: %v", err)
	}
	if got != nested {
		t.Fatalf("nested fallback got %s, want %s", got, nested)
	}
	// 根级存在时优先于嵌套
	if err := os.RemoveAll(nested); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, payload)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	got2, err := updatePayloadPath(dir)
	if err != nil {
		t.Fatalf("root case failed: %v", err)
	}
	if got2 != root {
		t.Fatalf("root should win, got %s", got2)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// 用户反馈场景：GitHub 最新 dsh-v0.1.2-alpha.2 > 已装 0.1.1-rc.2
		{"v0.1.2-alpha.2", "v0.1.1-rc.2", 1},
		{"dsh-v0.1.2-alpha.2", "0.1.1-rc.2", 1},
		// 预发布内部排序
		{"0.1.2-alpha.2", "0.1.2-alpha.1", 1},
		{"0.1.2-alpha", "0.1.2-alpha.1", -1},
		// 稳定版 > 预发布版
		{"0.1.2", "0.1.2-alpha.2", 1},
		{"0.1.2-alpha.2", "0.1.2", -1},
		// dsh-systray 自身版本
		{"v0.4.7", "0.4.7", 0},
		{"v0.4.8", "v0.4.7", 1},
		{"v0.10.0", "v0.9.9", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q)=%d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsNewerVersion(t *testing.T) {
	if !isNewerVersion("dsh-v0.1.2-alpha.2", "v0.1.1-rc.2") {
		t.Errorf("expected dsh-v0.1.2-alpha.2 newer than v0.1.1-rc.2")
	}
	if isNewerVersion("v0.4.7", "0.4.7") {
		t.Errorf("v0.4.7 should NOT be newer than 0.4.7")
	}
}

func TestIsStableVersion(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"0.1.2", true},
		{"0.4.13", true},
		{"1.0.0", true},
		{"0.1.1-rc.2", false},
		{"0.1.2-alpha.2", false},
		{"0.1.2-beta.1", false},
	}
	for _, c := range cases {
		if got := isStableVersion(c.v); got != c.want {
			t.Errorf("isStableVersion(%q)=%v, want %v", c.v, got, c.want)
		}
	}
}

func TestPickHarnessVersion(t *testing.T) {
	tags := []string{
		"dsh-v0.1.1-rc.1",
		"dsh-v0.1.1-rc.2",
		"dsh-v0.1.2-alpha.2",
		"dsh-v0.1.1",
	}
	// 默认仅稳定版：0.1.1（稳定）> 0.1.1-rc.2（预发布），alpha/rc 全部排除
	if got := pickHarnessVersion(tags, false); got != "0.1.1" {
		t.Errorf("stable-only pick = %q, want 0.1.1", got)
	}
	// 允许预发布：0.1.2-alpha.2 > 0.1.1
	if got := pickHarnessVersion(tags, true); got != "0.1.2-alpha.2" {
		t.Errorf("prerelease pick = %q, want 0.1.2-alpha.2", got)
	}
	// 全部为预发布且不允许预发布 → 无可更新版本
	preOnly := []string{"dsh-v0.1.1-rc.2", "dsh-v0.1.2-alpha.2"}
	if got := pickHarnessVersion(preOnly, false); got != "" {
		t.Errorf("pre-only stable pick = %q, want empty", got)
	}
	if got := pickHarnessVersion(nil, true); got != "" {
		t.Errorf("empty tags pick = %q, want empty", got)
	}
}

// TestResolveHarnessLatest 预发布通道关闭时的“无法获取”修复：仓库只有预发布时，
// 应返回说明文案而非空错误；通道开启或存在稳定版时无说明。
func TestResolveHarnessLatest(t *testing.T) {
	preOnly := []string{"dsh-v0.1.1-rc.2", "dsh-v0.1.2-alpha.2", "dsh-v0.1.2-rc.1"}
	withStable := append(preOnly, "dsh-v0.1.1")

	// 仓库全为预发布 + 通道关闭：无可更新版本，但给出面向用户说明（含最新预发布版号）
	latest, newest, note := resolveHarnessLatest(preOnly, false)
	if latest != "" {
		t.Errorf("pre-only + channel off: latest = %q, want empty", latest)
	}
	if newest != "0.1.2-rc.1" {
		t.Errorf("pre-only: newest = %q, want 0.1.2-rc.1", newest)
	}
	if note == "" || !strings.Contains(note, "v0.1.2-rc.1") {
		t.Errorf("pre-only + channel off: note = %q, want guidance mentioning v0.1.2-rc.1", note)
	}

	// 通道开启：latest 即仓库最新（预发布），无说明
	latest, newest, note = resolveHarnessLatest(preOnly, true)
	if latest != "0.1.2-rc.1" || newest != "0.1.2-rc.1" {
		t.Errorf("pre-only + channel on: latest/newest = %q/%q, want 0.1.2-rc.1", latest, newest)
	}
	if note != "" {
		t.Errorf("pre-only + channel on: note = %q, want empty", note)
	}

	// 存在稳定版 + 通道关闭：取稳定版，无说明
	latest, _, note = resolveHarnessLatest(withStable, false)
	if latest != "0.1.1" {
		t.Errorf("with stable + channel off: latest = %q, want 0.1.1", latest)
	}
	if note != "" {
		t.Errorf("with stable + channel off: note = %q, want empty", note)
	}

	// 空标签集：无可更新、无说明
	latest, _, note = resolveHarnessLatest(nil, false)
	if latest != "" || note != "" {
		t.Errorf("empty tags: latest/note = %q/%q, want empty", latest, note)
	}
}

// TestResolveNpmUpdateTarget npm 形态的预发布通道判定：npm 已发布版本全为预发布
// （实测 registry 现状：0.0.1-rc.1 … 0.1.3-alpha.2，无稳定版）时，通道关闭不应返回
// 任何更新目标（此前误报“检测到 0.1.3-alpha.2”），只给说明；通道开启才取最新预发布。
func TestResolveNpmUpdateTarget(t *testing.T) {
	// 真实 npm 版本表（@deepseek-ai/dsh，全预发布，最新 0.1.3-alpha.2）
	npmPreOnly := []string{
		"0.0.1-rc.1", "0.0.1-rc.2", "0.0.1-rc.5",
		"0.1.0-rc.2", "0.1.0-rc.3", "0.1.0-rc.6", "0.1.0-rc.7", "0.1.0-rc.8",
		"0.1.1-rc.1", "0.1.1-rc.2",
		"0.1.2-alpha.2", "0.1.2-alpha.3", "0.1.2-alpha.4", "0.1.2-alpha.5",
		"0.1.2-rc.1", "0.1.3-alpha.2",
	}

	// 通道关闭 + 全预发布：不返回更新目标（修复误报的核心断言），note 提及最新发布
	v, note, err := resolveNpmUpdateTarget(npmPreOnly, false)
	if err != nil {
		t.Fatalf("channel off: unexpected err %v", err)
	}
	if v != "" {
		t.Errorf("channel off + pre-only: version = %q, want empty (无稳定版不得检测预发布)", v)
	}
	if note == "" || !strings.Contains(note, "v0.1.3-alpha.2") {
		t.Errorf("channel off + pre-only: note = %q, want guidance mentioning v0.1.3-alpha.2", note)
	}

	// 通道开启 + 全预发布：取版本号最大者（0.1.3-alpha.2），附“全预发布”说明
	v, note, err = resolveNpmUpdateTarget(npmPreOnly, true)
	if err != nil {
		t.Fatalf("channel on: unexpected err %v", err)
	}
	if v != "0.1.3-alpha.2" {
		t.Errorf("channel on + pre-only: version = %q, want 0.1.3-alpha.2", v)
	}
	if note == "" {
		t.Error("channel on + pre-only: note empty, want 说明（npm 仅有预发布）")
	}

	// 存在稳定版 + 通道关闭：取最新稳定版，无说明
	npmWithStable := append(append([]string{}, npmPreOnly...), "0.1.2", "0.1.1")
	v, note, err = resolveNpmUpdateTarget(npmWithStable, false)
	if err != nil {
		t.Fatalf("with stable + channel off: unexpected err %v", err)
	}
	if v != "0.1.2" {
		t.Errorf("with stable + channel off: version = %q, want 0.1.2", v)
	}
	if note != "" {
		t.Errorf("with stable + channel off: note = %q, want empty", note)
	}

	// 存在稳定版但更新预发布 + 通道开启：取版本号最大（与源码形态 resolveHarnessLatest 一致）
	v, note, err = resolveNpmUpdateTarget(npmWithStable, true)
	if err != nil {
		t.Fatalf("with stable + channel on: unexpected err %v", err)
	}
	if v != "0.1.3-alpha.2" {
		t.Errorf("with stable + channel on: version = %q, want 0.1.3-alpha.2（版本号最大含预发布）", v)
	}
	if note != "" {
		t.Errorf("with stable + channel on: note = %q, want empty（存在稳定版时不附全预发布说明）", note)
	}

	// 空版本表：错误返回
	if _, _, err = resolveNpmUpdateTarget(nil, false); err == nil {
		t.Error("empty versions: want error")
	}
}

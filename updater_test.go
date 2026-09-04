package main

import (
	"strings"
	"testing"
)

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

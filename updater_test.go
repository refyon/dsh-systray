package main

import "testing"

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

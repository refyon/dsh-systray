package main

import (
	"strings"
	"testing"
)

// TestBuildResetVersionOptions 重置目标版本候选构建（纯函数，不触达网络/配置）：
// 列出 npm 全部已发布版本（含高于当前版本与预发布——不再按「不高于当前」过滤，支持升级重装）、
// 按新→旧排序、预发布标注；默认选中当前版本（同版本重装）；当前不在列表时取「不高于当前的
// 最近稳定版」，再取最新稳定版，全部为预发布时取最新发布；current 为空时默认最新稳定版。
func TestBuildResetVersionOptions(t *testing.T) {
	t.Run("all-versions-incl-newer-sorted-default-current", func(t *testing.T) {
		versions := []string{"0.1.1", "0.1.2-rc.1", "0.1.2", "0.1.2-alpha.1", "0.1.1-rc.2", "0.1.0"}
		opts, def := buildResetVersionOptions(versions, "0.1.2-rc.1")
		// 稳定版 0.1.2 高于当前 rc 也列入（升级重装候选）；整体按版本号新→旧排序
		want := []string{"0.1.2", "0.1.2-rc.1", "0.1.2-alpha.1", "0.1.1", "0.1.1-rc.2", "0.1.0"}
		if got := versionSeq(opts); !sameStrings(got, want) {
			t.Fatalf("opts=%v want %v", got, want)
		}
		if def != "0.1.2-rc.1" {
			t.Errorf("default=%s want current 0.1.2-rc.1（同版本重装）", def)
		}
		if opts[0].Prerelease || !opts[1].Prerelease || !opts[2].Prerelease || opts[3].Prerelease || !opts[4].Prerelease || opts[5].Prerelease {
			t.Errorf("prerelease flags wrong: %+v", opts)
		}
	})
	t.Run("newest-stable-listed-first-current-stable-default-current", func(t *testing.T) {
		versions := []string{"0.1.0", "0.1.1", "0.1.1-rc.2", "0.1.2"}
		opts, def := buildResetVersionOptions(versions, "0.1.1")
		if len(opts) != 4 {
			t.Fatalf("len=%d want 4: %v", len(opts), opts)
		}
		if opts[0].Version != "0.1.2" || def != "0.1.1" {
			t.Errorf("opts=%v def=%s want newest-first options and current as default", opts, def)
		}
	})
	t.Run("current-absent-default-nearest-stable-not-newer", func(t *testing.T) {
		versions := []string{"0.1.1", "0.1.2-rc.1", "0.1.0"}
		opts, def := buildResetVersionOptions(versions, "0.1.2-rc.2") // 该 rc 未发布
		if len(opts) != 3 {
			t.Fatalf("len=%d want 3: %v", len(opts), opts)
		}
		// 0.1.2-rc.1 高于 0.1.2-rc.2？否——rc.2 更新，故其不高于当前；0.1.1 稳定且不高于当前 → 默认 0.1.1
		if def != "0.1.1" {
			t.Errorf("default=%s want 0.1.1", def)
		}
	})
	t.Run("current-older-than-all-defaults-newest-stable", func(t *testing.T) {
		versions := []string{"0.1.0", "0.1.1"}
		opts, def := buildResetVersionOptions(versions, "0.0.9")
		if len(opts) != 2 {
			t.Fatalf("len=%d want 2: %v", len(opts), opts)
		}
		// 无不高于当前的稳定版 → 取最新稳定版（0.1.1，即升级重装）
		if opts[0].Version != "0.1.1" || def != "0.1.1" {
			t.Errorf("opts=%v def=%s", opts, def)
		}
	})
	t.Run("prerelease-only-default-newest", func(t *testing.T) {
		versions := []string{"0.1.2-alpha.1", "0.1.2-alpha.3", "0.1.2-alpha.2"}
		opts, def := buildResetVersionOptions(versions, "0.1.2-rc.1")
		if len(opts) != 3 {
			t.Fatalf("len=%d", len(opts))
		}
		if opts[0].Version != "0.1.2-alpha.3" || def != "0.1.2-alpha.3" {
			t.Errorf("opts=%v def=%s", opts, def)
		}
	})
	t.Run("current-unknown-lists-all-default-newest-stable", func(t *testing.T) {
		versions := []string{"0.1.1-rc.1", "0.1.1", "0.1.2-alpha.1", "0.1.0"}
		opts, def := buildResetVersionOptions(versions, "")
		if len(opts) != 4 {
			t.Fatalf("len=%d want 4", len(opts))
		}
		if opts[0].Version != "0.1.2-alpha.1" {
			t.Errorf("first=%s want 0.1.2-alpha.1", opts[0].Version)
		}
		if def != "0.1.1" {
			t.Errorf("default=%s want newest stable 0.1.1", def)
		}
	})
	t.Run("dedupe-and-prefix-strip", func(t *testing.T) {
		versions := []string{"0.1.1", "v0.1.1", "dsh-0.1.1", "0.1.2", "0.1.2-rc.1", "0.1.0"}
		opts, def := buildResetVersionOptions(versions, "0.1.2-rc.1")
		want := []string{"0.1.2", "0.1.2-rc.1", "0.1.1", "0.1.0"}
		if got := versionSeq(opts); !sameStrings(got, want) {
			t.Fatalf("opts=%v want %v", got, want)
		}
		if def != "0.1.2-rc.1" {
			t.Errorf("default=%s want current 0.1.2-rc.1", def)
		}
	})
}

// TestValidResetTarget 执行期目标格式校验（防异常入参进入 pnpm add 拼接）。
func TestValidResetTarget(t *testing.T) {
	good := []string{"0.1.1", "0.1.2-rc.1", "0.1.3-alpha.2", "1.2.3", "0.1.0+build.5"}
	for _, v := range good {
		if !validResetTarget(v) {
			t.Errorf("expected valid: %q", v)
		}
	}
	bad := []string{"", "latest", "..", "0..1", "0.1.1\n--config", "0.1.1;rm", " 0.1.1", "0.1.1 ",
		"../../etc", "@deepseek-ai/dsh", strings.Repeat("0", 70) + ".1"}
	for _, v := range bad {
		if validResetTarget(v) {
			t.Errorf("expected invalid: %q", v)
		}
	}
}

// versionSeq 提取选项的版本序列（断言顺序用）。
func versionSeq(opts []ResetVersionOption) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Version)
	}
	return out
}

// sameStrings 逐元素比较两个字符串切片。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

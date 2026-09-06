package main

import (
	"strings"
	"testing"
)

// TestBuildResetVersionOptions 重置目标版本候选构建（纯函数，不触达网络/配置）：
// 只列不高于 current 的版本（含当前版本=同版本重装）、按新→旧排序、
// 默认选最新稳定版、边界（无候选/当前未知）行为。
func TestBuildResetVersionOptions(t *testing.T) {
	t.Run("at-or-below-current-desc-default-stable", func(t *testing.T) {
		versions := []string{"0.1.1", "0.1.2-rc.1", "0.1.2-alpha.1", "0.1.1-rc.2", "0.1.0"}
		opts, def := buildResetVersionOptions(versions, "0.1.2-rc.1")
		got := versionSeq(opts)
		// 当前版本 0.1.2-rc.1 本身可入选（同版本重装）；0.1.2 稳定版高于 rc 不提供
		want := []string{"0.1.2-rc.1", "0.1.2-alpha.1", "0.1.1", "0.1.1-rc.2", "0.1.0"}
		if len(got) != len(want) {
			t.Fatalf("len=%d want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("order[%d]=%s want %s (full %v)", i, got[i], want[i], got)
			}
		}
		if def != "0.1.1" {
			t.Errorf("default=%s want 0.1.1", def)
		}
		if !opts[0].Prerelease || !opts[1].Prerelease || opts[2].Prerelease || !opts[3].Prerelease || opts[4].Prerelease {
			t.Errorf("prerelease flags wrong: %+v", opts)
		}
	})
	t.Run("equal-current-stable-included-as-default", func(t *testing.T) {
		versions := []string{"0.1.0", "0.1.1", "0.1.1-rc.2"}
		opts, def := buildResetVersionOptions(versions, "0.1.1")
		if len(opts) != 3 {
			t.Fatalf("len=%d want 3: %v", len(opts), opts)
		}
		if opts[0].Version != "0.1.1" || def != "0.1.1" {
			t.Errorf("opts=%v def=%s want current 0.1.1 as default", opts, def)
		}
	})
	t.Run("prerelease-only-fallback-newest", func(t *testing.T) {
		versions := []string{"0.1.2-alpha.1", "0.1.2-alpha.3", "0.1.2-alpha.2"}
		opts, def := buildResetVersionOptions(versions, "0.1.2-rc.1")
		if len(opts) != 3 {
			t.Fatalf("len=%d", len(opts))
		}
		if opts[0].Version != "0.1.2-alpha.3" || def != "0.1.2-alpha.3" {
			t.Errorf("opts=%v def=%s", opts, def)
		}
	})
	t.Run("no-candidate-current-older-than-all", func(t *testing.T) {
		versions := []string{"0.1.0", "0.1.1"}
		opts, def := buildResetVersionOptions(versions, "0.0.9")
		if len(opts) != 0 {
			t.Errorf("len=%d want 0: %v", len(opts), opts)
		}
		if def != "" {
			t.Errorf("default=%s want empty", def)
		}
	})
	t.Run("current-unknown-lists-all", func(t *testing.T) {
		versions := []string{"0.1.1-rc.1", "0.1.1", "0.1.2-alpha.1", "0.1.0"}
		opts, def := buildResetVersionOptions(versions, "")
		if len(opts) != 4 {
			t.Fatalf("len=%d want 4", len(opts))
		}
		if opts[0].Version != "0.1.2-alpha.1" {
			t.Errorf("first=%s want 0.1.2-alpha.1", opts[0].Version)
		}
		if def != "0.1.1" {
			t.Errorf("default=%s want 0.1.1", def)
		}
	})
	t.Run("dedupe-and-prefix-strip", func(t *testing.T) {
		versions := []string{"0.1.1", "v0.1.1", "dsh-0.1.1", "0.1.2", "0.1.0"}
		opts, def := buildResetVersionOptions(versions, "0.1.2-rc.1")
		// 0.1.2 稳定版高于 0.1.2-rc.1 → 不提供；v/dsh- 前缀重复项去重。
		if len(opts) != 2 {
			t.Fatalf("len=%d want 2: %v", len(opts), opts)
		}
		if opts[0].Version != "0.1.1" || opts[1].Version != "0.1.0" || def != "0.1.1" {
			t.Errorf("opts=%v def=%s", opts, def)
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

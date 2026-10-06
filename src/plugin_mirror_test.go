package main

// 自建 GitHub 中转 Worker（mirrorBase）相关逻辑的单测：地址解析/组装、来源分类、镜像链顺序。
// 网络部分（解析 commit SHA）由端到端验证覆盖（真实私有仓 pnpm 安装），此处只测纯函数。

import (
	"strings"
	"testing"
)

// withMirrorBase 临时设置 mirrorBase 并在测试后还原。
func withMirrorBase(t *testing.T, base string) {
	t.Helper()
	prev := mirrorBase
	mirrorBase = base
	t.Cleanup(func() { mirrorBase = prev })
}

func TestMirrorTarballURLRoundTrip(t *testing.T) {
	withMirrorBase(t, "https://mirror.example.com")
	sha := "a6360c98bab3d5b1bbc8ea09df57221dd79729af"
	u := mirrorTarballURL("acme", "relay", sha)
	if u != "https://mirror.example.com/p/acme/relay/tar.gz/"+sha {
		t.Fatalf("mirrorTarballURL = %q", u)
	}
	owner, repo, gotSHA, ok := parseMirrorTarballURL(u)
	if !ok || owner != "acme" || repo != "relay" || gotSHA != sha {
		t.Fatalf("parseMirrorTarballURL = (%q,%q,%q,%v)", owner, repo, gotSHA, ok)
	}
	// 其它 host 的中转地址不认（避免把第三方 URL 误判为 GitHub 来源）
	if _, _, _, ok := parseMirrorTarballURL("https://other.example.com/p/o/r/tar.gz/" + sha); ok {
		t.Fatal("非配置 host 的地址不应被识别")
	}
	// 非 commit（分支名）不认：解析目标必须是可缓存的不变地址
	if _, _, _, ok := parseMirrorTarballURL("https://mirror.example.com/p/o/r/tar.gz/main"); ok {
		t.Fatal("分支名地址不应被识别为 pinned tarball")
	}
}

func TestMirrorTarballSpecDisabled(t *testing.T) {
	withMirrorBase(t, "")
	got, err := mirrorTarballSpec("github:acme/relay")
	if err != nil || got != "" {
		t.Fatalf("未配置 mirrorBase 时应返回空（got=%q err=%v）", got, err)
	}
	// 非 GitHub 来源也不改写（npm 依赖不经过中转）
	spec, err := mirrorTarballSpec("^1.2.3")
	if err != nil || spec != "" {
		t.Fatalf("npm 范围 spec 不应被改写（got=%q err=%v）", spec, err)
	}
}

func TestClassifyPluginSpecMirrorTarball(t *testing.T) {
	withMirrorBase(t, "https://mirror.example.com")
	spec := mirrorTarballURL("acme", "relay", "a6360c98bab3d5b1bbc8ea09df57221dd79729af")
	source, canUpdate, reason := classifyPluginSpec(spec)
	if source != "github" || !canUpdate || reason != "" {
		t.Fatalf("中转 tarball 地址应为可更新的 github 来源，得到 (%q,%v,%q)", source, canUpdate, reason)
	}
	// 普通第三方固定压缩包仍是 tarball / 不可更新
	source, canUpdate, _ = classifyPluginSpec("https://example.com/x-1.0.0.tgz")
	if source != "tarball" || canUpdate {
		t.Fatalf("普通固定压缩包分类变化：(%q,%v)", source, canUpdate)
	}
}

// TestGithubRepoFromSpecMirrorTarball 中转 tarball 地址也要能反解出 owner/repo：
// 版本解析（fetchGithubLatestVersion）与「仓库不可见 → 引导授权」都走这条路径，
// 只认显式 GitHub 形态时行内会报「无法解析 GitHub 来源：<中转地址>」（2026-10-06 现场）。
func TestGithubRepoFromSpecMirrorTarball(t *testing.T) {
	sha := "4456a2ae85ce886483063a63c5fa1e792ca9e6d1"
	// 形态识别与 host 无关：账号同步记录里的 spec 可能是另一台机器（另一台镜像）写下的
	for _, spec := range []string{
		"https://dsh-mirror.refyon.ccwu.cc/p/refyon/restrict-discipline/tar.gz/" + sha,
		"https://other-mirror.example.com/p/refyon/restrict-discipline/tar.gz/" + sha,
		"  https://dsh-mirror.refyon.ccwu.cc/p/refyon/restrict-discipline/tar.gz/" + sha + "  ",
	} {
		o, r, ok := githubRepoFromSpec(spec)
		if !ok || o != "refyon" || r != "restrict-discipline" {
			t.Errorf("spec %q → (%q,%q,%v)，want (refyon,restrict-discipline,true)", spec, o, r, ok)
		}
	}
	// 显式 GitHub 形态不受影响
	if o, r, ok := githubRepoFromSpec("github:acme/relay#main"); !ok || o != "acme" || r != "relay" {
		t.Errorf("显式 github: 形态被破坏：(%q,%q,%v)", o, r, ok)
	}
	// 非中转形态的固定压缩包/本地路径仍然不认
	for _, spec := range []string{"https://example.com/pack-1.0.0.tgz", "file:D:/workspace/plugins/x", ""} {
		if _, _, ok := githubRepoFromSpec(spec); ok {
			t.Errorf("spec %q 不应被识别为 GitHub 来源", spec)
		}
	}
}

// TestNormalizeSpecTextMirrorTarball 中转 tarball 地址与 github:owner/repo 是同一来源：
// 不归一的话账号记录永远判不满足 → 已应用记录每轮重入队、界面常驻「待生效 1 项」。
func TestNormalizeSpecTextMirrorTarball(t *testing.T) {
	withMirrorBase(t, "https://dsh-mirror.refyon.ccwu.cc")
	spec := mirrorTarballURL("refyon", "restrict-discipline", "4456a2ae85ce886483063a63c5fa1e792ca9e6d1")
	if got := normalizeSpecText(spec); got != "refyon/restrict-discipline" {
		t.Fatalf("normalizeSpecText(中转地址) = %q，want refyon/restrict-discipline", got)
	}
	if !pluginSpecSatisfied(pluginOpValue{Spec: spec, Version: "1.5.0"}, "github:refyon/restrict-discipline") {
		t.Error("本机声明为中转地址、账号记录为 github: 形态时应判为已满足")
	}
	if pluginSpecSatisfied(pluginOpValue{Spec: spec, Version: "1.5.0"}, "github:refyon/dsh-ui-taste") {
		t.Error("不同仓库不得因归一化被误判为同一来源")
	}
	// 既有写法不受影响
	if !pluginSpecSatisfied(pluginOpValue{Spec: "github:refyon/dsh-ui-taste", Version: "0.3.0"},
		"https://github.com/refyon/dsh-ui-taste.git") {
		t.Error("既有 github 写法归一化被破坏")
	}
}

func TestSpecGitHubRepoRefParse(t *testing.T) {
	cases := []struct {
		spec, owner, repo, ref string
	}{
		{"github:acme/relay", "acme", "relay", ""},
		{"github:acme/relay#main", "acme", "relay", "main"},
		{"github:acme/relay.git#v1.0.0", "acme", "relay", "v1.0.0"},
	}
	for _, c := range cases {
		m := specGitHubRepoRefRe.FindStringSubmatch(c.spec)
		if m == nil {
			t.Fatalf("%q 未匹配", c.spec)
		}
		if m[1] != c.owner || m[2] != c.repo || m[3] != c.ref {
			t.Errorf("%q → (%q,%q,%q)，want (%q,%q,%q)", c.spec, m[1], m[2], m[3], c.owner, c.repo, c.ref)
		}
	}
	if specGitHubRepoRefRe.MatchString("github:acme/relay/tree/main") {
		t.Error("带路径的 spec 不应匹配（避免把子目录当 repo）")
	}
}

func TestBuildMirrorsWithMirrorBase(t *testing.T) {
	prevOverride := updateMirrorOverride
	t.Cleanup(func() { updateMirrorOverride = prevOverride })
	withMirrorBase(t, "https://mirror.example.com")
	updateMirrorOverride = ""

	got := buildMirrors()
	if len(got) == 0 || got[0] != "https://mirror.example.com/gh/" {
		t.Fatalf("mirrorBase 应排在镜像链最前：%v", got)
	}
	// 直连与第三方镜像仍作为回退保留
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "ghfast.top") {
		t.Errorf("第三方镜像回退被误删：%v", got)
	}
	if got[1] != "" {
		t.Errorf("第二位应是直连（空前缀）：%v", got)
	}

	// 显式 updateMirror 与 mirrorBase 指向同一端点时不得重复
	updateMirrorOverride = "https://mirror.example.com/gh/"
	got = buildMirrors()
	n := 0
	for _, m := range got {
		if m == "https://mirror.example.com/gh/" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("同一端点重复出现 %d 次：%v", n, got)
	}
	withMirrorBase(t, "")
	if got := buildMirrors(); got[0] != "https://mirror.example.com/gh/" {
		t.Fatalf("未配置 mirrorBase 时应以 updateMirror 为首：%v", got)
	}
}

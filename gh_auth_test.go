package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitHubAuthStringsResolve 私有仓库授权弹窗文案必须能解析出非空按钮与完整提示。
// 回归背景（2026-09-13）：授权询问曾用 Wails MessageDialog，其 Windows 实现是 Win32
// MessageBoxW(MB_YESNO)，忽略自定义 Buttons——「登录 GitHub」按钮从不出现。改为自绘弹窗
// （platform_*.go 的 askGitHubAuth）后，按钮文案由本映射提供，缺失/空串会再次让按钮消失。
func TestGitHubAuthStringsResolve(t *testing.T) {
	if T(ghLoginLabel) != ghLoginLabel { // zh 生效时原样返回
		t.Errorf("zh 按钮文案 = %q, want %q", T(ghLoginLabel), ghLoginLabel)
	}
	if got, ok := i18nEnMap[ghLoginLabel]; !ok || got != "Sign in to GitHub" {
		t.Errorf("i18nEnMap[%q] = (%q, ok=%v), want \"Sign in to GitHub\"", ghLoginLabel, got, ok)
	}
	if got, ok := i18nEnMap["取消"]; !ok || got != "Cancel" {
		t.Errorf("i18nEnMap[\"取消\"] = (%q, ok=%v), want \"Cancel\"", got, ok)
	}

	prev := curLang
	curLang = "en"
	t.Cleanup(func() { curLang = prev })

	if got := T(ghLoginLabel); got != "Sign in to GitHub" {
		t.Errorf("en 按钮文案 = %q", got)
	}
	msg := ghAuthPromptMsg("dsh-ui-taste", "refyon/dsh-ui-taste")
	for _, want := range []string{"dsh-ui-taste", "refyon/dsh-ui-taste", "private repository", "Sign in to GitHub"} {
		if !strings.Contains(msg, want) {
			t.Errorf("en 授权提示缺少 %q：\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "%!") {
		t.Errorf("授权提示格式化失败：\n%s", msg)
	}
}

// TestGHVerifyURLFromOutput 从 gh 真实输出里取出设备授权页地址。
// 回归（2026-09-13 实证）：gh 在管道 stdio（非 TTY）下**不会自己拉起浏览器**，只打印地址；
// 浏览器必须由本程序解析输出后代为打开，所以这行解析错了用户就永远看不到授权页。
func TestGHVerifyURLFromOutput(t *testing.T) {
	real := "\n! One-time code (D41C-AB86) copied to clipboard\n" +
		"Open this URL to continue in your web browser: https://github.com/login/device\n"
	cases := []struct {
		desc, in, want string
	}{
		{"真实输出", real, "https://github.com/login/device"},
		{"行尾带句号", "see https://github.com/login/device.", "https://github.com/login/device"},
		{"先出现其它链接", "docs https://cli.github.com/manual then https://github.com/login/device", "https://github.com/login/device"},
		{"只有非 github 链接", "https://cli.github.com/manual", ""},
		{"没有链接", "no url here", ""},
		{"空输出", "", ""},
	}
	for _, c := range cases {
		if got := ghVerifyURLFromOutput(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.desc, got, c.want)
		}
	}
}

// TestGHDeviceCodeFromOutput 从 gh 输出里取出一次性授权码。
// 网页要求用户手工填入该码，剪贴板可能被覆盖 —— 解析错了用户就无从填入。
func TestGHDeviceCodeFromOutput(t *testing.T) {
	real := "\n! One-time code (D41C-AB86) copied to clipboard\n" +
		"Open this URL to continue in your web browser: https://github.com/login/device\n"
	cases := []struct{ desc, in, want string }{
		{"真实输出", real, "D41C-AB86"},
		{"只打印代码", "First copy your one-time code: 29EE-5C98\n", "29EE-5C98"},
		{"大小写与数字混合", "code (1a2B-3c4D)", "1a2B-3c4D"},
		{"位数不足不误报", "code (D41C-AB8)", ""},
		{"没有代码", "Open this URL: https://github.com/login/device", ""},
		{"空输出", "", ""},
	}
	for _, c := range cases {
		if got := ghDeviceCodeFromOutput(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.desc, got, c.want)
		}
	}
}

// TestEnsureGHToolAlreadyInstalled 已有 gh 时 ensureGHTool 直接复用并跳过下载：
// 进度回调为空也不得 panic（进度窗口首帧就位前可能先走这条路径）。
func TestEnsureGHToolAlreadyInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home) // 隔绝真实机器上的 ~/.dsh

	ghBinaryMu.Lock()
	prevCache := ghBinaryCache
	ghBinaryMu.Unlock()
	t.Cleanup(func() {
		ghBinaryMu.Lock()
		ghBinaryCache = prevCache
		ghBinaryMu.Unlock()
	})

	fake := filepath.Join(t.TempDir(), ghBinName())
	if err := os.WriteFile(fake, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	ghBinaryMu.Lock()
	ghBinaryCache = fake
	ghBinaryMu.Unlock()

	called := false
	got, err := ensureGHTool(func(float64) { called = true })
	if err != nil || got != fake {
		t.Fatalf("ensureGHTool = (%q, %v), want (%q, nil)", got, err, fake)
	}
	if called {
		t.Error("已安装路径不应回调进度")
	}
	if got, err := ensureGHTool(nil); err != nil || got != fake {
		t.Fatalf("ensureGHTool(nil) = (%q, %v), want (%q, nil)", got, err, fake)
	}
}

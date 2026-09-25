package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
		// 回归（2026-09-14 本机实证）：设备码请求网络失败时 gh 打印的是 Go 的 *url.Error
		// （`Post "https://github.com/login/device/code": …`）。其中的 API 端点不是授权页——
		// 旧逻辑把它当授权页打开，用户看到的是 API 页面且拿不到一次性代码，整条授权流程
		// 停在死路上（日志实证：opening https://github.com/login/device/code": ，code length=0）。
		{"网络错误里的 API 端点", `failed to authenticate via web browser: Post "https://github.com/login/device/code": dial tcp 20.205.243.166:443: i/o timeout`, ""},
		// gh 的升级提示也是 github.com 链接：同样不是授权页。
		{"升级提示的 release 链接", "A new release of gh is available: https://github.com/cli/cli/releases/tag/v2.101.0", ""},
		{"授权页带尾随引号", `see https://github.com/login/device" now`, "https://github.com/login/device"},
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

// TestGHDeviceLoginRetryPolicy 设备流「起流」失败（用户还没看到代码，通常是网络瞬断）自动重试；
// 代码/地址一旦交给用户就不再重试（重试会换一个新码，让用户手里的码失效）。
// 回归（2026-09-14 本机实证）：设备码请求 21s 后连接超时、gh 直接退出，旧逻辑一次失败即收尾，
// 用户只能反复点「检查更新」而每次都撞在同一处。
func TestGHDeviceLoginRetryPolicy(t *testing.T) {
	prevFlow, prevDelay := ghDeviceFlowOnce, ghAuthRetryDelay
	t.Cleanup(func() { ghDeviceFlowOnce, ghAuthRetryDelay = prevFlow, prevDelay })
	ghAuthRetryDelay = 0 // 测试不真的睡

	t.Run("起流失败重试到成功", func(t *testing.T) {
		calls := 0
		ghDeviceFlowOnce = func(string, func(url, code string)) (bool, error) {
			calls++
			if calls < 3 {
				return false, errors.New(`Post "https://github.com/login/device/code": i/o timeout`)
			}
			return true, nil
		}
		var attempts []int
		if err := ghDeviceLogin("gh", nil, func(a, total int) { attempts = append(attempts, a) }); err != nil {
			t.Fatalf("ghDeviceLogin = %v, want nil（第三次应成功）", err)
		}
		if calls != 3 {
			t.Errorf("尝试次数 = %d, want 3", calls)
		}
		if len(attempts) != 3 {
			t.Errorf("onAttempt 回调次数 = %d, want 3", len(attempts))
		}
	})

	t.Run("已展示代码后不重试", func(t *testing.T) {
		calls := 0
		ghDeviceFlowOnce = func(string, func(url, code string)) (bool, error) {
			calls++
			return true, errors.New("等待 GitHub 授权超时")
		}
		if err := ghDeviceLogin("gh", nil, nil); err == nil {
			t.Error("ghDeviceLogin = nil, want error")
		}
		if calls != 1 {
			t.Errorf("尝试次数 = %d, want 1（代码已给用户，重试会让它失效）", calls)
		}
	})

	t.Run("全部失败收敛到最后一次错误", func(t *testing.T) {
		calls := 0
		ghDeviceFlowOnce = func(string, func(url, code string)) (bool, error) {
			calls++
			return false, errors.New("网络不可达")
		}
		err := ghDeviceLogin("gh", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "网络不可达") {
			t.Errorf("ghDeviceLogin = %v, want 含最后一次错误", err)
		}
		if calls != ghAuthStartAttempts {
			t.Errorf("尝试次数 = %d, want %d", calls, ghAuthStartAttempts)
		}
	})
}

// TestUpsertNpmrcTokenHelper 只动 codeload 那一行：用户 npmrc 里的其它配置、换行风格、
// 历史重复行都要正确处理（写坏用户的 ~/.npmrc 是这条链路最贵的失败）。
func TestUpsertNpmrcTokenHelper(t *testing.T) {
	// 新格式：值是包装器脚本的**裸路径**（不带参数），pnpm 10 与 11 都接受
	line := codeloadTokenHelperKey + `C:\tools\gh-token.cmd`
	cases := []struct {
		desc, in, want string
		changed        bool
	}{
		{"空文件", "", line + "\n", true},
		{"保留其它配置", "registry=https://registry.npmjs.org/\n", "registry=https://registry.npmjs.org/\n" + line + "\n", true},
		{"已一致则不改", line + "\n", line + "\n", false},
		{"迁移旧格式（带参数）", codeloadTokenHelperKey + "old-gh auth token\n", line + "\n", true},
		{"合并重复行", line + "\n" + line + "\n", line + "\n", true},
		{"保留 CRLF", "registry=x\r\n", "registry=x\r\n" + line + "\r\n", true},
		{"无末尾换行则补上", "registry=x", "registry=x\n" + line + "\n", true},
	}
	for _, c := range cases {
		got, changed := upsertNpmrcTokenHelper(c.in, line)
		if got != c.want || changed != c.changed {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.desc, got, changed, c.want, c.changed)
		}
	}
}

// TestNpmrcTokenHelperPathSafe pnpm 按空白切分 tokenHelper 且禁止 $ % ` " '：
// 这类路径写进去会让 pnpm 报配置错，必须提前拒绝而不是写坏 npmrc。
func TestNpmrcTokenHelperPathSafe(t *testing.T) {
	cases := []struct {
		bin  string
		want bool
	}{
		{`C:\Users\work\AppData\Roaming\dsh-systray\tools\gh\bin\gh-token.cmd`, true},
		{`/usr/local/bin/gh-token.sh`, true},
		{`C:\Program Files\gh-token.cmd`, false},
		{`C:\Users\a b\gh-token.cmd`, false},
		{`C:\gh$%.cmd`, false},
		{"", false},
		{" gh-token.cmd", false},
	}
	for _, c := range cases {
		if got := npmrcTokenHelperPathSafe(c.bin); got != c.want {
			t.Errorf("npmrcTokenHelperPathSafe(%q) = %v, want %v", c.bin, got, c.want)
		}
	}
}

// TestWriteCodeloadTokenHelper 落盘路径：生成包装器脚本 + 在用户级 npmrc 写入**裸路径**值、
// 内容一致时不改写文件、保留既有配置行；路径含空格（无法表达）时整条链路拒绝且不动文件。
// 值必须不带参数：pnpm 10.34.5 会把「路径 + 参数」判为 BAD_TOKEN_HELPER_PATH 并让该版本下
// 所有 pnpm 命令失败（2026-09-25 源码级实证）。
func TestWriteCodeloadTokenHelper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // Windows 的 os.UserHomeDir
	t.Setenv("HOME", home)        // macOS

	bin := filepath.Join(t.TempDir(), ghBinName())
	helper := ghTokenHelperPath(bin)
	want := codeloadTokenHelperKey + helper + "\n"
	if err := writeCodeloadTokenHelper(bin); err != nil {
		t.Fatalf("writeCodeloadTokenHelper = %v", err)
	}
	p := filepath.Join(home, ".npmrc")
	if b, err := os.ReadFile(p); err != nil || string(b) != want {
		t.Fatalf("npmrc = (%q, %v), want %q", string(b), err, want)
	}
	if strings.ContainsAny(strings.TrimPrefix(want, codeloadTokenHelperKey), " \t") {
		t.Fatalf("tokenHelper 值必须是裸路径（不含空白）：%q", want)
	}
	// 包装器脚本：存在、内含 gh 路径与取 token 的参数
	sb, err := os.ReadFile(helper)
	if err != nil {
		t.Fatalf("包装器未生成：%v", err)
	}
	for _, must := range []string{bin, "auth token --hostname github.com"} {
		if !strings.Contains(string(sb), must) {
			t.Errorf("包装器内容缺少 %q：%q", must, string(sb))
		}
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(helper); err != nil || st.Mode().Perm()&0o100 == 0 {
			t.Errorf("非 Windows 平台包装器需可执行位：%v %v", st.Mode(), err)
		}
	}

	// 内容已一致：不得改写文件（mtime 是这里唯一可观测的「没写」证据）
	old := time.Unix(1000000, 0)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if err := writeCodeloadTokenHelper(bin); err != nil {
		t.Fatalf("第二次 writeCodeloadTokenHelper = %v", err)
	}
	if st, err := os.Stat(p); err != nil || !st.ModTime().Equal(old) {
		t.Errorf("内容一致时不应改写文件（mtime=%v）", st.ModTime())
	}

	// 已有其它配置时必须保留
	pre := "registry=https://registry.npmjs.org/\n"
	if err := os.WriteFile(p, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeCodeloadTokenHelper(bin); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != pre+want {
		t.Errorf("既有配置被破坏：%q", string(b))
	}

	// 含空格的 gh 路径 → 包装器路径同样含空格：拒绝且不动文件、不留半成品
	before, _ := os.ReadFile(p)
	badBin := filepath.Join(t.TempDir(), "Program Files", ghBinName())
	if err := writeCodeloadTokenHelper(badBin); err == nil {
		t.Error("含空格的路径应被拒绝")
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Error("拒绝路径时不应改动 npmrc")
	}
	if _, err := os.Stat(ghTokenHelperPath(badBin)); !os.IsNotExist(err) {
		t.Error("拒绝路径时不应留下包装器脚本")
	}
}

// TestDropLegacyTokenHelperLine 旧格式（值含空白 = 带参数）行删除：它是 pnpm 10.34.5 下
// 所有 pnpm 命令失败的根因；其它 host 的 tokenHelper 与其它配置必须原样保留。
func TestDropLegacyTokenHelperLine(t *testing.T) {
	legacy := codeloadTokenHelperKey + `C:\tools\gh.exe auth token --hostname github.com`
	bare := codeloadTokenHelperKey + `C:\tools\gh-token.cmd`
	cases := []struct {
		desc, in, want string
		changed        bool
	}{
		{"仅旧行→清空", legacy + "\n", "", true},
		{"保留其它配置", "registry=x\n" + legacy + "\n", "registry=x\n", true},
		{"新格式不动", bare + "\n", bare + "\n", false},
		{"其它 host 不动", "//npm.pkg.github.com/:tokenHelper=/opt/h.sh\n" + legacy + "\n",
			"//npm.pkg.github.com/:tokenHelper=/opt/h.sh\n", true},
		{"CRLF 保留", "registry=x\r\n" + legacy + "\r\n", "registry=x\r\n", true},
		{"无该行不动", "registry=x\n", "registry=x\n", false},
	}
	for _, c := range cases {
		got, changed := dropLegacyTokenHelperLine(c.in)
		if got != c.want || changed != c.changed {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.desc, got, changed, c.want, c.changed)
		}
	}
}

// TestRemoveLegacyCodeloadTokenHelper 无 gh 时的兜底：只删旧行、不动别的；已是新格式则不改文件。
func TestRemoveLegacyCodeloadTokenHelper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".npmrc")

	legacy := codeloadTokenHelperKey + `C:\tools\gh.exe auth token --hostname github.com`
	if err := os.WriteFile(p, []byte("registry=x\n"+legacy+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyCodeloadTokenHelper(); err != nil {
		t.Fatalf("removeLegacyCodeloadTokenHelper = %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "registry=x\n" {
		t.Fatalf("旧行未清理干净：%q", string(b))
	}

	// 已是新格式（裸路径）：不改动文件
	bare := codeloadTokenHelperKey + `C:\tools\gh-token.cmd` + "\n"
	if err := os.WriteFile(p, []byte(bare), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1000000, 0)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyCodeloadTokenHelper(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(p); err != nil || !st.ModTime().Equal(old) {
		t.Errorf("新格式不应被改写（mtime=%v, err=%v）", st.ModTime(), err)
	}

	// npmrc 不存在：静默成功
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyCodeloadTokenHelper(); err != nil {
		t.Errorf("npmrc 不存在时应静默成功：%v", err)
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

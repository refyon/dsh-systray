package main

import (
	"crypto/sha512"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 官方桌面端（desktop_app.go）纯逻辑测试：启动方式解析、更新源与清单解析、安装包地址与校验。
// 真实安装检测按平台实现，仅在对应平台断言（CI 的 macOS/Windows 通道各自覆盖）。

// appUpdateYML 取自真实安装包的 resources/app-update.yml（Windows x64，2026-09 版本）。
const appUpdateYML = `provider: generic
url: https://download.deepseek.com/dsh-desk/feeds/win-x64/
channel: nightly
updaterCacheDirName: '@deepseek-aidsh-desktop-updater'
publisherName:
  - CN=Hangzhou\20DeepSeek\20Artificial\20Intelligence\20Co.\2c\20Ltd.,O=Hangzhou\20DeepSeek\20Artificial\20Intelligence\20Co.\2c\20Ltd.,C=CN
`

// desktopFeedYML 取自真实更新清单（win-x64/nightly.yml，折叠标量 + 根层 path）。
const desktopFeedYML = `version: 0.1.7-rc.2
files:
  - url: >-
      https://download.deepseek.com/dsh-desk/bin/win-x64/deepseek-harness-0.1.7-rc.2-win-x64.exe
    sha512: >-
      AY7f45dYO7BFrfgaLmzXNWP0pavlxkSbsehPo/WF6PXcFdDK3fF1oHUPHs/4f2bzROgQvm6wSgawZ/g7UzbPRmw==
    size: 288245480
path: >-
  https://download.deepseek.com/dsh-desk/bin/win-x64/deepseek-harness-0.1.7-rc.2-win-x64.exe
sha512: >-
  AY7f45dYO7BFrfgaLmzXNWP0pavlxkSbsehPo/WF6PXcFdDK3fF1oHUPHs/4f2bzROgQvm6wSgawZ/g7UzbPRmw==
releaseDate: '2026-09-24T14:11:01.715Z'
`

// TestLaunchTargetResolution 启动方式解析矩阵：auto 跟随检测，显式选择优先，桌面端缺失回退 web。
func TestLaunchTargetResolution(t *testing.T) {
	cases := []struct {
		pref      string
		installed bool
		want      string
	}{
		{"", true, launchTargetDesktop},        // 旧配置（无字段）= auto：装了桌面端就用桌面端
		{"", false, launchTargetWeb},           // 没装 → Web UI
		{"auto", true, launchTargetDesktop},    // 显式 auto
		{"auto", false, launchTargetWeb},       //
		{"web", true, launchTargetWeb},         // 显式 web 不被桌面端覆盖
		{"web", false, launchTargetWeb},        //
		{"desktop", true, launchTargetDesktop}, // 显式 desktop 且已安装
		{"desktop", false, launchTargetWeb},    // 桌面端被卸载 → 回退，避免「打开」无操作
		{"DESKTOP", true, launchTargetDesktop}, // 大小写与空白容忍
		{" desktop ", true, launchTargetDesktop},
		{"bogus", true, launchTargetDesktop}, // 非法值 = auto 语义
		{"bogus", false, launchTargetWeb},
	}
	for _, c := range cases {
		if got := resolveLaunchTargetPref(c.pref, c.installed); got != c.want {
			t.Fatalf("resolveLaunchTargetPref(%q, installed=%v) = %q, want %q", c.pref, c.installed, got, c.want)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"web", launchTargetWeb}, {"desktop", launchTargetDesktop},
		{"auto", launchTargetAuto}, {"", launchTargetAuto}, {"nope", launchTargetAuto},
	} {
		if got := normalizeLaunchTarget(c.in); got != c.want {
			t.Fatalf("normalizeLaunchTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseDesktopUpdateConfig 解析安装包自带的更新源声明（真实 app-update.yml）。
func TestParseDesktopUpdateConfig(t *testing.T) {
	cfg := parseDesktopUpdateConfig(appUpdateYML)
	if cfg.BaseURL != "https://download.deepseek.com/dsh-desk/feeds/win-x64/" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.Channel != "nightly" {
		t.Fatalf("Channel = %q", cfg.Channel)
	}
	if got := parseDesktopUpdateConfig(""); got.BaseURL != "" || got.Channel != "" {
		t.Fatalf("empty input should yield zero value, got %+v", got)
	}
}

// TestDesktopFeedFileName 清单文件名：Windows 用 <通道>.yml，macOS 用 <通道>-mac.yml。
func TestDesktopFeedFileName(t *testing.T) {
	if got := desktopFeedFileNameFor("nightly", "windows"); got != "nightly.yml" {
		t.Fatalf("windows feed = %q", got)
	}
	if got := desktopFeedFileNameFor("nightly", "darwin"); got != "nightly-mac.yml" {
		t.Fatalf("darwin feed = %q", got)
	}
	if got := desktopFeedFileNameFor("", "windows"); got != "nightly.yml" {
		t.Fatalf("empty channel should fall back to nightly, got %q", got)
	}
	if got := desktopFeedFileNameFor("latest", "darwin"); got != "latest-mac.yml" {
		t.Fatalf("stable darwin feed = %q", got)
	}
}

// TestDesktopFeedURL 清单地址拼接：基址缺尾斜杠要补齐；基址为空走平台兜底基址。
func TestDesktopFeedURL(t *testing.T) {
	got := desktopFeedURL("https://download.deepseek.com/dsh-desk/feeds/win-x64", "nightly", "windows", "amd64")
	want := "https://download.deepseek.com/dsh-desk/feeds/win-x64/nightly.yml"
	if got != want {
		t.Fatalf("desktopFeedURL = %q, want %q", got, want)
	}
	if got := desktopFeedURL("", "nightly", "windows", "amd64"); !strings.HasSuffix(got, "/win-x64/nightly.yml") {
		t.Fatalf("fallback base url = %q", got)
	}
	if got := desktopFeedURL("", "nightly", "darwin", "arm64"); !strings.HasSuffix(got, "/mac-arm64/nightly-mac.yml") {
		t.Fatalf("darwin fallback base url = %q", got)
	}
	if got := desktopFeedURL("", "nightly", "darwin", "amd64"); !strings.HasSuffix(got, "/mac-x64/nightly-mac.yml") {
		t.Fatalf("intel mac fallback base url = %q", got)
	}
	if got := defaultDesktopFeedBaseFor("darwin", "amd64"); !strings.HasSuffix(got, "/mac-x64/") {
		t.Fatalf("intel mac base = %q", got)
	}
	if got := defaultDesktopFeedBaseFor("darwin", "arm64"); !strings.HasSuffix(got, "/mac-arm64/") {
		t.Fatalf("apple silicon base = %q", got)
	}
}

// TestParseDesktopFeed 解析真实更新清单：折叠标量、根层 path/sha512/size 优先。
func TestParseDesktopFeed(t *testing.T) {
	feed := parseDesktopFeed(desktopFeedYML)
	if feed.Version != "0.1.7-rc.2" {
		t.Fatalf("Version = %q", feed.Version)
	}
	wantURL := "https://download.deepseek.com/dsh-desk/bin/win-x64/deepseek-harness-0.1.7-rc.2-win-x64.exe"
	if feed.URL != wantURL {
		t.Fatalf("URL = %q, want %q", feed.URL, wantURL)
	}
	if !strings.HasPrefix(feed.SHA512, "AY7f45dYO7BFrfgaLmzXNWP0") {
		t.Fatalf("SHA512 = %q", feed.SHA512)
	}
	if feed.Size != 288245480 {
		t.Fatalf("Size = %d", feed.Size)
	}
	// 只有嵌套 files[] 而没有根层 path 的清单：取嵌套 url 兜底
	nestedOnly := "version: 1.2.3\nfiles:\n  - url: https://download.deepseek.com/x.dmg\n    size: 42\n"
	n2 := parseDesktopFeed(nestedOnly)
	if n2.URL != "https://download.deepseek.com/x.dmg" || n2.Size != 42 {
		t.Fatalf("nested-only parse = %+v", n2)
	}
	// 引号标量与行内写法
	inline := "version: '2.0.0'\npath: \"https://download.deepseek.com/y.exe\"\nsize: 7\n"
	n3 := parseDesktopFeed(inline)
	if n3.Version != "2.0.0" || n3.URL != "https://download.deepseek.com/y.exe" || n3.Size != 7 {
		t.Fatalf("inline parse = %+v", n3)
	}
	if empty := parseDesktopFeed(""); empty.Version != "" || empty.URL != "" {
		t.Fatalf("empty feed should be zero value, got %+v", empty)
	}
}

// TestDesktopInstallerURL 安装包地址：Windows 用清单里的 exe（带校验和）；
// macOS 把自动更新用的 zip 换成同目录的 dmg（官方每次发布同时产出，DMG 无清单校验和）。
func TestDesktopInstallerURL(t *testing.T) {
	feed := parseDesktopFeed(desktopFeedYML)
	u, hasSum := desktopInstallerURL(feed, "windows")
	if !strings.HasSuffix(u, "-win-x64.exe") || !hasSum {
		t.Fatalf("windows installer = %q hasSum=%v", u, hasSum)
	}

	macFeed := desktopFeed{
		Version: "0.1.7-rc.2",
		URL:     "https://download.deepseek.com/dsh-desk/bin/mac-arm64/deepseek-harness-0.1.7-rc.2-mac-arm64.zip",
		SHA512:  "aOfxtRvFqTRRp3zqu6nXNgILgVdHDPyfslhUVNnU1wMYw1dY1vTelptJeAmUDt7G822SCqHzxtPwMmvCGekveA==",
	}
	u, hasSum = desktopInstallerURL(macFeed, "darwin")
	if !strings.HasSuffix(u, "-mac-arm64.dmg") {
		t.Fatalf("darwin installer = %q, want .dmg", u)
	}
	if hasSum {
		t.Fatal("dmg has no manifest checksum; hasSum should be false")
	}
	if u, _ := desktopInstallerURL(desktopFeed{}, "windows"); u != "" {
		t.Fatalf("empty feed should give empty installer url, got %q", u)
	}
}

// TestIsDesktopUpdateAssetURL 安装包地址白名单：必须 https 且属于 deepseek.com。
func TestIsDesktopUpdateAssetURL(t *testing.T) {
	ok := []string{
		"https://download.deepseek.com/dsh-desk/bin/win-x64/deepseek-harness-0.1.7-rc.2-win-x64.exe",
		"https://download.deepseek.com/dsh-desk/bin/mac-arm64/deepseek-harness-0.1.7-rc.2-mac-arm64.dmg",
		"https://cdn.deepseek.com/x.exe",
	}
	for _, u := range ok {
		if !isDesktopUpdateAssetURL(u) {
			t.Fatalf("should accept %q", u)
		}
	}
	bad := []string{
		"",
		"http://download.deepseek.com/x.exe", // 非 https
		"https://evil.example.com/x.exe",     // 非官方域名
		"https://download.deepseek.com.evil.io/x.exe", // 后缀伪装
		"file:///C:/x.exe",
	}
	for _, u := range bad {
		if isDesktopUpdateAssetURL(u) {
			t.Fatalf("should reject %q", u)
		}
	}
}

// TestVerifyFileSHA512 安装包校验：命中通过、内容不符报错、空校验和直接通过。
func TestVerifyFileSHA512(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.bin")
	payload := []byte("deepseek-harness-desktop-installer")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(payload)
	if err := verifyFileSHA512(path, base64.StdEncoding.EncodeToString(sum[:])); err != nil {
		t.Fatalf("matching checksum should pass: %v", err)
	}
	if err := verifyFileSHA512(path, ""); err != nil {
		t.Fatalf("empty checksum should be skipped: %v", err)
	}
	if err := verifyFileSHA512(path, base64.StdEncoding.EncodeToString(make([]byte, sha512.Size))); err == nil {
		t.Fatal("mismatching checksum should fail")
	}
	if err := verifyFileSHA512(path, "not-base64!!"); err == nil {
		t.Fatal("invalid base64 should fail")
	}
}

// TestDesktopInstallerPath 安装包落盘名：取 URL 末段、去掉查询串、落到下载目录。
func TestDesktopInstallerPath(t *testing.T) {
	got, err := desktopInstallerPath("https://download.deepseek.com/dsh-desk/bin/win-x64/deepseek-harness-0.1.7-rc.2-win-x64.exe")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "deepseek-harness-0.1.7-rc.2-win-x64.exe" {
		t.Fatalf("basename = %q", filepath.Base(got))
	}
	if filepath.Dir(got) != downloadsDir() {
		t.Fatalf("dir = %q, want downloadsDir %q", filepath.Dir(got), downloadsDir())
	}
	got, err = desktopInstallerPath("https://download.deepseek.com/x.dmg?token=1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "x.dmg" {
		t.Fatalf("query string should be stripped, got %q", filepath.Base(got))
	}
	if _, err := desktopInstallerPath(""); err == nil {
		t.Fatal("empty url should fail")
	}
}

// TestDesktopDisplayNameMatching 卸载登记识别：只认官方桌面端，排除本程序与其它产品。
func TestDesktopDisplayNameMatching(t *testing.T) {
	cases := []struct {
		display string
		want    bool
	}{
		{"DeepSeek Harness 0.1.7-rc.2", true},
		{"DeepSeek Harness", true},
		{"dsh-systray", false},
		{"dsh-systray 1.0.0", false},
		{"DeepSeek Chat", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isDesktopDisplayName(c.display); got != c.want {
			t.Fatalf("isDesktopDisplayName(%q) = %v, want %v", c.display, got, c.want)
		}
	}
	if got := versionFromDesktopDisplayName("DeepSeek Harness 0.1.7-rc.2"); got != "0.1.7-rc.2" {
		t.Fatalf("version = %q", got)
	}
	if got := versionFromDesktopDisplayName("DeepSeek Harness"); got != "" {
		t.Fatalf("no version expected, got %q", got)
	}
	if got := versionFromDesktopDisplayName("DeepSeek Harness 1.0.0"); got != "1.0.0" {
		t.Fatalf("stable version = %q", got)
	}
}

// TestDesktopExeFromIcon Windows 卸载登记 DisplayIcon 解析（带图标索引 / 带引号 / 非法值）。
func TestDesktopExeFromIcon(t *testing.T) {
	cases := []struct{ icon, want string }{
		{`D:\Program Files\deepseek-harness\DeepSeek Harness.exe,0`, `D:\Program Files\deepseek-harness\DeepSeek Harness.exe`},
		{`"C:\Apps\DeepSeek Harness.exe",1`, `C:\Apps\DeepSeek Harness.exe`},
		{`C:\Apps\DeepSeek Harness.exe`, `C:\Apps\DeepSeek Harness.exe`},
		{`C:\Apps\deepseek-harness`, ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := desktopExeFromIcon(c.icon); got != c.want {
			t.Fatalf("desktopExeFromIcon(%q) = %q, want %q", c.icon, got, c.want)
		}
	}
}

// TestParsePlistStringValue macOS Info.plist 文本解析（二进制 plist 由 plutil 兜底，此处只测文本）。
func TestParsePlistStringValue(t *testing.T) {
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>DeepSeek Harness</string>
	<key>CFBundleShortVersionString</key>
	<string>0.1.7-rc.2</string>
	<key>CFBundleIdentifier</key>
	<string>com.example.app</string>
</dict>
</plist>`
	if got := parsePlistStringValue(plist, "CFBundleShortVersionString"); got != "0.1.7-rc.2" {
		t.Fatalf("version = %q", got)
	}
	if got := parsePlistStringValue(plist, "CFBundleExecutable"); got != "DeepSeek Harness" {
		t.Fatalf("executable = %q", got)
	}
	if got := parsePlistStringValue(plist, "CFBundleIdentifier"); got != "com.example.app" {
		t.Fatalf("identifier = %q", got)
	}
	if got := parsePlistStringValue(plist, "Missing"); got != "" {
		t.Fatalf("missing key should be empty, got %q", got)
	}
	if got := parsePlistStringValue("<key>OnlyKey</key>", "OnlyKey"); got != "" {
		t.Fatalf("key without string value should be empty, got %q", got)
	}
}

// stubDesktopInstalled 把桌面端安装检测替换为固定结论（含缓存），供不依赖本机环境的测试使用。
func stubDesktopInstalled(t *testing.T, installed bool) {
	t.Helper()
	desktopDetectMu.Lock()
	oldOK, oldVal, oldAt := desktopDetectOK, desktopDetectVal, desktopDetectAt
	desktopDetectVal = desktopAppInfo{Installed: installed, Version: "9.9.9", Channel: "nightly", FeedURL: "https://example.invalid/feed.yml"}
	desktopDetectAt = time.Now()
	desktopDetectOK = true
	desktopDetectMu.Unlock()
	t.Cleanup(func() {
		desktopDetectMu.Lock()
		desktopDetectOK, desktopDetectVal, desktopDetectAt = oldOK, oldVal, oldAt
		desktopDetectMu.Unlock()
	})
}

// TestLaunchTargetMismatch 启动方式偏好与实机不符时才询问：auto 不询问、已确认过不重复询问、
// 开机自启不打扰（Web 形态可用，仅属提示）、截图模式不弹窗。
func TestLaunchTargetMismatch(t *testing.T) {
	oldPref, oldAck, oldShot, oldAuto := launchTargetPref, launchMismatchAck, shotMode, autostartLaunch
	t.Cleanup(func() {
		launchTargetPref, launchMismatchAck, shotMode, autostartLaunch = oldPref, oldAck, oldShot, oldAuto
	})
	shotMode, autostartLaunch = false, false

	stubDesktopInstalled(t, false)
	launchTargetPref, launchMismatchAck = launchTargetAuto, ""
	if kind, ok := launchTargetMismatch(); ok {
		t.Fatalf("auto 跟随检测，不应询问（kind=%s）", kind)
	}
	launchTargetPref = launchTargetDesktop
	if kind, ok := launchTargetMismatch(); !ok || kind != "desktop-missing" {
		t.Fatalf("配置 desktop 但未装桌面端：应询问 desktop-missing，实际 kind=%q ok=%v", kind, ok)
	}
	launchMismatchAck = launchTargetDesktop
	if _, ok := launchTargetMismatch(); ok {
		t.Fatal("用户已就同一偏好选择「保持现状」：不应重复询问")
	}

	stubDesktopInstalled(t, true)
	launchTargetPref, launchMismatchAck = launchTargetWeb, ""
	if kind, ok := launchTargetMismatch(); !ok || kind != "desktop-installed" {
		t.Fatalf("配置 web 但装了桌面端：应询问 desktop-installed，实际 kind=%q ok=%v", kind, ok)
	}
	autostartLaunch = true
	if _, ok := launchTargetMismatch(); ok {
		t.Fatal("开机自启下不应打扰（Web 形态可用，仅属「也可用桌面端」提示）")
	}
	autostartLaunch = false

	launchTargetPref = launchTargetAuto
	launchMismatchAck = ""
	shotMode = true
	launchTargetPref = launchTargetWeb
	if _, ok := launchTargetMismatch(); ok {
		t.Fatal("截图/演示模式不应弹窗")
	}
}

// TestServiceStopNeededForPluginOps 包操作前的停服判据：web 启动方式总是停；
// desktop 启动方式只有服务确实在跑时才停（解 node_modules 文件占用），否则不停也不重启。
func TestServiceStopNeededForPluginOps(t *testing.T) {
	oldPref, oldPort, oldURL, oldStarted := launchTargetPref, port, webURL, serverStartedPort
	t.Cleanup(func() {
		launchTargetPref, port, webURL, serverStartedPort = oldPref, oldPort, oldURL, oldStarted
	})
	stubDesktopInstalled(t, true)
	// 指向本机未监听的端口：resolveRunningService 快速判定「未运行」
	port, webURL, serverStartedPort = 9, "http://127.0.0.1:9/", 0

	launchTargetPref = launchTargetWeb
	if !serviceStopNeededForPluginOps() {
		t.Fatal("web 启动方式必须停后台服务（改完插件要重启校验）")
	}
	launchTargetPref = launchTargetDesktop
	if serviceStopNeededForPluginOps() {
		t.Fatal("desktop 启动方式且服务未运行：不应停服")
	}
}

// TestProfileNeedsServiceVerify 插件变更是否需要「重启服务 + 启动校验」：desktop profile 的加载
// 由官方桌面端负责；desktop 启动方式下托盘自带服务本就不在运行。
func TestProfileNeedsServiceVerify(t *testing.T) {
	oldPref := launchTargetPref
	t.Cleanup(func() { launchTargetPref = oldPref })
	stubDesktopInstalled(t, true)

	launchTargetPref = launchTargetWeb
	if !profileNeedsServiceVerify(accountPluginProfile) {
		t.Fatal("web profile + web 启动方式：必须重启并校验")
	}
	if profileNeedsServiceVerify("desktop") {
		t.Fatal("desktop profile：加载由桌面端负责，不应由托盘重启校验")
	}
	launchTargetPref = launchTargetDesktop
	if profileNeedsServiceVerify(accountPluginProfile) {
		t.Fatal("desktop 启动方式：后台服务未运行，不应重启校验")
	}
}

// TestTrayOpenTitleFollowsLaunchTarget 托盘「打开」菜单项文案随启动方式切换。
// 托盘菜单是原生菜单（无头环境无法截图），因此直接断言取文案的判据与两种语言的译文。
func TestTrayOpenTitleFollowsLaunchTarget(t *testing.T) {
	oldPref, oldLang := launchTargetPref, curLang
	t.Cleanup(func() { launchTargetPref, curLang = oldPref, oldLang })

	cases := []struct {
		installed bool
		pref      string
		wantZH    string
		wantEN    string
	}{
		{true, launchTargetAuto, "打开 Desktop UI", "Open Desktop UI"},    // 装了桌面端 + auto → 桌面端
		{true, launchTargetDesktop, "打开 Desktop UI", "Open Desktop UI"}, // 显式 desktop
		{true, launchTargetWeb, "打开 Web UI", "Open Web UI"},             // 显式 web 不被检测结果覆盖
		{false, launchTargetAuto, "打开 Web UI", "Open Web UI"},           // 未装 → Web UI
		{false, launchTargetDesktop, "打开 Web UI", "Open Web UI"},        // 未装 + desktop → 回退 Web UI
		{false, launchTargetWeb, "打开 Web UI", "Open Web UI"},
	}
	for _, c := range cases {
		stubDesktopInstalled(t, c.installed)
		launchTargetPref = c.pref
		curLang = "zh"
		if got := trayOpenTitle(); got != c.wantZH {
			t.Fatalf("installed=%v pref=%q: trayOpenTitle() = %q, want %q", c.installed, c.pref, got, c.wantZH)
		}
		curLang = "en" // 英文界面下必须命中 i18n 词典（缺键会回退中文，这里会失败）
		if got := trayOpenTitle(); got != c.wantEN {
			t.Fatalf("installed=%v pref=%q: trayOpenTitle() [en] = %q, want %q", c.installed, c.pref, got, c.wantEN)
		}
	}
}

// TestDesktopAppDetectionOnHost 本机检测（按平台断言）：装了就要求能定位到可执行文件与更新源，
// 没装则不应给出任何字段；两种结论都不允许把本程序（dsh-systray）误判成桌面端。
func TestDesktopAppDetectionOnHost(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("desktop detection only implemented on windows/darwin")
	}
	info := desktopApp()
	if !info.Installed {
		if info.Exe != "" || info.Version != "" || info.FeedURL != "" {
			t.Fatalf("not installed should be zero value, got %+v", info)
		}
		return
	}
	if !fileExists(info.Exe) {
		t.Fatalf("installed but exe missing: %q", info.Exe)
	}
	if strings.Contains(strings.ToLower(info.Exe), "systray") {
		t.Fatalf("detection matched dsh-systray itself: %q", info.Exe)
	}
	if info.FeedURL == "" || !strings.HasPrefix(info.FeedURL, "https://") {
		t.Fatalf("feed url = %q", info.FeedURL)
	}
	if !strings.Contains(info.FeedURL, "/feeds/") {
		t.Fatalf("feed url should point at the desktop feed dir: %q", info.FeedURL)
	}
}

// TestLaunchTargetConfigRoundTrip 启动方式随 config.json 持久化（含字段缺失的旧配置）。
func TestLaunchTargetConfigRoundTrip(t *testing.T) {
	old := launchTargetPref
	t.Cleanup(func() { launchTargetPref = old })

	launchTargetPref = launchTargetDesktop
	if got := currentConfig().LaunchTarget; got != launchTargetDesktop {
		t.Fatalf("currentConfig launchTarget = %q", got)
	}

	// 旧配置文件没有该字段：保持默认 auto，不得被写成 web（否则桌面端用户升级后会被"降级"）
	cfg := appConfig{Port: 3080}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"port": 3081}`), 0o644); err != nil {
		t.Fatal(err)
	}
	applyConfigFile(&cfg, path)
	if cfg.LaunchTarget != "" {
		t.Fatalf("missing field should stay empty, got %q", cfg.LaunchTarget)
	}
	if got := resolveLaunchTargetPref(cfg.LaunchTarget, true); got != launchTargetDesktop {
		t.Fatalf("legacy config on a desktop-installed machine should resolve to desktop, got %q", got)
	}
	if err := os.WriteFile(path, []byte(`{"launchTarget":"web"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	applyConfigFile(&cfg, path)
	if cfg.LaunchTarget != "web" {
		t.Fatalf("explicit launchTarget should be read, got %q", cfg.LaunchTarget)
	}
}

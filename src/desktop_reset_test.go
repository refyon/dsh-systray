package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// desktop_reset_test.go：桌面端重置（desktop_reset.go）的纯逻辑/结构校验测试：
// 安装包命名解析、安装包地址推导、本机安装包校验、重置目标候选构建。
// 真实下载与安装向导不在此覆盖（端到端需按平台手工验证）。

// TestParseDesktopInstallerName 官方安装包命名的解析与拒绝：平台/扩展名/前缀/版本段任一不符即不合法。
func TestParseDesktopInstallerName(t *testing.T) {
	cases := []struct {
		name   string
		goos   string
		goarch string
		base   string
		want   string
		ok     bool
	}{
		{"windows rc 包", "windows", "amd64", "deepseek-harness-0.2.0-rc.2-win-x64.exe", "0.2.0-rc.2", true},
		{"windows 稳定版包", "windows", "amd64", "deepseek-harness-0.1.7-win-x64.exe", "0.1.7", true},
		{"大写扩展名", "windows", "amd64", "deepseek-harness-0.1.7-win-x64.EXE", "0.1.7", true},
		{"mac arm64 dmg", "darwin", "arm64", "deepseek-harness-0.1.7-rc.2-mac-arm64.dmg", "0.1.7-rc.2", true},
		{"其它平台包（本机不适用）", "windows", "amd64", "deepseek-harness-0.1.7-mac-arm64.dmg", "", false},
		{"mac x64 包装在 arm64 上", "darwin", "arm64", "deepseek-harness-0.1.7-mac-x64.dmg", "", false},
		{"扩展名不符", "windows", "amd64", "deepseek-harness-0.1.7-win-x64.zip", "", false},
		{"前缀不符", "windows", "amd64", "deepseek-setup-0.1.7-win-x64.exe", "", false},
		{"版本段非法", "windows", "amd64", "deepseek-harness-abc-win-x64.exe", "", false},
		{"版本段为空", "windows", "amd64", "deepseek-harness--win-x64.exe", "", false},
		{"附带路径", "windows", "amd64", `C:\Users\demo\Downloads\deepseek-harness-0.2.0-rc.1-win-x64.exe`, "0.2.0-rc.1", true},
	}
	for _, c := range cases {
		got, ok := parseDesktopInstallerName(c.base, c.goos, c.goarch)
		if ok != c.ok || got != c.want {
			t.Errorf("%s：parseDesktopInstallerName(%q, %s/%s) = (%q, %v)，期望 (%q, %v)",
				c.name, c.base, c.goos, c.goarch, got, ok, c.want, c.ok)
		}
	}
}

// TestDesktopInstallerFileNameRoundTrip 文件名生成与解析互为逆运算（当前平台）。
func TestDesktopInstallerFileNameRoundTrip(t *testing.T) {
	name := desktopInstallerFileName("0.2.0-rc.2", runtime.GOOS, runtime.GOARCH)
	version, ok := parseDesktopInstallerName(name, runtime.GOOS, runtime.GOARCH)
	if !ok || version != "0.2.0-rc.2" {
		t.Fatalf("往返失败：name=%q version=%q ok=%v", name, version, ok)
	}
}

// TestDesktopInstallerURLFor 安装包直链由更新源地址推出；更新源不是官方域名时回落到官方兜底基址。
func TestDesktopInstallerURLFor(t *testing.T) {
	cases := []struct {
		name    string
		feedURL string
		goos    string
		goarch  string
		want    string
	}{
		{
			"官方更新源（win）",
			"https://download.deepseek.com/dsh-desk/feeds/win-x64/",
			"windows", "amd64",
			"https://download.deepseek.com/dsh-desk/bin/win-x64/deepseek-harness-0.2.0-rc.2-win-x64.exe",
		},
		{
			"官方更新源（mac arm64）",
			"https://download.deepseek.com/dsh-desk/feeds/mac-arm64/",
			"darwin", "arm64",
			"https://download.deepseek.com/dsh-desk/bin/mac-arm64/deepseek-harness-0.2.0-rc.2-mac-arm64.dmg",
		},
		{
			"非官方更新源 → 兜底基址",
			"https://mirror.example.com/dsh/feeds/win-x64/",
			"windows", "amd64",
			"https://download.deepseek.com/dsh-desk/bin/win-x64/deepseek-harness-0.2.0-rc.2-win-x64.exe",
		},
		{
			"更新源为空 → 兜底基址",
			"",
			"windows", "amd64",
			"https://download.deepseek.com/dsh-desk/bin/win-x64/deepseek-harness-0.2.0-rc.2-win-x64.exe",
		},
	}
	for _, c := range cases {
		got := desktopInstallerURLFor(c.feedURL, "0.2.0-rc.2", c.goos, c.goarch)
		if got != c.want {
			t.Errorf("%s：desktopInstallerURLFor = %q，期望 %q", c.name, got, c.want)
		}
		if !isDesktopUpdateAssetURL(got) {
			t.Errorf("%s：生成的地址必须通过官方域名白名单：%q", c.name, got)
		}
	}
}

// TestBuildDesktopResetOptions 重置目标候选：官方更新源当前版 + 本机已装版 + 本机已有安装包版本，
// 去重按新→旧；已有安装包的版本带 localPath；默认选中本机已装版本（同版本重装）。
func TestBuildDesktopResetOptions(t *testing.T) {
	locals := []ResetLocalInstaller{
		{Version: "0.1.7-rc.2", Path: `C:\Users\demo\Downloads\a.exe`, Size: 100},
		{Version: "0.2.0-rc.1", Path: `C:\Users\demo\Downloads\b.exe`, Size: 200},
	}
	opts, def := buildDesktopResetOptions("0.2.0-rc.2", "0.1.7-rc.2", locals)
	if len(opts) != 3 {
		t.Fatalf("候选应为 3 个（feed + 当前 + 另一个本机包），实际 %d：%+v", len(opts), opts)
	}
	wantOrder := []string{"0.2.0-rc.2", "0.2.0-rc.1", "0.1.7-rc.2"}
	for i, w := range wantOrder {
		if opts[i].Version != w {
			t.Fatalf("排序应为新→旧 %v，实际 %+v", wantOrder, opts)
		}
	}
	if def != "0.1.7-rc.2" {
		t.Fatalf("默认应选中本机已装版本（同版本重装），实际 %q", def)
	}
	if opts[2].LocalPath == "" || opts[2].Size != 100 {
		t.Fatalf("本机已有安装包的版本应带 localPath/size：%+v", opts[2])
	}
	if opts[0].LocalPath != "" {
		t.Fatalf("本机没有该版本的安装包时不应带 localPath：%+v", opts[0])
	}
}

// TestBuildDesktopResetOptionsCurrentNotInFeed 当前版本不在官方源候选里也要列出来
// （官方源保留历史 rc 包，同版本重装是最安全的默认目标）。
func TestBuildDesktopResetOptionsCurrentNotInFeed(t *testing.T) {
	opts, def := buildDesktopResetOptions("0.2.0-rc.2", "0.1.7-rc.2", nil)
	if len(opts) != 2 {
		t.Fatalf("应列出 feed 版 + 当前版，实际 %+v", opts)
	}
	if def != "0.1.7-rc.2" {
		t.Fatalf("默认应选中当前版本，实际 %q", def)
	}
}

// TestBuildDesktopResetOptionsEmpty 无任何来源时不给候选、不给默认（前端据此禁用「开始重置」）。
func TestBuildDesktopResetOptionsEmpty(t *testing.T) {
	opts, def := buildDesktopResetOptions("", "", nil)
	if len(opts) != 0 || def != "" {
		t.Fatalf("空输入应得空候选，实际 opts=%+v def=%q", opts, def)
	}
}

// TestResolveDesktopResetInstallerAlwaysDownloads 下拉选的版本一律从官方源重新下载：
// 本机已有同名（同版本）安装包也照样覆盖，保证用户「有机会重新下载」（用户约定 2026-10-09）；
// 想复用本机文件只能通过「选择本地安装包」显式指定。
func TestResolveDesktopResetInstallerAlwaysDownloads(t *testing.T) {
	oldMin := desktopInstallerMinSize
	desktopInstallerMinSize = 10
	t.Cleanup(func() { desktopInstallerMinSize = oldMin })

	// 本机已有一份完整安装包（放在临时"下载目录"里）
	dir := t.TempDir()
	local := filepath.Join(dir, desktopInstallerFileName("0.2.0-rc.2", runtime.GOOS, runtime.GOARCH))
	if err := os.WriteFile(local, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath, oldDL := desktopInstallerPathFn, downloadFileWithProgressFn
	t.Cleanup(func() { desktopInstallerPathFn, downloadFileWithProgressFn = oldPath, oldDL })
	desktopInstallerPathFn = func(string) (string, error) { return local, nil }

	downloads := 0
	downloadFileWithProgressFn = func(_ context.Context, _ string, dest string, _ func(float64)) error {
		downloads++
		return os.WriteFile(dest, make([]byte, 128), 0o644) // 模拟下载完成
	}

	splash := &SplashState{Update: func(string, float64) {}, Close: func() {}}
	pkg, err := resolveDesktopResetInstaller(desktopAppInfo{FeedURL: "https://example.invalid/feed.yml"}, "0.2.0-rc.2", "", splash)
	if err != nil {
		t.Fatalf("解析安装包失败：%v", err)
	}
	if downloads != 1 {
		t.Fatalf("应重新下载一次（不复用本机已有安装包），实际 %d 次", downloads)
	}
	if !pkg.Downloaded || pkg.Size != 128 {
		t.Fatalf("应使用本次下载的安装包：%+v", pkg)
	}
}

// TestResolveDesktopResetInstallerUsesPickedLocal 显式指定本地安装包时直接用文件、不下载。
func TestResolveDesktopResetInstallerUsesPickedLocal(t *testing.T) {
	oldMin := desktopInstallerMinSize
	desktopInstallerMinSize = 10
	t.Cleanup(func() { desktopInstallerMinSize = oldMin })
	oldDL := downloadFileWithProgressFn
	t.Cleanup(func() { downloadFileWithProgressFn = oldDL })
	downloadFileWithProgressFn = func(context.Context, string, string, func(float64)) error {
		t.Fatal("显式指定本地安装包时不该下载")
		return nil
	}

	dir := t.TempDir()
	picked := filepath.Join(dir, desktopInstallerFileName("0.2.0-rc.1", runtime.GOOS, runtime.GOARCH))
	if err := os.WriteFile(picked, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	splash := &SplashState{Update: func(string, float64) {}, Close: func() {}}
	pkg, err := resolveDesktopResetInstaller(desktopAppInfo{}, "0.2.0-rc.2", picked, splash)
	if err != nil {
		t.Fatalf("解析安装包失败：%v", err)
	}
	if pkg.Downloaded || pkg.Path != picked || pkg.Version != "0.2.0-rc.1" {
		t.Fatalf("应直接使用指定的本地安装包：%+v", pkg)
	}
}

// TestRefreshSyncAfterReset 重置收尾的同步状态刷新：
// 未登录 → 只清安装台账、不触发同步；已登录 → 先清台账再立刻触发一次完整同步检查。
func TestRefreshSyncAfterReset(t *testing.T) {
	setupAccountTest(t)
	oldTick := accountBackgroundTickFn
	t.Cleanup(func() { accountBackgroundTickFn = oldTick })
	ticks := make(chan struct{}, 4)
	accountBackgroundTickFn = func(context.Context) { ticks <- struct{}{} }

	// 未登录：不触发同步，但台账照清（重置清空插件后旧时刻无意义）
	pluginInstallTimeNoteInstalled("web", "pkg-a", 1790738817)
	refreshSyncAfterReset(true, "")
	if pluginInstallTimeLookup("web", "pkg-a") != 0 {
		t.Fatal("未登录也应清掉被重置删掉的插件的安装台账")
	}
	select {
	case <-ticks:
		t.Fatal("未登录不该触发同步检查")
	default:
	}

	// 已登录：触发一次同步检查
	setAccountState(loggedInState())
	refreshSyncAfterReset(false, "")
	select {
	case <-ticks:
	case <-time.After(2 * time.Second):
		t.Fatal("已登录应触发一次同步检查")
	}
}

// TestRunDesktopResetFlowRefreshesSyncAndLedger 桌面端重置成功后：清插件时清安装台账，
// 并立刻触发同步状态刷新（用户约定 2026-10-09：账号记录保持不变，但状态要按重置后的现状刷新）。
func TestRunDesktopResetFlowRefreshesSyncAndLedger(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())
	t.Setenv("DSH_HOME", t.TempDir()) // 隔离：本用例会真的执行「清空插件」，绝不能碰真实 ~/.dsh

	oldMin := desktopInstallerMinSize
	desktopInstallerMinSize = 10
	t.Cleanup(func() { desktopInstallerMinSize = oldMin })

	installDir := t.TempDir()
	stubDesktopInstalled(t, true)
	desktopDetectMu.Lock()
	desktopDetectVal.DisplayPath = installDir
	desktopDetectVal.Exe = filepath.Join(installDir, desktopExeName)
	desktopDetectMu.Unlock()

	installer := filepath.Join(t.TempDir(), desktopInstallerFileName("0.2.0-rc.2", runtime.GOOS, runtime.GOARCH))
	if err := os.WriteFile(installer, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}

	oldRunning, oldKill, oldSilent, oldVerify :=
		desktopAppProcessRunningFn, killDesktopAppProcessesFn, runDesktopInstallerSilentFn, waitDesktopInstalledVersionFn
	oldTick := accountBackgroundTickFn
	t.Cleanup(func() {
		desktopAppProcessRunningFn, killDesktopAppProcessesFn = oldRunning, oldKill
		runDesktopInstallerSilentFn, waitDesktopInstalledVersionFn = oldSilent, oldVerify
		accountBackgroundTickFn = oldTick
	})
	desktopAppProcessRunningFn = func(desktopAppInfo) bool { return false }
	killDesktopAppProcessesFn = func(desktopAppInfo) error { return nil }
	runDesktopInstallerSilentFn = func(string, string, time.Duration) error { return nil }
	waitDesktopInstalledVersionFn = func(string, time.Duration) error { return nil }
	ticks := make(chan struct{}, 4)
	accountBackgroundTickFn = func(context.Context) { ticks <- struct{}{} }

	pluginInstallTimeNoteInstalled("web", "pkg-a", 1790738817) // 重置前本机装着插件
	res := runDesktopResetFlow(false, true, "", installer, nil)
	if !res.OK {
		t.Fatalf("重置应成功：%+v", res)
	}
	if pluginInstallTimeLookup("web", "pkg-a") != 0 {
		t.Fatal("清插件后应清掉安装台账")
	}
	select {
	case <-ticks:
	case <-time.After(2 * time.Second):
		t.Fatal("重置成功后应立刻触发一次同步状态刷新")
	}
}

// TestValidateAndScanDesktopInstaller 本机安装包的结构校验与目录扫描：命名/平台/体积三重口径。
func TestValidateAndScanDesktopInstaller(t *testing.T) {
	old := desktopInstallerMinSize
	desktopInstallerMinSize = 10
	t.Cleanup(func() { desktopInstallerMinSize = old })

	dir := t.TempDir()
	good := filepath.Join(dir, desktopInstallerFileName("0.2.0-rc.2", runtime.GOOS, runtime.GOARCH))
	if err := os.WriteFile(good, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	// 残包：体积不足
	small := filepath.Join(dir, desktopInstallerFileName("0.2.0-rc.1", runtime.GOOS, runtime.GOARCH))
	if err := os.WriteFile(small, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 命名非法
	bad := filepath.Join(dir, "setup.exe")
	if err := os.WriteFile(bad, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}

	ins, err := validateDesktopInstaller(good, "picked")
	if err != nil || ins.Version != "0.2.0-rc.2" || ins.Size != 64 || ins.Source != "picked" {
		t.Fatalf("合法安装包应通过校验：%+v err=%v", ins, err)
	}
	if _, err := validateDesktopInstaller(small, "picked"); err == nil {
		t.Fatal("体积不足的残包应判为异常")
	}
	if _, err := validateDesktopInstaller(bad, "picked"); err == nil {
		t.Fatal("命名非法的文件应判为异常")
	}
	if _, err := validateDesktopInstaller(filepath.Join(dir, "missing.exe"), "picked"); err == nil {
		t.Fatal("不存在的文件应判为异常")
	}
	if _, err := validateDesktopInstaller("", "picked"); err == nil {
		t.Fatal("空路径应判为异常")
	}

	found := installersInDir(dir, "downloads")
	if len(found) != 1 || found[0].Path != good {
		t.Fatalf("目录扫描应只认出体积达标的官方命名安装包，实际 %+v", found)
	}
}

// TestFormatInstallerSize 体积文案（KB/MB）。
func TestFormatInstallerSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"}, {512, "1 KB"}, {289013760, "275.6 MB"},
	}
	for _, c := range cases {
		if got := formatInstallerSize(c.in); got != c.want {
			t.Errorf("formatInstallerSize(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestWaitDesktopAppStopped 结束桌面端后的等待：已退出立即返回；退出后返回；一直不退则超时报错
// （调用方据此中止重置并提示手动处理，而不是带着文件占用继续清数据）。
func TestWaitDesktopAppStopped(t *testing.T) {
	old := desktopAppProcessRunningFn
	t.Cleanup(func() { desktopAppProcessRunningFn = old })
	info := desktopAppInfo{Exe: "DeepSeek Harness.exe"}

	polls := 0
	desktopAppProcessRunningFn = func(desktopAppInfo) bool { polls++; return false }
	if err := waitDesktopAppStopped(info, 200*time.Millisecond); err != nil || polls != 1 {
		t.Fatalf("已退出应立即返回：err=%v polls=%d", err, polls)
	}

	polls = 0
	desktopAppProcessRunningFn = func(desktopAppInfo) bool { polls++; return polls < 3 }
	if err := waitDesktopAppStopped(info, 2*time.Second); err != nil {
		t.Fatalf("轮询到退出应返回 nil：%v", err)
	}

	desktopAppProcessRunningFn = func(desktopAppInfo) bool { return true }
	if err := waitDesktopAppStopped(info, 300*time.Millisecond); err == nil {
		t.Fatal("一直不退应超时报错")
	}
}

// TestStopDesktopAppForReset 重置前的结束动作：本来没运行 → 不结束、不报错；运行中 → 结束一次并汇报。
func TestStopDesktopAppForReset(t *testing.T) {
	oldRunning, oldKill := desktopAppProcessRunningFn, killDesktopAppProcessesFn
	t.Cleanup(func() { desktopAppProcessRunningFn, killDesktopAppProcessesFn = oldRunning, oldKill })
	info := desktopAppInfo{Exe: "DeepSeek Harness.exe"}

	running := false
	kills := 0
	desktopAppProcessRunningFn = func(desktopAppInfo) bool { return running }
	killDesktopAppProcessesFn = func(desktopAppInfo) error { kills++; running = false; return nil }

	if killed, err := stopDesktopAppForReset(info); killed || err != nil || kills != 0 {
		t.Fatalf("未运行时不该结束进程：killed=%v err=%v kills=%d", killed, err, kills)
	}
	running = true
	if killed, err := stopDesktopAppForReset(info); !killed || err != nil || kills != 1 {
		t.Fatalf("运行中应结束一次并汇报：killed=%v err=%v kills=%d", killed, err, kills)
	}
}

// TestDesktopAppKillHelper 靶子进程（不是真实用例）：TestKillDesktopAppProcesses 以
// 「DeepSeek Harness.exe」为名启动本测试二进制，它只睡不做事，等被结束。
func TestDesktopAppKillHelper(t *testing.T) {
	if os.Getenv("DSH_KILL_HELPER") != "1" {
		t.Skip("helper process only")
	}
	time.Sleep(60 * time.Second)
}

// TestKillDesktopAppProcesses 结束桌面端进程的真实闭环（Windows）：以官方主程序名启动一个
// 靶子进程 → 存活判定命中 → 结束 → 判定不再命中。
//
// 安全阀：本机装了官方桌面端时跳过——实现按可执行文件名匹配，真机上跑会误杀用户正在用的应用。
func TestKillDesktopAppProcesses(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("按进程名结束仅 Windows 实现")
	}
	if desktopApp().Installed {
		t.Skip("本机已安装官方桌面端：跳过以免误杀正在运行的应用")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("取测试二进制路径失败: %v", err)
	}
	target := filepath.Join(t.TempDir(), desktopExeName) // 与真实主程序同名
	if err := copyFileForTest(self, target); err != nil {
		t.Fatalf("复制靶子可执行文件失败: %v", err)
	}
	cmd := exec.Command(target, "-test.run=TestDesktopAppKillHelper")
	cmd.Env = append(os.Environ(), "DSH_KILL_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动靶子进程失败: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	info := desktopAppInfo{Exe: target}
	deadline := time.Now().Add(10 * time.Second)
	for !desktopAppProcessRunning(info) {
		if time.Now().After(deadline) {
			t.Fatal("靶子进程未出现在进程列表里")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := killDesktopAppProcesses(info); err != nil {
		t.Fatalf("结束进程失败: %v", err)
	}
	if err := waitDesktopAppStopped(info, 10*time.Second); err != nil {
		t.Fatalf("靶子进程未退出: %v", err)
	}
}

// copyFileForTest 复制文件（测试用：造一个与官方主程序同名的靶子进程）。
func copyFileForTest(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// TestDesktopSilentInstallArgs 覆盖安装参数与桌面端自带更新器一致：
// --updated（按更新处理）、/S（静默）、/D=<目录>（最后一个参数，NSIS 要求）。
func TestDesktopSilentInstallArgs(t *testing.T) {
	got := desktopSilentInstallArgs(`D:\Program Files\DeepSeek Harness`)
	want := []string{"--updated", "/S", `/D=D:\Program Files\DeepSeek Harness`}
	if len(got) != len(want) {
		t.Fatalf("静默安装参数错误：%q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("静默安装参数错误：%q（期望 %q）", got, want)
		}
	}
}

// TestDesktopInstallPath 原安装路径的解析：优先安装登记给的落地路径；没给则按主程序路径反推。
func TestDesktopInstallPath(t *testing.T) {
	if got := desktopInstallPath(desktopAppInfo{}); got != "" {
		t.Fatalf("未安装时应为空，实际 %q", got)
	}
	if got := desktopInstallPath(desktopAppInfo{DisplayPath: `D:\Program Files\DeepSeek Harness`}); got != `D:\Program Files\DeepSeek Harness` {
		t.Fatalf("应取安装登记的落地路径，实际 %q", got)
	}
	exe := filepath.Join(string(filepath.Separator)+"opt", "app", desktopExeName)
	got := desktopInstallPath(desktopAppInfo{Exe: exe})
	if runtime.GOOS == "darwin" {
		if got != "" { // 没有 .app 段的路径在 macOS 上反推不出 http://包路径
			t.Fatalf("macOS 上非 .app 路径应反推为空，实际 %q", got)
		}
		return
	}
	if got != filepath.Dir(exe) {
		t.Fatalf("应按主程序路径取目录 %q，实际 %q", filepath.Dir(exe), got)
	}
}

// TestDesktopResetModeFor 落地方式：已安装且原路径已知 → 静默覆盖；未装/路径未知 → 安装向导。
func TestDesktopResetModeFor(t *testing.T) {
	if mode, _ := desktopResetModeFor(desktopAppInfo{}); mode != desktopResetInstallWizard {
		t.Fatal("未安装应走官方安装向导")
	}
	if mode, _ := desktopResetModeFor(desktopAppInfo{Installed: true}); mode != desktopResetInstallWizard {
		t.Fatal("原路径未知时应回退安装向导")
	}
	dir := t.TempDir()
	mode, path := desktopResetModeFor(desktopAppInfo{Installed: true, DisplayPath: dir})
	if mode != desktopResetInstallInPlace || path != dir {
		t.Fatalf("已安装应静默覆盖原路径：mode=%v path=%q", mode, path)
	}
}

// TestDesktopInstalledVersionMatches 安装后的版本验收：Windows 全量比较（-rc.N 也参与），
// macOS 只比数值核心（CFBundleShortVersionString 不带预发布后缀）。
func TestDesktopInstalledVersionMatches(t *testing.T) {
	if !desktopInstalledVersionMatches("v0.2.0-rc.2", "0.2.0-rc.2") {
		t.Fatal("同版本（忽略 v 前缀）应通过")
	}
	if desktopInstalledVersionMatches("0.1.7-rc.2", "0.2.0-rc.2") {
		t.Fatal("不同数值核心不应通过")
	}
	if got := versionCore("0.2.0-rc.2+build"); got != "0.2.0" {
		t.Fatalf("versionCore = %q", got)
	}
	// 仅预发布后缀不同：macOS 视为达到（包版本号规则不同），其它平台视为未达到
	preOnly := desktopInstalledVersionMatches("0.2.0", "0.2.0-rc.2")
	if runtime.GOOS == "darwin" && !preOnly {
		t.Fatal("macOS 上数值核心一致应通过")
	}
	if runtime.GOOS != "darwin" && preOnly {
		t.Fatal("非 macOS 上预发布后缀不同应判为未达到")
	}
}

// TestRunDesktopResetFlowInPlace 已安装设备的重置流程：结束运行中的桌面端 → 清数据 →
// 静默覆盖到原路径 → 版本验收 → 重新启动桌面端；不启动官方安装向导（用户约定 2026-10-09）。
func TestRunDesktopResetFlowInPlace(t *testing.T) {
	setupAccountTest(t) // 隔离账号态：收尾的同步刷新在未登录时不触发（见 refreshSyncAfterReset）
	oldMin := desktopInstallerMinSize
	desktopInstallerMinSize = 10
	t.Cleanup(func() { desktopInstallerMinSize = oldMin })

	installDir := t.TempDir()
	stubDesktopInstalled(t, true)
	desktopDetectMu.Lock()
	desktopDetectVal.DisplayPath = installDir
	desktopDetectVal.Exe = filepath.Join(installDir, desktopExeName)
	desktopDetectMu.Unlock()

	// 本机已有的安装包（免下载）
	installerDir := t.TempDir()
	installer := filepath.Join(installerDir, desktopInstallerFileName("0.2.0-rc.2", runtime.GOOS, runtime.GOARCH))
	if err := os.WriteFile(installer, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}

	oldRunning, oldKill, oldSilent, oldVerify, oldLaunch :=
		desktopAppProcessRunningFn, killDesktopAppProcessesFn, runDesktopInstallerSilentFn,
		waitDesktopInstalledVersionFn, launchInstallerFileFn
	t.Cleanup(func() {
		desktopAppProcessRunningFn, killDesktopAppProcessesFn = oldRunning, oldKill
		runDesktopInstallerSilentFn, waitDesktopInstalledVersionFn = oldSilent, oldVerify
		launchInstallerFileFn = oldLaunch
	})

	running := true
	killed := 0
	desktopAppProcessRunningFn = func(desktopAppInfo) bool { return running }
	killDesktopAppProcessesFn = func(desktopAppInfo) error { killed++; running = false; return nil }

	silentArgs := [][2]string{}
	runDesktopInstallerSilentFn = func(installer, dir string, _ time.Duration) error {
		silentArgs = append(silentArgs, [2]string{installer, dir})
		return nil
	}
	verified := ""
	waitDesktopInstalledVersionFn = func(want string, _ time.Duration) error { verified = want; return nil }
	launched := 0
	launchInstallerFileFn = func(string) error { launched++; return nil }

	res := runDesktopResetFlow(false, false, "", installer, nil)
	if !res.OK {
		t.Fatalf("流程应成功：%+v", res)
	}
	if killed != 1 {
		t.Fatalf("应先结束运行中的桌面端一次，实际 %d", killed)
	}
	if len(silentArgs) != 1 || silentArgs[0][0] != installer || silentArgs[0][1] != installDir {
		t.Fatalf("应静默覆盖安装到原路径 %q：%+v", installDir, silentArgs)
	}
	if verified != "0.2.0-rc.2" {
		t.Fatalf("应验收目标版本，实际 %q", verified)
	}
	if launched != 0 {
		t.Fatal("已安装设备不该再走官方安装向导")
	}
	for _, want := range []string{"已覆盖安装到原路径", installDir, "已结束正在运行的官方桌面端"} {
		if !strings.Contains(res.Note, want) {
			t.Fatalf("结果说明应包含 %q：%s", want, res.Note)
		}
	}
}

// TestRunDesktopResetFlowWizardWhenNotInstalled 未安装（或原路径未知）时仍走官方安装向导。
func TestRunDesktopResetFlowWizardWhenNotInstalled(t *testing.T) {
	setupAccountTest(t) // 同上：隔离账号态
	oldMin := desktopInstallerMinSize
	desktopInstallerMinSize = 10
	t.Cleanup(func() { desktopInstallerMinSize = oldMin })

	stubDesktopInstalled(t, false)

	installerDir := t.TempDir()
	installer := filepath.Join(installerDir, desktopInstallerFileName("0.2.0-rc.2", runtime.GOOS, runtime.GOARCH))
	if err := os.WriteFile(installer, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	oldRunning, oldKill, oldSilent, oldVerify, oldLaunch :=
		desktopAppProcessRunningFn, killDesktopAppProcessesFn, runDesktopInstallerSilentFn,
		waitDesktopInstalledVersionFn, launchInstallerFileFn
	t.Cleanup(func() {
		desktopAppProcessRunningFn, killDesktopAppProcessesFn = oldRunning, oldKill
		runDesktopInstallerSilentFn, waitDesktopInstalledVersionFn = oldSilent, oldVerify
		launchInstallerFileFn = oldLaunch
	})

	desktopAppProcessRunningFn = func(desktopAppInfo) bool { return false }
	killDesktopAppProcessesFn = func(desktopAppInfo) error { t.Fatal("未运行不该结束进程"); return nil }
	silent := 0
	runDesktopInstallerSilentFn = func(string, string, time.Duration) error { silent++; return nil }
	waitDesktopInstalledVersionFn = func(string, time.Duration) error { return nil }
	launched := 0
	launchInstallerFileFn = func(string) error { launched++; return nil }

	res := runDesktopResetFlow(false, false, "", installer, nil)
	if !res.OK || launched != 1 || silent != 0 {
		t.Fatalf("未安装应走安装向导：ok=%v 向导=%d 静默安装=%d note=%s", res.OK, launched, silent, res.Note)
	}
}

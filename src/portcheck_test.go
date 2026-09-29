package main

// 端口预检与「端口类失败」分类的单测（见 portcheck.go 与 service_guard.go）。
//
// 现场依据：2026-09-28 10:27 起，本机 3080 落在 Windows 排除端口段 3004-3103
// （netsh int ipv4 show excludedportrange protocol=tcp），node 侧报
//
//	Error: listen EACCES: permission denied 127.0.0.1:3080
//
// 旧收尾把它当成「服务进程异常退出」→ disable-all 用户插件 + LKG 回退。这些用例锁住三件事：
//  1. 端口可用性判定（占用 / 可绑定 / 非法端口）；
//  2. Windows 上必须按 WSA 错误码数值直判（errors.Is 在 Windows 上对 EACCES/EADDRINUSE 恒 false）；
//  3. 端口证据与插件证据不能互相串味——串味的代价是跳过插件自愈，或反过来误禁用户的插件。

import (
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestProbePortInUseThenFree 端口占用 / 释放两条主路径：自己占住 → portInUse；释放后 → portOK。
func TestProbePortInUseThenFree(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	if kind, err := probePort(p); kind != portInUse {
		t.Fatalf("已被监听的端口应判为占用，实际 %v（err=%v）", kind, err)
	}
	_ = ln.Close()
	// 监听套接字未 accept 过连接，释放后不残留 TIME_WAIT；仍不可用只可能是被别的进程抢走
	deadline := time.Now().Add(2 * time.Second)
	for {
		switch kind, _ := probePort(p); kind {
		case portOK:
			return
		case portInUse:
			if time.Now().After(deadline) {
				t.Skipf("端口 %d 释放后被其它进程立即占用，跳过", p)
			}
		default:
			t.Fatalf("端口 %d 释放后仍判为不可用（kind=%v）", p, kind)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestProbePortInvalid 非法端口号直接判为不可绑定（不落到 net.Listen、不 panic）。
func TestProbePortInvalid(t *testing.T) {
	for _, p := range []int{0, -1, 70000} {
		if kind, err := probePort(p); kind != portBindFail || err == nil {
			t.Fatalf("端口 %d 应判为非法，实际 kind=%v err=%v", p, kind, err)
		}
	}
}

// TestPickFreePortSkipsUnusable hint 不可用时必须换一个可绑定端口，且从 freePortBase 起挑
// （15000 以下正是 Windows 动态端口范围，排除端口段就是从那里划走的）。
func TestPickFreePortSkipsUnusable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	taken := ln.Addr().(*net.TCPAddr).Port
	got := pickFreePort(taken)
	if got == taken {
		t.Fatalf("hint 端口 %d 已被占用，不应原样返回", taken)
	}
	if kind, err := probePort(got); kind != portOK {
		t.Fatalf("推荐端口 %d 应可绑定，实际 %v（err=%v）", got, kind, err)
	}
	if got < freePortBase {
		t.Fatalf("推荐端口 %d 应取 %d 及以上（避开 Windows 动态端口范围）", got, freePortBase)
	}
}

// TestPickFreePortKeepsUsableHint hint 可用时沿用当前端口——用户已改好的端口不该被无故改动。
func TestPickFreePortKeepsUsableHint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if kind, _ := probePort(p); kind != portOK {
		t.Skipf("端口 %d 释放后被其它进程占用，跳过", p)
	}
	if got := pickFreePort(p); got != p {
		t.Fatalf("hint 端口 %d 可用时应原样返回，实际 %d", p, got)
	}
}

// TestPortErrClassification 绑定失败的分类判据。
//
// 关键：Windows 上必须按 WSA 错误码数值直判——go1.27 windows/amd64 实测「已占用 = 10048」
// 「排除端口段 = 10013」，而 errors.Is(err, syscall.EADDRINUSE/EACCES) 恒为 false
// （syscall 里那两个是 C 运行时的 100/13）。写错这一处会让预检把「端口被封」当成「端口空闲」。
func TestPortErrClassification(t *testing.T) {
	eaccesWSA := os.NewSyscallError("bind", syscall.Errno(10013)) // WSAEACCES
	einuseWSA := os.NewSyscallError("bind", syscall.Errno(10048)) // WSAEADDRINUSE

	if !isForbiddenBindErr(eaccesWSA) {
		t.Fatal("WSAEACCES(10013) 应判为「系统不允许」")
	}
	if isAddrInUseErr(eaccesWSA) {
		t.Fatal("WSAEACCES 不应判为「已被占用」")
	}
	if !isAddrInUseErr(einuseWSA) {
		t.Fatal("WSAEADDRINUSE(10048) 应判为「已被占用」")
	}
	if isForbiddenBindErr(einuseWSA) {
		t.Fatal("WSAEADDRINUSE 不应判为「系统不允许」")
	}
	// darwin / POSIX 形态（同一份代码要跨平台）
	if !isForbiddenBindErr(os.NewSyscallError("bind", syscall.EACCES)) {
		t.Fatal("POSIX EACCES 应判为「系统不允许」")
	}
	if !isForbiddenBindErr(os.NewSyscallError("bind", syscall.EPERM)) {
		t.Fatal("POSIX EPERM 应判为「系统不允许」")
	}
	if !isAddrInUseErr(os.NewSyscallError("bind", syscall.EADDRINUSE)) {
		t.Fatal("POSIX EADDRINUSE 应判为「已被占用」")
	}
	if isForbiddenBindErr(nil) || isAddrInUseErr(nil) {
		t.Fatal("nil 不应命中任何分类")
	}
}

// TestPortFailKindFromText 端口证据与插件证据必须分开。
//
// 命中时收尾会跳过 disable-all 用户插件与 LKG 回退；漏判则回到旧行为（拿端口问题去动插件树），
// 误判更糟——真正的插件不兼容会被放过、不做自愈。
func TestPortFailKindFromText(t *testing.T) {
	// 现场原文（2026-09-28 10:29:33，已去掉统一日志前缀）
	blocked := "dsh: startup failed: 2 required plugins did not activate\n" +
		"  webserver (required)\n" +
		"    Package: @deepseek-ai/dsh-host-webserver\n" +
		"    Error: listen EACCES: permission denied 127.0.0.1:3080\n" +
		"        at Server.setupListenHandle [as _listen2] (node:net:2145:21)\n"
	if got := portFailKindFromText(blocked); got != failKindPortBlocked {
		t.Fatalf("排除端口段现场应判为 %q，实际 %q", failKindPortBlocked, got)
	}
	inUse := "    Error: listen EADDRINUSE: address already in use 127.0.0.1:3080\n"
	if got := portFailKindFromText(inUse); got != failKindPortInUse {
		t.Fatalf("端口占用应判为 %q，实际 %q", failKindPortInUse, got)
	}
	if got := portFailKindFromText("    Error: listen EPERM: operation not permitted\n"); got != failKindPortBlocked {
		t.Fatalf("EPERM 应判为 %q，实际 %q", failKindPortBlocked, got)
	}
	// 真正的插件 / 版本失败：不得命中
	notPort := []string{
		"",
		"dsh: startup failed: 2 required plugins did not activate",
		`cannot resolve profile bundle "deepseek-idesign"`,
		"plugin(s) failed to load: dsh-ui-taste, restrict-discipline; Cordis startup failed because…",
		"failed to import loader entry dsh-ui-taste (dsh-ui-taste): SyntaxError",
		`profile bundle "dsh-cost-meter" declares no dsh.bundle`,
	}
	for _, s := range notPort {
		if got := portFailKindFromText(s); got != "" {
			t.Fatalf("插件类失败不应判成端口问题：%q → %q", s, got)
		}
	}
}

// TestPortBlockedInUnifiedLogLine bootPortBlocked 只扫统一日志里 [server] 模块行的内容体，
// 因此判据要经得起「带前缀 → 提取内容体」这一步；非 server 模块（pnpm 输出等）不得成为证据。
func TestPortBlockedInUnifiedLogLine(t *testing.T) {
	line := "2026/09/28 10:29:33 [ERROR] [server]     Error: listen EACCES: permission denied 127.0.0.1:3080"
	m := logLineBodyRe.FindStringSubmatch(line)
	if len(m) != 3 || m[1] != "server" {
		t.Fatalf("统一日志行解析失败：%v", m)
	}
	if !portBlockedInText(m[2]) {
		t.Fatalf("提取出的 [server] 内容体应命中端口证据：%q", m[2])
	}

	other := "2026/09/28 10:29:33 [ERROR] [profile] ERR_PNPM_FETCH_404 GET https://registry.npmjs.org/x"
	if mo := logLineBodyRe.FindStringSubmatch(other); len(mo) == 3 && mo[1] == "server" {
		t.Fatal("非 server 模块不应被当成服务启动证据")
	}
}

// TestFailKindContract 前端契约（main.js 的 isPortFailKind 按字面量分流）：
// failKindFor 的两个字面量不能改；portFailReason 必须带上端口号，用户才能照着做。
func TestFailKindContract(t *testing.T) {
	cases := map[portErrKind]string{
		portOK:       "",
		portBlocked:  "port-blocked",
		portInUse:    "port-in-use",
		portBindFail: "port-blocked",
	}
	for kind, want := range cases {
		if got := failKindFor(kind); got != want {
			t.Fatalf("failKindFor(%v) = %q，期望 %q", kind, got, want)
		}
	}
	for _, kind := range []string{failKindPortBlocked, failKindPortInUse} {
		reason := portFailReason(kind, 3080)
		if !strings.Contains(reason, "3080") {
			t.Fatalf("%s 的原因文案应含端口号：%q", kind, reason)
		}
	}
}

// TestWaitPortReleased 端口释放轮询（替代固定 time.Sleep(1s)，见 portcheck.go）：
// 占用中等到超时，释放后立刻返回——且明显快于被替代的固定 1s 等待；不可绑定的端口不空等。
func TestWaitPortReleased(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := ln.Addr().(*net.TCPAddr).Port

	if waitPortReleased(p, 200*time.Millisecond) {
		t.Fatal("端口仍被监听时不应判为已释放")
	}

	// 释放：轮询应在远早于固定 1s 等待的时间内返回 true
	go func() {
		time.Sleep(120 * time.Millisecond)
		_ = ln.Close()
	}()
	start := time.Now()
	if !waitPortReleased(p, 3*time.Second) {
		t.Fatal("端口释放后应判为可用")
	}
	if el := time.Since(start); el > 900*time.Millisecond {
		t.Fatalf("释放轮询过慢：%s（它的替代对象是固定 1s 等待）", el)
	}

	// 非法端口（绑定必然失败）：等下去不会变，必须立即返回
	start = time.Now()
	if waitPortReleased(0, 3*time.Second) {
		t.Fatal("非法端口不应判为可用")
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("非法端口不应空等：%s", el)
	}
}

// netproxy_test.go：代理解析单测（不触网）。
//
// 重点覆盖现场问题（2026-09-26）：系统代理被检测到、回环地址不被代理转发、
// 显式配置优先级、以及绕过清单匹配。
package main

import (
	"net/http"
	"net/url"
	"testing"
)

// setupProxyTest 保存并复位代理相关的进程级状态，避免用例互相污染，
// 并清空进程里可能存在的代理环境变量（否则用例结果会随运行环境漂移）。
func setupProxyTest(t *testing.T) {
	t.Helper()
	prevMode, prevCfg, prevSys := proxyModeOverride, proxyConfigValue, systemProxy
	t.Cleanup(func() {
		outboundProxyMu.Lock()
		proxyModeOverride, proxyConfigValue = prevMode, prevCfg
		outboundProxyMu.Unlock()
		systemProxy = prevSys
	})
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	systemProxy = func() (systemProxyConfig, bool) { return systemProxyConfig{}, false }
	setOutboundProxy(proxyModeAuto)
	setProxyConfigValue("")
}

// stubSystemProxy 让「系统代理」返回固定配置。
func stubSystemProxy(t *testing.T, cfg systemProxyConfig) {
	t.Helper()
	systemProxy = func() (systemProxyConfig, bool) { return cfg, true }
}

// proxyFor 用生效设置解析目标 URL 的代理（nil 表示直连）。
func proxyFor(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("解析测试 URL 失败: %v", err)
	}
	req := &http.Request{URL: u}
	p, err := outboundProxyFunc()(req)
	if err != nil {
		t.Fatalf("代理解析返回错误: %v", err)
	}
	return p
}

func TestNormalizeProxyMode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", proxyModeAuto},
		{"  ", proxyModeAuto},
		{"AUTO", proxyModeAuto},
		{"system", proxyModeAuto},
		{"env", proxyModeAuto},
		{"Direct", proxyModeDirect},
		{"off", proxyModeDirect},
		{"HTTP://127.0.0.1:10808", "http://127.0.0.1:10808"},
		{"socks5://127.0.0.1:10808", "socks5://127.0.0.1:10808"},
	}
	for _, c := range cases {
		if got := normalizeProxyMode(c.in); got != c.want {
			t.Errorf("normalizeProxyMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestProxyURLFromMode(t *testing.T) {
	cases := []struct {
		mode string
		want string // 空 = 期望 nil
	}{
		{"", ""},
		{proxyModeAuto, ""},
		{proxyModeDirect, ""},
		{"http://127.0.0.1:10808", "http://127.0.0.1:10808"},
		{"127.0.0.1:10808", "http://127.0.0.1:10808"}, // 省略 scheme 补 http
		{"socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"ftp://127.0.0.1:21", ""}, // 不支持的 scheme
		{"http://", ""},            // 缺 host
		{"not a url", ""},          // 非法地址（含空格）
	}
	for _, c := range cases {
		got := proxyURLFromMode(c.mode)
		if c.want == "" {
			if got != nil {
				t.Errorf("proxyURLFromMode(%q) = %v, want nil", c.mode, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("proxyURLFromMode(%q) = nil, want %s", c.mode, c.want)
			continue
		}
		if got.String() != c.want {
			t.Errorf("proxyURLFromMode(%q) = %q, want %q", c.mode, got.String(), c.want)
		}
	}
}

func TestParseSystemProxy(t *testing.T) {
	cases := []struct {
		name              string
		enabled           bool
		server, override  string
		wantOK            bool
		wantHTTP          string
		wantHTTPS         string
		wantBypassContain string
	}{
		{
			name:    "单一代理地址（v2rayN 系统代理典型形态）",
			enabled: true, server: "127.0.0.1:10808",
			wantOK: true, wantHTTP: "127.0.0.1:10808", wantHTTPS: "127.0.0.1:10808",
		},
		{
			name:    "未启用",
			enabled: false, server: "127.0.0.1:10808",
			wantOK: false,
		},
		{
			name:    "启用但无地址",
			enabled: true, server: "",
			wantOK: false,
		},
		{
			name:    "按协议分列",
			enabled: true, server: "http=h1:80;https=h2:443",
			wantOK: true, wantHTTP: "h1:80", wantHTTPS: "h2:443",
		},
		{
			name:    "socks 兜底",
			enabled: true, server: "socks=127.0.0.1:1080",
			wantOK: true, wantHTTP: "socks5://127.0.0.1:1080", wantHTTPS: "socks5://127.0.0.1:1080",
		},
		{
			name:    "带引号与空白",
			enabled: true, server: ` "127.0.0.1:10808" `,
			wantOK: true, wantHTTP: "127.0.0.1:10808", wantHTTPS: "127.0.0.1:10808",
		},
		{
			name:    "绕过清单保留可判定条目、丢弃 <local>",
			enabled: true, server: "127.0.0.1:10808", override: "<local>;*.example.com;10.0.0.0/8",
			wantOK: true, wantHTTP: "127.0.0.1:10808", wantHTTPS: "127.0.0.1:10808",
			wantBypassContain: "*.example.com",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, ok := parseSystemProxy(c.enabled, c.server, c.override)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if cfg.HTTP != c.wantHTTP {
				t.Errorf("HTTP = %q, want %q", cfg.HTTP, c.wantHTTP)
			}
			if cfg.HTTPS != c.wantHTTPS {
				t.Errorf("HTTPS = %q, want %q", cfg.HTTPS, c.wantHTTPS)
			}
			for _, b := range cfg.Bypass {
				if b == "<local>" {
					t.Errorf("绕过清单不应保留语义标记 <local>：%v", cfg.Bypass)
				}
			}
			if c.wantBypassContain != "" {
				found := false
				for _, b := range cfg.Bypass {
					if b == c.wantBypassContain {
						found = true
					}
				}
				if !found {
					t.Errorf("绕过清单缺少 %q：%v", c.wantBypassContain, cfg.Bypass)
				}
			}
		})
	}
}

func TestNoProxyMatches(t *testing.T) {
	list := []string{"example.com", ".foo.com", "localhost:3080", "*.bar.com"}
	cases := []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"sub.example.com", true},
		{"notexample.com", false}, // 后缀必须落在点边界上
		{"foo.com", true},
		{"x.foo.com", true},
		{"bar.com", true},   // *.bar.com 归一到 bar.com 子域
		{"a.bar.com", true}, // 同上
		{"localhost:3080", true},
		{"localhost:9999", false}, // 条目带端口须端口一致
		{"127.0.0.1", false},
	}
	for _, c := range cases {
		if got := noProxyMatches(list, c.host); got != c.want {
			t.Errorf("noProxyMatches(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	if !noProxyMatches([]string{"*"}, "anything.test") {
		t.Error("通配 * 应命中任意主机")
	}
	if noProxyMatches(nil, "host.test") {
		t.Error("空清单不应命中")
	}
}

func TestBypassProxyHost(t *testing.T) {
	bypass := []string{
		"localhost", "127.0.0.1", "127.1.2.3", "::1", "0.0.0.0",
		"10.0.0.5", "192.168.3.186", "172.16.0.1", "172.31.255.254", "169.254.1.1",
		"LOCALHOST",
	}
	for _, h := range bypass {
		if !bypassProxyHost(h) {
			t.Errorf("bypassProxyHost(%q) = false, want true（本机/私网恒直连）", h)
		}
	}
	through := []string{"api.instantserv.ccwu.cc", "github.com", "172.32.0.1", "172.15.0.1", "8.8.8.8", "11.0.0.1"}
	for _, h := range through {
		if bypassProxyHost(h) {
			t.Errorf("bypassProxyHost(%q) = true, want false", h)
		}
	}
}

func TestOutboundProxyFunc_LoopbackAlwaysDirect(t *testing.T) {
	setupProxyTest(t)
	// 即使显式配置了代理，回环与本机服务也不得走代理（托盘自身 127.0.0.1:3080 探测）
	setOutboundProxy("http://127.0.0.1:10808")
	for _, target := range []string{
		"http://127.0.0.1:3080/",
		"http://localhost:3080/?token=x",
		"http://[::1]:3080/",
		"http://192.168.3.186:8899/",
	} {
		if p := proxyFor(t, target); p != nil {
			t.Errorf("%s 应直连，实际走代理 %v", target, p)
		}
	}
	if p := proxyFor(t, "https://api.instantserv.ccwu.cc/v1/me"); p == nil {
		t.Error("外部地址应走显式配置的代理")
	}
}

func TestOutboundProxyFunc_NonHTTPScheme(t *testing.T) {
	setupProxyTest(t)
	setOutboundProxy("http://127.0.0.1:10808")
	for _, target := range []string{"ftp://example.com/x", "ws://example.com/x"} {
		if p := proxyFor(t, target); p != nil {
			t.Errorf("%s 非 http/https，应直连，实际 %v", target, p)
		}
	}
}

func TestOutboundProxyFunc_ExplicitModeWins(t *testing.T) {
	setupProxyTest(t)
	// 显式地址优先于系统代理
	stubSystemProxy(t, systemProxyConfig{HTTP: "9.9.9.9:3128", HTTPS: "9.9.9.9:3128"})
	setOutboundProxy("http://127.0.0.1:10808")
	if p := proxyFor(t, "https://example.com/"); p == nil || p.Host != "127.0.0.1:10808" {
		t.Fatalf("显式配置应优先，得到 %v", p)
	}
	// direct 忽略系统代理与显式地址
	setOutboundProxy(proxyModeDirect)
	if p := proxyFor(t, "https://example.com/"); p != nil {
		t.Errorf("direct 模式应直连，得到 %v", p)
	}
}

func TestOutboundProxyFunc_SystemProxyUsedInAuto(t *testing.T) {
	setupProxyTest(t)
	stubSystemProxy(t, systemProxyConfig{
		HTTP:   "127.0.0.1:10808",
		HTTPS:  "127.0.0.1:10808",
		Bypass: []string{"internal.corp"},
	})
	setOutboundProxy(proxyModeAuto)

	if p := proxyFor(t, "https://api.instantserv.ccwu.cc/v1/me"); p == nil || p.Host != "127.0.0.1:10808" {
		t.Errorf("auto 模式应使用系统代理，得到 %v", p)
	}
	if p := proxyFor(t, "https://internal.corp/x"); p != nil {
		t.Errorf("命中绕过清单的主机应直连，得到 %v", p)
	}
}

func TestOutboundProxyFunc_EnvUsedInAuto(t *testing.T) {
	setupProxyTest(t)
	t.Setenv("HTTPS_PROXY", "http://env-proxy.test:8080")
	setOutboundProxy(proxyModeAuto)
	p := proxyFor(t, "https://example.com/")
	if p == nil || p.Host != "env-proxy.test:8080" {
		t.Fatalf("auto 模式应回退环境变量代理，得到 %v", p)
	}
}

func TestOutboundProxyFunc_DirectWhenNothingConfigured(t *testing.T) {
	setupProxyTest(t)
	setOutboundProxy(proxyModeAuto)
	if p := proxyFor(t, "https://example.com/"); p != nil {
		t.Errorf("无任何代理配置时应直连，得到 %v", p)
	}
}

func TestParseProxyEndpointRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "   ", "http://"} {
		if u, err := parseProxyEndpoint(raw); err == nil {
			t.Errorf("parseProxyEndpoint(%q) 应报错，得到 %v", raw, u)
		}
	}
	u, err := parseProxyEndpoint("127.0.0.1:10808")
	if err != nil || u.Scheme != "http" || u.Host != "127.0.0.1:10808" {
		t.Errorf("parseProxyEndpoint 补 http:// 失败：%v %v", u, err)
	}
}

// TestNewHTTPClientUsesOutboundTransport 保证新建客户端用的是同一套代理解析，
// 而不是 std 默认（后者不读系统代理——现场问题的根因）。
func TestNewHTTPClientUsesOutboundTransport(t *testing.T) {
	setupProxyTest(t)
	c := newHTTPClient(0)
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("newHTTPClient 应返回带 Transport 的客户端，得到 %T", c.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("Transport.Proxy 未装配，出网请求不会走代理解析")
	}
	if http.DefaultClient.Transport == nil {
		t.Fatal("DefaultClient.Transport 未装配")
	}
	if dt, ok := http.DefaultClient.Transport.(*http.Transport); !ok || dt.Proxy == nil {
		t.Fatal("DefaultTransport.Proxy 未装配（plugin_update/platform_windows 的默认客户端会直连）")
	}
}

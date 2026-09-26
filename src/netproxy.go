// netproxy.go：出网客户端统一的代理解析。
//
// 为什么需要它（2026-09-26 现场）：
//
//	api.instantserv.ccwu.cc 的直连路径被网络层阻断（TCP 能连、随后被 RST：wsarecv
//	"An existing connection was forcibly closed by the remote host"），而 dsh-systray 是
//	Go 程序——Go 的 http.ProxyFromEnvironment 只读 HTTP_PROXY/HTTPS_PROXY/NO_PROXY
//	环境变量，**不读 Windows 的「系统代理」设置**（注册表 Internet Settings）。
//	于是浏览器（走系统代理 127.0.0.1:10808 / xray）一切正常，托盘却全部出网失败：
//	账号同步/登录报「网络连接失败」、GitHub 下载与更新检查走直连。
//
// 本文件的职责：给所有出网客户端一个统一的代理判定——显式配置 > 环境变量 > 系统代理，
// 并把「本机回环不走代理」做成硬约束（托盘自己托管 127.0.0.1:3080 的 Web 服务，
// 健康探测/令牌校验若被代理转发会直接误判服务不可用）。
//
// 生效范围：DefaultTransport（http.DefaultClient、downloadClient 的 Clone 源、
// plugin_update.go 的裸 client）与 newHTTPClient() 构造的客户端。
package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// 代理模式（config.json 的 proxy 字段 / DSH_SYSTRAY_PROXY）。
const (
	// proxyModeAuto 默认：显式配置 > 环境变量 > 系统代理（Windows）> 直连，逐 URL 判定。
	proxyModeAuto = "auto"
	// proxyModeDirect 完全直连（忽略环境变量与系统代理）。
	proxyModeDirect = "direct"
	// 其余取值按代理地址解析（如 http://127.0.0.1:10808 或 socks5://127.0.0.1:10808）。
)

// systemProxy 读取操作系统代理设置（Windows 为注册表 Internet Settings）。
//
// 变量而非常量函数：单测需要替换它，避免测试结果依赖运行机器的系统代理
// （本机开着 v2rayN 系统代理，若真实读取会让「未配置」用例意外命中代理）。
var systemProxy = readSystemProxy

var (
	outboundProxyMu sync.RWMutex
	// proxyModeOverride 运行时生效的代理模式（config.json / 环境变量，见 applyProxyConfig）。
	proxyModeOverride = ""
	// proxyConfigValue config.json 里 proxy 字段的原值：与生效值分开保存，避免环境变量
	// 覆盖生效后又被写回配置文件（回写会污染用户配置）。
	proxyConfigValue = ""
)

// setOutboundProxy 设置生效代理模式（空值与非法值归一为 auto）。
func setOutboundProxy(v string) {
	outboundProxyMu.Lock()
	proxyModeOverride = normalizeProxyMode(v)
	outboundProxyMu.Unlock()
}

// setProxyConfigValue 记录 config.json 的 proxy 原值（保存配置时回写用）。
func setProxyConfigValue(v string) {
	outboundProxyMu.Lock()
	proxyConfigValue = v
	outboundProxyMu.Unlock()
}

// outboundProxyValue 当前生效的代理模式（代理判定与日志用）。
func outboundProxyValue() string {
	outboundProxyMu.RLock()
	defer outboundProxyMu.RUnlock()
	return proxyModeOverride
}

// proxyConfigValueOf config.json 的 proxy 原值（保存配置用）。
func proxyConfigValueOf() string {
	outboundProxyMu.RLock()
	defer outboundProxyMu.RUnlock()
	return proxyConfigValue
}

// normalizeProxyMode 归一化代理模式取值：空/未知 → auto；direct → direct；
// 其余（含代理地址）保留小写原值，地址是否合法由 proxyURLFromMode 判定。
func normalizeProxyMode(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "", "auto", "env", "system":
		return proxyModeAuto
	case "direct", "none", "off", "false":
		return proxyModeDirect
	}
	return v
}

// proxyURLFromMode 把显式代理地址解析成 *url.URL；非地址返回 nil（交给自动探测）。
func proxyURLFromMode(mode string) *url.URL {
	if mode == "" || mode == proxyModeAuto || mode == proxyModeDirect {
		return nil
	}
	raw := mode
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw // 允许写 127.0.0.1:10808
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return u
	}
	return nil
}

// outboundProxyFunc 返回 DefaultTransport 级代理判定函数（见包注释「生效范围」）。
// nil 表示「不经过代理」。
func outboundProxyFunc() func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if req == nil || req.URL == nil {
			return nil, nil
		}
		// 只代理 http/https；回环与本机/私网地址硬性直连（托盘自身服务的健康探测与
		// 令牌校验走 127.0.0.1:3080，被代理转发会误判服务不可用）。
		switch strings.ToLower(req.URL.Scheme) {
		case "http", "https":
		default:
			return nil, nil
		}
		if bypassProxyHost(req.URL.Hostname()) {
			return nil, nil
		}
		mode := outboundProxyValue()
		if u := proxyURLFromMode(mode); u != nil {
			return u, nil // 显式配置：对全部外部地址生效（不套用绕过清单）
		}
		if mode == proxyModeDirect {
			return nil, nil
		}
		if u, ok := envProxyForURL(req.URL); ok {
			return u, nil
		}
		if u, ok := systemProxyForURL(req.URL); ok {
			return u, nil
		}
		return nil, nil
	}
}

// envProxyForURL 解析 HTTP_PROXY/HTTPS_PROXY/NO_PROXY 环境变量；ok=false 表示不走代理。
//
// 有意不用 http.ProxyFromEnvironment：它把环境快照缓存在 sync.Once 里，首个请求之后再改
// 环境变量（含 setx 后重启前的进程、以及单测注入）都不会重新读取；而且它是全局的，
// 环境变量会盖过本程序的显式配置。这里自行解析以获得确定、可测、可覆盖的语义。
//
// http_proxy（小写）只在 CGI 场景被忽略，本程序是桌面托盘，不适用该规则。
func envProxyForURL(u *url.URL) (*url.URL, bool) {
	httpsRaw := firstNonEmptyEnv("HTTPS_PROXY", "https_proxy")
	httpRaw := firstNonEmptyEnv("HTTP_PROXY", "http_proxy")
	noProxy := firstNonEmptyEnv("NO_PROXY", "no_proxy")
	if noProxyMatches(parseBypassList(noProxy), u.Host) {
		return nil, false
	}
	raw := httpsRaw
	if strings.EqualFold(u.Scheme, "http") {
		raw = httpRaw
		if raw == "" {
			raw = httpsRaw // 仅配了 HTTPS_PROXY 时也用于 http，与多数工具一致
		}
	}
	if raw == "" {
		return nil, false
	}
	pu, err := parseProxyEndpoint(raw)
	if err != nil {
		return nil, false
	}
	return pu, true
}

// firstNonEmptyEnv 返回首个非空的同名环境变量取值。
func firstNonEmptyEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// systemProxyForURL 用「系统代理」设置解析目标 URL；ok=false 表示不走代理。
func systemProxyForURL(u *url.URL) (*url.URL, bool) {
	cfg, ok := systemProxy()
	if !ok {
		return nil, false
	}
	if noProxyMatches(cfg.Bypass, u.Host) {
		return nil, false
	}
	raw := cfg.HTTPS
	if strings.EqualFold(u.Scheme, "http") {
		raw = cfg.HTTP
		if raw == "" {
			raw = cfg.HTTPS
		}
	}
	pu, err := parseProxyEndpoint(raw)
	if err != nil {
		return nil, false
	}
	return pu, true
}

// systemProxyConfig 系统代理设置（WinINET 形态）。
type systemProxyConfig struct {
	HTTP   string   // http 协议代理（含端口）；空 = 无
	HTTPS  string   // https 协议代理（含端口）；空 = 无
	Bypass []string // 绕过清单（ProxyOverride / NO_PROXY 形态）
}

// parseSystemProxy 解析 WinINET 的 ProxyServer / ProxyOverride 取值。
//
//	ProxyServer 两种形态：
//	  host:port                            单一代理，http/https 共用
//	  http=h1:80;https=h2:443;socks=s:1080 按协议分列
//	ProxyOverride 以分号分隔，支持 * 通配、<local> 与裸主机名（含子域）。
//
// enabled 为 false 或解析不出任何代理时 ok=false。
func parseSystemProxy(enabled bool, proxyServer, proxyOverride string) (systemProxyConfig, bool) {
	var cfg systemProxyConfig
	if !enabled {
		return cfg, false
	}
	if cfg.Bypass = parseBypassList(proxyOverride); len(cfg.Bypass) == 0 && strings.TrimSpace(proxyOverride) != "" {
		cfg.Bypass = splitBypass(proxyOverride)
	}
	raw := strings.TrimSpace(proxyServer)
	if raw == "" {
		return cfg, false
	}
	if !strings.Contains(raw, "=") {
		host := normalizeProxyEndpoint(raw)
		cfg.HTTP, cfg.HTTPS = host, host
		return cfg, true
	}
	for _, part := range strings.Split(raw, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		v = normalizeProxyEndpoint(v)
		if v == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "http":
			cfg.HTTP = v
		case "https":
			cfg.HTTPS = v
		case "socks", "socks5":
			// 仅在没有 http/https 代理时兜底：Go 的 Transport 支持 socks5 代理，
			// 但对 http 目标同样可用，故填到空槽位。
			if cfg.HTTP == "" {
				cfg.HTTP = "socks5://" + v
			}
			if cfg.HTTPS == "" {
				cfg.HTTPS = "socks5://" + v
			}
		}
	}
	if cfg.HTTP == "" && cfg.HTTPS == "" {
		return cfg, false
	}
	return cfg, true
}

// parseProxyEndpoint 把 "host:port" / "socks5://host:port" 解析成代理 URL。
// 缺少 scheme 时补 http://。
func parseProxyEndpoint(raw string) (*url.URL, error) {
	raw = normalizeProxyEndpoint(raw)
	if raw == "" {
		return nil, fmt.Errorf("空代理地址")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("非法代理地址: %q", raw)
	}
	return u, nil
}

// normalizeProxyEndpoint 清理系统代理取值里的引号与空白。
func normalizeProxyEndpoint(raw string) string {
	return strings.Trim(strings.TrimSpace(raw), `"'`)
}

// parseBypassList 解析绕过清单，只保留可判定的条目（丢弃 <local> 这类语义标记，
// 本机/私网地址已由 bypassProxyHost 硬性直连）。
func parseBypassList(raw string) []string {
	items := splitBypass(raw)
	out := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.Trim(strings.TrimSpace(it), `"'`)
		if it == "" || strings.HasPrefix(it, "<") {
			continue // <local> 等语义标记：不做字符串比对
		}
		out = append(out, it)
	}
	return out
}

func splitBypass(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == ',' })
}

// noProxyMatches 判断 host（可带端口）是否命中绕过清单。
//
// 匹配规则（NO_PROXY 惯例）：条目可带端口（此时须与 host 的端口一致）、
// 精确主机名、或 .example.com / example.com 形态的子域后缀。
func noProxyMatches(list []string, host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	bare := h
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i+1:], "]") {
		bare = h[:i]
	}
	bare = strings.Trim(bare, "[]")
	for _, item := range list {
		pat := strings.ToLower(strings.Trim(strings.TrimSpace(item), `"'`))
		if pat == "" {
			continue
		}
		if pat == "*" {
			return true
		}
		if strings.Contains(pat, ":") { // 带端口：须整串一致
			if pat == h {
				return true
			}
			continue
		}
		pat = strings.TrimPrefix(pat, "*.") // Windows ProxyOverride 通配前缀：*.example.com
		pat = strings.TrimPrefix(pat, ".")
		if bare == pat || strings.HasSuffix(bare, "."+pat) {
			return true
		}
	}
	return false
}

// bypassProxyHost 本机 / 局域网地址永不走代理。
//
// 与浏览器一致（Chrome/Edge 默认绕过 localhost 与私网）：既保证托盘对自身
// 127.0.0.1:3080 服务的健康探测不被代理转发，也避免把局域网 Harness 服务
// 误送到外部代理。
func bypassProxyHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return true
	}
	h = strings.Trim(h, "[]")
	switch {
	case h == "localhost", strings.HasSuffix(h, ".localhost"):
		return true
	case h == "::1", h == "0.0.0.0":
		return true
	case h == "127.0.0.1", strings.HasPrefix(h, "127."):
		return true
	}
	ip := parseIPv4(h)
	if ip == nil {
		return false
	}
	switch {
	case ip[0] == 10:
		return true
	case ip[0] == 192 && ip[1] == 168:
		return true
	case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31:
		return true
	case ip[0] == 169 && ip[1] == 254: // link-local
		return true
	}
	return false
}

// parseIPv4 极简点分十进制解析（只用于私网判定，不接受域名）。
func parseIPv4(h string) []byte {
	parts := strings.Split(h, ".")
	if len(parts) != 4 {
		return nil
	}
	out := make([]byte, 4)
	for i, p := range parts {
		if p == "" || len(p) > 3 {
			return nil
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return nil
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return nil
		}
		out[i] = byte(n)
	}
	return out
}

// setupOutboundTransport 把代理判定挂到 http.DefaultTransport，并让
// http.DefaultClient 复用同一个 Transport。
//
// 必要性：plugin_update.go 的 probeNpmRegistry / gh 资产下载、platform_windows.go 的
// downloadFile 直接用 http.DefaultClient；updater.go 的 downloadClient 以
// http.DefaultTransport.Clone() 为模板——初始化时改一次即可全覆盖。
func setupOutboundTransport() {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return
	}
	tr.Proxy = outboundProxyFunc()
	// DefaultClient 的零值 Transport 字段用的是 DefaultTransport；显式赋值既表明意图，
	// 也便于后续被替换为同一实例。
	http.DefaultClient.Transport = tr
}

// newHTTPClient 构造启用统一代理解析的 HTTP 客户端。timeout<=0 表示不设整体超时。
// 所有直接 new(http.Client) 的出网路径都应改用本函数。
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: outboundTransport()}
}

// outboundTransport 返回启用统一代理解析的 Transport（需要自定义参数时用它 Clone）。
func outboundTransport() *http.Transport {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{Proxy: outboundProxyFunc()}
	}
	return tr
}

// initDefaultTransport 在**包级变量初始化阶段**装配代理解析。用变量而非 init() 是为了确定
// 顺序：同一包内包级变量先于所有 init() 完成初始化，确保 downloadClient（本包另一个包级
// 变量，以 outboundTransport().Clone() 构造）拿到的一定是已装上 Proxy 的 Transport。
// 读取 http.DefaultTransport 是安全的：它由 net/http 自己的包级变量初始化，
// 而依赖包的初始化必定早于本包。
var initDefaultTransport = func() bool {
	setupOutboundTransport()
	return true
}()

// applyProxyConfig 应用 config.json 的 proxy 配置（启动早期调用，先于任何出网请求）。
// 优先级：DSH_SYSTRAY_PROXY 环境变量 > config.json 的 proxy（均空 = auto）。
func applyProxyConfig(cfgValue string) {
	v := strings.TrimSpace(cfgValue)
	setProxyConfigValue(v)
	if env := strings.TrimSpace(envProxySetting()); env != "" {
		v = env
	}
	setOutboundProxy(v)
	logProxyResolution()
}

// logProxyResolution 记录一次代理判定结论，便于排障（现场问题几乎都从这句开始看）。
func logProxyResolution() {
	mode := outboundProxyValue()
	detail := "直连（未配置代理）"
	switch {
	case mode == proxyModeDirect:
		detail = "直连（配置为 direct）"
	case mode == proxyModeAuto:
		if cfg, ok := systemProxy(); ok {
			detail = fmt.Sprintf("自动：检测到系统代理 http=%q https=%q 绕过=%v", cfg.HTTP, cfg.HTTPS, cfg.Bypass)
		} else {
			detail = "自动：未检测到系统代理（回退环境变量 HTTP_PROXY/HTTPS_PROXY）"
		}
	default:
		if u := proxyURLFromMode(mode); u != nil {
			detail = "显式代理 " + u.String()
		} else {
			detail = fmt.Sprintf("配置项 %q 不是合法代理地址，按自动处理", mode)
		}
	}
	if env := proxyEnvHint(); env != "" {
		detail += fmt.Sprintf("；进程可见代理环境变量: %s", env)
	}
	log.Printf("[proxy] 出网代理模式=%s → %s", mode, detail)
}

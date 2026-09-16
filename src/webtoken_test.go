package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 统一日志中的两行访问链接（旧 token / 新 token，新行同条含 LAN 变体）。
const (
	tokenLineOld = "2026/09/05 09:00:00 [INFO] [server] dsh web: http://127.0.0.1:3080/?token=OLDTOKEN\n"
	tokenLineNew = "2026/09/05 09:00:05 [INFO] [server] dsh web: http://127.0.0.1:3080/?token=NEWTOKEN (LAN: http://192.168.1.5:3080/?token=NEWTOKEN)\n"
)

// writeTokenLog 在临时 logDir 下写入统一日志内容并返回日志路径。
func writeTokenLog(t *testing.T, content string) string {
	t.Helper()
	useTempLogDir(t)
	p := unifiedLogPath()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return p
}

// 取最新一条：旧行在前、新行在后 → 返回新行主链接（LAN 变体不误取）。
func TestFindLatestTokenURLTakesLast(t *testing.T) {
	p := writeTokenLog(t, tokenLineOld+tokenLineNew)
	got, ok := findLatestTokenURL(p, 0)
	if !ok || got != "http://127.0.0.1:3080/?token=NEWTOKEN" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

// after 偏移只解析新增部分：偏移之后的日志没有链接时不返回旧行（启动定点捕获不误用旧 token）。
func TestFindLatestTokenURLAfterOffset(t *testing.T) {
	p := writeTokenLog(t, tokenLineOld+tokenLineNew)
	if got, ok := findLatestTokenURL(p, int64(len(tokenLineOld))); !ok || strings.Contains(got, "OLDTOKEN") {
		t.Fatalf("offset scan got %q ok=%v", got, ok)
	}
	if u, ok := findLatestTokenURL(p, int64(len(tokenLineOld)+len(tokenLineNew))); ok {
		t.Fatalf("expected no match past end, got %q", u)
	}
}

// 日志被轮转/截断（长度小于偏移）→ 从头再扫。
func TestFindLatestTokenURLRotationFallback(t *testing.T) {
	p := writeTokenLog(t, tokenLineNew)
	if got, ok := findLatestTokenURL(p, 1<<20); !ok || !strings.Contains(got, "NEWTOKEN") {
		t.Fatalf("rotation fallback got %q ok=%v", got, ok)
	}
}

// 文件不存在 / 无访问链接行 / 空文件 → 读不到。
func TestFindLatestTokenURLMisses(t *testing.T) {
	useTempLogDir(t)
	if _, ok := findLatestTokenURL(unifiedLogPath(), 0); ok {
		t.Fatal("missing file should not match")
	}
	if _, ok := findLatestTokenURL(writeTokenLog(t, "2026/09/05 09:00:00 [INFO] [app] 启动完成\n"), 0); ok {
		t.Fatal("log without dsh web line should not match")
	}
	if _, ok := findLatestTokenURL(writeTokenLog(t, ""), 0); ok {
		t.Fatal("empty file should not match")
	}
}

// webTokenURL/webTokenFound：内存缓存优先、日志扫描兜底、都没有时回退基础地址。
func TestWebTokenCacheThenLogFallback(t *testing.T) {
	t.Cleanup(func() { setServerTokenURL("") })
	setServerTokenURL("")
	writeTokenLog(t, tokenLineNew)
	if !webTokenFound() {
		t.Fatal("log line should count as found")
	}
	if got := webTokenURL(); !strings.Contains(got, "NEWTOKEN") {
		t.Fatalf("log fallback got %q", got)
	}
	setServerTokenURL("http://127.0.0.1:3080/?token=CACHED")
	if got := webTokenURL(); got != "http://127.0.0.1:3080/?token=CACHED" {
		t.Fatalf("cache should win, got %q", got)
	}
	setServerTokenURL("")
	useTempLogDir(t) // 换空目录：缓存与日志都没有
	prevURL := webURL
	webURL = "http://127.0.0.1:3080/"
	t.Cleanup(func() { webURL = prevURL })
	if webTokenFound() {
		t.Fatal("no cache/log should report not found")
	}
	if got := webTokenURL(); got != webURL {
		t.Fatalf("expected base URL fallback, got %q", got)
	}
}

// WebTokenURL 绑定：截图模式返回脱敏演示链接；非截图模式无令牌链接时返回空串（前端禁用复制）。
func TestWebTokenURLBinding(t *testing.T) {
	t.Cleanup(func() { setServerTokenURL("") })
	setServerTokenURL("")
	useTempLogDir(t)
	prevShot, prevPort := shotMode, port
	t.Cleanup(func() { shotMode, port = prevShot, prevPort })
	port = 3080
	shotMode = false
	if got := (&App{}).WebTokenURL(); got != "" {
		t.Fatalf("no token should return empty, got %q", got)
	}
	shotMode = true
	if !webTokenFound() {
		t.Fatal("shot mode should report found")
	}
	if got := (&App{}).WebTokenURL(); got != "http://127.0.0.1:3080/?token=demo" {
		t.Fatalf("shot mode binding got %q", got)
	}
}

// useTempTokenFile 把访问链接缓存文件指向临时目录（避免写进真实配置目录）。
func useTempTokenFile(t *testing.T) string {
	t.Helper()
	prev := webTokenStateFile
	webTokenStateFile = filepath.Join(t.TempDir(), "web-token.json")
	t.Cleanup(func() { webTokenStateFile = prev })
	return webTokenStateFile
}

// 缓存文件读写：端口一致才复用；端口已改视为失效并清除。
func TestPersistedTokenURLPortGuard(t *testing.T) {
	p := useTempTokenFile(t)
	useTempLogDir(t)
	prevPort, prevStarted := port, serverStartedPort
	t.Cleanup(func() { port, serverStartedPort = prevPort, prevStarted })
	port, serverStartedPort = 3080, 0

	persistTokenURL("http://127.0.0.1:3080/?token=KEEP")
	if got := persistedTokenURL(); !strings.Contains(got, "KEEP") {
		t.Fatalf("persisted url got %q", got)
	}
	port = 3099 // 端口已改：旧链接指向别的端口
	if got := persistedTokenURL(); got != "" {
		t.Fatalf("port change should invalidate, got %q", got)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("stale cache file should be removed, err=%v", err)
	}
}

// clearPersistedTokenURLIf 只清除同一链接：并发写入的新链接不能被旧校验结果清掉。
func TestClearPersistedTokenURLIf(t *testing.T) {
	useTempTokenFile(t)
	useTempLogDir(t)
	prevPort := port
	t.Cleanup(func() { port = prevPort })
	port = 3080

	persistTokenURL("http://127.0.0.1:3080/?token=NEW")
	clearPersistedTokenURLIf("http://127.0.0.1:3080/?token=OLD")
	if got := persistedTokenURL(); !strings.Contains(got, "NEW") {
		t.Fatalf("different url must not clear cache, got %q", got)
	}
	clearPersistedTokenURLIf("http://127.0.0.1:3080/?token=NEW")
	if got := persistedTokenURL(); got != "" {
		t.Fatalf("same url should be cleared, got %q", got)
	}
}

// verifyTokenURL：令牌匹配 303 → 有效；401 → 明确失效；5xx / 连接失败 → 无法判定。
func TestVerifyTokenURL(t *testing.T) {
	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") == "good" {
			w.Header().Set("location", "/")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer valid.Close()
	if v := verifyTokenURL(valid.URL + "/?token=good"); v != tokenURLValid {
		t.Fatalf("valid token verdict=%v", v)
	}
	if v := verifyTokenURL(valid.URL + "/?token=old"); v != tokenURLStale {
		t.Fatalf("stale token verdict=%v", v)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	broken.Close() // 连接失败：无法判定（不得据此清除缓存）
	if v := verifyTokenURL(broken.URL + "/?token=x"); v != tokenURLUnknown {
		t.Fatalf("closed server verdict=%v", v)
	}
}

// adoptPersistedTokenURL：有效则写入内存缓存；明确失效（401）则清除缓存文件。
func TestAdoptPersistedTokenURL(t *testing.T) {
	useTempLogDir(t)
	t.Cleanup(func() { setServerTokenURL("") })
	prevPort := port
	t.Cleanup(func() { port = prevPort })
	port = 3080

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("location", "/")
		w.WriteHeader(http.StatusSeeOther)
	}))
	defer good.Close()
	useTempTokenFile(t)
	setServerTokenURL("")
	persistTokenURL(good.URL + "/?token=good")
	if !adoptPersistedTokenURL() {
		t.Fatal("valid persisted url should be adopted")
	}
	if got := startedTokenURL(); got != good.URL+"/?token=good" {
		t.Fatalf("cache got %q", got)
	}

	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer stale.Close()
	setServerTokenURL("")
	persistTokenURL(stale.URL + "/?token=old")
	if adoptPersistedTokenURL() {
		t.Fatal("stale persisted url must not be adopted")
	}
	if got := persistedTokenURL(); got != "" {
		t.Fatalf("stale cache file should be removed, got %q", got)
	}
}

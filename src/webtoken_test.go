package main

import (
	"os"
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

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// useTempLogDir 把 logDir 指向临时目录并在测试结束后还原（logDir 为包级全局变量）。
func useTempLogDir(t *testing.T) string {
	t.Helper()
	prev := logDir
	logDir = t.TempDir()
	t.Cleanup(func() { logDir = prev })
	return logDir
}

func TestRotateServerLog(t *testing.T) {
	dir := useTempLogDir(t)
	p := filepath.Join(dir, "server.log")

	// 文件不存在 / 为空：不轮转，基线 0
	if got := rotateServerLog(); got != 0 {
		t.Fatalf("empty rotate baseline = %d, want 0", got)
	}
	if _, err := os.Stat(p + ".1"); !os.IsNotExist(err) {
		t.Fatalf("empty log should not be rotated, .1 err=%v", err)
	}

	// 写入旧日志后轮转：归档到 .1，原文件被移走（startServer 会新建）
	if err := os.WriteFile(p, []byte("old-line-1\nold-line-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := rotateServerLog(); got != 0 {
		t.Fatalf("rotate baseline = %d, want 0 (new file starts empty)", got)
	}
	data, err := os.ReadFile(p + ".1")
	if err != nil {
		t.Fatalf("read .1: %v", err)
	}
	if string(data) != "old-line-1\nold-line-2\n" {
		t.Fatalf(".1 content = %q", data)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("server.log should be moved away after rotate, err=%v", err)
	}

	// 再写一轮并轮转：级联 .1→.2，新内容进 .1
	if err := os.WriteFile(p, []byte("new-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rotateServerLog()
	if data, err := os.ReadFile(p + ".2"); err != nil || string(data) != "old-line-1\nold-line-2\n" {
		t.Fatalf(".2 content = %q err=%v", data, err)
	}
	if data, err := os.ReadFile(p + ".1"); err != nil || string(data) != "new-line\n" {
		t.Fatalf(".1 content = %q err=%v", data, err)
	}
}

func TestParseBootLogSuspects(t *testing.T) {
	useTempLogDir(t)
	fixture := `dsh: host preparation failed ...
cannot resolve profile bundle "dsh-plugin-codegraph" from the dsh installation or C:\...\profiles\web; run 'dsh plugin --profile web install'
Error: plugin(s) failed to load: @scope/design-a, dsh-plugin-broken; Cordis startup failed because these plugin(s) could not be resolved (see the error(s) logged above)
profile bundle "restrict-discipline" declares no dsh.bundle in its package.json
dsh-plugin-activate-x: Error: boom at bootstrap
  at file:///.../loader.js:1:1
dsh: 2 plugins did not activate
`
	p := filepath.Join(logDir, "server.log")
	if err := os.WriteFile(p, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	got := parseBootLogSuspects(0)
	want := []string{"@scope/design-a", "dsh-plugin-activate-x", "dsh-plugin-broken", "dsh-plugin-codegraph", "restrict-discipline"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("suspects = %v, want %v", got, want)
	}

	// offset 语义：只扫追加段
	if err := os.WriteFile(p, []byte("cannot resolve profile bundle \"later-plugin\" from ...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 覆盖式写入后 offset=0 扫到的是后者（文件被覆盖，前面内容已无）
	got = parseBootLogSuspects(0)
	want = []string{"later-plugin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("offset-scan suspects = %v, want %v", got, want)
	}

	// 文件不存在：nil
	logDir = t.TempDir()
	if got := parseBootLogSuspects(0); got != nil {
		t.Fatalf("missing file suspects = %v, want nil", got)
	}
}

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
	p := filepath.Join(dir, unifiedLogName)

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
		t.Fatalf("log should be moved away after rotate, err=%v", err)
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

// TestRotateReopensHandle 打开句柄后轮转：轮转成功（mac/POSIX）时基础文件重建为空且后续
// 写入落到新基础文件；轮转失败（Windows 打开文件被锁、保留原文件）时后续写入仍追加原文件。
func TestRotateReopensHandle(t *testing.T) {
	dir := useTempLogDir(t)
	p := filepath.Join(dir, unifiedLogName)
	if !initUnifiedLog() {
		t.Fatal("initUnifiedLog failed")
	}
	t.Cleanup(func() {
		unifiedMu.Lock()
		if unifiedFile != nil {
			_ = unifiedFile.Close()
			unifiedFile = nil
		}
		unifiedMu.Unlock()
	})
	writeUnifiedRaw([]byte("old-line\n"))

	if rotateServerLog() == 0 {
		// 轮转成功（rename 成功，mac/POSIX）：基础文件重建为空，旧内容进 .1
		if data, err := os.ReadFile(p); err != nil || len(data) != 0 {
			t.Fatalf("base log should be recreated empty, data=%q err=%v", data, err)
		}
		if data, err := os.ReadFile(p + ".1"); err != nil || string(data) != "old-line\n" {
			t.Fatalf(".1 content = %q err=%v", data, err)
		}
		// 轮转后的写入落到新基础文件而非 .1
		writeUnifiedRaw([]byte("fresh-line\n"))
		if data, err := os.ReadFile(p); err != nil || string(data) != "fresh-line\n" {
			t.Fatalf("post-rotate write should land in base log, data=%q err=%v", data, err)
		}
		if data, err := os.ReadFile(p + ".1"); err != nil || string(data) != "old-line\n" {
			t.Fatalf(".1 should stay unchanged, data=%q err=%v", data, err)
		}
		return
	}
	// 轮转失败（Windows 打开文件被锁、保留原文件）：后续写入仍追加原基础文件
	writeUnifiedRaw([]byte("fresh-line\n"))
	if data, err := os.ReadFile(p); err != nil || string(data) != "old-line\nfresh-line\n" {
		t.Fatalf("post-rotate write should append to base log, data=%q err=%v", data, err)
	}
}

func TestParseBootLogSuspects(t *testing.T) {
	dir := useTempLogDir(t)
	// 统一日志 fixture：只有 [server] 模块行参与定位；harness 模块的 ERR_PNPM 行必须被忽略
	fixture := `2026/09/05 09:00:00 [INFO] [app] something normal
2026/09/05 09:00:01 [ERROR] [harness] ERR_PNPM_NO_MATCHING_VERSION @deepseek-ai/dsh@0.1.3-alpha.1
2026/09/05 09:00:02 [ERROR] [server] cannot resolve profile bundle "dsh-plugin-codegraph" from the dsh installation or C:\...\profiles\web; run 'dsh plugin --profile web install'
2026/09/05 09:00:03 [ERROR] [server] Error: plugin(s) failed to load: @scope/design-a, dsh-plugin-broken; Cordis startup failed because these plugin(s) could not be resolved (see the error(s) logged above)
2026/09/05 09:00:04 [ERROR] [server] profile bundle "restrict-discipline" declares no dsh.bundle in its package.json
2026/09/05 09:00:05 [ERROR] [server] dsh-plugin-activate-x: Error: boom at bootstrap
2026/09/05 09:00:05 [ERROR] [server] Error: failed to import loader entry new-format-plugin (new-format-plugin): The requested module 'x' does not provide an export named 'y'
2026/09/05 09:00:06 [INFO] [app] service resumed after restore
`
	p := filepath.Join(dir, unifiedLogName)
	if err := os.WriteFile(p, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	got := parseBootLogSuspects(0)
	want := []string{"@scope/design-a", "dsh-plugin-activate-x", "dsh-plugin-broken", "dsh-plugin-codegraph", "new-format-plugin", "restrict-discipline"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("suspects = %v, want %v", got, want)
	}

	// offset 语义：只扫追加段
	appendLine := "2026/09/05 09:00:07 [ERROR] [server] cannot resolve profile bundle \"later-plugin\" from ...\n"
	f2, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.WriteString(appendLine); err != nil {
		t.Fatal(err)
	}
	f2.Close()
	got = parseBootLogSuspects(0)
	want = []string{"@scope/design-a", "dsh-plugin-activate-x", "dsh-plugin-broken", "dsh-plugin-codegraph", "later-plugin", "new-format-plugin", "restrict-discipline"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("offset-scan suspects = %v, want %v", got, want)
	}

	// 文件不存在：nil
	logDir = t.TempDir()
	if got := parseBootLogSuspects(0); got != nil {
		t.Fatalf("missing file suspects = %v, want nil", got)
	}
}

func TestLogLineModule(t *testing.T) {
	if got := logLineModule("2026/09/05 09:00:00 [INFO] [server] boot"); got != "server" {
		t.Fatalf("module = %q, want server", got)
	}
	if got := logLineModule("plain line without prefix"); got != "" {
		t.Fatalf("module = %q, want empty", got)
	}
}

// TestReadLogTailResetOnTruncate 轮转/截断（offset 越过文件末尾）时：Reset=true 且从头
// 重读，前端据此清空视图并重载归档——此前直接跳到末尾会把「轮转后新写入的行」整体跳过
// （mac 每次服务重启都轮转日志，这段现场正是排障最需要的部分）。
func TestReadLogTailResetOnTruncate(t *testing.T) {
	dir := useTempLogDir(t)
	p := filepath.Join(dir, unifiedLogName)
	if err := os.WriteFile(p, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{}
	tail := app.ReadLogTail(unifiedLogName, 0)
	if len(tail.Lines) != 3 || tail.Reset || tail.NextOffset != 6 {
		t.Fatalf("first read wrong: %+v", tail)
	}
	// 模拟轮转：旧文件被 rename，新基础文件从空开始增长（此时旧 offset 越过新文件末尾）
	if err := os.WriteFile(p, []byte("x\ny\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail = app.ReadLogTail(unifiedLogName, 1000)
	if !tail.Reset {
		t.Fatalf("want reset when offset beyond size: %+v", tail)
	}
	if len(tail.Lines) != 2 || tail.Lines[0] != "x" || tail.Lines[1] != "y" {
		t.Fatalf("reset read must re-read from start: %+v", tail)
	}
	if tail.NextOffset != 4 {
		t.Fatalf("nextOffset = %d, want 4", tail.NextOffset)
	}
}

// TestReadLogArchivesMergesOldestFirst 归档按时间从旧到新（.3 → .2 → .1）合并返回，
// 与基础文件尾读拼成完整历史（日志页「完整显示所有已写入内容」）。
func TestReadLogArchivesMergesOldestFirst(t *testing.T) {
	dir := useTempLogDir(t)
	base := filepath.Join(dir, unifiedLogName)
	if err := os.WriteFile(base+".3", []byte("old3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".2", []byte("old2-a\nold2-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".1", []byte("old1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{}
	tail := app.ReadLogArchives()
	want := []string{"old3", "old2-a", "old2-b", "old1"}
	if !reflect.DeepEqual(tail.Lines, want) {
		t.Fatalf("archive merge = %v, want %v", tail.Lines, want)
	}
}

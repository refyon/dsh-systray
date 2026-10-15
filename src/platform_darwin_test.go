//go:build darwin

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenFailureTranslatesMissingHandler 本机没有应用认领该文件类型时（LaunchServices 原文
// 是英文的 kLSApplicationNotFoundErr），要给用户可照着做的中文提示，而不是把系统原文丢出来。
func TestOpenFailureTranslatesMissingHandler(t *testing.T) {
	raw := `No application knows how to open URL file:///tmp/a.md (Error Domain=NSOSStatusErrorDomain ` +
		`Code=-10814 "kLSApplicationNotFoundErr: E.g. no application claims the file" UserInfo={_LSLine=1796})`
	err := openFailure(raw, errors.New("exit status 1"))
	if err == nil || !strings.Contains(err.Error(), "选择打开方式") {
		t.Fatalf("缺少可操作提示：%v", err)
	}
	if strings.Contains(err.Error(), "NSOSStatusErrorDomain") {
		t.Fatalf("不应把系统原文丢给用户：%v", err)
	}
}

// TestOpenFailureKeepsOtherErrors 其它失败原因原样保留（便于定位），不吞成统一文案。
func TestOpenFailureKeepsOtherErrors(t *testing.T) {
	got := openFailure("open: something else went wrong\n", errors.New("exit status 1"))
	if got == nil || !strings.Contains(got.Error(), "something else went wrong") {
		t.Fatalf("应保留原始原因：%v", got)
	}
	if got := openFailure("", errors.New("exit status 1")); got == nil {
		t.Fatal("无 stderr 时也要把 err 带出去")
	}
}

// TestAsQuoteEscapesQuotes 文案里出现引号/反斜杠时不能破坏 AppleScript 脚本。
func TestAsQuoteEscapesQuotes(t *testing.T) {
	if got := asQuote(`a"b\c`); got != `"a\"b\\c"` {
		t.Fatalf("转义不正确：%s", got)
	}
}

// TestOpenFileReportsLaunchServicesFailure 回归用例（2026-10-15 现场）：本机没有应用认领该类型时
// openFile 必须返回错误——旧实现只 Start 不 Wait，一律返回 nil，界面因此既不报错也不给回退入口，
// 用户看到的是「点击/双击毫无反应」。
func TestOpenFileReportsLaunchServicesFailure(t *testing.T) {
	if _, err := os.Stat("/usr/bin/open"); err != nil {
		t.Skip("本机没有 /usr/bin/open")
	}
	p := filepath.Join(t.TempDir(), "probe.dsh-no-handler")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := openFile(p)
	if err == nil {
		t.Fatal("无应用认领该类型时应返回错误（旧实现此处返回 nil）")
	}
	t.Logf("openFile 返回：%v", err)
}

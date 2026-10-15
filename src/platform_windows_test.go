//go:build windows

package main

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// TestShellExecuteFailureNoAssoc 本机没有能打开该类型的应用时（SE_ERR_NOASSOC 31 /
// SE_ERR_ASSOCINCOMPLETE 27），要给与 macOS 同一句可操作提示，而不是把返回码丢给用户
// ——界面据此弹层引导「选择打开方式」（2026-10-15 现场）。
func TestShellExecuteFailureNoAssoc(t *testing.T) {
	for _, r := range []uintptr{31, 27} {
		err := shellExecuteFailure(r, syscall.Errno(0))
		if err == nil || !strings.Contains(err.Error(), "选择打开方式") {
			t.Fatalf("返回码 %d 应给出可操作提示，实际：%v", r, err)
		}
		if strings.Contains(err.Error(), "ShellExecuteW") {
			t.Fatalf("返回码 %d 不应把系统调用名丢给用户：%v", r, err)
		}
	}
}

// TestShellExecuteFailureCanceled 用户在「打开方式」对话框里取消不是失败（界面不报错）。
func TestShellExecuteFailureCanceled(t *testing.T) {
	if err := shellExecuteFailure(0, syscall.Errno(1223)); !errors.Is(err, errOpenCanceled) {
		t.Fatalf("取消应返回 errOpenCanceled，实际：%v", err)
	}
}

// TestShellExecuteFailureKeepsOthers 其它失败保留原始原因与返回码，便于定位。
func TestShellExecuteFailureKeepsOthers(t *testing.T) {
	if got := shellExecuteFailure(5, syscall.Errno(0)); got == nil || !strings.Contains(got.Error(), "5") {
		t.Fatalf("应保留返回码：%v", got)
	}
	if got := shellExecuteFailure(0, syscall.Errno(2)); got == nil || !strings.Contains(got.Error(), "ShellExecuteW 失败") {
		t.Fatalf("应保留系统错误：%v", got)
	}
}

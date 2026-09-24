//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// parentDirOf 返回目录的父目录（空串表示无父目录）。
func parentDirOf(dir string) string {
	p := filepath.Dir(dir)
	if p == dir || p == "." {
		return ""
	}
	return p
}

// ensureLowIntegrityWritable 给目录（及其父目录）打上「低完整性可写」标签：
//
//	Mandatory Label\Low Mandatory Level:(OI)(CI)(NW)
//
// 为什么需要：Windows 默认只给 %TEMP% 打这个标签，%APPDATA% 不带。当 dsh-systray 由受限/
// 低完整性上下文启动时，它能在 %TEMP% 建文件、却在 %APPDATA% 被拒，于是启动日志被整体
// 回退到 Temp，日志页（读主目录）看不到本次启动——表现为"日志时有时无"（2026-09-24 现场）。
// 打上标签后低完整性进程也能在主目录落盘，日志始终待在用户期望的位置。
//
// (NW)=NO_WRITE_UP：低完整性可写；中/高完整性不受影响。仅对 dsh-systray 自己的日志目录生效。
// 幂等：已带标签时 icacls 重复设置无副作用。失败返回错误由调用方决定是否忽略。
func ensureLowIntegrityWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, target := range []string{dir, parentDirOf(dir)} {
		if target == "" {
			continue
		}
		cmd := exec.Command("icacls", target, "/setintegritylevel", "(OI)(CI)L")
		if err := cmd.Run(); err != nil {
			return err
		}
	}
	return nil
}

// logWritableByCurrentToken 追加方式试开日志文件，返回错误表示当前令牌不可写。
func logWritableByCurrentToken(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

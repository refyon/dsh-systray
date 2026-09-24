//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

// ensureLowIntegrityWritable 非 Windows 平台无「完整性级别」概念：确保目录存在即可
// （POSIX 下由 chmod/umask 决定可写性）。
func ensureLowIntegrityWritable(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// logWritableByCurrentToken 追加方式试开日志文件，返回错误表示当前进程不可写。
func logWritableByCurrentToken(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// parentDirOf 返回目录的父目录（空串表示无父目录）。
func parentDirOf(dir string) string {
	p := filepath.Dir(dir)
	if p == dir || p == "." {
		return ""
	}
	return p
}

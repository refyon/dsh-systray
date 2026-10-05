//go:build windows

// reparse_windows.go：Windows 下的重解析点判定（junction / 符号链接 / 挂载点）。
//
// 为什么不能只看 os.ModeSymlink：**目录 junction 不会被 DirEntry.Type() 标记为 ModeSymlink**
// （目标悬空时更是如此），只有读文件属性里的 FILE_ATTRIBUTE_REPARSE_POINT 才靠得住。
// pnpm 的 node_modules 链接正是 junction —— 2026-10-05 现场：悬空链接导致「读取失败」，
// 整个条目被判为同步失败。
package main

import (
	"os"
	"syscall"
)

// isReparsePoint 该文件/目录是否为重解析点。
func isReparsePoint(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	d, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	return d.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

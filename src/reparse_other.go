//go:build !windows

// reparse_other.go：非 Windows 平台的重解析点判定（符号链接）。
package main

import "os"

// isReparsePoint 该文件/目录是否为符号链接。
func isReparsePoint(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSymlink != 0
}

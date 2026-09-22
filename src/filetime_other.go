//go:build !windows

// filetime_other.go：非 Windows 平台没有可靠的「创建时间」（部分文件系统返回零值），
// 由调用方退回修改时间。
package main

import "os"

// creationTimeOf 该平台不使用创建时间，恒返回 0。
func creationTimeOf(os.FileInfo) int64 { return 0 }

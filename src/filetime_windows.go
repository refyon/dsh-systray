//go:build windows

// filetime_windows.go：Windows 下的文件创建时间（插件对账判断「本机装得比账号记录更晚」要用它）。
package main

import (
	"os"
	"syscall"
	"time"
)

// creationTimeOf 文件创建时间（Unix 秒）；读不到返回 0。
func creationTimeOf(st os.FileInfo) int64 {
	d, ok := st.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return 0
	}
	ns := d.CreationTime.Nanoseconds() // Win32 FILETIME 已相对 Unix 纪元换算
	if ns <= 0 {
		return 0
	}
	return time.Unix(0, ns).Unix()
}

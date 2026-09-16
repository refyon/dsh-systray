//go:build darwin

package main

import (
	"log"
	"os/exec"
	"strings"
)

// copyToClipboard 把文本写进 macOS 剪贴板（pbcopy 从管道读取，无窗口）。
// 失败仅记日志：授权码同时显示在窗口里，用户可手动复制。
func copyToClipboard(text string) {
	if text == "" {
		return
	}
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		log.Printf("clipboard: pbcopy failed: %v", err)
	}
}

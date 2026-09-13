//go:build windows

package main

import (
	"log"
	"syscall"
	"unsafe"
)

var (
	modUser32Clip       = syscall.NewLazyDLL("user32.dll")
	modKernel32Clip     = syscall.NewLazyDLL("kernel32.dll")
	pOpenClipboard      = modUser32Clip.NewProc("OpenClipboard")
	pCloseClipboard     = modUser32Clip.NewProc("CloseClipboard")
	pEmptyClipboard     = modUser32Clip.NewProc("EmptyClipboard")
	pSetClipboardData   = modUser32Clip.NewProc("SetClipboardData")
	pGlobalAlloc        = modKernel32Clip.NewProc("GlobalAlloc")
	pGlobalLock         = modKernel32Clip.NewProc("GlobalLock")
	pGlobalUnlock       = modKernel32Clip.NewProc("GlobalUnlock")
	pGlobalFree         = modKernel32Clip.NewProc("GlobalFree")
	pRtlMoveMemory      = modKernel32Clip.NewProc("RtlMoveMemory")
	clipboardTextFormat = uintptr(13) // CF_UNICODETEXT
)

// copyToClipboard 把文本写进 Windows 剪贴板（Win32 API，不 spawn 子进程，因此不会有
// 一闪而过的控制台窗口）。失败仅记日志：授权码同时显示在窗口里，用户可手动复制。
func copyToClipboard(text string) {
	if text == "" {
		return
	}
	utf16, err := syscall.UTF16FromString(text)
	if err != nil {
		log.Printf("clipboard: encode failed: %v", err)
		return
	}
	if r, _, _ := pOpenClipboard.Call(0); r == 0 {
		log.Printf("clipboard: OpenClipboard failed")
		return
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	h, _, _ := pGlobalAlloc.Call(0x0002, uintptr(len(utf16)*2)) // GMEM_MOVEABLE，含结尾 NUL
	if h == 0 {
		log.Printf("clipboard: GlobalAlloc failed")
		return
	}
	pMem, _, _ := pGlobalLock.Call(h)
	if pMem == 0 {
		pGlobalFree.Call(h)
		log.Printf("clipboard: GlobalLock failed")
		return
	}
	copyUTF16To(pMem, utf16)
	pGlobalUnlock.Call(h)
	if r, _, _ := pSetClipboardData.Call(clipboardTextFormat, h); r == 0 {
		pGlobalFree.Call(h) // 失败时所有权仍在本进程
		log.Printf("clipboard: SetClipboardData failed")
	}
}

// copyUTF16To 把 utf16 复制到目标地址（kernel32 的 RtlMoveMemory）；unsafe 转换集中
// 在这里（dst 是明确的内存地址），避免 vet 的 unsafeptr 误报。
func copyUTF16To(dst uintptr, utf16 []uint16) {
	if len(utf16) == 0 {
		return
	}
	pRtlMoveMemory.Call(dst, uintptr(unsafe.Pointer(&utf16[0])), uintptr(len(utf16)*2))
}

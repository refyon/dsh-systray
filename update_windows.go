//go:build windows

package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func updateSupported() bool { return true }

// confirmUpdate 弹窗询问是否升级（返回 true=立即升级）。
func confirmUpdate(newVersion string) bool {
	return runModernDialog(appName,
		"发现新版本 "+newVersion+"（当前 "+version+"）。\n是否立即升级？",
		[]string{"立即升级", "暂不升级"}, 0) == 0
}

// spawnSwapper 启动下载好的新 exe 的替换模式（--update-swap <旧PID> <目标exe路径>）。
// 调用后应立即退出当前进程。
func spawnSwapper(newExePath string) {
	target, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(newExePath, updateSwapFlag, strconv.Itoa(os.Getpid()), target)
	hideCmdWindow(cmd)
	_ = cmd.Start()
}

// runUpdateSwap 新 exe 以 --update-swap <旧PID> <目标exe路径> 启动时执行：
// 等待旧进程退出 → 复制自身覆盖目标 exe → 启动新版本。
func runUpdateSwap(oldPID int, target string) {
	waitForProcessExit(oldPID, 30*time.Second)
	self, err := os.Executable()
	if err != nil {
		return
	}
	var lastErr error
	for i := 0; i < 20; i++ {
		if lastErr = copyFile(self, target); lastErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		log.Printf("update swap copy failed: %v, starting new exe from temp", lastErr)
		runDetached(self)
		return
	}
	log.Printf("exe replaced at %s, restarting new version", target)
	runDetached(target)
}

// runDetached 后台启动 exe（隐藏窗口）。
func runDetached(exePath string) {
	cmd := exec.Command(exePath)
	cmd.Dir = filepath.Dir(exePath)
	hideCmdWindow(cmd)
	_ = cmd.Start()
}

// copyFile 复制并替换：先写临时文件再 rename，避免复制中途损坏目标 exe。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".update-tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ---- 等待旧进程退出（OpenProcess + WaitForSingleObject） ----

var (
	modKernelUpd = syscall.NewLazyDLL("kernel32.dll")
	pOpenProc    = modKernelUpd.NewProc("OpenProcess")
	pWaitProc    = modKernelUpd.NewProc("WaitForSingleObject")
	pCloseProc   = modKernelUpd.NewProc("CloseHandle")
)

func waitForProcessExit(pid int, timeout time.Duration) {
	h, _, _ := pOpenProc.Call(0x00100000 /* SYNCHRONIZE */, 0, uintptr(pid))
	if h != 0 {
		ms := uintptr(timeout / time.Millisecond)
		pWaitProc.Call(h, ms)
		pCloseProc.Call(h)
		return
	}
	// 兜底轮询（OpenProcess 失败通常意味着进程已退出）
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func processAlive(pid int) bool {
	h, _, _ := pOpenProc.Call(0x00100000, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer pCloseProc.Call(h)
	ret, _, _ := pWaitProc.Call(h, 0)
	return ret == 0x00000102 // WAIT_TIMEOUT：仍在运行
}

// server_log.go：把后台服务（dsh web）的 stdout/stderr 从**管道**改为**真实文件句柄**，
// 再由托盘 tail 该文件合并进统一日志。
//
// 为什么必须改（2026-10-06 实证的「服务自己终止」缺陷）：
//   - 原先 `cmd.Stdout = newModuleLogWriter("server")` 不是 *os.File，Go 的 os/exec 于是建
//     **管道**：写端给服务，读端留在托盘进程里；
//   - 托盘自更新 relaunch、或「退出并保留服务」时托盘进程退出 → 读端随之消失 →
//     服务下一次写日志（deepseek-account 心跳每约 5 分钟一次）拿到 EPIPE →
//     Node 以 exit(1) 静默退出，崩溃文本也写进断管道，**全局无痕**；
//   - 更糟的是新托盘只用 HTTP 探测判断「服务已在运行」而跳过 spawn，它没有该子进程句柄，
//     `cmd.Wait()` 与 service_supervisor 的自动重启都不会触发。
//
// 改成文件句柄后：句柄由服务进程自己持有，托盘退出/自更新都不影响它继续写入；
// 托盘无论自己拉起的还是沿用先前进程的服务，都能通过 tail 把输出并回统一日志
// （行前缀仍是 `[server]`，格式与其它子进程一致）。
package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// serverLogName 服务输出文件（与统一日志同目录，便于日志页与排障一起取）。
	serverLogName = "server.log"
	// serverLogRotateBytes 单文件上限：超过则在下次启动服务前轮转为 server.log.1。
	// 只在服务已退出（或即将被替换）时轮转——Windows 不允许重命名仍被占用的文件。
	serverLogRotateBytes = 4 << 20
	// serverLogTailInterval tail 轮询间隔（服务输出不频繁，250ms 足够且几乎不耗 CPU）。
	serverLogTailInterval = 250 * time.Millisecond
)

// serverLogPath 服务输出文件路径（日志目录不可用时为空串）。
func serverLogPath() string {
	if strings.TrimSpace(logDir) == "" {
		return ""
	}
	return filepath.Join(logDir, serverLogName)
}

// openServerLogForChild 打开服务输出文件供子进程继承（append）。返回的句柄在 cmd.Start 后
// 由调用方关闭：子进程持有自己的副本，托盘关掉本地副本不影响它继续写。
func openServerLogForChild() (*os.File, error) {
	p := serverLogPath()
	if p == "" {
		return nil, os.ErrNotExist
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// rotateServerLogIfLarge 服务启动前轮转过大文件（此时旧进程已退出/即将退出，重命名安全）。
func rotateServerLogIfLarge() {
	p := serverLogPath()
	if p == "" {
		return
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Size() < serverLogRotateBytes {
		return
	}
	_ = os.Rename(p, p+".1")
}

// ==================== tail：把服务输出并回统一日志 ====================

var (
	serverLogTailMu     sync.Mutex
	serverLogTailCancel chan struct{}
	serverLogTailDone   chan struct{}
)

// startServerLogTail 开始把服务输出文件的新内容并入统一日志（重复调用先停旧的）。
// fromEnd=true 时从当前文件末尾开始（沿用先前进程启动的服务：只并新输出，不重放历史）。
func startServerLogTail(fromEnd bool) {
	stopServerLogTail()
	p := serverLogPath()
	if p == "" {
		return
	}
	cancel := make(chan struct{})
	done := make(chan struct{})
	serverLogTailMu.Lock()
	serverLogTailCancel, serverLogTailDone = cancel, done
	serverLogTailMu.Unlock()

	go func() {
		defer close(done)
		tailServerLog(p, fromEnd, cancel)
	}()
}

// stopServerLogTail 停止 tail 并补出残留半行（进程退出/托盘退出时调用）。
func stopServerLogTail() {
	serverLogTailMu.Lock()
	cancel, done := serverLogTailCancel, serverLogTailDone
	serverLogTailCancel, serverLogTailDone = nil, nil
	serverLogTailMu.Unlock()
	if cancel == nil {
		return
	}
	close(cancel)
	<-done
}

// tailServerLog tail 主循环：只在文件增长时读取新内容，按行改写成统一日志格式。
// 文件被轮转/截断（长度小于已读偏移）时回到文件开头重读。
func tailServerLog(path string, fromEnd bool, cancel <-chan struct{}) {
	w := newModuleLogWriter("server")
	var offset int64
	if fi, err := os.Stat(path); err == nil && fromEnd {
		offset = fi.Size()
	}
	ticker := time.NewTicker(serverLogTailInterval)
	defer ticker.Stop()
	for {
		select {
		case <-cancel:
			w.Flush()
			return
		case <-ticker.C:
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if fi.Size() < offset { // 被截断/轮转：从头再来
			offset = 0
		}
		if fi.Size() == offset {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if _, err := f.Seek(offset, 0); err == nil {
			buf := make([]byte, 64<<10)
			for {
				n, rerr := f.Read(buf)
				if n > 0 {
					_, _ = w.Write(buf[:n]) // 半行由 writer 缓冲，整行立即落盘
					offset += int64(n)
				}
				if rerr != nil {
					break
				}
			}
		}
		_ = f.Close()
	}
}

package main

import (
	"bytes"
	"strings"
	"time"
)

// ==================== 子进程输出行改写器（统一日志格式） ====================
// moduleLogWriter 把子进程（dsh web / pnpm / git 等）的原始输出按行改写为统一格式
// 写入 dsh-systray.log：每行 ts [LEVEL] [module] content，LEVEL 由内容启发判定
// （detectLevel），module 标明输出来源。未遇换行的半行暂存缓冲；命令/进程结束后
// 必须调用 Flush 补出残留半行（崩溃进程的末行往往无换行——此前半行随进程消失，
// 是 server.log 空档的帮凶之一）。

// moduleLogWriter 每行前加时间戳/等级/模块前缀。
type moduleLogWriter struct {
	module string
	buf    []byte // 尚未遇到换行的半行
}

func newModuleLogWriter(module string) *moduleLogWriter {
	return &moduleLogWriter{module: module}
}

// Write 处理输入：完整行立即落盘；半行暂存。返回原输入长度。
// exec 包保证同一 writer 挂 stdout/stderr 时同一时刻只有一个 goroutine 调 Write。
func (w *moduleLogWriter) Write(p []byte) (int, error) {
	orig := len(p)
	writeUnifiedRaw(w.format(p))
	return orig, nil
}

func (w *moduleLogWriter) format(p []byte) []byte {
	var out []byte
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.buf = append(w.buf, p...)
			break
		}
		line := strings.TrimRight(string(append(w.buf, p[:i]...)), "\r")
		w.buf = w.buf[:0]
		p = p[i+1:]
		if line == "" {
			continue
		}
		ts := time.Now().Format("2006/01/02 15:04:05")
		out = append(out, []byte(ts+" ["+detectLevel(line)+"] ["+w.module+"] "+line+"\n")...)
	}
	return out
}

// Flush 把残留半行（无换行结尾）补上前缀写出。同步命令在 Run 后调用；
// 长驻服务在 cmd.Wait 后调用（进程退出后，崩溃末行也能留痕）。
func (w *moduleLogWriter) Flush() {
	if len(w.buf) == 0 {
		return
	}
	line := strings.TrimRight(string(w.buf), "\r")
	w.buf = w.buf[:0]
	if line == "" {
		return
	}
	ts := time.Now().Format("2006/01/02 15:04:05")
	writeUnifiedRaw([]byte(ts + " [" + detectLevel(line) + "] [" + w.module + "] " + line + "\n"))
}

package main

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// 需求：日志页要保证每个行头都有时间戳。app.log 由 Go log 输出自带时间戳；
// 但子进程输出（server/install/build/harness-update/plugin-update）只有原始内容——
// 用 timePrefixWriter 包一层，把每行输出前加上与 Go log 同格式的时间戳前缀
// （"2006/01/02 15:04:05 "），跨文件对时与排查都更直观。

// timePrefixWriter 为写入的每一行添加时间戳前缀。跨行缓冲保证每个换行符后的整行
// 作为前缀边界；同一包装器同时挂 cmd.Stdout/Stderr，互斥保证行不交错。
type timePrefixWriter struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte // 尚未遇到换行的半行
}

func newTimePrefixWriter(w io.Writer) *timePrefixWriter {
	return &timePrefixWriter{w: w}
}

// Write 返回原输入长度（语义与文件直写一致），错误时已写部分照常计数。
func (t *timePrefixWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	orig := len(p)
	ts := time.Now().Format("2006/01/02 15:04:05") // 每个新行取一次时间
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.buf = append(t.buf, p...)
			return orig, nil
		}
		if _, err := io.WriteString(t.w, ts+" "); err != nil {
			return orig, err
		}
		if _, err := t.w.Write(append(t.buf, p[:i]...)); err != nil {
			return orig, err
		}
		t.buf = t.buf[:0]
		if _, err := io.WriteString(t.w, "\n"); err != nil {
			return orig, err
		}
		p = p[i+1:]
		ts = time.Now().Format("2006/01/02 15:04:05")
	}
	return orig, nil
}

// Flush 把残留的半行（无换行结尾）也加上时间戳写出。同步子进程命令（install/build 等）
// 结束后调用一次，避免结尾内容丢失；长驻进程（server）正常退出前由其日志闭环覆盖。
func (t *timePrefixWriter) Flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.buf) == 0 {
		return
	}
	ts := time.Now().Format("2006/01/02 15:04:05")
	_, _ = io.WriteString(t.w, ts+" ")
	_, _ = t.w.Write(t.buf)
	t.buf = t.buf[:0]
}

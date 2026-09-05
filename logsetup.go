package main

// ==================== 统一日志（单文件 + 等级 + 模块） ====================
// 所有行为（自身日志、设置页操作、托盘、后台服务与 pnpm/git 子进程输出）统一写入
// logDir/dsh-systray.log，每行格式：
//
//	2026/09/05 09:00:00 [INFO] [module] message
//
// 等级（DEBUG/INFO/WARN/ERROR）以纯文本写入，日志页按等级着色；模块标识来源：
// app（自身）/ ui（设置页操作）/ tray（托盘）/ server（dsh web 输出）/
// harness（pnpm·git 于 harness 目录）/ profile（pnpm 于 profile 目录）/ install / build。
// 统一文件为进程级单例句柄 + 互斥写：子进程输出与自身日志并发追加时行不交错。
//
// 历史教训：server.log 恒空是因为 startServer 用 defer 在返回时（毫秒级）关闭了日志
// 文件，dsh web 的输出（启动 1-3 秒后）全部写到已关闭的句柄而丢失；且 timePrefixWriter
// 的半行缓冲在进程崩溃时随进程消失。统一单例句柄 + 每行立即落盘 + Wait 后 Flush 修复之。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// unifiedLogName 统一日志文件名（日志页与弹窗文案引用的唯一日志文件）。
const unifiedLogName = "dsh-systray.log"

var (
	unifiedMu   sync.Mutex
	unifiedFile *os.File // 进程生命周期持有，永不关闭；nil = 未初始化（bindingsRun / 打开失败，日志静默丢弃）
)

// unifiedLogPath 统一日志文件完整路径。
func unifiedLogPath() string {
	return filepath.Join(logDir, unifiedLogName)
}

// initUnifiedLog 打开统一日志句柄（进程级）。打开失败仅丢弃日志，不阻塞功能
// （与既有 O_CREATE 失败静默语义一致）。
func initUnifiedLog() {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	if f, err := os.OpenFile(unifiedLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		unifiedFile = f
	}
}

// writeUnifiedRaw 加锁写原始字节（行级写；单次 Write 由 OS 保证原子追加）。
func writeUnifiedRaw(p []byte) {
	if unifiedFile == nil {
		return
	}
	unifiedMu.Lock()
	_, _ = unifiedFile.Write(p)
	unifiedMu.Unlock()
}

// unifiedLine 拼一行完整日志并写入：ts [LEVEL] [module] message\n（ts 为空取当前时间）。
func unifiedLine(ts, level, module, msg string) {
	if ts == "" {
		ts = time.Now().Format("2006/01/02 15:04:05")
	}
	writeUnifiedRaw([]byte(ts + " [" + level + "] [" + module + "] " + msg + "\n"))
}

// detectLevel 按内容启发判定日志等级（自身日志与子进程行共用）。错误特征优先于警告。
func detectLevel(s string) string {
	for _, k := range []string{
		"失败", "failed", "FAILED", "error", "Error", "ERROR", "panic", "exited",
		"denied", "无法", "崩溃", "crash", "FATAL", "SIGSEGV", "no such", "not found",
		"ERR_",
	} {
		if strings.Contains(s, k) {
			return "ERROR"
		}
	}
	for _, k := range []string{"WARN", "warning", "警告", "deprecated", "超时", "timeout"} {
		if strings.Contains(s, k) {
			return "WARN"
		}
	}
	return "INFO"
}

// appLogWriter 承接 Go 标准 log（log.Printf 系列）并改写为统一格式：
// Go log 已带时间戳（LstdFlags），此处提取并插入等级（内容启发）与模块
// （[UI] / [tray] 前缀识别）。Go log 对每条日志逐行调用 Write。
type appLogWriter struct{}

func (appLogWriter) Write(p []byte) (int, error) {
	s := strings.TrimRight(string(p), "\n")
	ts := ""
	// "2006/01/02 15:04:05" 形状（位置 4/7 为 '/'）
	if len(s) >= 19 && s[4] == '/' && s[7] == '/' && s[13] == ':' {
		ts = s[:19]
		s = strings.TrimSpace(s[19:])
	}
	module := "app"
	if strings.HasPrefix(s, "[UI] ") {
		module = "ui"
		s = s[len("[UI] "):]
	} else if strings.HasPrefix(s, "[tray] ") {
		module = "tray"
		s = s[len("[tray] "):]
	}
	unifiedLine(ts, detectLevel(s), module, s)
	return len(p), nil
}

// logMod 显式写一条指定等级/模块的日志（关键失败用 ERROR、降级用 WARN，其余走 log.Printf）。
func logMod(level, module, format string, args ...any) {
	unifiedLine("", level, module, fmt.Sprintf(format, args...))
}

// logInfo / logWarn / logError 便捷封装。
func logInfo(module, format string, args ...any)  { logMod("INFO", module, format, args...) }
func logWarn(module, format string, args ...any)  { logMod("WARN", module, format, args...) }
func logError(module, format string, args ...any) { logMod("ERROR", module, format, args...) }

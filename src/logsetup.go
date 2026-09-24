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

// initUnifiedLog 打开统一日志句柄（进程级），返回失败原因（nil = 成功）。
//
// 为什么返回原因：本函数失败时调用方会把日志整体回退到临时目录，而回退后**日志页（读主
// 目录）永远看不到这些行**，表现为"日志时有时无"（2026-09-24 现场：同一个二进制，有的
// 启动写主目录、有的写 Temp）。把失败原因带出去，调用方才能在启动首行与 stderr 里说清
// "为什么这次写到 Temp 了"。
func initUnifiedLog() error {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("创建日志目录 %s 失败: %w", logDir, err)
	}
	f, err := os.OpenFile(unifiedLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开日志文件 %s 失败: %w", unifiedLogPath(), err)
	}
	unifiedFile = f
	return nil
}

// initUnifiedLogWithRepair 先直接打开主日志；失败则尝试自愈后重试，全程把动作与原因写进
// 调用方可见的说明。返回 (是否成功, 自愈/失败说明)。
//
// 自愈顺序：
//  1. chmod 0644 —— 修 POSIX 权限位（macOS 上最常见的不可写原因）；
//  2. 改名让位 —— 目标文件本身打不开（典型：它由提权进程创建，其 ACL 不含当前受限令牌的
//     写权限）时，把旧文件改名为 .stale-<时间戳> 保留现场，再新建一个。目录可写即可成功，
//     这样主目录不会被整体放弃、日志页仍能看到本次启动。
//
// 仍失败才由调用方回退临时目录；此时说明里带上失败原因与文件属主，便于定位。
func initUnifiedLogWithRepair() (bool, string) {
	if err := initUnifiedLog(); err == nil {
		return true, ""
	} else {
		first := err
		notes := make([]string, 0, 2)

		// 1) 权限位（POSIX 语义；Windows 上是 no-op）
		_ = os.Chmod(unifiedLogPath(), 0o644)
		if err2 := initUnifiedLog(); err2 == nil {
			return true, "已修正日志文件权限位后重开"
		}

		// 2) 关键自愈：给日志目录打「低完整性可写」标签。
		//    Windows 默认只给 %TEMP% 打该标签；受限/低完整性进程因此能在 Temp 落盘、却写不了
		//    %APPDATA%，导致日志整体回退到 Temp（日志页看不到）。打标签后主目录恢复可写。
		lowNote := ""
		if lerr := ensureLowIntegrityWritable(logDir); lerr != nil {
			lowNote = "；打低完整性标签失败(" + lerr.Error() + ")"
		} else {
			lowNote = "；已给日志目录打低完整性可写标签"
		}
		if err3 := initUnifiedLog(); err3 == nil {
			return true, "日志目录修正后重开" + lowNote
		}

		// 3) 目标文件本身打不开（多为提权进程创建、ACL 不含当前令牌）→ 改名让位保留现场后新建
		if rerr := renameStaleLog(unifiedLogPath()); rerr == nil {
			if err4 := initUnifiedLog(); err4 == nil {
				return true, "原日志文件打不开，已改名让位并新建" + lowNote
			}
		}

		if werr := logWritableByCurrentToken(unifiedLogPath()); werr != nil {
			notes = append(notes, "复核仍不可写："+werr.Error())
		}
		if len(notes) > 0 {
			return false, "主日志不可写（" + strings.Join(notes, "；") + "）" + lowNote + "：" + first.Error()
		}
		return false, "主日志不可写" + lowNote + "：" + first.Error()
	}
}

// renameStaleLog 把无法打开的目标文件改名为 .stale-<时间戳>，为新建让位（保留现场，不删除）。
func renameStaleLog(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil // 不存在：无需让位
	}
	return os.Rename(path, fmt.Sprintf("%s.stale-%d", path, time.Now().Unix()))
}

// reopenUnifiedLog 轮转后重建统一日志句柄。POSIX（mac）允许 rename 打开中的文件：
// rotateServerLog 把 dsh-systray.log 改名 .1 后，旧句柄仍持续写入 .1，基础文件不再存在
// → 日志页（只读基础文件）空白、启动日志扫描基线失效（0.8.x mac 实证）。轮转成功后
// 关闭旧句柄并重开，使后续写入落到新建的 dsh-systray.log。无打开句柄时不动作（测试/异常态）。
func reopenUnifiedLog() {
	unifiedMu.Lock()
	defer unifiedMu.Unlock()
	if unifiedFile == nil {
		return
	}
	_ = unifiedFile.Close()
	unifiedFile = nil
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

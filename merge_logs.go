package main

// ==================== 历史日志合并（统一日志升级的一次性迁移） ====================
// 统一日志（b4c3a7b）后所有新写入进 logDir/dsh-systray.log；旧 sink 文件（app/server/
// install/build/harness-update/plugin-update.log）停写但留存旧格式。托盘启动时把旧文件
// 逐行改写为统一格式（ts [LEVEL] [module] content，无时间戳裸行用源文件 mtime 兜底）
// 追加进统一日志，随后删除源文件（幂等：源已删则下次启动跳过）。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// legacyLogFiles 旧 sink 文件名（合并清单；轮转副本 .1/.2/.3 不在此列）。
var legacyLogFiles = []string{"app.log", "server.log", "install.log", "build.log", "harness-update.log", "plugin-update.log"}

// legacyLogModule 旧文件 → 统一日志模块（app.log 为空，按内容前缀再细分 ui/tray）。
var legacyLogModule = map[string]string{
	"server.log":         "server",
	"install.log":        "install",
	"build.log":          "build",
	"harness-update.log": "harness",
	"plugin-update.log":  "profile",
}

// legacyTsRe 旧日志行首时间戳（Go log 与 timePrefixWriter 同格式 "2006/01/02 15:04:05 "）。
var legacyTsRe = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) (.*)$`)

// mergeLegacyLogs 把旧 sink 文件合并进统一日志并删除源文件（源已删则幂等跳过）。
// 调用时机：initUnifiedLog 之后、任何新日志写入之前；unifiedFile 未初始化（bindingsRun 等）时跳过。
func mergeLegacyLogs() {
	if unifiedFile == nil {
		return
	}
	for _, name := range legacyLogFiles {
		mergeLegacyLog(name)
	}
}

// mergeLegacyLog 合并单个旧日志文件。
func mergeLegacyLog(name string) {
	p := filepath.Join(logDir, name)
	data, err := os.ReadFile(p)
	if err != nil {
		return // 不存在 / 读取失败：跳过且不删（可能被占用，留待下次或保留）
	}
	// 无时间戳裸行的兜底时间：取源文件修改时间（比合并时刻更接近真实发生时间）
	fallback := time.Now().Format("2006/01/02 15:04:05")
	if fi, serr := os.Stat(p); serr == nil {
		fallback = fi.ModTime().Format("2006/01/02 15:04:05")
	}
	n := 0
	for _, raw := range strings.Split(string(data), "\n") {
		raw = strings.TrimRight(raw, "\r")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		ts := fallback
		line := raw
		if m := legacyTsRe.FindStringSubmatch(raw); m != nil {
			ts, line = m[1], m[2]
		}
		module := legacyLogModule[name]
		if module == "" {
			// app.log：按内容前缀细分模块（与运行期 appLogWriter 的判定一致）
			switch {
			case strings.HasPrefix(line, "[UI] "):
				module, line = "ui", strings.TrimPrefix(line, "[UI] ")
			case strings.HasPrefix(line, "[tray] "):
				module, line = "tray", strings.TrimPrefix(line, "[tray] ")
			default:
				module = "app"
			}
		}
		unifiedLine(ts, detectLevel(line), module, line)
		n++
	}
	if err := os.Remove(p); err != nil {
		logWarn("log", "历史日志合并后删除失败（保留原文件避免数据丢失）：%s：%v", name, err)
		return
	}
	if n > 0 {
		logInfo("log", "已合并历史日志 %s（%d 行）到 %s", name, n, unifiedLogPath())
	}
}

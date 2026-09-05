package main

// ==================== 服务启动守护（插件导入 / 常规页重启的自愈与定位） ====================
// 覆盖「插件导入后 / 常规页重启后服务无法启动」的容错：
//  1. 统一日志轮转留档：每次真正拉起服务前把上一份 dsh-systray.log 归档为 .1/.2/.3，
//     启动现场不再灭失（此前 server.log 恒空——句柄过早关闭——现场全丢，排障失明）；
//  2. 启动健康校验失败（版本混装 / 插件不兼容等加载错误）时，自动对受影响 profile 执行
//     一次 pnpm install 对齐（等价用户手动 `dsh plugin … remove/add` 触发的全树 reconcile），
//     再重试一次启动；
//  3. 仍失败时从统一日志中 [server] 模块行提取疑似导致启动失败的插件名，给用户可行动提示。

import (
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
)

// logLineModuleRe 统一日志行的模块段："ts [LEVEL] [module] message" 中的 module。
// 前两段是时间戳（日期/时间），随后依次为等级与模块两个中括号。
var logLineModuleRe = regexp.MustCompile(`^[^ ]+ [^ ]+ \[[A-Za-z]+\] \[([A-Za-z]+)\] `)

// logLineBodyRe 提取统一日志行的内容体（去掉时间戳/等级/模块前缀），
// 供启动健康校验与可疑插件定位在纯内容上匹配（与旧 server.log 语义一致）。
var logLineBodyRe = regexp.MustCompile(`^[^ ]+ [^ ]+ \[[A-Za-z]+\] \[([A-Za-z]+)\] (.*)$`)

// logLineModule 提取统一日志行（ts [LEVEL] [module] msg）的模块名；不匹配返回空。
func logLineModule(line string) string {
	m := logLineModuleRe.FindStringSubmatch(line)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

// serverLogLines 读取统一日志 offset 追加段中 [server] 模块行的内容体（拼接为一个文本，
// 供启动健康校验与可疑插件定位——避免把 pnpm 输出（ERR_PNPM_*）误判为服务启动错误，
// 也避免统一行前缀干扰按行匹配的正则）。
func serverLogLines(offset int64) string {
	f, err := os.Open(unifiedLogPath())
	if err != nil {
		return ""
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			offset = 0
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for _, ln := range strings.Split(string(data), "\n") {
		if m := logLineBodyRe.FindStringSubmatch(ln); len(m) == 3 && m[1] == "server" {
			sb.WriteString(m[2])
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// rotateServerLog 把当前统一日志轮转归档（保留最近 3 份 .1/.2/.3）并返回本次启动的
// 日志扫描基线偏移。文件不存在或为空时不轮转，直接返回 0；轮转失败（如文件被其它进程
// 占用）时保留原文件并返回其当前大小，健康校验仍按追加段语义工作。
// 调用方应在 killServer 之后、startServer 之前调用，使本次启动独占新文件、旧现场完整归档。
func rotateServerLog() int64 {
	p := unifiedLogPath()
	fi, err := os.Stat(p)
	if err != nil || fi.Size() == 0 {
		return 0
	}
	// 级联轮转：.2→.3、.1→.2、dsh-systray.log→.1（.3 旧内容直接丢弃）。
	// 中间缺失时 Rename 报错可忽略：只要最后一步成功即可。
	_ = os.Remove(p + ".3")
	_ = os.Rename(p+".2", p+".3")
	_ = os.Rename(p+".1", p+".2")
	if os.Rename(p, p+".1") != nil {
		log.Printf("log rotate failed (file may be locked), keeping in place")
		return unifiedLogSize()
	}
	log.Printf("log rotated to %s.1 (archived %d bytes)", p, fi.Size())
	return 0
}

// bootSuspectNameRe 启动失败日志中的插件定位线索（错误文本来自 dsh-app-boot / cordis
// 的 fail-loud 启动：解析失败、patch 缺失、加载失败都会点名 bundle 包名）。
var (
	bootSuspectResolveRe = regexp.MustCompile(`cannot resolve profile bundle "([^"]+)"`)
	bootSuspectPatchRe   = regexp.MustCompile(`profile bundle "([^"]+)" declares no dsh\.bundle`)
	// load 行的名字列表到分号为止（行尾是 “; Cordis startup failed because…” 说明，不能并入）
	bootSuspectLoadRe     = regexp.MustCompile(`plugin\(s\) failed to load:\s*([^;\n]+)`)
	bootSuspectActivateRe = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9@/._-]+):\s+(?:Error|SyntaxError|TypeError|ReferenceError)`)
)

// splitPluginNames 兼容「plugin(s) failed to load: a, b, c」一行含多个名字的切分。
func splitPluginNames(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		name := strings.Trim(strings.TrimSpace(part), `"'`)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// parseBootLogSuspects 从统一日志的 offset 追加段（仅 [server] 模块行）提取疑似导致
// 启动失败的插件包名，按首次出现顺序去重。读取失败返回 nil（调用方降级为通用提示）。
func parseBootLogSuspects(offset int64) []string {
	s := serverLogLines(offset)
	if s == "" {
		return nil
	}
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.Trim(strings.TrimSpace(name), `"'`)
		if name != "" && !seen[name] {
			seen[name] = true
		}
	}
	// 捕获组 1 是包名；load 一行可能含逗号分隔的多个名字
	collect := func(re *regexp.Regexp, names bool) {
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			if len(m) < 2 {
				continue
			}
			if names {
				for _, n := range splitPluginNames(m[1]) {
					add(n)
				}
			} else {
				add(m[1])
			}
		}
	}
	collect(bootSuspectResolveRe, false)
	collect(bootSuspectPatchRe, false)
	collect(bootSuspectLoadRe, true)
	// 激活失败块内的缩进错误行（名字: 错误类型）
	for _, m := range bootSuspectActivateRe.FindAllStringSubmatch(s, -1) {
		if len(m) > 1 {
			add(m[1])
		}
	}
	// 官方 in-box bundle 失败时同样需要暴露（无法解析即安装不完整），不过滤。
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	// map 无序：按名字排序保证输出稳定（测试友好）
	sort.Strings(out)
	return out
}

// reconcileProfileDeps 在 profile 目录执行 pnpm install，把恢复/变更后的依赖树与
// package.json / 锁文件对齐（重建 .modules.yaml、junction 与虚拟商店引用——即用户手动
// `pnpm dsh plugin remove/add` 时 pnpm 完成的 reconcile）。网络失败时返回错误，
// 由调用方按「降级为未对齐提示」或「回退」处理。
func reconcileProfileDeps(dir string) error {
	log.Printf("reconciling profile deps: pnpm install (dir=%s)", dir)
	if err := runProfileCmd(dir, pnpmCmd(), "install"); err != nil {
		return fmt.Errorf("pnpm install 对齐失败（%s）：%v", dir, err)
	}
	return nil
}

// restartAndVerifyHealing 重启并健康校验（复用 restartAndVerifyServer 语义）；首次校验
// 失败时对 dirs 逐一 pnpm 对齐并重试一次。最终健康返回 true；失败返回 false 与
// server.log 中定位到的疑似插件名（供 UI 给用户可行动的提示）。
func restartAndVerifyHealing(dirs []string) (bool, []string) {
	if restartAndVerifyServer() {
		return true, nil
	}
	for _, dir := range dirs {
		if err := reconcileProfileDeps(dir); err != nil {
			log.Printf("healing reconcile failed, skip: %v", err)
		}
	}
	if restartAndVerifyServer() {
		return true, nil
	}
	return false, parseBootLogSuspects(0)
}

// startAndVerifyOnce 单次启动周期：日志轮转 → 拉起 → 就绪 → 健康窗口。
// 端口已有可用服务时视为成功（与本机异常残留场景的既有语义一致）。
// 返回 (是否就绪且健康, 失败原因文案)。健康窗口确保迟于 HTTP 就绪数秒才刷出的
// 加载错误（版本/插件不兼容）不会被当作启动成功。
func startAndVerifyOnce() (bool, string) {
	if serverResponding(webURL) {
		return true, ""
	}
	before := rotateServerLog()
	started, exitCh := startServer()
	if !started {
		return false, "无法启动后台服务进程。"
	}
	if ok, msg := waitForServerReady(webURL, exitCh, startupTimeout); !ok {
		return false, "服务未在预期时间内就绪（" + msg + "）。"
	}
	if !verifyServerBoot(before, exitCh) {
		return false, "服务已响应，但启动日志存在加载错误（版本/插件不兼容）。"
	}
	return true, ""
}

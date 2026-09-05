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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
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
	bootSuspectLoadRe = regexp.MustCompile(`plugin\(s\) failed to load:\s*([^;\n]+)`)
	// cordis 新版 loader 报错格式：「failed to import loader entry <name> (<name>): …」
	// （new_device.log 实证：旧 load 行格式消失后嫌疑解析为空，禁用自愈永远不触发）
	bootSuspectImportRe   = regexp.MustCompile(`failed to import loader entry\s+([A-Za-z0-9@/._-]+)`)
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
	collect(bootSuspectImportRe, false)
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

// bootErrorHintRe 判断某行是否为「加载失败证据」（给嫌疑插件摘原因文案用，仅用于展示）。
var bootErrorHintRe = regexp.MustCompile(`(?i)(error|syntaxerror|typeerror|referenceerror|does not provide an export|failed to import loader entry|cannot resolve|not found|failed to load)`)

// bootSuspectReasons 为嫌疑插件名摘取启动日志（[server] 追加段）中的首条加载错误证据行，
// 压成单行限长；无证据行时回退通用文案。供「禁用原因」的记录与展示。
func bootSuspectReasons(offset int64, names []string) map[string]string {
	s := serverLogLines(offset)
	out := map[string]string{}
	for _, name := range names {
		best := ""
		for _, ln := range strings.Split(s, "\n") {
			line := strings.TrimSpace(ln)
			if line == "" || !strings.Contains(line, name) || !bootErrorHintRe.MatchString(line) {
				continue
			}
			best = trimHintLine(line)
			break
		}
		if best == "" {
			best = "与当前 harness 版本不兼容（启动日志存在加载错误）"
		}
		out[name] = best
	}
	return out
}

// trimHintLine 把错误行压成单行并限长（避免整段堆栈进 UI / 记录）。
func trimHintLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 220 {
		return string(r[:220]) + "…"
	}
	return s
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

// runProfileCmdCapture 在 profile 目录执行命令并捕获输出（运行环境与 runProfileCmd 相同；
// 输出同时写入统一日志与返回值，供失败归因解析——pnpm 报错需要原文才能定位依赖名）。
func runProfileCmdCapture(dir, name string, args ...string) (string, error) {
	return runProfileCmdCaptureWatch(dir, nil, name, args...)
}

// runProfileCmdCaptureWatch 同 runProfileCmdCapture，另通过 watch 轮询取消信号：watch()
// 返回 true（或 6 分钟兜底超时）时终止子进程**整棵进程树**——先 taskkill /T /F 再 cancel
// （若先 cancel，cmd.exe 立刻死亡、taskkill 找不到树根，孤儿 node 会继续跑并让 Wait 卡
// 在继承的管道上，new_device.log 实证取消后 2 分钟才返回）。杀树后最多再等 5s 进程退出，
// 仍不退出则放弃等待直接返回（孤儿进程由系统回收，上层立即走取消收尾）。
func runProfileCmdCaptureWatch(dir string, watch func() bool, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), pnpmTunedEnv()...)
	hideCmdWindow(cmd)
	var buf bytes.Buffer
	w := newModuleLogWriter("profile")
	log.Printf("profile cmd: %s %v (dir=%s)", name, args, dir)
	cmd.Stdout = io.MultiWriter(w, &buf)
	cmd.Stderr = io.MultiWriter(w, &buf)
	if err := cmd.Start(); err != nil {
		w.Flush()
		return "", err
	}
	stop := make(chan struct{})
	killed := make(chan struct{})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				// watch 置位（用户取消）或兜底超时（ctx.Err()）→ 整树终止
				if (watch != nil && watch()) || ctx.Err() != nil {
					if cmd.Process != nil {
						killProcessTreePID(cmd.Process.Pid) // 先杀树根，保证 cmd.exe 仍在
					}
					cancel()
					close(killed)
					return
				}
			}
		}
	}()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var err error
	select {
	case err = <-waitCh:
	case <-killed:
		// 已杀树：最多再等 5s 让 Wait 返回；孤儿进程持有管道时可放弃等待
		select {
		case err = <-waitCh:
		case <-time.After(5 * time.Second):
			err = fmt.Errorf("pnpm install 已终止，但进程退出等待超时（已放弃等待）")
		}
	}
	close(stop)
	w.Flush()
	return buf.String(), err
}

// noMatchingVersionRe pnpm 对不存在的版本报错会点名：No matching version found for <name>@<spec>。
var noMatchingVersionRe = regexp.MustCompile(`No matching version found for\s+([^\s@]+)`)

// fetchTgzFolderRe pnpm 下载失败（registry 不可达/超时/网络中断）日志形如：
//
//	GET https://registry.npmjs.org/<pkg>/-/<pkg>-<ver>.tgz error (23). Will retry…
//
// 捕获 <pkg>（registry 布局中「/-/」前的包名文件夹段），用于把失败归因到具体依赖。
var fetchTgzFolderRe = regexp.MustCompile(`(?i)(?:GET|fetch)[^\n]*?/([^\s/]+)/-/`)

// reconcileProfileDepsRepair 对齐 profile 依赖树；pnpm install 失败时解析输出定位失败依赖，
// 从 package.json 摘除后重试——可连续摘除多个下载失败的依赖（一个坏网络包不阻塞其它插件），
// 覆盖跨机恢复的三类确定性失败：ERR_PNPM_LINKED_PKG_DIR_NOT_FOUND（本地链接目录不存在）、
// No matching version（版本不可安装）、tarball 下载失败（registry 不可达/超时）。
// 返回本次摘除的依赖名与最终错误（仍失败时给出含原因的说明，由健康校验/回退兜底）。
func reconcileProfileDepsRepair(dir string) ([]string, error) {
	return reconcileProfileDepsRepairWatch(dir, nil)
}

// reconcileProfileDepsRepairWatch 同 reconcileProfileDepsRepair，但 watch 置位（用户取消）时
// 立即终止正在运行的 pnpm 并快速返回——不再做「摘除依赖重试」，由调用方走取消收尾。
func reconcileProfileDepsRepairWatch(dir string, watch func() bool) ([]string, error) {
	log.Printf("reconciling profile deps (repair): pnpm install (dir=%s)", dir)
	const maxDrops = 10 // 单轮对齐最多自动摘除数，防失控循环
	var dropped []string
	out, err := runProfileCmdCaptureWatch(dir, watch, pnpmCmd(), "install")
	for i := 0; err != nil; i++ {
		if watch != nil && watch() {
			// 用户已取消：pnpm 已被终止，快速返回让上层走收尾（不再继续摘除）
			return dropped, fmt.Errorf("pnpm install 已因取消中断（%s）", dir)
		}
		if i >= maxDrops {
			return dropped, fmt.Errorf("pnpm install 对齐失败（%s，已摘除 %d 个仍无法安装）：%v", dir, len(dropped), err)
		}
		dep := failingPnpmDep(out, dir)
		if dep == "" {
			return dropped, fmt.Errorf("pnpm install 对齐失败（%s）：%v", dir, err)
		}
		if !dropProfileDependency(dir, dep) {
			return dropped, fmt.Errorf("pnpm install 对齐失败（%s）：%v", dir, err)
		}
		dropped = append(dropped, dep)
		log.Printf("reconcile repair: dropped unresolvable dependency %s (dir=%s)", dep, dir)
		// 继续尝试安装剩余插件：逐个摘除下载失败的包，直至对齐成功或不可归因
		out, err = runProfileCmdCaptureWatch(dir, watch, pnpmCmd(), "install")
	}
	return dropped, nil
}

// failingPnpmDep 从 pnpm 输出解析导致安装失败的依赖名：
//   - "No matching version found for <name>" 直接点名；
//   - ERR_PNPM_LINKED_PKG_DIR_NOT_FOUND 不点名依赖（只报缺失路径），回查该 profile
//     package.json 中首个本地目录 spec 的依赖名（本地链接跨机失效场景）；
//   - tarball 下载失败（registry 不可达/超时）：从 URL 包名段归因（如 deepseek-idesign）。
//
// 无法归因返回空。
func failingPnpmDep(out, dir string) string {
	if m := noMatchingVersionRe.FindStringSubmatch(out); len(m) > 1 {
		return m[1]
	}
	if strings.Contains(out, "ERR_PNPM_LINKED_PKG_DIR_NOT_FOUND") {
		data, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			return ""
		}
		var m struct {
			Dependencies map[string]string `json:"dependencies"`
		}
		if json.Unmarshal(data, &m) != nil {
			return ""
		}
		names := make([]string, 0, len(m.Dependencies))
		for n := range m.Dependencies {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if _, ok := localSpecPath(m.Dependencies[n]); ok {
				return n
			}
		}
	}
	// 下载失败：pnpm 输出形如 GET https://registry…/<name>/-/<name>-<ver>.tgz error (23)
	if m := fetchTgzFolderRe.FindStringSubmatch(out); len(m) > 1 && strings.TrimSpace(m[1]) != "" {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// dropProfileDependency 从 profile package.json 移除依赖 name（含 dsh.profile.bundles 中同名
// 声明，避免「依赖已删但 bundle 仍声明」导致启动时无法解析 bundle）。返回是否有变化。
func dropProfileDependency(dir, name string) bool {
	pj := filepath.Join(dir, "package.json")
	data, err := os.ReadFile(pj)
	if err != nil {
		return false
	}
	root := map[string]interface{}{}
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	deps, _ := root["dependencies"].(map[string]interface{})
	if deps == nil {
		return false
	}
	if _, ok := deps[name]; !ok {
		return false
	}
	delete(deps, name)
	root["dependencies"] = deps
	stripBundleEntry(root, name)
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false
	}
	if err := os.WriteFile(pj, append(b, '\n'), 0o644); err != nil {
		log.Printf("dropProfileDependency: write failed (%s): %v", pj, err)
		return false
	}
	return true
}

// unresolvedBundleRe 启动日志「cannot resolve profile bundle "X"」：dsh.profile.bundles 里
// 残留了已删除/未安装的插件名，导致服务启动硬失败（本机删除 deepseek-idesign 实证）。
var unresolvedBundleRe = regexp.MustCompile(`cannot resolve profile bundle "([^"]+)"`)

// unresolvedBundleNames 从统一日志的 offset 追加段提取无法解析的 bundle 插件名（去重排序）。
func unresolvedBundleNames(offset int64) []string {
	s := serverLogLines(offset)
	if s == "" {
		return nil
	}
	seen := map[string]bool{}
	for _, m := range unresolvedBundleRe.FindAllStringSubmatch(s, -1) {
		if len(m) > 1 && strings.TrimSpace(m[1]) != "" {
			seen[strings.TrimSpace(m[1])] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// stripUnresolvedBundles 把无法解析的 bundle 名从各 profile 的 dsh.profile.bundles 摘除
// （不触碰 dependencies/禁用记录——插件已删除或未安装，仅清理激活声明）。返回摘除次数。
func stripUnresolvedBundles(names []string) int {
	if len(names) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, n := range names {
		if n != "" && !isOfficialHarnessPkg(n) {
			set[n] = true
		}
	}
	if len(set) == 0 {
		return 0
	}
	removed := 0
	for _, pf := range enumeratePluginProfiles() {
		root := readProfileRoot(pf.dir)
		changed := false
		for n := range set {
			if bundleEntryExists(root, n) {
				stripBundleEntry(root, n)
				changed = true
				removed++
			}
		}
		if changed {
			if err := writeProfileRoot(pf.dir, root); err != nil {
				log.Printf("stripUnresolvedBundles: write %s failed: %v", pf.dir, err)
			}
		}
	}
	return removed
}

// restartAndVerifyHealing 重启并健康校验（复用 restartAndVerifyServer 语义）；首次校验
// 失败时对 dirs 逐一 pnpm 对齐并重试一次。最终健康返回 true；失败返回 false 与
// server.log 中定位到的疑似插件名（供 UI 给用户可行动的提示）。
// 本函数为「启动自愈」主体，运行期间不可中断（调用方在进入前已进入自愈阶段，
// CancelRestore 会忽略请求），保证自愈运行到确定结果。
func restartAndVerifyHealing(dirs []string) (bool, []string) {
	if restartAndVerifyServer() {
		return true, nil
	}
	for _, dir := range dirs {
		if err := reconcileProfileDeps(dir); err != nil {
			log.Printf("healing reconcile failed, skip: %v", err)
		}
	}
	// 无法解析 bundle 特例自愈：摘除残留激活声明后重试（删除插件后 bundle 未清的本机实证）
	if names := unresolvedBundleNames(0); len(names) > 0 {
		log.Printf("healing: stripping unresolved profile bundles: %s", strings.Join(names, "、"))
		_ = stripUnresolvedBundles(names)
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

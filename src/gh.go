package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ==================== GitHub CLI（gh）：私有仓库授权与凭据 ====================
// 隐私设计：dsh-systray 自身不保存任何 GitHub 凭据。私有仓库插件的检查更新依赖
// gh CLI 的**设备流授权**（浏览器 + 一次性代码），token 由 gh 写入系统凭据库
// （Windows Credential Manager / macOS Keychain），本程序只在内存中短暂持有，
// 随进程退出消失——config.json / 日志 / 任何落盘文件都不含 token。
//
// 首次使用若本机没有 gh，会从官方 Release 下载便携版到 tools/gh（与 7za 同一目录约定）。

const (
	ghAuthFlowTimeout = 5 * time.Minute  // 设备流授权等待上限（用户浏览器内完成的窗口期）
	ghTokenFailQuiet  = 30 * time.Second // 取 token 失败后的静默期：避免每次检查都去 spawn gh
	ghTokenCmdTimeout = 15 * time.Second // 单次 gh auth token 调用上限
	ghRepoOwner       = "cli"
	ghRepoName        = "cli"

	// ghAuthStartAttempts 设备流「起流」阶段的尝试次数。本机到 github.com 的连接会周期性
	// 被阻断（2026-09-14 实证：设备码请求 21s 后失败，同期 git pull 也是 21s 连接超时），
	// 起流失败时用户还没拿到任何东西，自动重试比让用户反复点「检查更新」可靠。
	ghAuthStartAttempts = 3

	// ghLoginLabel 授权确认弹窗的主按钮文案（zh 字面量，en 由 i18nEnMap 翻译）。
	// 平台层 askGitHubAuth 与回归测试共用，避免按钮文案漂移成空串而不可见。
	ghLoginLabel = "登录 GitHub"

	// ghDownloadFrom, ghDownloadTo 下载 GitHub CLI 的进度区间（0~1）：只在总进度条的
	// 下半段推进，解压安装阶段再补到 1。
	ghDownloadFrom = 0.05
	ghDownloadTo   = 0.9

	// ghDLBudget 单次 gh 下载的整体预算；ghDLStall 为「无数据」停滞上限
	// （由 downloadWithRetry 的停滞看门狗执行，见 updater.go）。
	// 官方包实测 15.3 MB（windows_amd64 解压后 43 MB）：慢网整包可跑几分钟，
	// 过早超时会留下半截文件（2026-09-13 实测：5 分钟预算下下载中途报超时失败）。
	// 停滞上限放到 90s：镜像只是慢、仍有数据流动时不应被误杀。
	ghDLBudget = 20 * time.Minute
	ghDLStall  = 90 * time.Second
)

var (
	ghBinaryCache   string
	ghBinaryMu      sync.Mutex
	ghTokenCache    string
	ghTokenFailedAt time.Time
	ghTokenMu       sync.Mutex
	ghAuthFlowMu    sync.Mutex // 串行化授权流程：并发点「检查更新」不会起第二个 gh

	// ghAuthRetryDelay 起流失败到重试之间的等待（变量而非常量：测试里置 0，不真的睡）。
	ghAuthRetryDelay = 2 * time.Second

	// ghDeviceFlowOnce 单次设备流执行（起流 → 展示代码 → 等用户完成授权）。抽成变量以便
	// 测试注入假实现验证重试策略，不必在测试里 spawn 真实 gh。
	ghDeviceFlowOnce = ghDeviceLoginOnce

	// ghDownloadMu 压制重复的 GitHub CLI 下载（结果经 ghDownloadDone 广播）。
	ghDownloadMu   sync.Mutex
	ghDownloadDone chan struct{}
	ghDownloadErr  error
)

// ghToolsDir gh 便携版的存放目录（与 7za 同在用户配置目录的 tools/ 下）。
func ghToolsDir() string { return filepath.Join(archiveToolsDir(), "gh") }

// ghBinName 各平台 gh 可执行文件名。
func ghBinName() string {
	if runtime.GOOS == "windows" {
		return "gh.exe"
	}
	return "gh"
}

// findGHBinary 查找可用的 gh：先便携目录，再系统 PATH（结果缓存，失效则重探）。
func findGHBinary() string {
	ghBinaryMu.Lock()
	defer ghBinaryMu.Unlock()
	if ghBinaryCache != "" {
		if st, err := os.Stat(ghBinaryCache); err == nil && !st.IsDir() {
			return ghBinaryCache
		}
		ghBinaryCache = ""
	}
	p := filepath.Join(ghToolsDir(), "bin", ghBinName())
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		ghBinaryCache = p
		return p
	}
	if p, err := exec.LookPath("gh"); err == nil {
		ghBinaryCache = p
		return p
	}
	return ""
}

// ensureGHTool 确保有可用的 gh：已有→直接用；没有→下载官方便携版（失败返回错误）。
// progress 为下载/解压进度回调（0~1，nil 表示不展示进度）。
func ensureGHTool(progress func(pct float64)) (string, error) {
	if p := findGHBinary(); p != "" {
		return p, nil
	}
	if progress == nil {
		progress = func(float64) {}
	}
	binDir := filepath.Join(ghToolsDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	url, err := ghReleaseAssetURL()
	if err != nil {
		return "", err
	}
	log.Printf("downloading github cli: %s", url)
	tmp, err := os.CreateTemp(ghToolsDir(), "gh-dl-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	ctx, cancel := context.WithTimeout(context.Background(), ghDLBudget)
	defer cancel()
	// 下载走带停滞看门狗与重试的通道：镜像中途断开／挂起时自动换候选重来，
	// 不再让一次网络抖动把整个「检查更新」打成失败。
	if err := downloadWithRetry(ctx, url, tmpPath, ghDLStall,
		func(pct float64) { progress(ghDownloadFrom + (ghDownloadTo-ghDownloadFrom)*pct) }); err != nil {
		return "", fmt.Errorf("下载 GitHub CLI 失败：%w", err)
	}
	progress(0.95) // 解压安装阶段（zip 很小，无需再细分）
	bin, err := unpackGHTool(tmpPath, binDir)
	if err != nil {
		return "", err
	}
	ghBinaryMu.Lock()
	ghBinaryCache = bin
	ghBinaryMu.Unlock()
	log.Printf("github cli installed: %s", bin)
	return bin, nil
}

// ghReleaseAssetURL 通过 GitHub API 查最新 Release，拼出本平台 zip 的下载地址。
func ghReleaseAssetURL() (string, error) {
	api := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", ghRepoOwner, ghRepoName)
	body, err := getWithMirrors(mirrorCandidates(api), pluginCheckDeadline)
	if err != nil {
		return "", fmt.Errorf("查询 GitHub CLI 最新版本失败：%w", err)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if json.Unmarshal(body, &rel) != nil {
		return "", fmt.Errorf("GitHub CLI 版本响应无法解析")
	}
	ver := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	if ver == "" {
		return "", fmt.Errorf("GitHub CLI 版本响应缺少 tag_name")
	}
	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	platform := "macOS"
	if runtime.GOOS == "windows" {
		platform = "windows"
	}
	asset := fmt.Sprintf("gh_%s_%s_%s.zip", ver, platform, arch)
	return fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s",
		ghRepoOwner, ghRepoName, ver, asset), nil
}

// unpackGHTool 从官方 zip 中取出 bin/gh(.exe) 写入 destBinDir，返回其路径。
// 注意条目名是相对路径 "bin/gh.exe"（**没有前导斜杠**，2026-09-13 实测），
// 因此按 basename 判定，同时兼容带前导斜杠或直接平铺的打包形态。
func unpackGHTool(pkgPath, destBinDir string) (string, error) {
	zr, err := zip.OpenReader(pkgPath)
	if err != nil {
		return "", fmt.Errorf("打开 GitHub CLI 包失败：%w", err)
	}
	defer zr.Close()
	want := ghBinName()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || path.Base(f.Name) != want {
			continue
		}
		if err := copyZipEntryToDir(f, destBinDir, want); err != nil {
			return "", err
		}
		return filepath.Join(destBinDir, want), nil
	}
	return "", fmt.Errorf("GitHub CLI 包中未找到 %s", want)
}

// ghAuthToken 取 gh 当前凭据用于访问私有仓库（内存缓存；不可用时返回空串）。
// 凭据来自 gh 的系统凭据库，本函数不落盘、不写日志。
func ghAuthToken() string {
	ghTokenMu.Lock()
	defer ghTokenMu.Unlock()
	if ghTokenCache != "" {
		return ghTokenCache
	}
	if !ghTokenFailedAt.IsZero() && time.Since(ghTokenFailedAt) < ghTokenFailQuiet {
		return ""
	}
	bin := findGHBinary()
	if bin == "" {
		ghTokenFailedAt = time.Now()
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), ghTokenCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "auth", "token", "--hostname", "github.com")
	cmd.SysProcAttr = ghSysProcAttr() // 不留控制台窗口（见 procattr_windows.go）
	out, err := cmd.Output()
	tok := strings.TrimSpace(string(out))
	if err != nil || tok == "" {
		ghTokenFailedAt = time.Now()
		return ""
	}
	ghTokenCache = tok
	ghTokenFailedAt = time.Time{}
	return tok
}

// invalidateGHToken 失效 token 缓存（授权完成后调用，强制重新读取）。
func invalidateGHToken() {
	ghTokenMu.Lock()
	defer ghTokenMu.Unlock()
	ghTokenCache = ""
	ghTokenFailedAt = time.Time{}
}

// ==================== 私有仓库的拉包凭据（gh → git / pnpm） ====================
// 检查更新只用内存里的 token；**更新**（pnpm 重解析并下载私有仓库）还要两个凭据出口：
//  1. git：pnpm 解析 github: spec 的引用时跑 `git ls-remote`（本机实证：未认证时
//     could not read Username），用 gh 写进 git 配置的凭据助手取 token；
//  2. codeload：pnpm 把 hosted-git spec 解析成 codeload tarball 下载（本机实证：
//     未认证 404），用用户级 npmrc 里的 tokenHelper 取 token（pnpm 执行它、把 stdout 当 token）。
// 两处配置都只写「命令」、不写 token（token 由 gh 从系统凭据库读出），与 gh.go 顶部
// 「令牌只经内存」的约定一致。
//
// tokenHelper 的值必须是**一个存在的绝对路径、不带参数**（2026-09-25 源码级实证）：
//   - pnpm 10.34.5（托盘自带便携运行时的版本）：`loadToken` 先做
//     `path.isAbsolute(值) && existsSync(值)`，再把整个值交给 `spawnSync(值, {shell:true})`
//     —— 所以「路径 + 参数」的写法会被判为 BAD_TOKEN_HELPER_PATH 并**让该版本下所有 pnpm
//     命令直接失败**（连装公共包都跑不动，因为启动时会解析并加载全部 tokenHelper）；
//   - pnpm 11.7.0：`parseTokenHelper` 按空白切分成「命令 + 参数」再执行，因此旧的带参写法
//     在它上面恰好能用 —— 这正是该缺陷长期未被发现的原因（开发机走系统 pnpm 11）。
// 故统一改为：写一个**包装器脚本的裸路径**，脚本内部再带参数调 gh。两个版本都接受
// （10.34.5 shell:true 能跑 .cmd；11.7.0 对 .cmd/.bat 显式设 shell:true）。

// codeloadTokenHelperKey 用户级 .npmrc 的 codeload 凭据键。必须用户级：pnpm 明确拒绝
// 项目级 .npmrc 里的 tokenHelper（TOKEN_HELPER_IN_PROJECT_CONFIG）。
const codeloadTokenHelperKey = "//codeload.github.com/:tokenHelper="

// ghTokenHelperScriptName 包装器脚本文件名（Windows .cmd / 其它平台 .sh）。
func ghTokenHelperScriptName() string {
	if runtime.GOOS == "windows" {
		return "gh-token.cmd"
	}
	return "gh-token.sh"
}

// ghTokenHelperPath 与 gh 同目录的 tokenHelper 包装器路径。与 gh 同目录保证：它是绝对路径、
// 通常不含空白，且随 gh 一起在便携工具目录里（不额外引入新的目录约定）。
func ghTokenHelperPath(bin string) string {
	return filepath.Join(filepath.Dir(bin), ghTokenHelperScriptName())
}

var (
	ghCredsMu   sync.Mutex
	ghCredsDone bool // 本进程是否已尝试配置（失败也不反复 spawn gh / 改文件）
	ghCredsOK   bool
)

// ensureGitHubPrivateRepoCreds 把 gh 的凭据接给 git 与 pnpm（幂等，进程内只成功/失败一次）。
// 失败只影响私有仓库的「更新」拉包，不影响检查更新与授权，调用方不必当致命错误处理。
func ensureGitHubPrivateRepoCreds() bool {
	ghCredsMu.Lock()
	defer ghCredsMu.Unlock()
	if ghCredsDone {
		return ghCredsOK
	}
	bin := findGHBinary()
	if bin == "" {
		return false // gh 尚未就位：不记结果，装好 gh 后再调用仍有机会成功
	}
	ok := true
	if err := ghSetupGitCredentials(bin); err != nil {
		logWarn("app", "gh auth setup-git failed: %v", err)
		ok = false
	}
	if err := writeCodeloadTokenHelper(bin); err != nil {
		logWarn("app", "npmrc codeload credential failed: %v", err)
		ok = false
	}
	ghCredsDone, ghCredsOK = true, ok
	if ok {
		logInfo("app", "private repo credentials ready (git credential helper + npmrc tokenHelper)")
	}
	return ok
}

// repairCodeloadTokenHelper 启动期的凭据自愈（幂等，可重复调用）：
//   - 有 gh：重写 tokenHelper（把历史的「路径 + 参数」旧格式迁移为包装器裸路径）；
//   - 无 gh：只清理旧格式行——它会让 pnpm 10.34.5 下所有 pnpm 操作失败，留着一定是坏事。
//
// 为什么要主动做：旧格式只在下过私有仓插件、且当时 gh 已就位的机器上被写入；那些机器若用
// 托盘自带的 pnpm 10.34.5，此后连公共包都装不动，却没有任何路径会自动触发修复。
func repairCodeloadTokenHelper() {
	if bin := findGHBinary(); bin != "" {
		if err := writeCodeloadTokenHelper(bin); err != nil {
			logWarn("app", "codeload tokenHelper repair failed: %v", err)
		}
		return
	}
	if err := removeLegacyCodeloadTokenHelper(); err != nil {
		logWarn("app", "legacy codeload tokenHelper cleanup failed: %v", err)
	}
}

// ghSetupGitCredentials 让 git 也用 gh 的凭据（pnpm 解析 github: spec 走 git ls-remote，
// 不经过 npmrc）。gh 只把「凭据助手命令」写进 git 配置，token 仍在系统凭据库。
// 需要已登录：未登录时 gh 直接报错退出，不会写入任何配置（本机实证）。
func ghSetupGitCredentials(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), ghTokenCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "auth", "setup-git", "--hostname", "github.com")
	cmd.SysProcAttr = ghSysProcAttr() // 不留控制台窗口（见 procattr_windows.go）
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v（%s）", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeGHTokenHelperScript 写 tokenHelper 包装器脚本（内容不含 token：脚本内部调 gh 取）。
// 返回可用于 npmrc 的脚本路径。原子写（同目录临时文件 + 改名），内容一致时不重写。
func writeGHTokenHelperScript(bin string) (string, error) {
	p := ghTokenHelperPath(bin)
	var body string
	if runtime.GOOS == "windows" {
		// 只回显 gh 的 stdout（token 本身），并把 gh 的退出码作为脚本退出码——
		// pnpm 判 status!=0 即报 TOKEN_HELPER_ERROR_STATUS，不能吞掉失败。
		body = "@echo off\r\n\"" + bin + "\" auth token --hostname github.com\r\n"
	} else {
		body = "#!/bin/sh\nexec \"" + bin + "\" auth token --hostname github.com\n"
	}
	if cur, err := os.ReadFile(p); err == nil && string(cur) == body {
		return p, nil
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(p, 0o755); err != nil { // 非 Windows 需可执行位（pnpm 直接 exec）
			return "", err
		}
	}
	return p, nil
}

// writeCodeloadTokenHelper 生成包装器脚本，并在用户级 ~/.npmrc 写入/更新 codeload 的
// tokenHelper（值为**裸路径**，见文件顶部的版本差异说明）。
// 幂等：内容已一致时不碰文件；只动这一行，保留用户其它配置。
func writeCodeloadTokenHelper(bin string) error {
	// 先判可表达性再落盘：路径不可表达时不留下任何半成品（包装器脚本）
	helper := ghTokenHelperPath(bin)
	if !npmrcTokenHelperPathSafe(helper) {
		return fmt.Errorf("包装器路径含空白或 pnpm 保留字符，无法用 tokenHelper 表达：%s", helper)
	}
	path, err := writeGHTokenHelperScript(bin)
	if err != nil {
		return err
	}
	if err := upsertUserNpmrcLine(codeloadTokenHelperKey + path); err != nil {
		return err
	}
	// 自建中转 Worker（mirrorBase）同样要凭据：私有仓 tarball 走 `<base>/p/...`，
	// pnpm 需要该 host 的 token 才会带 Authorization（与 codeload 同一套 npmrc 机制）。
	if h := mirrorHost(); h != "" {
		if err := upsertUserNpmrcLineForKey("//"+h+"/:tokenHelper=", "//"+h+"/:tokenHelper="+path); err != nil {
			return err
		}
	}
	return nil
}

// mirrorHost 自建中转 Worker 的 host（未配置 mirrorBase 时为空）。
func mirrorHost() string {
	if mirrorBase == "" {
		return ""
	}
	u, err := url.Parse(mirrorBase)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// removeLegacyCodeloadTokenHelper 删除旧格式（值含空白 = 带参数）的 codeload tokenHelper 行。
// 无 gh 时用它兜底：旧格式会让 pnpm 10.34.5 下所有 pnpm 命令失败，必须清掉。
// 已是新格式（裸路径）或本来就没有该行时不改动文件。
func removeLegacyCodeloadTokenHelper() error {
	p, err := userNpmrcPath()
	if err != nil {
		return err
	}
	cur, rerr := os.ReadFile(p)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return nil
		}
		return rerr
	}
	next, changed := dropLegacyTokenHelperLine(string(cur))
	if !changed {
		return nil
	}
	return writeFileAtomic(p, next)
}

// dropLegacyTokenHelperLine 去掉值里含空白（带参数）的 tokenHelper 行，返回新内容与是否变更。
func dropLegacyTokenHelperLine(content string) (string, bool) {
	nl := "\n"
	if strings.Contains(content, "\r\n") {
		nl = "\r\n"
	}
	var lines []string
	if body := strings.TrimSuffix(content, "\n"); body != "" {
		lines = strings.Split(body, "\n")
	}
	out := make([]string, 0, len(lines))
	changed := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(strings.TrimSuffix(ln, "\r"))
		if strings.HasPrefix(trimmed, codeloadTokenHelperKey) {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, codeloadTokenHelperKey))
			if strings.ContainsAny(value, " \t") {
				changed = true
				continue // 旧格式：删
			}
		}
		out = append(out, strings.TrimSuffix(ln, "\r"))
	}
	if !changed {
		return content, false
	}
	if len(out) == 0 {
		return "", true
	}
	return strings.Join(out, nl) + nl, true
}

// upsertUserNpmrcLine 把单行配置写入/更新到用户级 npmrc（幂等 + 原子写，保留其它配置）。
func upsertUserNpmrcLine(line string) error {
	return upsertUserNpmrcLineForKey(codeloadTokenHelperKey, line)
}

// upsertUserNpmrcLineForKey 同 upsertUserNpmrcLine，但按任意 key 前缀匹配（多 host 凭据用）。
func upsertUserNpmrcLineForKey(key, line string) error {
	p, err := userNpmrcPath()
	if err != nil {
		return err
	}
	cur := ""
	if b, rerr := os.ReadFile(p); rerr == nil {
		cur = string(b)
	} else if !os.IsNotExist(rerr) {
		return rerr
	}
	next, changed := upsertNpmrcKeyedLine(key, cur, line)
	if !changed {
		return nil
	}
	return writeFileAtomic(p, next)
}

// writeFileAtomic 先写同目录临时文件再改名，避免写一半留下残缺文件。
func writeFileAtomic(p, content string) error {
	tmp := p + ".dsh-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// userNpmrcPath 用户级 npmrc 路径（npm 的 userconfig）。
func userNpmrcPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("无法确定用户主目录")
	}
	return filepath.Join(home, ".npmrc"), nil
}

// npmrcTokenHelperPathSafe tokenHelper 的值（此处为包装器路径）能否被 pnpm 表达：
// pnpm 按空白切分该配置，并禁止 $ % ` " ' 这些字符（parseCreds.js 的 RESERVED_CHARACTERS），
// 因此路径带空格/特殊字符时无法配置，只能让用户改用不含特殊字符的目录。
func npmrcTokenHelperPathSafe(bin string) bool {
	if bin == "" || strings.TrimSpace(bin) != bin {
		return false
	}
	if strings.ContainsAny(bin, " \t") {
		return false
	}
	return !strings.ContainsAny(bin, "$%`\"'")
}

// upsertNpmrcTokenHelper 在 npmrc 文本里写入或更新 codeload 的 tokenHelper 行（兼容既有调用）。
func upsertNpmrcTokenHelper(content, line string) (string, bool) {
	return upsertNpmrcKeyedLine(codeloadTokenHelperKey, content, line)
}

// upsertNpmrcKeyedLine 在 npmrc 文本里写入或更新以 key 开头的行，返回新内容与是否变更。
// 只动匹配 key 的那一行：同键历史重复行合并为一行，其余内容原样保留（含原有换行风格）。
// key 泛化是为了支持多个 host 的凭据（codeload 与自建中转 Worker 各一行）。
func upsertNpmrcKeyedLine(key, content, line string) (string, bool) {
	nl := "\n"
	if strings.Contains(content, "\r\n") {
		nl = "\r\n"
	}
	var lines []string
	if body := strings.TrimSuffix(content, "\n"); body != "" {
		lines = strings.Split(body, "\n")
	}
	out := make([]string, 0, len(lines)+1)
	replaced := false
	for _, ln := range lines {
		ln = strings.TrimSuffix(ln, "\r")
		if strings.HasPrefix(strings.TrimSpace(ln), key) {
			if replaced {
				continue // 历史重复行：合并为一行
			}
			out = append(out, line)
			replaced = true
			continue
		}
		out = append(out, ln)
	}
	if !replaced {
		out = append(out, line)
	}
	next := strings.Join(out, nl) + nl
	if next == content {
		return content, false
	}
	return next, true
}

// ghVerifyURLRe 从 gh 输出里取设备授权页地址。
// gh 在非 TTY 环境（本程序以管道 stdio 运行它）**不会自己拉起浏览器**，只打印：
//
//	! One-time code (D41C-AB86) copied to clipboard
//	Open this URL to continue in your web browser: https://github.com/login/device
//
// 所以浏览器由本程序代为拉起（2026-09-13 实证）。
var ghVerifyURLRe = regexp.MustCompile(`https://\S+`)

// ghVerifyURLFromOutput 从 gh 输出里取设备授权页地址；没有合法地址时返回空串。
// 只认设备授权页本身（github.com/login/device）：gh 输出里别的 github.com 链接都不是它——
// 升级提示指向 release 页，而设备码请求失败时打印的是 Go 的 *url.Error，里面那个
// https://github.com/login/device/code 是 **API 端点**。旧逻辑按 host 判定，把 API 端点
// 当授权页打开（2026-09-14 本机日志实证：opening https://github.com/login/device/code": ，
// 一次性代码长度 0）：用户看到 API 页面、无从填码，整条授权流程停在死路上。
func ghVerifyURLFromOutput(out string) string {
	for _, u := range ghVerifyURLRe.FindAllString(out, -1) {
		u = strings.TrimRight(u, `.,;:)]}"'`) // 去掉行尾标点与引号（错误文本里 URL 后面常跟 ": ）
		if ghIsDeviceVerifyURL(u) {
			return u
		}
	}
	return ""
}

// ghIsDeviceVerifyURL 是否为设备授权页：host=github.com 且 path=/login/device。
func ghIsDeviceVerifyURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return false
	}
	return strings.EqualFold(strings.TrimRight(u.Path, "/"), "/login/device")
}

// ghDeviceCodeRe 抓一次性授权码（形如 D41C-AB86：4 位 + 连字符 + 4 位，字母数字）。
// gh 在非 TTY 下只把它打进输出并（尝试）复制到剪贴板，不会显示在窗口里——
// 剪贴板可能被覆盖，所以必须解析出来展示给用户（2026-09-13 需求）。
var ghDeviceCodeRe = regexp.MustCompile(`(?i)\b([0-9A-Z]{4}-[0-9A-Z]{4})\b`)

// ghDeviceCodeFromOutput 从 gh 输出里取一次性授权码；取不到返回空串。
func ghDeviceCodeFromOutput(out string) string {
	if m := ghDeviceCodeRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// ghDeviceLogin 跑 gh 设备流授权，起流失败（用户还没看到代码，通常是网络瞬断）时自动重试。
// onWait(url, code) 在抓到授权页地址时回调一次。onAttempt(attempt, total) 每次尝试开始前
// 回调（nil 表示不关心），供调用方展示「正在重连」而不是干等。
func ghDeviceLogin(bin string, onWait func(url, code string), onAttempt func(attempt, total int)) error {
	var lastErr error
	for attempt := 1; attempt <= ghAuthStartAttempts; attempt++ {
		if onAttempt != nil {
			onAttempt(attempt, ghAuthStartAttempts)
		}
		shown, err := ghDeviceFlowOnce(bin, onWait)
		if err == nil {
			return nil
		}
		lastErr = err
		// 代码/地址已经给到用户后不再重试：此时失败属于「用户没在窗口期内完成」，
		// 重试会换一个新码，只会让用户手里的码失效。
		if shown || attempt == ghAuthStartAttempts {
			return err
		}
		logWarn("app", "github device flow start failed (attempt %d/%d), retrying: %s",
			attempt, ghAuthStartAttempts, strings.SplitN(err.Error(), "\n", 2)[0])
		time.Sleep(ghAuthRetryDelay)
	}
	return lastErr
}

// ghDeviceLoginOnce 单次设备流授权：等 gh 打出一次性代码与授权页地址后，由本程序
// 打开浏览器（并把代码写进剪贴板），再等待用户在浏览器内完成授权。
// 输出不含凭据（token 由 gh 写入系统凭据库，不经过这里）。
// 返回 shown=是否已把授权页地址与代码展示给用户（决定上层能否重试）。
// SysProcAttr.HideWindow 必须设置：否则 Windows 会为 gh.exe 新建一个命令行窗口，
// 用户看到的是黑窗口而不是浏览器（2026-09-13 实证）。
func ghDeviceLoginOnce(bin string, onWait func(url, code string)) (shown bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghAuthFlowTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "auth", "login",
		"--hostname", "github.com", "--git-protocol", "https", "--web", "--clipboard")
	cmd.SysProcAttr = ghSysProcAttr()
	cmd.Stdin = strings.NewReader("\n") // 非交互运行：用回车跳过 "Press Enter" 提示

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("无法建立 GitHub 授权输出通道：%w", err)
	}
	cmd.Stderr = cmd.Stdout // gh 的提示统一走 stderr，合并后一次扫描
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("无法启动 GitHub 授权：%w", err)
	}

	var (
		buf     bytes.Buffer
		tailLn  []string
		scanMu  sync.Mutex
		opened  bool
		scanErr error
	)
	// 边读边扫：gh 打印完地址就阻塞等授权，不能等进程结束再解析。
	scan := func(ev string) {
		scanMu.Lock()
		defer scanMu.Unlock()
		if ev != "" {
			tailLn = append(tailLn, ev)
			if len(tailLn) > 4 {
				tailLn = tailLn[len(tailLn)-4:]
			}
		}
		if opened {
			return
		}
		if u := ghVerifyURLFromOutput(buf.String()); u != "" {
			opened = true
			code := ghDeviceCodeFromOutput(buf.String())
			log.Printf("github device flow: opening %s (code length=%d)", u, len(code))
			openBrowser(u)
			copyToClipboard(code) // gh 自己的 --clipboard 是子进程，这里自己来更可靠
			if onWait != nil {
				go onWait(u, code)
			}
		}
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			buf.WriteString(sc.Text())
			buf.WriteByte('\n')
			scan(sc.Text())
		}
		if err := sc.Err(); err != nil {
			scanMu.Lock()
			scanErr = err
			scanMu.Unlock()
		}
	}()

	err = cmd.Wait()
	scanMu.Lock()
	opened, out := opened, strings.Join(append([]string(nil), tailLn...), "\n")
	scanMu.Unlock()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return opened, fmt.Errorf("等待 GitHub 授权超时（%s 内未完成）。\n可稍后在终端执行 gh auth login 重试。", ghAuthFlowTimeout)
	case err != nil:
		return opened, fmt.Errorf("GitHub 授权未完成：%v\n%s", err, out)
	case !opened:
		// 没抓到地址：把 gh 的提示原样带出去，别让用户面对一句没有线索的失败。
		if scanErr != nil {
			return false, fmt.Errorf("GitHub 授权流程异常：%v\n%s", scanErr, out)
		}
		return false, fmt.Errorf("未能获取 GitHub 授权页地址。\n%s", out)
	}
	return true, nil
}

// startGHDownload 首次授权前下载 gh 便携版：在设置窗口的进度视图里展示下载／安装进度
// （与 harness 更新同一个 splash 进度界面），完成后隐藏窗口并返回可执行文件路径。
// 同一时刻只允许一个下载：重复点击复用同一个结果。
// 关键在于**失败即复位**：留下 ghDownloadDone 会让失败结果被后续所有点击复用，
// 用户重试「检查更新」永远看到同一条超时错误（2026-09-13 实测）。
func startGHDownload() (string, error) {
	ghDownloadMu.Lock()
	if ghDownloadDone != nil {
		done := ghDownloadDone
		ghDownloadMu.Unlock()
		<-done
		ghDownloadMu.Lock()
		defer ghDownloadMu.Unlock()
		err := ghDownloadErr
		if err != nil {
			// 失败结果不保留：清掉后由下一次点击重新发起下载。
			ghDownloadDone, ghDownloadErr = nil, nil
			return "", err
		}
		return ghBinaryCache, nil
	}
	done := make(chan struct{})
	ghDownloadDone, ghDownloadErr = done, nil
	ghDownloadMu.Unlock()

	logUI("下载 GitHub CLI", "首次授权私有仓库插件所需（约 15 MB）")
	if appCtx != nil {
		wruntime.WindowShow(appCtx)
		ensureMainWindowForeground()
	}
	splash := startSplash(T("正在下载 GitHub CLI（首次约 15 MB）…"))
	go func() {
		_, err := ensureGHTool(func(pct float64) {
			splash.Update(fmt.Sprintf(T("正在下载 GitHub CLI（%.0f%%）…"), pct*100),
				ghDownloadFrom+(ghDownloadTo-ghDownloadFrom)*pct)
		})
		// 先落结果再复位状态：等待方被唤醒时一定能读到本次结果。
		ghDownloadMu.Lock()
		ghDownloadErr = err
		if err != nil {
			ghDownloadDone = nil // 失败复位：下一次点击重新下载
		}
		ghDownloadMu.Unlock()
		if err == nil {
			splash.Update(T("GitHub CLI 已就绪"), 1)
		}
		splash.Close()
		notifySplashDone()
		close(done)
	}()

	<-done
	ghDownloadMu.Lock()
	defer ghDownloadMu.Unlock()
	return ghBinaryCache, ghDownloadErr
}

// promptGitHubAuth 私有仓库检查更新前的授权引导（用户点「检查更新」触发）：
// 已登录→直接放行；未登录→询问并跑设备流授权，成功返回 true（调用方据此重试检查）。
func promptGitHubAuth(plugin, repo string) bool {
	if !ghAuthFlowMu.TryLock() {
		showMessageBox(T("GitHub 授权流程正在进行中，请先在浏览器中完成。"), appName)
		return false
	}
	defer ghAuthFlowMu.Unlock()
	if ghAuthToken() != "" {
		return true
	}
	// 授权确认走本程序自绘弹窗（平台层 askGitHubAuth）而非 Wails MessageDialog：
	// 后者在 Windows 上只是 Win32 MessageBoxW（MB_YESNO），**忽略自定义 Buttons**，
	// 按钮恒为「是/否」——「登录 GitHub」按钮永远不会出现（2026-09-13 实证）。
	msg := ghAuthPromptMsg(plugin, repo)
	if !askGitHubAuth(msg) {
		return false
	}
	bin := findGHBinary()
	if bin == "" {
		b, derr := startGHDownload()
		if derr != nil {
			showMessageBox(T("GitHub CLI 下载失败：")+"\n"+derr.Error()+"\n\n请稍后重试「检查更新」。", appName)
			return false
		}
		bin = b
	}
	// 授权在浏览器里完成，systray 只等结果：把设置窗口切到进度视图，避免用户对着
	// 无反馈的界面猜发生了什么（关闭浏览器授权页或超时前一直停在这一步）。
	splash := startSplash(T("正在准备 GitHub 授权…"))
	err := ghDeviceLogin(bin, func(url, code string) {
		// 授权码必须显示出来：网页要用户手工填入，剪贴板可能被覆盖。
		// 状态行是单行（前端 textContent + 不保留换行），所以代码与说明同行、用括号框住。
		if code == "" {
			// 没解析到代码（gh 输出格式变动）：至少引导用户去浏览器完成，不显示空的【】。
			splash.Update(T("请在浏览器中完成 GitHub 授权（一次性代码已复制到剪贴板）。"), 0.33)
		} else {
			splash.Update(TF("请在浏览器中填入一次性代码：【%s】", code), 0.33)
		}
		logUI("打开 GitHub 授权页", "一次性代码 "+orDash(code)+"，地址 "+url)
	}, func(attempt, total int) {
		// 起流重试期间给可见反馈：本机到 github.com 的连接会周期性被阻断，第一次尝试
		// 可能要等 20s 才失败（2026-09-14 实证），没有提示用户只会以为界面卡死。
		if attempt > 1 {
			splash.Update(TF("正在重新连接 GitHub（第 %d/%d 次尝试）…", attempt, total), 0.05)
		}
	})
	splash.Close()
	notifySplashDone()
	if err != nil {
		// 失败也要留痕：此前只有成功路径写日志，出问题时日志页只剩「仓库不可见」，
		// 看不出授权卡在哪一步（2026-09-14 排障实证）。
		logUI("GitHub 授权失败", strings.SplitN(err.Error(), "\n", 2)[0])
		if appCtx != nil {
			wruntime.WindowShow(appCtx)
			ensureMainWindowForeground()
		}
		showMessageBox(err.Error(), appName)
		return false
	}
	invalidateGHToken()
	if ghAuthToken() == "" {
		showMessageBox(T("授权已完成，但未能读取到凭据。\n可在终端执行 gh auth status 查看登录状态。"), appName)
		return false
	}
	// 授权成功顺手把「更新」需要的凭据出口接好（git 凭据助手 + npmrc tokenHelper）：
	// 检查更新只用内存 token，但 pnpm 拉私有仓库包要走这两个出口。
	if ensureGitHubPrivateRepoCreds() {
		logUI("GitHub 授权成功", "私有仓库插件的检查更新与更新拉包均已启用")
	} else {
		logUI("GitHub 授权成功", "检查更新已启用；拉包凭据未就绪（见上方告警，更新可能失败）")
	}
	return true
}

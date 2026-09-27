package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportImportPipeline(t *testing.T) {
	root := t.TempDir()
	homeA := filepath.Join(root, "homeA")
	// 源环境：sessions（两个 scope 各一个会话）+ 命名 profile（web）的 plugins + 一个用户目录
	if err := os.MkdirAll(filepath.Join(homeA, "sessions", "--S1--", "session-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeA, "sessions", "--S1--", "session-1", "session.jsonl.zstd"), []byte("data1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(homeA, "sessions", "--S2--", "session-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeA, "sessions", "--S2--", "session-2", "session.jsonl.zstd"), []byte("data2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(homeA, "profiles", "web", "node_modules", "pkg-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeA, "profiles", "web", "node_modules", "pkg-a", "index.js"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeA, "profiles", "web", "package.json"), []byte(`{"dependencies":{"pkg-a":"1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(root, "mydocs")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "note.txt"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DSH_HOME", homeA)

	destDir := filepath.Join(root, "exports")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := buildExportZip(true, true, true, []string{userDir}, destDir, nil, "")
	if err != nil {
		t.Fatalf("buildExportZip: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(p), "dsh-systray-export-") || !strings.HasSuffix(p, ".zip") {
		t.Fatalf("bad export name: %s", p)
	}

	items, err := parseExportZip(p)
	if err != nil {
		t.Fatalf("parseExportZip: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %v", items)
	}
	kinds := map[string]bool{}
	for _, it := range items {
		kinds[it.Kind] = true
	}
	for _, k := range []string{"sessions", "plugins", "files"} {
		if !kinds[k] {
			t.Fatalf("missing kind %s in %v", k, kinds)
		}
	}

	// 恢复到全新的 homeB
	homeB := filepath.Join(root, "homeB")
	t.Setenv("DSH_HOME", homeB)
	if n, err := countRestoreConflicts("sessions", mustInner(t, p, exportZipSessions)); err != nil || n != 0 {
		t.Fatalf("conflicts in fresh home: n=%d err=%v", n, err)
	}
	if _, err := restoreItem("sessions", mustInner(t, p, exportZipSessions), "", true, nil); err != nil {
		t.Fatalf("restore sessions: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeB, "sessions", "--S1--", "session-1", "session.jsonl.zstd")); err != nil {
		t.Fatalf("restored session missing: %v", err)
	}
	if _, err := restoreItem("plugins", mustInner(t, p, exportZipPlugins), "", true, nil); err != nil {
		t.Fatalf("restore plugins: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeB, "profiles", "web", "node_modules", "pkg-a", "index.js")); err != nil {
		t.Fatalf("restored plugin missing: %v", err)
	}
	filesDest := filepath.Join(root, "files-restored")
	if _, err := restoreItem("files", mustInner(t, p, exportZipFiles), filesDest, true, nil); err != nil {
		t.Fatalf("restore files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filesDest, "mydocs", "note.txt")); err != nil {
		t.Fatalf("restored file missing: %v", err)
	}

	// 冲突检测：homeB 已有数据，再次统计应 >0
	if n, err := countRestoreConflicts("sessions", mustInner(t, p, exportZipSessions)); err != nil || n == 0 {
		t.Fatalf("expected conflicts, n=%d err=%v", n, err)
	}
	// 跳过已有：不覆盖
	if _, err := restoreItem("sessions", mustInner(t, p, exportZipSessions), "", false, nil); err != nil {
		t.Fatalf("restore sessions skip: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(homeB, "sessions", "--S1--", "session-1", "session.jsonl.zstd"))
	if string(data) != "data1" {
		t.Fatalf("skip-existing overwrote data: %q", data)
	}
	// 覆盖更新：内容替换
	if _, err := restoreItem("sessions", mustInner(t, p, exportZipSessions), "", true, nil); err != nil {
		t.Fatalf("restore sessions overwrite: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(homeB, "sessions", "--S1--", "session-1", "session.jsonl.zstd"))
	if string(data) != "data1" { // 内容相同，但路径完整即可
		t.Fatalf("overwrite data mismatch: %q", data)
	}

	// 解析异常：非导出包
	if _, err := parseExportZip(filepath.Join(root, "mydocs", "note.txt")); err == nil {
		t.Fatal("expected parse error for non-zip")
	}
}

func mustInner(t *testing.T, master, name string) string {
	t.Helper()
	p, cleanup, err := extractInnerZip(master, name)
	if err != nil {
		t.Fatalf("extractInnerZip %s: %v", name, err)
	}
	t.Cleanup(cleanup)
	return p
}

// TestExportSkipsMissingPlugins 回归：勾选「已安装的插件」但机器上没有通过 dsh add 安装任何插件时，
// 导出不应中断（此前会硬报错并作废同时勾选的会话），而是跳过插件导出其余内容。
func TestExportSkipsMissingPlugins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "sessions", "--S1--", "session-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sessions", "--S1--", "session-1", "session.jsonl.zstd"), []byte("data1"), 0o644); err != nil {
		t.Fatal(err)
	}
	destDir := filepath.Join(home, "exports")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// sessions + plugins（但无插件）→ 应成功，只含会话
	p, err := buildExportZip(true, true, false, nil, destDir, nil, "")
	if err != nil {
		t.Fatalf("export with missing plugins should not abort: %v", err)
	}
	items, err := parseExportZip(p)
	if err != nil {
		t.Fatalf("parseExportZip: %v", err)
	}
	if len(items) != 1 || items[0].Kind != "sessions" {
		t.Fatalf("expected only sessions item, got %v", items)
	}

	// 只勾选插件（且无插件）→ 应报「没有可导出的内容」而非「没有通过 dsh add 安装的插件」
	if _, err := buildExportZip(false, true, false, nil, destDir, nil, ""); err == nil {
		t.Fatal("expected error when only empty plugins selected")
	} else if strings.Contains(err.Error(), "没有通过 dsh add 安装的插件") {
		t.Fatalf("should report nothing-to-export, got: %v", err)
	}
}

// TestAskStopServerForQuitSkipsPromptInDesktopMode desktop 启动方式下退出托盘不询问「是否保留
// 后台服务」：直接按「停止并退出」处理（服务不在运行，问了也没有可保留的对象）。
func TestAskStopServerForQuitSkipsPromptInDesktopMode(t *testing.T) {
	stubDesktopPref(t, launchTargetDesktop)
	if got := askStopServerForQuit(); got != 0 {
		t.Fatalf("desktop 模式退出不应询问：应得 0（停止并退出），实际 %d", got)
	}
}

// stubDesktopPref 固定启动方式解析 + 指向未监听的端口（不触碰真实桌面端检测与真实服务）。
func stubDesktopPref(t *testing.T, pref string) {
	t.Helper()
	stubDesktopInstalled(t, true)
	oldPref, oldPort, oldURL, oldStarted := launchTargetPref, port, webURL, serverStartedPort
	oldReady := serverReady.Load()
	launchTargetPref = pref
	port, webURL, serverStartedPort = 9, "http://127.0.0.1:9/", 0
	t.Cleanup(func() {
		launchTargetPref, port, webURL, serverStartedPort = oldPref, oldPort, oldURL, oldStarted
		serverReady.Store(oldReady)
	})
}

// TestFinishPluginImportDesktopSkipsServiceVerify desktop 启动方式下导入收尾只清事务快照、
// 不拉起后台服务（其加载由官方桌面端负责），并如实说明「改动在桌面端重启后生效」。
func TestFinishPluginImportDesktopSkipsServiceVerify(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	stubDesktopPref(t, launchTargetDesktop)

	dir := filepath.Join(home, "profiles", "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// 事务快照残骸：收尾必须清理（否则下次启动仍被当作未完成事务）
	if err := os.WriteFile(filepath.Join(dir, "package.json"+importBakSuffix), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	note, err := finishPluginImport([]string{dir}, []bool{false})
	if err != nil {
		t.Fatalf("desktop 模式导入收尾不应失败: %v", err)
	}
	if !strings.Contains(note, "Desktop UI") {
		t.Fatalf("成功说明应点明跳过启动校验，实际 %q", note)
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json"+importBakSuffix)); !os.IsNotExist(err) {
		t.Fatal("导入事务快照应已清理")
	}
	if serverCmd != nil {
		t.Fatal("desktop 模式不应拉起后台服务进程")
	}
}

// TestResumeServiceAfterRestoreStaysStoppedInDesktopMode 恢复完成后 desktop 模式不拉起后台服务：
// 状态落到「已停止」（serverReady=true）而不是被拉起的服务覆盖。
func TestResumeServiceAfterRestoreStaysStoppedInDesktopMode(t *testing.T) {
	stubDesktopPref(t, launchTargetDesktop)
	serverReady.Store(false)

	resumeServiceAfterRestore()

	if serverCmd != nil {
		t.Fatal("desktop 模式不应拉起后台服务")
	}
	if !serverReady.Load() {
		t.Fatal("恢复完成后服务应显示「已停止」")
	}
}

// TestRecoverProfileTransactionsIdempotent 事务自愈重复执行不会把刚还原的 node_modules 删掉：
// 快照还原是「删活体 + 改名回填」的非幂等操作，而多条启动路径都会调用它（见 profileRecoveryMu）。
func TestRecoverProfileTransactionsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	dir := filepath.Join(home, "profiles", "web")
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "pkg-live"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "pkg-live", "index.js"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 快照 = 操作前状态：node_modules 改名暂存 + 声明文件备份
	if err := os.Rename(filepath.Join(dir, "node_modules"), filepath.Join(dir, "node_modules"+pluginSnapSuffix)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"half":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"+pluginSnapSuffix), []byte(`{"snapshot":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "pkg-half"), 0o755); err != nil {
		t.Fatal(err) // 快照之后又改过的半装活体树
	}

	recoverProfileTransactions(nil)
	recoverProfileTransactions(nil) // 重复执行：第二条启动路径也会调用

	if _, err := os.Stat(filepath.Join(dir, "node_modules", "pkg-half")); !os.IsNotExist(err) {
		t.Fatal("半装活体树应被快照还原替换掉")
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "pkg-live", "index.js")); err != nil {
		t.Fatalf("快照内容应已还原：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules"+pluginSnapSuffix)); !os.IsNotExist(err) {
		t.Fatal("快照残骸应已清理")
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil || !strings.Contains(string(data), "snapshot") {
		t.Fatalf("声明文件应还原为快照内容：%v (%s)", err, data)
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFileUnder 写文件并建好父目录（测试用；writeFile 本身不建目录）。
func writeFileUnder(t *testing.T, p, content string) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(p))
	writeFile(t, p, content)
}

// ==================== 插件操作批处理队列 ====================

// TestPluginOpPrepare 入队校验与任务分类：更新仅接受远程来源；删除对「待重指定」「无依赖的
// 自动禁用」行标记 recordOnly（不进事务、不停服）。
func TestPluginOpPrepare(t *testing.T) {
	remote := PluginRow{ID: "a", Name: "dsh-cost-meter", Source: "npm", CanUpdate: true}
	if task, why := pluginOpPrepare(remote, "update"); why != "" || task == nil || task.recordOnly {
		t.Fatalf("remote update should be accepted without recordOnly, got task=%v why=%q", task, why)
	}
	local := PluginRow{ID: "b", Name: "dsh-ui-taste", Source: "file", CanUpdate: false}
	if _, why := pluginOpPrepare(local, "update"); why == "" {
		t.Fatal("file-source update must be rejected (needs directory picker)")
	}
	pending := PluginRow{ID: "c", Name: "dsh-x", Source: "file", CanUpdate: false, PendingLocal: true}
	if _, why := pluginOpPrepare(pending, "update"); why == "" {
		t.Fatal("pending-local update must be rejected")
	}
	task, why := pluginOpPrepare(pending, "remove")
	if why != "" || task == nil || !task.recordOnly {
		t.Fatalf("pending-local remove should be recordOnly, got task=%v why=%q", task, why)
	}
	ghost := PluginRow{ID: "d", Name: "dsh-ghost", GhostDisabled: true}
	task, why = pluginOpPrepare(ghost, "remove")
	if why != "" || task == nil || !task.recordOnly {
		t.Fatalf("ghost-disabled remove should be recordOnly, got task=%v why=%q", task, why)
	}
	real := PluginRow{ID: "e", Name: "dsh-real", Source: "github", CanUpdate: true, Locs: []string{"C:/x"}}
	if task, why = pluginOpPrepare(real, "remove"); why != "" || task.recordOnly {
		t.Fatalf("real remove should enter transaction, got task=%v why=%q", task, why)
	}
	if _, why = pluginOpPrepare(real, "frobnicate"); why == "" {
		t.Fatal("unknown op must be rejected")
	}
}

// TestPluginBatchRollbackOrder 逐项快照 + 逆序回退：同一 profile 上先后两项操作各自快照，
// 整批回退时逆序还原，最终必须回到批处理前状态（多插件同环境场景）。
func TestPluginBatchRollbackOrder(t *testing.T) {
	dir := t.TempDir()
	writeFileUnder(t, filepath.Join(dir, "package.json"), `{"dependencies":{"a":"1.0.0"}}`)
	writeFileUnder(t, filepath.Join(dir, "node_modules", "a", "package.json"), `{"name":"a","version":"1.0.0"}`)

	// 第一项：快照 → 改 a 到 2.0.0
	t1, why := pluginOpPrepare(PluginRow{ID: "a", Name: "a", Source: "npm", CanUpdate: true, Locs: []string{dir}}, "update")
	if why != "" {
		t.Fatalf("prepare t1: %s", why)
	}
	t1.hadNM = append(t1.hadNM, snapshotPluginProfileSuffix(dir, t1.snap))
	writeFileUnder(t, filepath.Join(dir, "package.json"), `{"dependencies":{"a":"2.0.0"}}`)
	writeFileUnder(t, filepath.Join(dir, "node_modules", "a", "package.json"), `{"name":"a","version":"2.0.0"}`)

	// 第二项：快照（此刻含第一项的改动）→ 加 b
	t2, why := pluginOpPrepare(PluginRow{ID: "b", Name: "b", Source: "npm", CanUpdate: true, Locs: []string{dir}}, "update")
	if why != "" {
		t.Fatalf("prepare t2: %s", why)
	}
	if t1.snap == t2.snap {
		t.Fatalf("每项必须分配独立快照后缀，否则同环境多项快照互相覆盖（got %q）", t1.snap)
	}
	t2.hadNM = append(t2.hadNM, snapshotPluginProfileSuffix(dir, t2.snap))
	writeFileUnder(t, filepath.Join(dir, "node_modules", "b", "package.json"), `{"name":"b","version":"1.0.0"}`)

	// 逆序回退（与 rollbackPluginBatch 同序）
	for _, task := range []*pluginOpTask{t2, t1} {
		restorePluginTaskSnapshot(task)
	}
	if got := readFile(t, filepath.Join(dir, "package.json")); got != `{"dependencies":{"a":"1.0.0"}}` {
		t.Fatalf("package.json not restored: %s", got)
	}
	if got := readFile(t, filepath.Join(dir, "node_modules", "a", "package.json")); got != `{"name":"a","version":"1.0.0"}` {
		t.Fatalf("plugin a not restored: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "b")); !os.IsNotExist(err) {
		t.Fatal("plugin b should be gone after rolling back the later task")
	}
}

// TestBatchTaskDirs 批内目录收集去重（自愈范围）。
func TestBatchTaskDirs(t *testing.T) {
	tasks := []*pluginOpTask{
		{locs: []string{"A", "B"}},
		{locs: []string{"B", "C"}},
	}
	got := batchTaskDirs(tasks)
	want := []string{"A", "B", "C"}
	if len(got) != len(want) {
		t.Fatalf("batchTaskDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("batchTaskDirs = %v, want %v", got, want)
		}
	}
}

// ==================== harness 更新：安装结果判定 ====================

// TestHarnessFamilyMismatches 家族版本一致性检查：顶层已装包版本不一致即计入；
// 未在顶层安装的家族包（纯传递依赖）与无法比对的目标（latest）不计入。
func TestHarnessFamilyMismatches(t *testing.T) {
	dir := t.TempDir()
	writeFileUnder(t, filepath.Join(dir, "node_modules", "@deepseek-ai", "dsh", "package.json"),
		`{"name":"@deepseek-ai/dsh","version":"0.1.5-rc.2"}`)
	writeFileUnder(t, filepath.Join(dir, "node_modules", "@deepseek-ai", "dsh-llm", "package.json"),
		`{"name":"@deepseek-ai/dsh-llm","version":"0.1.1-rc.2"}`)
	// 仅出现在锁文件、未在顶层安装的家族包：不作为不一致证据
	writeFileUnder(t, filepath.Join(dir, "pnpm-lock.yaml"),
		"'@deepseek-ai/dsh-agent@0.1.5-rc.2':\n  resolution: {integrity: sha}\n")

	bad := harnessFamilyMismatches(dir, "0.1.5-rc.2")
	if len(bad) != 1 || bad[0] != "@deepseek-ai/dsh-llm@0.1.1-rc.2" {
		t.Fatalf("mismatches = %v, want only dsh-llm", bad)
	}
	if bad := harnessFamilyMismatches(dir, "v0.1.5-rc.2"); len(bad) != 1 {
		t.Fatalf("v-prefixed target should compare equal, got %v", bad)
	}
	if bad := harnessFamilyMismatches(dir, "latest"); bad != nil {
		t.Fatalf("latest target must skip the check, got %v", bad)
	}
	// 入口包缺失：必须计入（树不可用）
	dir2 := t.TempDir()
	if bad := harnessFamilyMismatches(dir2, "0.1.5-rc.2"); len(bad) != 1 {
		t.Fatalf("missing @deepseek-ai/dsh must be reported, got %v", bad)
	}
}

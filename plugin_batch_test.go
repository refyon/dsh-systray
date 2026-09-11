package main

import (
	"archive/zip"
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

// ==================== 待应用变更（登记 → 应用） ====================

// TestPendingUpsertLastWins 同一插件重复登记按最后操作生效：同操作幂等、换操作覆盖。
func TestPendingUpsertLastWins(t *testing.T) {
	upd := &pluginOpTask{id: "a", name: "dsh-x", op: "update"}
	list, replaced := pendingUpsertTask(nil, upd)
	if len(list) != 1 || replaced {
		t.Fatalf("首次登记应新增，got len=%d replaced=%v", len(list), replaced)
	}
	// 同操作重复登记：幂等（列表不变、不覆盖）
	list, replaced = pendingUpsertTask(list, &pluginOpTask{id: "a", name: "dsh-x", op: "update"})
	if len(list) != 1 || replaced {
		t.Fatalf("同操作重复登记应幂等，got len=%d replaced=%v", len(list), replaced)
	}
	// 换操作：覆盖原条目（最后操作生效）
	list, replaced = pendingUpsertTask(list, &pluginOpTask{id: "a", name: "dsh-x", op: "remove"})
	if len(list) != 1 || !replaced || list[0].op != "remove" {
		t.Fatalf("换操作应覆盖为 remove，got len=%d replaced=%v op=%s", len(list), replaced, list[0].op)
	}
	// 不同插件：各自独立
	list, _ = pendingUpsertTask(list, &pluginOpTask{id: "b", name: "dsh-y", op: "update"})
	if len(list) != 2 {
		t.Fatalf("不同插件应各自保留，got len=%d", len(list))
	}
}

// TestPendingDropNames 导入恢复对账：本次恢复到的插件，其先前登记的变更一律作废（按最后操作
// 生效——先登记删除、随后又导回来，则不应再删）；其它插件不受影响。
func TestPendingDropNames(t *testing.T) {
	list := []*pluginOpTask{
		{id: "a", name: "dsh-x", op: "remove"},
		{id: "b", name: "dsh-y", op: "update"},
		{id: "c", name: "dsh-x", op: "update"},
	}
	kept, dropped := pendingDropNames(list, []string{"dsh-x"})
	if len(kept) != 1 || kept[0].name != "dsh-y" {
		t.Fatalf("只应保留未重新导入的插件，got %v", len(kept))
	}
	if len(dropped) != 2 {
		t.Fatalf("应作废 dsh-x 的两条登记，got %v", dropped)
	}
	if dropped[0] != "dsh-x（删除）" || dropped[1] != "dsh-x（更新）" {
		t.Fatalf("作废文案应含操作类型，got %v", dropped)
	}
	if kept2, dropped2 := pendingDropNames(list, nil); len(kept2) != 3 || len(dropped2) != 0 {
		t.Fatalf("空名单不应改动列表，got kept=%d dropped=%d", len(kept2), len(dropped2))
	}
}

// TestRestoredPluginNames 从导入包 manifest 取本次恢复的插件名：dependencies 与 bundles 并集、
// 去重、去除 name@version 变体、过滤官方包。
func TestRestoredPluginNames(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "master.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{"format":"dsh-systray-export","version":1,"plugins":{"profile":"web",` +
		`"dependencies":{"dsh-x":"^1.0.0","dsh-y":"github:o/r","@deepseek-ai/dsh-base":"0.1.5-rc.2"},` +
		`"bundles":["dsh-x@1.0.0","dsh-z"]}}`
	if _, err := w.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got := restoredPluginNames(zipPath)
	want := []string{"dsh-x", "dsh-y", "dsh-z"}
	if len(got) != len(want) {
		t.Fatalf("restoredPluginNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restoredPluginNames = %v, want %v", got, want)
		}
	}
	if names := restoredPluginNames(filepath.Join(dir, "missing.zip")); names != nil {
		t.Fatalf("缺失包应返回 nil，got %v", names)
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

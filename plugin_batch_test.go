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

// ==================== 启用并入批量操作 ====================

// TestPluginOpPrepareEnable 启用登记校验：仅接受「已禁用且仍有依赖声明」的行——未禁用的行无事
// 可做，无依赖声明的自动禁用记录（ghost）没有可加回的声明（应直接删除记录），二者都必须拒绝。
func TestPluginOpPrepareEnable(t *testing.T) {
	disabled := PluginRow{ID: "a", Name: "dsh-x", Source: "github", Disabled: true, Locs: []string{"C:/x"}}
	task, why := pluginOpPrepare(disabled, "enable")
	if why != "" || task == nil {
		t.Fatalf("禁用行的启用应被受理，got task=%v why=%q", task, why)
	}
	if !task.pkgOnly || task.recordOnly {
		t.Fatalf("启用必须 pkgOnly（只改声明）且非 recordOnly（需参与批末重启校验），got pkgOnly=%v recordOnly=%v",
			task.pkgOnly, task.recordOnly)
	}
	enabled := PluginRow{ID: "b", Name: "dsh-y", Source: "github"}
	if _, why = pluginOpPrepare(enabled, "enable"); why == "" {
		t.Fatal("未禁用行的启用必须拒绝")
	}
	ghost := PluginRow{ID: "c", Name: "dsh-ghost", GhostDisabled: true, Disabled: true}
	if _, why = pluginOpPrepare(ghost, "enable"); why == "" {
		t.Fatal("无依赖声明的自动禁用记录必须拒绝启用（应直接删除记录）")
	}
}

// TestPendingEnableOps 启用与更新/删除同一套「最后操作生效」：同操作幂等、换操作覆盖；
// 导入恢复对账的作废文案带「（启用）」。
func TestPendingEnableOps(t *testing.T) {
	list, _ := pendingUpsertTask(nil, &pluginOpTask{id: "a", name: "dsh-x", op: "enable"})
	list, replaced := pendingUpsertTask(list, &pluginOpTask{id: "a", name: "dsh-x", op: "enable"})
	if len(list) != 1 || replaced {
		t.Fatalf("重复登记启用应幂等，got len=%d replaced=%v", len(list), replaced)
	}
	// 先启用后更新：更新吸收启用意图（最后操作生效）
	list, replaced = pendingUpsertTask(list, &pluginOpTask{id: "a", name: "dsh-x", op: "update"})
	if len(list) != 1 || !replaced || list[0].op != "update" {
		t.Fatalf("启用后登记更新应覆盖为 update，got len=%d replaced=%v op=%s", len(list), replaced, list[0].op)
	}
	kept, dropped := pendingDropNames([]*pluginOpTask{{id: "a", name: "dsh-x", op: "enable"}}, []string{"dsh-x"})
	if len(kept) != 0 || len(dropped) != 1 || dropped[0] != "dsh-x（启用）" {
		t.Fatalf("作废文案应含「（启用）」，got kept=%d dropped=%v", len(kept), dropped)
	}
}

// TestPluginEnablePhaseAndRollback 启用阶段只改 profile 声明（清禁用记录 + 加回激活清单），
// 不搬 node_modules、不发 pnpm；回退（pkgOnly）写回声明快照、node_modules 原样保留。
func TestPluginEnablePhaseAndRollback(t *testing.T) {
	dir := t.TempDir()
	writePkgJSON(t, dir, `{"name":"web","dependencies":{"alpha":"^1.0.0"},
	  "dsh":{"profile":{"bundles":[],"disabledPlugins":{"alpha":"启动日志存在加载错误"}}}}`)
	writeFileUnder(t, filepath.Join(dir, "node_modules", "alpha", "package.json"), `{"name":"alpha","version":"1.0.0"}`)

	row := PluginRow{ID: "alpha", Name: "alpha", Source: "github", Disabled: true, Locs: []string{dir}}
	task, why := pluginOpPrepare(row, "enable")
	if why != "" {
		t.Fatalf("prepare: %s", why)
	}
	runPluginEnablePhase(task, &SplashState{Update: func(string, float64) {}}, 1)
	if !task.ok {
		t.Fatalf("启用阶段应成功，got ok=%v reason=%s", task.ok, task.reason)
	}
	f := readPkgFixture(t, dir)
	if len(f.Dsh.Profile.DisabledPlugins) != 0 {
		t.Fatalf("禁用记录未清除：%+v", f.Dsh.Profile.DisabledPlugins)
	}
	if len(f.Dsh.Profile.Bundles) != 1 || f.Dsh.Profile.Bundles[0] != "alpha" {
		t.Fatalf("启用未加回激活清单：%+v", f.Dsh.Profile.Bundles)
	}
	// 只改声明：node_modules 必须原地未动（pkgOnly 快照不搬运目录）
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "alpha", "package.json")); err != nil {
		t.Fatalf("node_modules 应原地保留：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules"+task.snap)); !os.IsNotExist(err) {
		t.Fatal("pkgOnly 快照不应搬运 node_modules")
	}
	// 回退：写回声明快照 → 回到「禁用态」，node_modules 仍在
	restorePluginTaskSnapshot(task)
	f = readPkgFixture(t, dir)
	if f.Dsh.Profile.DisabledPlugins["alpha"] != "启动日志存在加载错误" {
		t.Fatalf("回退未恢复禁用记录：%+v", f.Dsh.Profile.DisabledPlugins)
	}
	if len(f.Dsh.Profile.Bundles) != 0 {
		t.Fatalf("回退未恢复激活清单：%+v", f.Dsh.Profile.Bundles)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "alpha", "package.json")); err != nil {
		t.Fatalf("回退不应动 node_modules：%v", err)
	}
}

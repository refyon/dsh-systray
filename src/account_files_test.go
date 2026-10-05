// account_files_test.go：文件同步引擎用例——扫描/排除、上传与删除传播、容量拦下、
// 对账动作过滤、应用（下载/冲突副本/远端删除/改条目名）与状态视图。
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 夹具 ----------

// setupFileSyncTest 隔离 filesync.json 与接收目录，并复位进程内状态（复用账号测试的隔离）。
func setupFileSyncTest(t *testing.T) string {
	t.Helper()
	setupAccountTest(t)
	dir := t.TempDir()
	oldStateDir, oldRecv := fileSyncStateDirValue(), fileSyncReceiveDirValue()
	setFileSyncStateDir(dir)
	setFileSyncReceiveDir(filepath.Join(dir, "recv"))
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{}
	fileSyncSyncing, fileSyncApplying = false, false
	fileSyncLastCheck = 0
	fileSyncMu.Unlock()
	t.Cleanup(func() {
		setFileSyncStateDir(oldStateDir)
		setFileSyncReceiveDir(oldRecv)
		fileSyncMu.Lock()
		fileSyncCur = fileSyncState{}
		fileSyncSyncing, fileSyncApplying = false, false
		fileSyncMu.Unlock()
	})
	return dir
}

func fsWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

func fsSetEntry(t *testing.T, entries ...fileSyncEntry) {
	t.Helper()
	fileSyncMu.Lock()
	fileSyncCur.Entries = entries
	fileSyncMu.Unlock()
}

func fsEntry(t *testing.T, id int) fileSyncEntry {
	t.Helper()
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	if id < 0 || id >= len(fileSyncCur.Entries) {
		t.Fatalf("条目下标越界: %d", id)
	}
	return fileSyncCur.Entries[id]
}

func fsSetPending(t *testing.T, actions ...fileSyncAction) {
	t.Helper()
	fileSyncMu.Lock()
	fileSyncCur.PendingApply = actions
	fileSyncMu.Unlock()
}

// ---------- 假文件服务端（端点 13-20 的最小实现） ----------

type fakeFilesServer struct {
	mu      sync.Mutex
	limit   int64
	used    int64
	rev     int64
	entries map[string]fileSyncRemoteEntry
	objects map[string][]byte
	meta    map[string]fileSyncLocalMeta
	actions []fileSyncAction
	uploads []string
	deletes []string
	gets    []string
	// failUploads 按 relPath 注入上传失败（验证「单文件失败不拖垮整批、下次重试」）。
	failUploads map[string]bool
}

func newFakeFilesServer() *fakeFilesServer {
	return &fakeFilesServer{
		limit:       10 * 1024 * 1024,
		entries:     map[string]fileSyncRemoteEntry{},
		objects:     map[string][]byte{},
		meta:        map[string]fileSyncLocalMeta{},
		failUploads: map[string]bool{},
	}
}

func fakeKey(entryID, rel string) string { return entryID + "\x00" + rel }

func (f *fakeFilesServer) putObject(entryID, rel string, data []byte, mtime int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rev++
	f.objects[fakeKey(entryID, rel)] = data
	f.meta[fakeKey(entryID, rel)] = fileSyncLocalMeta{
		EntryID: entryID, RelPath: rel, Size: int64(len(data)), Sha256: fileSyncHashBytes(data), Mtime: mtime,
	}
	f.used += int64(len(data))
}

func (f *fakeFilesServer) hasDelete(rel string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.deletes {
		if r == rel {
			return true
		}
	}
	return false
}

func (f *fakeFilesServer) uploadMeta(rel string) (fileSyncLocalMeta, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, m := range f.meta {
		if strings.HasSuffix(k, "\x00"+rel) {
			return m, true
		}
	}
	return fileSyncLocalMeta{}, false
}

func fsWriteJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("写响应失败: %v", err)
	}
}

func fsWriteErr(t *testing.T, w http.ResponseWriter, status int, code string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}

func (f *fakeFilesServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/v1/files/quota":
			fsWriteJSON(t, w, fileQuota{Used: f.used, Limit: f.limit, Tier: "free"})
		case "/v1/files/entries":
			var body fileSyncEntryBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			e := fileSyncRemoteEntry{ID: body.ID, Name: body.Name, Kind: body.Kind}
			f.entries[body.ID] = e
			fsWriteJSON(t, w, e)
		case "/v1/files/sync":
			var body fileSyncRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			entries := make([]fileSyncRemoteEntry, 0, len(f.entries))
			for _, e := range f.entries {
				entries = append(entries, e)
			}
			fsWriteJSON(t, w, fileSyncResponse{
				Entries: entries,
				Actions: f.actions,
				Quota:   fileQuota{Used: f.used, Limit: f.limit, Tier: "free"},
			})
		case "/v1/files/objects":
			entryID := r.URL.Query().Get("entryId")
			rel := r.URL.Query().Get("relPath")
			key := fakeKey(entryID, rel)
			switch r.Method {
			case http.MethodPut:
				data, _ := io.ReadAll(r.Body)
				sha := r.Header.Get("x-file-sha256")
				if sha != fileSyncHashBytes(data) {
					fsWriteErr(t, w, 400, "checksum_mismatch")
					return
				}
				if f.failUploads[rel] {
					fsWriteErr(t, w, 500, "server_error")
					return
				}
				if old, ok := f.meta[key]; ok {
					f.used -= old.Size
				}
				if f.used+int64(len(data)) > f.limit {
					fsWriteErr(t, w, 409, "quota_exceeded")
					return
				}
				mtime, _ := strconv.ParseInt(r.Header.Get("x-file-mtime"), 10, 64)
				f.rev++
				f.objects[key] = data
				f.meta[key] = fileSyncLocalMeta{EntryID: entryID, RelPath: rel, Size: int64(len(data)), Sha256: sha, Mtime: mtime}
				f.used += int64(len(data))
				f.uploads = append(f.uploads, rel)
				fsWriteJSON(t, w, fileUploadResponse{Rev: f.rev, Used: f.used, Limit: f.limit, Tier: "free"})
			case http.MethodGet:
				data, ok := f.objects[key]
				if !ok {
					fsWriteErr(t, w, 404, "not_found")
					return
				}
				f.gets = append(f.gets, rel)
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				_, _ = w.Write(data)
			case http.MethodDelete:
				if _, ok := f.meta[key]; !ok {
					fsWriteErr(t, w, 404, "not_found")
					return
				}
				f.used -= f.meta[key].Size
				delete(f.meta, key)
				delete(f.objects, key)
				f.deletes = append(f.deletes, rel)
				fsWriteJSON(t, w, fileQuota{Used: f.used, Limit: f.limit, Tier: "free"})
			}
		default:
			fsWriteErr(t, w, 404, "not_found")
		}
	}
}

// ---------- 扫描 ----------

func TestFileSyncScanDiscoversExcludesAndTracksDeletion(t *testing.T) {
	dir := setupFileSyncTest(t)
	src := filepath.Join(dir, "src")
	fsWriteFile(t, filepath.Join(src, "a.txt"), "hello")
	fsWriteFile(t, filepath.Join(src, "sub", "b.txt"), "world")
	fsWriteFile(t, filepath.Join(src, "Thumbs.db"), "junk")
	fsWriteFile(t, filepath.Join(src, "~$draft.docx"), "junk")
	fsWriteFile(t, filepath.Join(src, ".DS_Store"), "junk")
	fsWriteFile(t, filepath.Join(src, "$RECYCLE.BIN", "x.txt"), "junk")

	e := fileSyncEntry{ID: "e1", Name: "notes", Kind: "dir", SourcePath: src}
	fileSyncMu.Lock()
	if err := fileSyncScanEntryLocked(&e); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	fileSyncMu.Unlock()

	if len(e.Files) != 2 {
		t.Fatalf("应只发现 2 个文件，实际 %d：%v", len(e.Files), e.Files)
	}
	if m := e.Files["a.txt"]; m.Size != 5 || m.Sha256 != fileSyncHashBytes([]byte("hello")) {
		t.Fatalf("a.txt 状态不对: %+v", m)
	}
	if _, ok := e.Files["sub/b.txt"]; !ok {
		t.Fatalf("子目录文件未发现: %v", e.Files)
	}

	// 修改文件 → 摘要更新；删除文件 → 记入删除台账
	fsWriteFile(t, filepath.Join(src, "a.txt"), "hello world")
	os.Remove(filepath.Join(src, "sub", "b.txt"))
	fileSyncMu.Lock()
	err := fileSyncScanEntryLocked(&e)
	fileSyncMu.Unlock()
	if err != nil {
		t.Fatalf("复扫失败: %v", err)
	}
	if m := e.Files["a.txt"]; m.Sha256 != fileSyncHashBytes([]byte("hello world")) {
		t.Fatalf("修改后摘要未更新: %+v", m)
	}
	if _, ok := e.Deletions["sub/b.txt"]; !ok {
		t.Fatalf("删除未登记: %v", e.Deletions)
	}

	// 文件又出现 → 撤销删除台账
	fsWriteFile(t, filepath.Join(src, "sub", "b.txt"), "world")
	fileSyncMu.Lock()
	_ = fileSyncScanEntryLocked(&e)
	fileSyncMu.Unlock()
	if _, ok := e.Deletions["sub/b.txt"]; ok {
		t.Fatalf("文件恢复后删除台账未清理: %v", e.Deletions)
	}
}

func TestFileSyncScanSkipsReceiveDir(t *testing.T) {
	dir := setupFileSyncTest(t)
	src := filepath.Join(dir, "src")
	fsWriteFile(t, filepath.Join(src, "a.txt"), "hello")
	// 接收目录恰好位于源目录内：必须跳过（否则自嵌套）
	recv := fileSyncReceiveDir()
	fsWriteFile(t, filepath.Join(recv, "downloaded.txt"), "from-cloud")
	e := fileSyncEntry{ID: "e1", Name: "notes", Kind: "dir", SourcePath: src}
	fileSyncMu.Lock()
	_ = fileSyncScanEntryLocked(&e)
	fileSyncMu.Unlock()
	if _, ok := e.Files["downloaded.txt"]; ok {
		t.Fatalf("接收目录内容被误纳入同步: %v", e.Files)
	}
	if _, ok := e.Files["a.txt"]; !ok {
		t.Fatalf("源目录文件丢失: %v", e.Files)
	}
}

func TestFileSyncScanFileEntryDeleted(t *testing.T) {
	dir := setupFileSyncTest(t)
	target := filepath.Join(dir, "one.txt")
	fsWriteFile(t, target, "data")
	e := fileSyncEntry{ID: "e1", Name: "one.txt", Kind: "file", SourcePath: target}
	fileSyncMu.Lock()
	_ = fileSyncScanEntryLocked(&e)
	fileSyncMu.Unlock()
	if len(e.Files) != 1 {
		t.Fatalf("单文件条目应有一个文件: %v", e.Files)
	}
	os.Remove(target)
	fileSyncMu.Lock()
	_ = fileSyncScanEntryLocked(&e)
	fileSyncMu.Unlock()
	if _, ok := e.Deletions["one.txt"]; !ok {
		t.Fatalf("单文件删除未登记: %v", e.Deletions)
	}
}

// ---------- 对账动作过滤 ----------

func TestFileSyncFilterActions(t *testing.T) {
	setupFileSyncTest(t)
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	fileSyncCur.Entries = []fileSyncEntry{{
		ID: "e1", Name: "n", Kind: "dir", SourcePath: "x",
		Files: map[string]fileSyncLocalFile{"a.txt": {Sha256: "aa", SyncedSha: "aa"}},
	}}
	actions := []fileSyncAction{
		{Kind: "upload", EntryID: "e1", RelPath: "a.txt"},                 // 已同步 → 丢弃
		{Kind: "download", EntryID: "e1", RelPath: "a.txt", Sha256: "aa"}, // 内容一致 → 丢弃
		{Kind: "download", EntryID: "e1", RelPath: "b.txt", Sha256: "bb"}, // 保留
		{Kind: "create_entry", EntryID: "e1", Name: "n"},                  // 已有 → 丢弃
		{Kind: "create_entry", EntryID: "e2", Name: "m"},                  // 保留
		{Kind: "remove_entry", EntryID: "e9", Name: "z"},                  // 本机没有 → 丢弃
		{Kind: "rename_entry", EntryID: "e1", Name: "n"},                  // 同名 → 丢弃
		{Kind: "rename_entry", EntryID: "e1", Name: "n2"},                 // 保留
		{Kind: "remove_file", EntryID: "e1", RelPath: "zz.txt"},           // 本机没有 → 丢弃
	}
	out, skipped := fileSyncFilterActionsLocked(actions)
	if skipped != 6 {
		t.Fatalf("应丢弃 6 条，实际 %d（保留 %v）", skipped, out)
	}
	kinds := make([]string, 0, len(out))
	for _, a := range out {
		kinds = append(kinds, a.Kind+":"+a.EntryID)
	}
	want := []string{"download:e1", "create_entry:e2", "rename_entry:e1"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("保留动作不对：%v", kinds)
	}
}

// ---------- 上传 / 删除传播 / 容量拦下 ----------

func TestFileSyncCheckUploadsAndPropagatesDeletion(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	src := filepath.Join(dir, "src")
	fsWriteFile(t, filepath.Join(src, "a.txt"), "hello")
	fsWriteFile(t, filepath.Join(src, "sub", "b.txt"), "world")
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "notes", Kind: "dir", SourcePath: src})

	fake := newFakeFilesServer()
	client, _ := newTestClient(t, fake.handler(t))

	res, err := fileSyncCheck(context.Background(), client)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if res.Uploaded != 2 {
		t.Fatalf("应上传 2 个文件，实际 %d", res.Uploaded)
	}
	meta, ok := fake.uploadMeta("a.txt")
	if !ok {
		t.Fatalf("服务端未收到 a.txt")
	}
	if meta.Sha256 != fileSyncHashBytes([]byte("hello")) || meta.Size != 5 || meta.Mtime == 0 {
		t.Fatalf("上传元数据不对: %+v", meta)
	}
	e := fsEntry(t, 0)
	if m := e.Files["a.txt"]; m.SyncedSha != m.Sha256 || m.Rev == 0 {
		t.Fatalf("上传后本机状态未更新: %+v", m)
	}
	fileSyncMu.Lock()
	used := fileSyncCur.QuotaUsed
	lastSynced := fileSyncCur.LastSyncedAt
	fileSyncMu.Unlock()
	if used != int64(len("hello")+len("world")) {
		t.Fatalf("容量统计不对: %d", used)
	}
	if lastSynced == 0 {
		t.Fatalf("LastSyncedAt 未记录")
	}

	// 本机删除 → 传播到服务端并清理台账
	if err := os.Remove(filepath.Join(src, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}
	res, err = fileSyncCheck(context.Background(), client)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if res.Deleted != 1 || !fake.hasDelete("sub/b.txt") {
		t.Fatalf("删除未传播：res=%+v deletes=%v", res, fake.deletes)
	}
	e = fsEntry(t, 0)
	if _, ok := e.Files["sub/b.txt"]; ok {
		t.Fatalf("删除后本机仍保留元数据")
	}
	if len(e.Deletions) != 0 {
		t.Fatalf("删除台账未清理: %v", e.Deletions)
	}
}

func TestFileSyncCheckBlocksWhenOverQuota(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	src := filepath.Join(dir, "src")
	fsWriteFile(t, filepath.Join(src, "big.bin"), "0123456789")
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "big", Kind: "dir", SourcePath: src})

	fake := newFakeFilesServer()
	fake.limit = 4
	client, _ := newTestClient(t, fake.handler(t))

	res, err := fileSyncCheck(context.Background(), client)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if res.Blocked != 1 || res.Uploaded != 0 {
		t.Fatalf("应被容量拦下 1 个、上传 0 个：res=%+v", res)
	}
	fake.mu.Lock()
	uploads := len(fake.uploads)
	fake.mu.Unlock()
	if uploads != 0 {
		t.Fatalf("容量不足时不该发起上传（服务端收到 %d 次）", uploads)
	}
	snap := fileSyncStatusSnapshot()
	if snap.BlockedCount != 1 {
		t.Fatalf("快照未标记容量拦下: %+v", snap)
	}
	if m := fsEntry(t, 0).Files["big.bin"]; !m.Blocked || m.Error == "" {
		t.Fatalf("文件状态未标记容量不足: %+v", m)
	}
}

// ---------- 应用 ----------

// ---------- 错误路径 ----------

func TestFileSyncCheckIsolatesUploadFailure(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	src := filepath.Join(dir, "src")
	fsWriteFile(t, filepath.Join(src, "ok.txt"), "fine")
	fsWriteFile(t, filepath.Join(src, "bad.txt"), "broken")
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "notes", Kind: "dir", SourcePath: src})

	fake := newFakeFilesServer()
	fake.failUploads["bad.txt"] = true
	client, _ := newTestClient(t, fake.handler(t))

	res, err := fileSyncCheck(context.Background(), client)
	if err != nil {
		t.Fatalf("同步整体不应失败: %v", err)
	}
	if res.Uploaded != 1 {
		t.Fatalf("应只有 1 个文件上传成功，实际 %d", res.Uploaded)
	}
	e := fsEntry(t, 0)
	if m := e.Files["ok.txt"]; m.SyncedSha == "" || m.Error != "" {
		t.Fatalf("成功文件状态不对: %+v", m)
	}
	if m := e.Files["bad.txt"]; m.Error == "" || m.SyncedSha != "" {
		t.Fatalf("失败文件应记错误且不标记已同步: %+v", m)
	}

	// 服务端恢复后重试成功，错误清空
	fake.mu.Lock()
	fake.failUploads["bad.txt"] = false
	fake.mu.Unlock()
	res, err = fileSyncCheck(context.Background(), client)
	if err != nil || res.Uploaded != 1 {
		t.Fatalf("重试应成功: res=%+v err=%v", res, err)
	}
	if m := fsEntry(t, 0).Files["bad.txt"]; m.Error != "" || m.SyncedSha != m.Sha256 {
		t.Fatalf("重试后错误未清空: %+v", m)
	}
}

func TestFileSyncApplyKeepsFailedAction(t *testing.T) {
	setupFileSyncTest(t)
	setAccountState(loggedInState())
	fake := newFakeFilesServer() // 服务端没有该对象 → 下载 404
	client, _ := newTestClient(t, fake.handler(t))
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "notes", Kind: "dir", Files: map[string]fileSyncLocalFile{}})
	fsSetPending(t, fileSyncAction{Kind: "download", EntryID: "e1", RelPath: "missing.txt", Sha256: "aa", Mtime: 1, Rev: 1})

	res, err := fileSyncApplyAll(context.Background(), client)
	if err != nil {
		t.Fatalf("应用流程不应返回致命错误: %v", err)
	}
	if res.Failed != 1 || res.Applied != 0 {
		t.Fatalf("应记 1 项失败: %+v", res)
	}
	if got := fileSyncStatusSnapshot().PendingCount; got != 1 {
		t.Fatalf("失败项应保留待应用（可重试），实际 %d", got)
	}
}

func TestFileSyncDeletion404ClearsLedger(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	src := filepath.Join(dir, "src")
	fsWriteFile(t, filepath.Join(src, "a.txt"), "x")
	fake := newFakeFilesServer() // 服务端没有这个文件：DELETE 返回 404
	client, _ := newTestClient(t, fake.handler(t))
	fsSetEntry(t, fileSyncEntry{
		ID: "e1", Name: "notes", Kind: "dir", SourcePath: src,
		Files:     map[string]fileSyncLocalFile{"a.txt": {Size: 1, Mtime: 1, Sha256: "aa", SyncedSha: "aa", Rev: 1}},
		Deletions: map[string]int64{"a.txt": 1},
	})
	// 真实流程：本机文件已被删除，删除台账待传播（若文件仍在，扫描会把它重新纳入同步）
	if err := os.Remove(filepath.Join(src, "a.txt")); err != nil {
		t.Fatal(err)
	}

	if _, err := fileSyncCheck(context.Background(), client); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	e := fsEntry(t, 0)
	if len(e.Deletions) != 0 {
		t.Fatalf("服务端本就无此文件时删除台账应清空: %v", e.Deletions)
	}
	if _, ok := e.Files["a.txt"]; ok {
		t.Fatalf("已删除文件不该留在同步清单: %v", e.Files)
	}
}

func TestFileSyncAddPathValidationAndNaming(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	fake := newFakeFilesServer()
	_, srv := newTestClient(t, fake.handler(t))
	setAccountAPIBase(srv.URL)

	// 正常添加：条目名取目录名，文件被扫描进清单，服务端收到登记
	src := filepath.Join(dir, "资料")
	fsWriteFile(t, filepath.Join(src, "a.txt"), "x")
	snap, err := fileSyncAddPath("dir", src)
	if err != nil {
		t.Fatalf("添加失败: %v", err)
	}
	if len(snap.Entries) != 1 || snap.Entries[0].Name != "资料" || !snap.Entries[0].IsSource {
		t.Fatalf("条目快照不对: %+v", snap.Entries)
	}
	if len(snap.Entries[0].Files) != 1 {
		t.Fatalf("未扫描到文件: %+v", snap.Entries[0].Files)
	}
	fake.mu.Lock()
	_, registered := fake.entries[fsEntry(t, 0).ID]
	fake.mu.Unlock()
	if !registered {
		t.Fatalf("未向服务端登记条目")
	}

	// 嵌套路径：拒绝（避免同一批文件被两个条目重复同步）
	if _, err := fileSyncAddPath("dir", filepath.Join(src, "sub")); err == nil {
		t.Fatalf("已同步目录内的路径应被拒绝")
	}
	// 父目录：同样拒绝
	if _, err := fileSyncAddPath("dir", dir); err == nil {
		t.Fatalf("包含已同步条目的父目录应被拒绝")
	}
	// 同名目录：自动加后缀
	other := filepath.Join(dir, "other", "资料")
	fsWriteFile(t, filepath.Join(other, "b.txt"), "y")
	snap, err = fileSyncAddPath("dir", other)
	if err != nil {
		t.Fatalf("同名添加失败: %v", err)
	}
	if len(snap.Entries) != 2 || snap.Entries[1].Name != "资料 (2)" {
		t.Fatalf("同名条目未加后缀: %+v", snap.Entries)
	}
	// 接收目录自身不能作为同步来源
	recv := fileSyncReceiveDir()
	fsWriteFile(t, filepath.Join(recv, "note.txt"), "z")
	if _, err := fileSyncAddPath("dir", recv); err == nil {
		t.Fatalf("接收目录应被拒绝")
	}
}

func TestFileSyncApplyDownloadAndConflictCopy(t *testing.T) {
	setupFileSyncTest(t)
	setAccountState(loggedInState())
	recv := fileSyncReceiveDir()
	target := filepath.Join(recv, "notes", "a.txt")
	fsWriteFile(t, target, "local-edit")

	serverData := []byte("server-version")
	fake := newFakeFilesServer()
	fake.putObject("e1", "a.txt", serverData, 1700000005)
	client, _ := newTestClient(t, fake.handler(t))

	fsSetEntry(t, fileSyncEntry{
		ID: "e1", Name: "notes", Kind: "dir",
		Files: map[string]fileSyncLocalFile{"a.txt": {
			Size: 10, Mtime: 1700000000, Sha256: fileSyncHashBytes([]byte("local-edit")),
			SyncedSha: "old-synced", SyncedSize: 10, Rev: 1,
		}},
	})
	fsSetPending(t, fileSyncAction{
		Kind: "download", EntryID: "e1", RelPath: "a.txt",
		Size: int64(len(serverData)), Sha256: fileSyncHashBytes(serverData), Mtime: 1700000005, Rev: 2,
	})

	res, err := fileSyncApplyAll(context.Background(), client)
	if err != nil || res.Failed != 0 {
		t.Fatalf("应用失败: %+v err=%v", res, err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "server-version" {
		t.Fatalf("未写入服务端内容: %q", got)
	}
	copies, _ := filepath.Glob(filepath.Join(recv, "notes", "*冲突-本机*"))
	if len(copies) != 1 {
		t.Fatalf("应保留 1 份冲突副本，实际 %v", copies)
	}
	if body, _ := os.ReadFile(copies[0]); string(body) != "local-edit" {
		t.Fatalf("冲突副本内容不对: %q", body)
	}
	st, err := os.Stat(target)
	if err != nil || st.ModTime().Unix() != 1700000005 {
		t.Fatalf("未保持服务端修改时间: %v (%v)", st.ModTime(), err)
	}
	if m := fsEntry(t, 0).Files["a.txt"]; m.SyncedSha != fileSyncHashBytes(serverData) || m.Rev != 2 {
		t.Fatalf("下载后本机状态未更新: %+v", m)
	}
	if len(fsEntry(t, 0).Files) != 1 {
		t.Fatalf("冲突副本不应进入同步清单: %v", fsEntry(t, 0).Files)
	}
}

func TestFileSyncApplyDownloadWithoutLocalChangeOverwrites(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	recv := fileSyncReceiveDir()
	target := filepath.Join(recv, "notes", "a.txt")
	fsWriteFile(t, target, "old-synced")

	serverData := []byte("server-version")
	fake := newFakeFilesServer()
	fake.putObject("e1", "a.txt", serverData, 1700000005)
	client, _ := newTestClient(t, fake.handler(t))

	syncedSha := fileSyncHashBytes([]byte("old-synced"))
	fsSetEntry(t, fileSyncEntry{
		ID: "e1", Name: "notes", Kind: "dir",
		Files: map[string]fileSyncLocalFile{"a.txt": {Size: 10, Mtime: 1, Sha256: syncedSha, SyncedSha: syncedSha, Rev: 1}},
	})
	fsSetPending(t, fileSyncAction{Kind: "download", EntryID: "e1", RelPath: "a.txt", Sha256: fileSyncHashBytes(serverData), Mtime: 1700000005, Rev: 2})

	if _, err := fileSyncApplyAll(context.Background(), client); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if body, _ := os.ReadFile(target); string(body) != "server-version" {
		t.Fatalf("未覆盖为服务端内容: %q", body)
	}
	copies, _ := filepath.Glob(filepath.Join(recv, "notes", "*冲突*"))
	if len(copies) != 0 {
		t.Fatalf("无本机改动时不该保留冲突副本: %v", copies)
	}
	_ = dir
}

func TestFileSyncApplyRemoveFileSourceKeepsOriginal(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	src := filepath.Join(dir, "src")
	target := filepath.Join(src, "a.txt")
	fsWriteFile(t, target, "keep-me")

	fake := newFakeFilesServer()
	client, _ := newTestClient(t, fake.handler(t))
	fsSetEntry(t, fileSyncEntry{
		ID: "e1", Name: "notes", Kind: "dir", SourcePath: src,
		Files: map[string]fileSyncLocalFile{"a.txt": {Size: 7, Mtime: 1, Sha256: fileSyncHashBytes([]byte("keep-me")), SyncedSha: "old", Rev: 1}},
	})
	fsSetPending(t, fileSyncAction{Kind: "remove_file", EntryID: "e1", RelPath: "a.txt"})

	if _, err := fileSyncApplyAll(context.Background(), client); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("源设备原文件被删除: %v", err)
	}
	e := fsEntry(t, 0)
	if _, ok := e.Files["a.txt"]; ok {
		t.Fatalf("远端删除后仍保留同步元数据")
	}
	if e.Ignored["a.txt"] == 0 {
		t.Fatalf("未登记忽略名单: %v", e.Ignored)
	}
	// 重新扫描：忽略名单生效，不重新上传
	fileSyncMu.Lock()
	_ = fileSyncScanEntryLocked(&fileSyncCur.Entries[0])
	fileSyncMu.Unlock()
	if _, ok := fsEntry(t, 0).Files["a.txt"]; ok {
		t.Fatalf("忽略名单未生效（文件又被纳入同步）")
	}
	// 用户改动文件（mtime 更新）→ 重新进入同步
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(target, future, future); err != nil {
		t.Fatal(err)
	}
	fileSyncMu.Lock()
	_ = fileSyncScanEntryLocked(&fileSyncCur.Entries[0])
	fileSyncMu.Unlock()
	if _, ok := fsEntry(t, 0).Files["a.txt"]; !ok {
		t.Fatalf("文件被改动后未重新纳入同步")
	}
}

func TestFileSyncApplyRemoveFileReceiverDeletesCopy(t *testing.T) {
	setupFileSyncTest(t)
	setAccountState(loggedInState())
	recv := fileSyncReceiveDir()
	target := filepath.Join(recv, "notes", "a.txt")
	fsWriteFile(t, target, "copy")

	fake := newFakeFilesServer()
	client, _ := newTestClient(t, fake.handler(t))
	fsSetEntry(t, fileSyncEntry{
		ID: "e1", Name: "notes", Kind: "dir",
		Files: map[string]fileSyncLocalFile{"a.txt": {Size: 4, Mtime: 1, Sha256: "aa", SyncedSha: "aa", Rev: 1}},
	})
	fsSetPending(t, fileSyncAction{Kind: "remove_file", EntryID: "e1", RelPath: "a.txt"})

	if _, err := fileSyncApplyAll(context.Background(), client); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("接收设备的本地副本未删除: %v", err)
	}
}

func TestFileSyncApplyEntryRenameAndRemove(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	recv := fileSyncReceiveDir()
	fsWriteFile(t, filepath.Join(recv, "old", "a.txt"), "x")

	fake := newFakeFilesServer()
	client, _ := newTestClient(t, fake.handler(t))
	fsSetEntry(t, fileSyncEntry{
		ID: "e1", Name: "old", Kind: "dir",
		Files: map[string]fileSyncLocalFile{"a.txt": {Size: 1, Mtime: 1, Sha256: "aa", SyncedSha: "aa", Rev: 1}},
	})
	fsSetPending(t, fileSyncAction{Kind: "rename_entry", EntryID: "e1", Name: "new"})

	if _, err := fileSyncApplyAll(context.Background(), client); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if e := fsEntry(t, 0); e.Name != "new" {
		t.Fatalf("条目名未更新: %q", e.Name)
	}
	if _, err := os.Stat(filepath.Join(recv, "new", "a.txt")); err != nil {
		t.Fatalf("接收目录未随重命名迁移: %v", err)
	}

	// create_entry + remove_entry
	fsSetPending(t, fileSyncAction{Kind: "create_entry", EntryID: "e2", Name: "incoming", EntryKind: "dir"})
	if _, err := fileSyncApplyAll(context.Background(), client); err != nil {
		t.Fatalf("应用 create_entry 失败: %v", err)
	}
	if len(fsEntry(t, 0).ID) == 0 || fsEntry(t, 1).Name != "incoming" {
		t.Fatalf("create_entry 未生效: %+v", fileSyncCur.Entries)
	}
	fsWriteFile(t, filepath.Join(recv, "incoming", "b.txt"), "y")
	fsSetPending(t, fileSyncAction{Kind: "remove_entry", EntryID: "e2", Name: "incoming"})
	if _, err := fileSyncApplyAll(context.Background(), client); err != nil {
		t.Fatalf("应用 remove_entry 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(recv, "incoming")); !os.IsNotExist(err) {
		t.Fatalf("接收设备条目目录未删除: %v", err)
	}
	if len(fileSyncCur.Entries) != 1 {
		t.Fatalf("条目未移除: %+v", fileSyncCur.Entries)
	}
	_ = dir
}

// ---------- 校验与状态视图 ----------

func TestFileSyncCheckOverlap(t *testing.T) {
	dir := setupFileSyncTest(t)
	root := filepath.Join(dir, "root")
	fsWriteFile(t, filepath.Join(root, "a.txt"), "x")
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "root", Kind: "dir", SourcePath: root})

	if err := fileSyncCheckOverlap(filepath.Join(root, "sub"), ""); err == nil {
		t.Fatalf("子目录应被判定为重叠")
	}
	if err := fileSyncCheckOverlap(dir, ""); err == nil {
		t.Fatalf("父目录应被判定为包含已同步条目")
	}
	if err := fileSyncCheckOverlap(filepath.Join(dir, "other"), ""); err != nil {
		t.Fatalf("无关路径不该报重叠: %v", err)
	}
	if err := fileSyncCheckOverlap(root, "e1"); err != nil {
		t.Fatalf("排除自身后不该报重叠: %v", err)
	}
}

func TestFileSyncEntryNameRules(t *testing.T) {
	valid := []string{"notes", "a.txt", "我的文档", "a b", "v1.2.3"}
	for _, n := range valid {
		if !fileSyncValidEntryName(n) {
			t.Fatalf("应合法: %q", n)
		}
	}
	invalid := []string{"", " ", ".", "..", "a/b", `a\b`, "a:b", "a*b", "a?b", `a"b`, "a<b", "a>b", "a|b", "CON", "nul.txt", "com1", "  x", "x  "}
	for _, n := range invalid {
		if fileSyncValidEntryName(n) {
			t.Fatalf("应非法: %q", n)
		}
	}
}

func TestFileSyncConflictPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	fsWriteFile(t, target, "x")
	got := fileSyncConflictPath(target)
	if filepath.Base(got) != "a (冲突-本机).txt" {
		t.Fatalf("冲突副本命名不对: %s", filepath.Base(got))
	}
	fsWriteFile(t, got, "y")
	got2 := fileSyncConflictPath(target)
	if filepath.Base(got2) != "a (冲突-本机 2).txt" {
		t.Fatalf("冲突副本未追加序号: %s", filepath.Base(got2))
	}
}

func TestFileSyncSafeToDelete(t *testing.T) {
	dir := t.TempDir()
	if !fileSyncSafeToDelete(dir) {
		t.Fatalf("普通目录应允许删除")
	}
	volumeRoot := filepath.VolumeName(dir) + string(os.PathSeparator)
	if fileSyncSafeToDelete(volumeRoot) {
		t.Fatalf("卷根不该允许删除")
	}
	if home, err := os.UserHomeDir(); err == nil && fileSyncSafeToDelete(home) {
		t.Fatalf("用户主目录不该允许删除")
	}
	if fileSyncSafeToDelete(fileSyncReceiveDir()) {
		t.Fatalf("接收目录本身不该允许删除")
	}
}

func TestFileSyncSnapshotView(t *testing.T) {
	setupFileSyncTest(t)
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{
		QuotaUsed:  10,
		QuotaLimit: 100,
		QuotaTier:  "free",
		Entries: []fileSyncEntry{
			{ID: "e2", Name: "zeta", Kind: "dir", SourcePath: "s", Files: map[string]fileSyncLocalFile{
				"b.txt":    {Size: 5, Sha256: "x", SyncedSha: "x"},
				"a.txt":    {Size: 7, Sha256: "y", SyncedSha: "z", Blocked: true, Error: "可用容量不足"},
				"sub/c.md": {Size: 3, Sha256: "q", SyncedSha: "q"},
			}},
			{ID: "e1", Name: "alpha", Kind: "file", SourcePath: "f", Files: map[string]fileSyncLocalFile{}},
		},
		PendingApply: []fileSyncAction{{Kind: "download", EntryID: "e2", RelPath: "b.txt"}},
	}
	snap := fileSyncSnapshotLocked()
	fileSyncMu.Unlock()

	if len(snap.Entries) != 2 || snap.Entries[0].Name != "alpha" || snap.Entries[1].Name != "zeta" {
		t.Fatalf("条目未按名称排序: %+v", snap.Entries)
	}
	zeta := snap.Entries[1]
	if zeta.Size != 15 {
		t.Fatalf("条目大小合计不对: %d", zeta.Size)
	}
	if zeta.Status != "error" {
		t.Fatalf("有容量不足文件时状态应为 error: %q", zeta.Status)
	}
	if len(zeta.Files) != 3 || zeta.Files[0].RelPath != "a.txt" || zeta.Files[2].RelPath != "sub/c.md" {
		t.Fatalf("文件行未按相对路径排序: %+v", zeta.Files)
	}
	if !zeta.Files[0].Blocked || zeta.Files[0].Status != "error" {
		t.Fatalf("容量不足文件行状态不对: %+v", zeta.Files[0])
	}
	if snap.BlockedCount != 1 || snap.QuotaUsed != 10 || snap.QuotaLimit != 100 || snap.QuotaTier != "free" {
		t.Fatalf("快照汇总不对: %+v", snap)
	}
	if snap.PendingCount != 1 || len(snap.PendingFiles) != 1 || !strings.Contains(snap.PendingFiles[0], "zeta/b.txt") {
		t.Fatalf("待应用信息不对: %+v", snap.PendingFiles)
	}
	if snap.Entries[1].IsSource != true || snap.Entries[0].IsSource != true {
		t.Fatalf("IsSource 判定不对")
	}
}

func TestFileSyncRemovePathDeletesFilesAndPropagates(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	src := filepath.Join(dir, "src")
	fsWriteFile(t, filepath.Join(src, "sub", "a.txt"), "a")
	fsWriteFile(t, filepath.Join(src, "sub", "b.txt"), "b")
	fsWriteFile(t, filepath.Join(src, "keep.txt"), "k")

	fake := newFakeFilesServer()
	fake.putObject("e1", "sub/a.txt", []byte("a"), 1)
	fake.putObject("e1", "sub/b.txt", []byte("b"), 1)
	fake.putObject("e1", "keep.txt", []byte("k"), 1)
	_, srv := newTestClient(t, fake.handler(t))
	setAccountAPIBase(srv.URL)

	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "notes", Kind: "dir", SourcePath: src, Files: map[string]fileSyncLocalFile{
		"sub/a.txt": {Size: 1, Mtime: 1, Sha256: "a", SyncedSha: "a", Rev: 1},
		"sub/b.txt": {Size: 1, Mtime: 1, Sha256: "b", SyncedSha: "b", Rev: 1},
		"keep.txt":  {Size: 1, Mtime: 1, Sha256: "k", SyncedSha: "k", Rev: 1},
	}})

	app := &App{}
	if _, err := app.FilesRemovePath("e1", "sub", true); err != nil {
		t.Fatalf("删除子目录失败: %v", err)
	}
	if !fake.hasDelete("sub/a.txt") || !fake.hasDelete("sub/b.txt") {
		t.Fatalf("未传播删除: %v", fake.deletes)
	}
	if fake.hasDelete("keep.txt") {
		t.Fatalf("误删同条目的其它文件: %v", fake.deletes)
	}
	if _, err := os.Stat(filepath.Join(src, "sub")); !os.IsNotExist(err) {
		t.Fatalf("本机子目录未删除: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "keep.txt")); err != nil {
		t.Fatalf("保留文件被误删: %v", err)
	}
	if e := fsEntry(t, 0); len(e.Files) != 1 {
		t.Fatalf("元数据未清理: %v", e.Files)
	}
	// 不存在的路径：明确报错，不静默成功
	if _, err := app.FilesRemovePath("e1", "nope.txt", true); err == nil {
		t.Fatalf("删除不存在的文件应报错")
	}
}

func TestFileSyncInitRevalidatesPendingActions(t *testing.T) {
	dir := setupFileSyncTest(t)
	src := filepath.Join(dir, "src")
	target := filepath.Join(src, "a.txt")
	fsWriteFile(t, target, "hello")
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	sha := fileSyncHashBytes([]byte("hello"))
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{
		Entries: []fileSyncEntry{{
			ID: "e1", Name: "notes", Kind: "dir", SourcePath: src,
			Files: map[string]fileSyncLocalFile{"a.txt": {Size: 5, Mtime: st.ModTime().Unix(), Sha256: sha, SyncedSha: sha}},
		}},
		PendingApply: []fileSyncAction{
			{Kind: "download", EntryID: "e1", RelPath: "a.txt", Sha256: sha}, // 内容已一致 → 丢弃
			{Kind: "create_entry", EntryID: "e1", Name: "notes"},             // 条目已存在 → 丢弃
		},
	}
	if err := saveFileSyncStateLocked(fileSyncCur); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	fileSyncMu.Unlock()

	initFileSyncState() // 从磁盘载入并按本机现状重判

	if got := fileSyncStatusSnapshot().PendingCount; got != 0 {
		t.Fatalf("启动重判未丢弃已满足的待应用动作: %d", got)
	}
}

func TestFileSyncResetOnAccountSwitch(t *testing.T) {
	dir := setupFileSyncTest(t)
	src := filepath.Join(dir, "src")
	fsWriteFile(t, filepath.Join(src, "a.txt"), "x")
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "notes", Kind: "dir", SourcePath: src})
	fileSyncMu.Lock()
	_ = saveFileSyncStateLocked(fileSyncCur)
	fileSyncMu.Unlock()

	resetFileSyncStateForAccountSwitch()

	if snap := fileSyncStatusSnapshot(); len(snap.Entries) != 0 || snap.PendingCount != 0 {
		t.Fatalf("换账号后内存清单未清空: %+v", snap)
	}
	if got := loadFileSyncState(); len(got.Entries) != 0 || len(got.PendingApply) != 0 {
		t.Fatalf("换账号后磁盘清单未清空: %+v", got)
	}
}

// TestFileSyncWireContract 锁定客户端与服务端（dsh-connect docs/API.md 端点 13-20）的线格式：
// 字段名一旦写错，只有真机联调才会暴露，这里用固定 JSON 双向比对把契约钉住。
func TestFileSyncWireContract(t *testing.T) {
	// 请求：对账入参（客户端 → 服务端）
	req := fileSyncRequest{
		Entries: []fileSyncEntryBody{{ID: "e1", Name: "notes", Kind: "dir"}},
		Files:   []fileSyncLocalMeta{{EntryID: "e1", RelPath: "sub/a.txt", Size: 5, Sha256: "aa", Mtime: 1700000000}},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("序列化请求失败: %v", err)
	}
	var reqMap map[string]any
	if err := json.Unmarshal(raw, &reqMap); err != nil {
		t.Fatal(err)
	}
	if _, ok := reqMap["entries"]; !ok {
		t.Fatalf("请求缺少 entries 字段: %s", raw)
	}
	entries := reqMap["entries"].([]any)
	first := entries[0].(map[string]any)
	for _, k := range []string{"id", "name", "kind"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("条目字段缺 %s: %s", k, raw)
		}
	}
	file := reqMap["files"].([]any)[0].(map[string]any)
	for _, k := range []string{"entryId", "relPath", "size", "sha256", "mtime"} {
		if _, ok := file[k]; !ok {
			t.Fatalf("文件字段缺 %s: %s", k, raw)
		}
	}

	// 响应：对账结果（服务端 → 客户端），形状取自 docs/API.md 端点 17 示例
	const syncJSON = `{
	  "entries": [{"id":"e1","name":"notes","kind":"dir","createdAt":1700000000,"updatedAt":1700000001}],
	  "actions": [
	    {"kind":"create_entry","entryId":"e2","name":"m","entryKind":"dir"},
	    {"kind":"rename_entry","entryId":"e1","name":"n2"},
	    {"kind":"remove_entry","entryId":"e3","name":"z"},
	    {"kind":"download","entryId":"e1","relPath":"a.txt","size":5,"sha256":"aa","mtime":1700000002,"rev":3},
	    {"kind":"upload","entryId":"e1","relPath":"b.txt"},
	    {"kind":"remove_file","entryId":"e1","relPath":"c.txt"}
	  ],
	  "quota": {"used":5,"limit":10485760,"tier":"free"}
	}`
	var resp fileSyncResponse
	if err := json.Unmarshal([]byte(syncJSON), &resp); err != nil {
		t.Fatalf("解析对账响应失败: %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Name != "notes" || resp.Entries[0].Kind != "dir" {
		t.Fatalf("条目解析不对: %+v", resp.Entries)
	}
	if len(resp.Actions) != 6 {
		t.Fatalf("动作条数不对: %d", len(resp.Actions))
	}
	if a := resp.Actions[3]; a.Kind != "download" || a.RelPath != "a.txt" || a.Sha256 != "aa" || a.Rev != 3 || a.Size != 5 {
		t.Fatalf("download 动作解析不对: %+v", a)
	}
	if a := resp.Actions[0]; a.EntryKind != "dir" || a.Name != "m" {
		t.Fatalf("create_entry 动作解析不对: %+v", a)
	}
	if resp.Quota.Used != 5 || resp.Quota.Limit != 10485760 || resp.Quota.Tier != "free" {
		t.Fatalf("容量解析不对: %+v", resp.Quota)
	}

	// 响应：上传结果（服务端 → 客户端）
	var up fileUploadResponse
	if err := json.Unmarshal([]byte(`{"rev":2,"used":9,"limit":10485760,"tier":"free"}`), &up); err != nil {
		t.Fatalf("解析上传响应失败: %v", err)
	}
	if up.Rev != 2 || up.Used != 9 || up.Limit != 10485760 {
		t.Fatalf("上传响应解析不对: %+v", up)
	}

	// 对象定位参数：query 名必须与服务端一致（entryId + relPath）
	q := fileSyncObjectQuery("e1", "sub/a b.txt")
	for _, want := range []string{"entryId=e1", "relPath=sub%2Fa+b.txt"} {
		if !strings.Contains(q, want) {
			t.Fatalf("对象参数缺 %q: %s", want, q)
		}
	}
}

func TestFileSyncStateRoundTrip(t *testing.T) {
	dir := setupFileSyncTest(t)
	st := fileSyncState{
		Entries: []fileSyncEntry{{
			ID: "e1", Name: "notes", Kind: "dir", SourcePath: filepath.Join(dir, "src"),
			Files:     map[string]fileSyncLocalFile{"a.txt": {Size: 5, Mtime: 1700000000, Sha256: "aa", SyncedSha: "aa", SyncedSize: 5, Rev: 1}},
			Deletions: map[string]int64{"b.txt": 1700000001},
			Ignored:   map[string]int64{"c.txt": 1700000002},
		}},
		PendingApply: []fileSyncAction{{Kind: "download", EntryID: "e1", RelPath: "a.txt", Sha256: "bb"}},
		QuotaUsed:    5,
		QuotaLimit:   10 * 1024 * 1024,
		QuotaTier:    "free",
		LastSyncedAt: 1700000003,
	}
	fileSyncMu.Lock()
	err := saveFileSyncStateLocked(st)
	fileSyncMu.Unlock()
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got := loadFileSyncState()
	if len(got.Entries) != 1 || got.Entries[0].Files["a.txt"].SyncedSha != "aa" ||
		got.Entries[0].Deletions["b.txt"] != 1700000001 || got.Entries[0].Ignored["c.txt"] != 1700000002 {
		t.Fatalf("往返后条目状态丢失: %+v", got.Entries)
	}
	if got.QuotaUsed != 5 || got.QuotaLimit != 10*1024*1024 || got.LastSyncedAt != 1700000003 {
		t.Fatalf("往返后容量/时间丢失: %+v", got)
	}
	if len(got.PendingApply) != 1 || got.PendingApply[0].Sha256 != "bb" {
		t.Fatalf("往返后待应用动作丢失: %+v", got.PendingApply)
	}
}

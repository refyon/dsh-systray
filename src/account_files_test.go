// account_files_test.go：文件同步引擎用例——扫描/排除、上传与删除传播、容量拦下、
// 对账动作过滤、应用（下载/冲突副本/远端删除/改条目名）与状态视图。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	mu       sync.Mutex
	limit    int64
	used     int64
	rev      int64
	entries  map[string]fileSyncRemoteEntry
	objects  map[string][]byte
	meta     map[string]fileSyncLocalMeta
	requests []fileSyncRequest
	actions  []fileSyncAction
	uploads  []string
	deletes  []string
	gets     []string
	// uploadDelay 每个上传请求的人为延迟（验证「移除后停止排队上传」这类时序行为）。
	uploadDelay time.Duration
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
			f.requests = append(f.requests, body) // handler 开头已持有 f.mu（勿重复加锁：Go 互斥不可重入）
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
				if f.uploadDelay > 0 {
					time.Sleep(f.uploadDelay)
				}
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

	// 修改文件 → 摘要更新
	fsWriteFile(t, filepath.Join(src, "a.txt"), "hello world")
	fileSyncMu.Lock()
	err := fileSyncScanEntryLocked(&e)
	fileSyncMu.Unlock()
	if err != nil {
		t.Fatalf("复扫失败: %v", err)
	}
	if m := e.Files["a.txt"]; m.Sha256 != fileSyncHashBytes([]byte("hello world")) {
		t.Fatalf("修改后摘要未更新: %+v", m)
	}

	// 删除**从未上传成功**的文件（读取失败被跳过的链接等）：直接从清单移除（账号里本来就没有它）
	os.Remove(filepath.Join(src, "sub", "b.txt"))
	fileSyncMu.Lock()
	_ = fileSyncScanEntryLocked(&e)
	fileSyncMu.Unlock()
	if _, ok := e.Files["sub/b.txt"]; ok {
		t.Fatalf("未上传过的已删文件应移出清单: %v", e.Files)
	}

	// 删除**已上传过**的文件：只在本机停止同步（标记 RemovedLocally），不传播到账号
	fsWriteFile(t, filepath.Join(src, "sub", "b.txt"), "world")
	fileSyncMu.Lock()
	_ = fileSyncScanEntryLocked(&e)
	fileSyncMu.Unlock()
	m := e.Files["sub/b.txt"]
	m.SyncedSha, m.Rev, m.SyncedSize = m.Sha256, 1, m.Size
	e.Files["sub/b.txt"] = m
	os.Remove(filepath.Join(src, "sub", "b.txt"))
	fileSyncMu.Lock()
	_ = fileSyncScanEntryLocked(&e)
	fileSyncMu.Unlock()
	if m := e.Files["sub/b.txt"]; !m.RemovedLocally {
		t.Fatalf("已上传文件被删除后应标记「已在本机移除」: %+v", m)
	}
	if got := fileSyncFileStatus(e.Files["sub/b.txt"]); got != "removed-local" {
		t.Fatalf("状态应为 removed-local，实际 %s", got)
	}

	// 文件又出现 → 撤销「已在本机移除」，重新纳入同步
	fsWriteFile(t, filepath.Join(src, "sub", "b.txt"), "world")
	fileSyncMu.Lock()
	_ = fileSyncScanEntryLocked(&e)
	fileSyncMu.Unlock()
	if m := e.Files["sub/b.txt"]; m.RemovedLocally {
		t.Fatalf("文件恢复后不应仍标记「已在本机移除」: %+v", m)
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
	if m := e.Files["one.txt"]; !m.RemovedLocally {
		t.Fatalf("单文件条目源文件被删除后应标记「已在本机移除」: %+v", m)
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
	out, skipped, orphans := fileSyncFilterActionsLocked(actions, nil)
	if skipped != 6 {
		t.Fatalf("应丢弃 6 条，实际 %d（保留 %v）", skipped, out)
	}
	if len(orphans) != 0 {
		t.Fatalf("本机设备 id 为空时不应判为自家孤儿：%v", orphans)
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

func TestFileSyncCheckUploadsAndKeepsLocalDeletion(t *testing.T) {
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

	// 本机删除 → **只在本机停止同步**：不调 DELETE、不动账号内容，标记「已在本机移除」可重新同步
	if err := os.Remove(filepath.Join(src, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}
	res, err = fileSyncCheck(context.Background(), client)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(fake.deletes) != 0 {
		t.Fatalf("本机删除不应传播到账号：%v", fake.deletes)
	}
	if _, still := fake.uploadMeta("sub/b.txt"); !still {
		t.Fatalf("账号里应仍保留该文件")
	}
	if m := fsEntry(t, 0).Files["sub/b.txt"]; !m.RemovedLocally {
		t.Fatalf("应标记「已在本机移除」: %+v", m)
	}
	if used := fileSyncQuotaUsed(t); used != int64(len("hello")+len("world")) {
		t.Fatalf("账号容量不应因本机删除而释放：%d", used)
	}
}

// fileSyncQuotaUsed 读取当前已用容量（测试用）。
func fileSyncQuotaUsed(t *testing.T) int64 {
	t.Helper()
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	return fileSyncCur.QuotaUsed
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

// TestFileSyncLocalDeleteKeepsServerCopy 本机删除后账号内容仍在（不传播），界面显示「已在本机移除」；
// 用户点「重新同步」后清掉本机摘要，下一轮对账会重新下载。
func TestFileSyncLocalDeleteKeepsServerCopy(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	src := filepath.Join(dir, "src")
	fsWriteFile(t, filepath.Join(src, "a.txt"), "x")
	fake := newFakeFilesServer()
	fake.putObject("e1", "a.txt", []byte("x"), 1)
	client, _ := newTestClient(t, fake.handler(t))
	fsSetEntry(t, fileSyncEntry{
		ID: "e1", Name: "notes", Kind: "dir", SourcePath: src,
		Files: map[string]fileSyncLocalFile{"a.txt": {Size: 1, Mtime: 1, Sha256: fileSyncHashBytes([]byte("x")), SyncedSha: fileSyncHashBytes([]byte("x")), Rev: 1}},
	})
	if err := os.Remove(filepath.Join(src, "a.txt")); err != nil {
		t.Fatal(err)
	}

	if _, err := fileSyncCheck(context.Background(), client); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(fake.deletes) != 0 {
		t.Fatalf("不应传播删除：%v", fake.deletes)
	}
	if m := fsEntry(t, 0).Files["a.txt"]; !m.RemovedLocally {
		t.Fatalf("应标记「已在本机移除」: %+v", m)
	}
	if fileSyncStatusSnapshot().Entries[0].Files[0].Status != "removed-local" {
		t.Fatalf("快照状态应为 removed-local：%+v", fileSyncStatusSnapshot().Entries[0].Files[0])
	}

	// 重新同步：把该文件从本机清单移除 → 服务端下发的下载动作不再被「内容一致」过滤掉
	app := &App{}
	if _, err := app.FilesRestorePath("e1", "a.txt"); err != nil {
		t.Fatalf("重新同步失败: %v", err)
	}
	if _, ok := fsEntry(t, 0).Files["a.txt"]; ok {
		t.Fatalf("重新同步后本机清单不应再持有该文件：%+v", fsEntry(t, 0).Files)
	}
	sha := fileSyncHashBytes([]byte("x"))
	fake.actions = []fileSyncAction{{Kind: "download", EntryID: "e1", RelPath: "a.txt", Size: 1, Sha256: sha, Mtime: 1, Rev: 1}}
	if _, err := fileSyncCheck(context.Background(), client); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	// 远端改动**直接落地**：文件回到本机，不留待应用，并记下「远端更新」痕迹
	if _, err := os.Stat(filepath.Join(src, "a.txt")); err != nil {
		t.Fatalf("远端改动应已自动落地到本机：%v", err)
	}
	snap := fileSyncStatusSnapshot()
	if snap.PendingCount != 0 {
		t.Fatalf("远端改动已落地，不该残留待应用：%d", snap.PendingCount)
	}
	// 计数口径：只数真正落地的文件（这条只有 1 个下载动作 → 1）
	if snap.RemoteAppliedCount != 1 || snap.RemoteAppliedAt == 0 {
		t.Fatalf("未记录远端更新痕迹：applied=%d at=%d", snap.RemoteAppliedCount, snap.RemoteAppliedAt)
	}
}

// TestFileSyncRemoteAppliedCountCountsFilesOnly 「远端更新 N 项」只数文件：
// 新建条目 + 下载 1 个文件应显示 1 项（而不是 2 项）。
func TestFileSyncRemoteAppliedCountCountsFilesOnly(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	receive := fileSyncReceiveDir()
	fake := newFakeFilesServer()
	fake.putObject("e1", "notes.txt", []byte("hello"), 1)
	client, _ := newTestClient(t, fake.handler(t))
	sha := fileSyncHashBytes([]byte("hello"))
	fake.actions = []fileSyncAction{
		{Kind: "create_entry", EntryID: "e1", Name: "notes.txt", EntryKind: "file"},
		{Kind: "download", EntryID: "e1", RelPath: "notes.txt", Size: 5, Sha256: sha, Mtime: 1, Rev: 1},
		{Kind: "upload", EntryID: "e1", RelPath: "notes.txt"}, // 服务器仍可能下发；不计入
	}
	_ = dir
	fileSyncMu.Lock()
	fileSyncCur.Entries = nil
	fileSyncMu.Unlock()

	res, err := fileSyncCheck(context.Background(), client)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if res.Applied != 2 { // 条目 + 文件都被应用（upload 不计）
		t.Fatalf("应用项数应为 2，实际 %d", res.Applied)
	}
	snap := fileSyncStatusSnapshot()
	if snap.RemoteAppliedCount != 1 {
		t.Fatalf("小字应只数文件（1 项），实际 %d", snap.RemoteAppliedCount)
	}
	// 单文件条目的落点就是接收目录下的显示名本身（不再多一层 relPath）
	if _, err := os.Stat(filepath.Join(receive, "notes.txt")); err != nil {
		t.Fatalf("文件应落到接收目录：%v", err)
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
	setAccountState(loggedInState()) // 未登录时快照清空（板块显示「登录后可查看」提示）
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

func TestFileSyncRemovePathPropagatesAndKeepsLocalFiles(t *testing.T) {
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
	if _, err := app.FilesRemovePath("e1", "sub"); err != nil {
		t.Fatalf("移除子目录失败: %v", err)
	}
	if !fake.hasDelete("sub/a.txt") || !fake.hasDelete("sub/b.txt") {
		t.Fatalf("未传播删除: %v", fake.deletes)
	}
	if fake.hasDelete("keep.txt") {
		t.Fatalf("误删同条目的其它文件: %v", fake.deletes)
	}
	// 本机文件一律保留（用户决策：移除只影响同步清单）
	if _, err := os.Stat(filepath.Join(src, "sub", "a.txt")); err != nil {
		t.Fatalf("本机文件不应被删除: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "keep.txt")); err != nil {
		t.Fatalf("保留文件被误删: %v", err)
	}
	if e := fsEntry(t, 0); len(e.Files) != 1 {
		t.Fatalf("元数据未清理: %v", e.Files)
	}
	// 不存在的路径：明确报错，不静默成功
	if _, err := app.FilesRemovePath("e1", "nope.txt"); err == nil {
		t.Fatalf("移除不存在的文件应报错")
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

func TestFileSyncAccountOwnership(t *testing.T) {
	dir := setupFileSyncTest(t)
	src := filepath.Join(dir, "src")
	fsWriteFile(t, filepath.Join(src, "a.txt"), "x")
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "notes", Kind: "dir", SourcePath: src})
	fileSyncMu.Lock()
	_ = saveFileSyncStateLocked(fileSyncCur)
	fileSyncMu.Unlock()

	// 旧版本清单没有归属记录：首次登录应**认领**（保留），而不是清空
	setAccountState(loggedInState())
	fileSyncOnAccountLogin("u-1")
	if got := loadFileSyncState(); len(got.Entries) != 1 || got.UserID != "u-1" {
		t.Fatalf("首次登录应认领清单并保留: %+v", got)
	}
	// 同一账号重新登录（退出再登录）：清单保留，不重下/重传
	fileSyncOnAccountLogin("u-1")
	if got := loadFileSyncState(); len(got.Entries) != 1 {
		t.Fatalf("同账号重新登录应保留清单: %+v", got)
	}
	if snap := fileSyncStatusSnapshot(); len(snap.Entries) != 1 {
		t.Fatalf("同账号重新登录后列表应还在: %+v", snap)
	}

	// 换账号：清单作废（远端条目属于旧账号）
	fileSyncOnAccountLogin("u-2")
	if got := loadFileSyncState(); len(got.Entries) != 0 || len(got.PendingApply) != 0 || got.UserID != "u-2" {
		t.Fatalf("换账号后磁盘清单未清空: %+v", got)
	}
	if snap := fileSyncStatusSnapshot(); len(snap.Entries) != 0 || snap.PendingCount != 0 {
		t.Fatalf("换账号后内存清单未清空: %+v", snap)
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

// TestFileSyncBuildRequestSendsEmptyArrays 空清单也必须是数组：Go 的 nil slice 会序列化成
// `null`，服务端 schema 只认数组 → 400（2026-10-05 v1.3.0 现场：容量读不出 + 「操作失败」）。
func TestFileSyncBuildRequestSendsEmptyArrays(t *testing.T) {
	setupFileSyncTest(t)
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{}
	req := fileSyncBuildRequestLocked()
	fileSyncMu.Unlock()

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, "null") {
		t.Fatalf("空请求不应含 null: %s", got)
	}
	if !strings.Contains(got, `"entries":[]`) || !strings.Contains(got, `"files":[]`) {
		t.Fatalf("空请求应为空数组: %s", got)
	}
}

// countingRT 统计在途请求数与峰值（验证上传并发上限）。
type countingRT struct {
	mu       sync.Mutex
	inFlight int
	max      int
}

func (c *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.inFlight++
	if c.inFlight > c.max {
		c.max = c.inFlight
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inFlight--
		c.mu.Unlock()
	}()
	return http.DefaultTransport.RoundTrip(req)
}

func TestFileSyncUploadsAreConcurrentAndCapped(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	src := filepath.Join(dir, "src")
	for i := 0; i < 12; i++ {
		fsWriteFile(t, filepath.Join(src, fmt.Sprintf("f%02d.txt", i)), strings.Repeat("x", 2048))
	}
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "many", Kind: "dir", SourcePath: src})

	fake := newFakeFilesServer()
	client, _ := newTestClient(t, fake.handler(t))
	rt := &countingRT{}
	client.fileHTTP = &http.Client{Transport: rt}

	res, err := fileSyncCheck(context.Background(), client)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if res.Uploaded != 12 {
		t.Fatalf("应上传 12 个文件，实际 %d", res.Uploaded)
	}
	if rt.max > fileSyncUploadConcurrency {
		t.Fatalf("并发数超过上限 %d：实测峰值 %d", fileSyncUploadConcurrency, rt.max)
	}
	if rt.max < 2 {
		t.Fatalf("未观察到并发上传（峰值 %d）", rt.max)
	}
	// 上传完成：全部标记已同步，进度归零，逐文件速度已记录
	e := fsEntry(t, 0)
	for rel, m := range e.Files {
		if m.SyncedSha != m.Sha256 || m.Error != "" {
			t.Fatalf("文件 %s 未标记已同步: %+v", rel, m)
		}
	}
	snap := fileSyncStatusSnapshot()
	if snap.Uploading {
		t.Fatalf("上传结束后应退出上传态: %+v", snap)
	}
	if snap.UploadTotal != 12 || snap.UploadDone != 12 {
		t.Fatalf("进度未走完：done=%d total=%d", snap.UploadDone, snap.UploadTotal)
	}
	// 逐文件速度：至少记录了一部分（Windows 计时器粒度下毫秒级上传可能测得 0，属正常），
	// 且快照里至少有一个文件带上速度（前端就是按这个字段显示的）。
	fileSyncMu.Lock()
	speeds := len(fileSyncProg.FileSpeeds)
	fileSyncMu.Unlock()
	if speeds == 0 || speeds > 12 {
		t.Fatalf("逐文件速度记录异常：%d", speeds)
	}
	withSpeed := 0
	for _, f := range fileSyncStatusSnapshot().Entries[0].Files {
		if f.SpeedBps > 0 {
			withSpeed++
		}
	}
	if withSpeed == 0 {
		t.Fatalf("快照里没有任何文件带上传速度")
	}
}

// TestFileSyncSingleFileEntryUsesSelectedPath 单文件条目（kind=file）的本机路径就是选中的文件本身：
// 再拼一次 relPath 会得到 `H:\DOC\info.txt\info.txt` → 读取失败（2026-10-05 现场）。
func TestFileSyncSingleFileEntryUsesSelectedPath(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	target := filepath.Join(dir, "info.txt")
	fsWriteFile(t, target, "hello single file")
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "info.txt", Kind: "file", SourcePath: target})

	if got := fileSyncEntryLocalPath(fsEntry(t, 0), "info.txt"); got != target {
		t.Fatalf("单文件条目路径错误：%s（应为 %s）", got, target)
	}

	fileSyncMu.Lock()
	e := fileSyncCur.Entries[0]
	err := fileSyncScanEntryLocked(&e)
	fileSyncCur.Entries[0] = e
	fileSyncMu.Unlock()
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	m, ok := fsEntry(t, 0).Files["info.txt"]
	if !ok {
		t.Fatalf("单文件条目应登记 info.txt：%+v", fsEntry(t, 0).Files)
	}
	if m.Error != "" {
		t.Fatalf("不应有读取错误：%s", m.Error)
	}
	if m.Sha256 != fileSyncHashBytes([]byte("hello single file")) {
		t.Fatalf("摘要错误：%+v", m)
	}

	fake := newFakeFilesServer()
	client, _ := newTestClient(t, fake.handler(t))
	res, err := fileSyncCheck(context.Background(), client)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if res.Uploaded != 1 {
		t.Fatalf("单文件条目应上传 1 个，实际 %d", res.Uploaded)
	}
	if m := fsEntry(t, 0).Files["info.txt"]; m.SyncedSha == "" {
		t.Fatalf("未标记已同步：%+v", m)
	}
}

// TestFileSyncCancelStopsPendingUploads 取消（移除条目/内容）后，排队中的上传不再发出。
func TestFileSyncCancelStopsPendingUploads(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	src := filepath.Join(dir, "src")
	for i := 0; i < 40; i++ {
		fsWriteFile(t, filepath.Join(src, fmt.Sprintf("f%02d.txt", i)), strings.Repeat("x", 4096))
	}
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "many", Kind: "dir", SourcePath: src})

	fake := newFakeFilesServer()
	fake.uploadDelay = 10 * time.Millisecond
	client, _ := newTestClient(t, fake.handler(t))

	done := make(chan fileSyncResult, 1)
	go func() {
		res, _ := fileSyncCheck(context.Background(), client)
		done <- res
	}()
	time.Sleep(60 * time.Millisecond) // 让前几个文件先传起来
	fileSyncCancelUploads("e1", "")
	res := <-done

	fake.mu.Lock()
	sent := len(fake.uploads)
	fake.mu.Unlock()
	if sent >= 40 {
		t.Fatalf("取消后仍在继续上传：%d/40", sent)
	}
	if res.Uploaded >= 40 {
		t.Fatalf("取消后不应再计入上传成功：%d", res.Uploaded)
	}
	fileSyncMu.Lock()
	total := fileSyncProg.Total
	fileSyncMu.Unlock()
	if total > 40 {
		t.Fatalf("进度分母异常：%d", total)
	}
}

// TestFileSyncFilterKeepsOwnEntriesWithoutTombstone 来源设备=本机、本机没有该条目时：
//   - **有移除记录**（用户点过「移除」，服务端删除失败）→ 自家残留，交给调用方清理服务端；
//   - **没有移除记录**（清单丢了）→ 必须当新条目建回来，绝不能删云端数据
//     ——2026-10-06 现场：清单被清空后，这台机器把账号上两个条目连同服务端数据一起删了。
func TestFileSyncFilterKeepsOwnEntriesWithoutTombstone(t *testing.T) {
	dir := setupFileSyncTest(t)
	_ = dir
	setAccountState(loggedInState())
	accountMu.Lock()
	accountCur.DeviceID = "dev-self"
	accountMu.Unlock()

	fileSyncMu.Lock()
	fileSyncCur.Entries = nil
	fileSyncCur.RemovedEntries = map[string]int64{"removed": 1700000000} // 这个条目用户确实移除过
	out, _, orphans := fileSyncFilterActionsLocked([]fileSyncAction{
		{Kind: "create_entry", EntryID: "removed", Name: "removed", OriginDevice: "dev-self"}, // 有移除记录 → 清理残留
		{Kind: "create_entry", EntryID: "lost", Name: "lost", OriginDevice: "dev-self"},       // 清单丢失 → 建回来
		{Kind: "create_entry", EntryID: "other", Name: "other", OriginDevice: "dev-peer"},     // 其它设备 → 保留
		{Kind: "create_entry", EntryID: "legacy", Name: "legacy"},                             // 旧数据无来源 → 保留
	}, nil)
	fileSyncMu.Unlock()

	if len(orphans) != 1 || orphans[0] != "removed" {
		t.Fatalf("只有本机移除过的条目才算残留：%v", orphans)
	}
	if len(out) != 3 {
		t.Fatalf("清单丢失/其它设备/旧数据的条目都应保留：%v", out)
	}
	for _, a := range out {
		if a.EntryID == "removed" {
			t.Fatalf("移除过的条目不该重新建回：%+v", a)
		}
	}
}

// TestFileSyncOnlyInFlightFilesShowUploading 只有真正拿到上传线程的文件才是「同步中」，
// 排队中的保持「待同步」——否则几百个文件全显示同步中（2026-10-05 现场）。
func TestFileSyncOnlyInFlightFilesShowUploading(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	src := filepath.Join(dir, "src")
	for i := 0; i < 40; i++ {
		fsWriteFile(t, filepath.Join(src, fmt.Sprintf("f%02d.txt", i)), strings.Repeat("x", 512))
	}
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "many", Kind: "dir", SourcePath: src})

	fake := newFakeFilesServer()
	fake.uploadDelay = 20 * time.Millisecond
	client, _ := newTestClient(t, fake.handler(t))

	done := make(chan struct{})
	go func() {
		_, _ = fileSyncCheck(context.Background(), client)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	snap := fileSyncStatusSnapshot()
	uploading := 0
	for _, f := range snap.Entries[0].Files {
		if f.Status == "uploading" {
			uploading++
		}
	}
	<-done

	if uploading == 0 {
		t.Fatalf("进行中应有文件显示「同步中」：%+v", snap.Entries[0].Files)
	}
	if uploading > fileSyncUploadConcurrency {
		t.Fatalf("「同步中」文件数超过并发上限 %d：实测 %d", fileSyncUploadConcurrency, uploading)
	}
	// 条目（以及前端按前缀聚合出的上级文件夹）也应显示「同步中」，而不是「待同步」
	if snap.Entries[0].Status != "uploading" {
		t.Fatalf("有文件在传时条目状态应为 uploading，实际 %s", snap.Entries[0].Status)
	}
	for _, f := range fileSyncStatusSnapshot().Entries[0].Files {
		if f.Status == "uploading" {
			t.Fatalf("同步结束后不应残留「同步中」：%+v", f)
		}
	}
	if got := fileSyncStatusSnapshot().Entries[0].Status; got != "synced" {
		t.Fatalf("同步结束后条目状态应为 synced，实际 %s", got)
	}
}

// TestFileSyncRemovedFileStaysOutOfSync 移除单个文件（本机保留）后，重新扫描不应把它拉回同步：
// 否则下一轮扫描发现文件还在 → 重新上传 → 用户的移除等于白做（2026-10-05 现场）。
func TestFileSyncRemovedFileStaysOutOfSync(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	src := filepath.Join(dir, "src")
	fsWriteFile(t, filepath.Join(src, "keep.txt"), "k")
	fsWriteFile(t, filepath.Join(src, "drop.txt"), "d")

	fake := newFakeFilesServer()
	fake.putObject("e1", "keep.txt", []byte("k"), 1)
	fake.putObject("e1", "drop.txt", []byte("d"), 1)
	_, srv := newTestClient(t, fake.handler(t))
	setAccountAPIBase(srv.URL)
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "notes", Kind: "dir", SourcePath: src, Files: map[string]fileSyncLocalFile{
		"keep.txt": {Size: 1, Mtime: 1, Sha256: "k", SyncedSha: "k", Rev: 1},
		"drop.txt": {Size: 1, Mtime: 1, Sha256: "d", SyncedSha: "d", Rev: 1},
	}})

	app := &App{}
	if _, err := app.FilesRemovePath("e1", "drop.txt"); err != nil {
		t.Fatalf("移除文件失败: %v", err)
	}
	if !fake.hasDelete("drop.txt") {
		t.Fatalf("未传播删除: %v", fake.deletes)
	}

	// 重新扫描：drop.txt 还在磁盘上，但不该回到同步清单
	fileSyncMu.Lock()
	e := fileSyncCur.Entries[0]
	_ = fileSyncScanEntryLocked(&e)
	fileSyncCur.Entries[0] = e
	fileSyncMu.Unlock()
	if _, ok := fsEntry(t, 0).Files["drop.txt"]; ok {
		t.Fatalf("已移除的文件不应被重新纳入同步：%+v", fsEntry(t, 0).Files)
	}
	if _, ok := fsEntry(t, 0).Files["keep.txt"]; !ok {
		t.Fatalf("其它文件不应受影响：%+v", fsEntry(t, 0).Files)
	}

	// 用户改动该文件（mtime 变新）→ 重新纳入同步
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(src, "drop.txt"), future, future); err != nil {
		t.Fatalf("改时间失败: %v", err)
	}
	fileSyncMu.Lock()
	e = fileSyncCur.Entries[0]
	_ = fileSyncScanEntryLocked(&e)
	fileSyncCur.Entries[0] = e
	fileSyncMu.Unlock()
	if _, ok := fsEntry(t, 0).Files["drop.txt"]; !ok {
		t.Fatalf("改动后的文件应重新纳入同步：%+v", fsEntry(t, 0).Files)
	}
}

// TestFileSyncOpenCanceledIsSilent 打开时用户在「打开方式」对话框取消 → 静默成功；
// 真正的启动失败才把原因交给界面（界面据此提示并允许重新选择打开方式）。
func TestFileSyncOpenCanceledIsSilent(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	target := filepath.Join(dir, "info.txt")
	fsWriteFile(t, target, "hello")
	fsSetEntry(t, fileSyncEntry{ID: "e1", Name: "info.txt", Kind: "file", SourcePath: target})

	origOpen, origOpenWith := openFileFn, openFileWithFn
	defer func() { openFileFn, openFileWithFn = origOpen, origOpenWith }()

	app := &App{}
	openFileFn = func(string) error { return errOpenCanceled }
	if err := app.FilesOpenEntry("e1", "info.txt"); err != nil {
		t.Fatalf("用户取消不该报错：%v", err)
	}

	openFileFn = func(string) error { return errors.New("没有可用的打开方式") }
	if err := app.FilesOpenEntry("e1", "info.txt"); err == nil {
		t.Fatalf("真正的启动失败应把原因交给界面")
	}

	openFileWithFn = func(string) error { return errOpenCanceled }
	if err := app.FilesOpenEntryWith("e1", "info.txt"); err != nil {
		t.Fatalf("选择打开方式时取消同样静默：%v", err)
	}
	openFileWithFn = func(string) error { return errors.New("对话框打不开") }
	if err := app.FilesOpenEntryWith("e1", "info.txt"); err == nil {
		t.Fatalf("选择打开方式失败应报错")
	}
}

// TestFileSyncSnapshotClearsWhenLoggedOut 退出登录后文件卡清空内容（容量/列表/进度不外露），
// 但本机清单保留在磁盘上，重新登录即恢复显示（用户要求）。
func TestFileSyncSnapshotClearsWhenLoggedOut(t *testing.T) {
	dir := setupFileSyncTest(t)
	_ = dir
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{
		QuotaUsed:  10,
		QuotaLimit: 100,
		Entries:    []fileSyncEntry{{ID: "e1", Name: "notes", Kind: "dir", SourcePath: "s"}},
	}
	snap := fileSyncSnapshotLocked()
	fileSyncMu.Unlock()
	if snap.LoggedIn {
		t.Fatalf("未登录时 LoggedIn 应为 false")
	}
	if len(snap.Entries) != 0 || snap.QuotaUsed != 0 || snap.QuotaLimit != 0 || snap.PendingCount != 0 {
		t.Fatalf("未登录时文件卡应清空：%+v", snap)
	}

	setAccountState(loggedInState())
	back := fileSyncStatusSnapshot()
	if !back.LoggedIn || len(back.Entries) != 1 || back.QuotaLimit != 100 {
		t.Fatalf("重新登录后应恢复显示本机清单：%+v", back)
	}
}

// TestFileSyncEntryFromOwnDeviceReattachesSourcePath 先校验设备身份再落盘：
//   - 本机创建的条目（originDevice=本机）+ 服务端记下的原路径在本机可用 → **自动同步回原路径**，
//     不在接收目录造副本（2026-10-06 现场：H:\DOC\info.txt 曾被下成 Documents 里的副本）；
//   - 原路径已不存在 → 才按接收端落到接收目录，并标记 sourcePathMissing 由界面说明；
//   - 其它设备创建的条目 → 始终按接收端处理。
func TestFileSyncEntryFromOwnDeviceReattachesSourcePath(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	accountMu.Lock()
	accountCur.DeviceID = "dev-self"
	accountMu.Unlock()

	// 场景 A：原路径仍在本机 → 自动回源
	original := filepath.Join(dir, "H-DOC-info.txt")
	fsWriteFile(t, original, "hello")
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{}
	fileSyncMu.Unlock()
	if err := fileSyncApplyEntryAction(fileSyncAction{
		Kind: "create_entry", EntryID: "mine", Name: "info.txt", EntryKind: "file",
		OriginDevice: "dev-self", SourcePath: original,
	}); err != nil {
		t.Fatalf("建条目失败: %v", err)
	}
	snap := fileSyncStatusSnapshot()
	if len(snap.Entries) != 1 {
		t.Fatalf("条目未建立：%+v", snap.Entries)
	}
	if !snap.Entries[0].IsSource || snap.Entries[0].Path != original {
		t.Fatalf("本机创建的条目应自动回源路径 %s：%+v", original, snap.Entries[0])
	}
	if snap.Entries[0].SourcePathMissing {
		t.Fatalf("原路径可用时不该标记缺失：%+v", snap.Entries[0])
	}

	// 场景 B：原路径已不存在 → 接收目录副本 + 标记
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{}
	fileSyncMu.Unlock()
	missing := filepath.Join(dir, "no-such-file.txt")
	if err := fileSyncApplyEntryAction(fileSyncAction{
		Kind: "create_entry", EntryID: "gone", Name: "gone.txt", EntryKind: "file",
		OriginDevice: "dev-self", SourcePath: missing,
	}); err != nil {
		t.Fatalf("建条目失败: %v", err)
	}
	snap = fileSyncStatusSnapshot()
	if snap.Entries[0].IsSource {
		t.Fatalf("原路径不存在时应按接收端处理：%+v", snap.Entries[0])
	}
	if !snap.Entries[0].SourcePathMissing || snap.Entries[0].OriginPath != missing {
		t.Fatalf("应标记原路径缺失并带上原路径：%+v", snap.Entries[0])
	}

	// 场景 C：别的设备创建的条目 → 接收端，且不标记
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{}
	fileSyncMu.Unlock()
	if err := fileSyncApplyEntryAction(fileSyncAction{
		Kind: "create_entry", EntryID: "peer", Name: "peer.txt", EntryKind: "file",
		OriginDevice: "dev-peer", SourcePath: "/home/peer/peer.txt",
	}); err != nil {
		t.Fatalf("建条目失败: %v", err)
	}
	snap = fileSyncStatusSnapshot()
	if snap.Entries[0].IsSource || snap.Entries[0].SourcePathMissing {
		t.Fatalf("其它设备的条目应按接收端处理：%+v", snap.Entries[0])
	}
}

// TestFileSyncSelfHealsReceiverEntryWithOwnSourcePath 服务端记着「本机创建 + 原路径」，
// 而本机这条是接收端（老数据 / 清单曾丢失）→ 下一轮对账自动回源路径（不靠人工指定）。
func TestFileSyncSelfHealsReceiverEntryWithOwnSourcePath(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	accountMu.Lock()
	accountCur.DeviceID = "dev-self"
	accountMu.Unlock()

	original := filepath.Join(dir, "H-DOC-note.txt")
	fsWriteFile(t, original, "world")
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{Entries: []fileSyncEntry{{
		ID: "e1", Name: "note.txt", Kind: "file", // 没有 SourcePath：被当成接收端
		Files: map[string]fileSyncLocalFile{},
	}}}
	n := fileSyncReattachOwnEntriesLocked([]fileSyncRemoteEntry{{
		ID: "e1", Name: "note.txt", Kind: "file", OriginDevice: "dev-self", SourcePath: original,
	}})
	fileSyncMu.Unlock()
	if n != 1 {
		t.Fatalf("应回源 1 个条目，实际 %d", n)
	}
	snap := fileSyncStatusSnapshot()
	if !snap.Entries[0].IsSource || snap.Entries[0].Path != original {
		t.Fatalf("应自动回源到 %s：%+v", original, snap.Entries[0])
	}
	if _, ok := snap.Entries[0].Files[0].RelPath, true; !ok {
		t.Fatalf("回源后应重扫出文件：%+v", snap.Entries[0].Files)
	}

	// 原路径不存在：不误回源，只标记说明
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{Entries: []fileSyncEntry{{ID: "e2", Name: "gone.txt", Kind: "file", Files: map[string]fileSyncLocalFile{}}}}
	fileSyncReattachOwnEntriesLocked([]fileSyncRemoteEntry{{
		ID: "e2", Name: "gone.txt", Kind: "file", OriginDevice: "dev-self", SourcePath: filepath.Join(dir, "nope.txt"),
	}})
	fileSyncMu.Unlock()
	got := fileSyncStatusSnapshot().Entries[0]
	if got.IsSource || !got.SourcePathMissing {
		t.Fatalf("原路径不存在时应保持接收端并标记：%+v", got)
	}

	// 别的设备创建的条目：不动
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{Entries: []fileSyncEntry{{ID: "e3", Name: "peer.txt", Kind: "file", Files: map[string]fileSyncLocalFile{}}}}
	n = fileSyncReattachOwnEntriesLocked([]fileSyncRemoteEntry{{
		ID: "e3", Name: "peer.txt", Kind: "file", OriginDevice: "dev-peer", SourcePath: original,
	}})
	fileSyncMu.Unlock()
	if n != 0 || fileSyncStatusSnapshot().Entries[0].IsSource {
		t.Fatalf("其它设备的条目不该被回源")
	}
}

// TestFileSyncStateBackupRestoresLedger 状态文件损坏/丢失时从备份恢复清单：
// 清单静默变空会让本机把账号上的条目误判成残留（2026-10-06 现场：云端两个条目被删）。
func TestFileSyncStateBackupRestoresLedger(t *testing.T) {
	dir := setupFileSyncTest(t)
	entry := fileSyncEntry{ID: "e1", Name: "notes", Kind: "dir", SourcePath: filepath.Join(dir, "notes")}
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{UserID: "u-1", Entries: []fileSyncEntry{entry}}
	if err := saveFileSyncStateLocked(fileSyncCur); err != nil {
		fileSyncMu.Unlock()
		t.Fatalf("保存失败: %v", err)
	}
	fileSyncMu.Unlock()

	p := fileSyncStatePath()
	if _, err := os.Stat(p + ".bak"); err != nil {
		t.Fatalf("应写出备份文件：%v", err)
	}
	// 主文件损坏 → 从备份恢复
	if err := os.WriteFile(p, []byte("{ 坏掉的 JSON"), 0o600); err != nil {
		t.Fatalf("写入损坏文件失败: %v", err)
	}
	got := loadFileSyncState()
	if len(got.Entries) != 1 || got.Entries[0].ID != "e1" || got.UserID != "u-1" {
		t.Fatalf("应从备份恢复清单：%+v", got)
	}
	// 主文件消失 → 同样从备份恢复
	if err := os.Remove(p); err != nil {
		t.Fatalf("删除主文件失败: %v", err)
	}
	got = loadFileSyncState()
	if len(got.Entries) != 1 {
		t.Fatalf("主文件丢失时应从备份恢复：%+v", got)
	}
	// 两个都没有 → 空清单（不 panic）
	_ = os.Remove(p + ".bak")
	if got = loadFileSyncState(); len(got.Entries) != 0 {
		t.Fatalf("无状态文件时应返回空清单：%+v", got)
	}
}

// TestFileSyncRelocateMovesLocalFiles 更改本机位置 = **移动**（用户要求）：
//   - 旧位置的文件搬到新位置，搬完清理源文件与空目录；
//   - 目标已有同名且内容相同 → 视为同一份（丢源那份）；内容不同 → 覆盖并留「(冲突-本机)」副本；
//   - 本机不上报新位置的数据（内容没变，不该重传）；
//   - 旧位置本来就没有的文件交给服务器按正常同步补下来。
func TestFileSyncRelocateMovesLocalFiles(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	accountMu.Lock()
	accountCur.DeviceID = "dev-self"
	accountCur.Token = "tok"
	accountMu.Unlock()

	sameSha := fileSyncHashBytes([]byte("same"))
	movedSha := fileSyncHashBytes([]byte("payload"))
	fake := newFakeFilesServer()
	fake.entries["e1"] = fileSyncRemoteEntry{ID: "e1", Name: "notes", Kind: "dir", OriginDevice: "dev-self"}
	fake.putObject("e1", "same.txt", []byte("same"), 100)
	fake.putObject("e1", "moved.txt", []byte("payload"), 100)
	fake.putObject("e1", "missing.txt", []byte("from-server"), 100)
	client, _ := newTestClient(t, fake.handler(t))
	setAccountAPIBase(client.base)
	t.Cleanup(func() { setAccountAPIBase("") })

	oldRoot := filepath.Join(dir, "old")
	fsWriteFile(t, filepath.Join(oldRoot, "same.txt"), "same")
	fsWriteFile(t, filepath.Join(oldRoot, "moved.txt"), "payload")
	fsWriteFile(t, filepath.Join(oldRoot, "sub", "nested.txt"), "nested") // 未纳入同步的本地文件也要跟着搬
	newRoot := filepath.Join(dir, "new")
	fsWriteFile(t, filepath.Join(newRoot, "same.txt"), "same") // 目标已有同一份

	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{Entries: []fileSyncEntry{{
		ID: "e1", Name: "notes", Kind: "dir", SourcePath: oldRoot, OriginDevice: "dev-self",
		Files: map[string]fileSyncLocalFile{
			"same.txt":  {Sha256: sameSha, SyncedSha: sameSha},
			"moved.txt": {Sha256: movedSha, SyncedSha: movedSha},
		},
	}}}
	fileSyncMu.Unlock()

	if _, err := fileSyncRelocateEntry("e1", newRoot); err != nil {
		t.Fatalf("更改位置失败: %v", err)
	}

	// 1) 文件被移动：新位置有内容，旧位置已清空（含子目录与空壳目录）
	if b, err := os.ReadFile(filepath.Join(newRoot, "moved.txt")); err != nil || string(b) != "payload" {
		t.Fatalf("文件应被移动到新位置：%v %q", err, string(b))
	}
	if b, err := os.ReadFile(filepath.Join(newRoot, "sub", "nested.txt")); err != nil || string(b) != "nested" {
		t.Fatalf("未纳入同步的本地文件也应一起搬：%v", err)
	}
	if _, err := os.Stat(filepath.Join(oldRoot, "moved.txt")); !os.IsNotExist(err) {
		t.Fatalf("移动后源文件应被清理：%v", err)
	}
	if _, err := os.Stat(oldRoot); !os.IsNotExist(err) {
		t.Fatalf("搬空的旧目录应被清理：%v", err)
	}
	// 目标已有同一份内容：保留一份即可（内容仍正确）
	if b, err := os.ReadFile(filepath.Join(newRoot, "same.txt")); err != nil || string(b) != "same" {
		t.Fatalf("内容相同的文件应合并为一份：%v", err)
	}

	// 2) 扫描后状态正确：两个文件都显示已同步，没有「已在本机移除」
	after := fileSyncStatusSnapshot().Entries[0]
	if !strings.HasPrefix(after.Path, newRoot) {
		t.Fatalf("条目应指向新位置：%+v", after)
	}
	statusOf := map[string]string{}
	for _, f := range after.Files {
		statusOf[f.RelPath] = f.Status
	}
	if statusOf["moved.txt"] != "synced" || statusOf["same.txt"] != "synced" {
		t.Fatalf("移动后应保持已同步：%+v", statusOf)
	}
	for _, f := range after.Files {
		if f.Status == "removed-local" {
			t.Fatalf("移动不该显示已在本机移除：%+v", after.Files)
		}
	}

	// 3) 没有把新位置的数据当改动上传（移动后内容没变；上报的记录与服务端一致，且没有上传请求）
	waitFileSyncIdle(t)
	fake.mu.Lock()
	uploads := append([]string(nil), fake.uploads...)
	reqs := append([]fileSyncRequest(nil), fake.requests...)
	metas := map[string]fileSyncLocalMeta{}
	for k, v := range fake.meta {
		metas[k] = v
	}
	fake.mu.Unlock()
	if len(uploads) != 0 {
		// 允许上传的只有「条目里本来没有、新位置才发现的文件」；已同步的文件不得被重传
		for _, up := range uploads {
			if strings.Contains(up, "moved.txt") || strings.Contains(up, "same.txt") {
				t.Fatalf("已同步的文件不该因移动被重传：%v", uploads)
			}
		}
	}
	for _, req := range reqs {
		for _, m := range req.Files {
			if m.EntryID != "e1" {
				continue
			}
			if want, ok := metas[fakeKey(m.EntryID, m.RelPath)]; ok && want.Sha256 != m.Sha256 {
				t.Fatalf("上报内容与服务端不一致（会被当成本机改动上传）：%+v vs %+v", m, want)
			}
		}
	}

	// 4) 服务器上本机缺的文件（missing.txt）由正常同步补下来
	fake.mu.Lock()
	fake.actions = []fileSyncAction{
		{Kind: "download", EntryID: "e1", RelPath: "missing.txt", Size: 11, Sha256: fileSyncHashBytes([]byte("from-server")), Mtime: 100, Rev: 1},
	}
	fake.mu.Unlock()
	fileSyncKick()
	waitFileSyncIdle(t)
	if b, err := os.ReadFile(filepath.Join(newRoot, "missing.txt")); err != nil || string(b) != "from-server" {
		t.Fatalf("本机缺的文件应补到新位置：%v %q", err, string(b))
	}
}

// waitFileSyncIdle 等待后台同步跑完（改位置会踢一次同步，测试里不能与它并发调 fileSyncCheck）。
func waitFileSyncIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		fileSyncMu.Lock()
		busy := fileSyncSyncing
		fileSyncMu.Unlock()
		if !busy {
			time.Sleep(50 * time.Millisecond) // 让收尾写盘落地
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等待后台同步结束超时")
}

// TestFileSyncRelocateBackToOriginal 回归用户现场：改过去再改回来不应卡住（移动是幂等的往返）。
func TestFileSyncRelocateBackToOriginal(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())

	orig := filepath.Join(dir, "orig")
	fsWriteFile(t, filepath.Join(orig, "a.txt"), "hello")
	other := filepath.Join(dir, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	sha := fileSyncHashBytes([]byte("hello"))
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{Entries: []fileSyncEntry{{
		ID: "e1", Name: "notes", Kind: "dir", SourcePath: orig, OriginDevice: "dev-peer",
		Files: map[string]fileSyncLocalFile{"a.txt": {Sha256: sha, SyncedSha: sha}},
	}}}
	fileSyncMu.Unlock()

	if _, err := fileSyncRelocateEntry("e1", other); err != nil {
		t.Fatalf("改到新位置失败: %v", err)
	}
	if _, err := fileSyncRelocateEntry("e1", orig); err != nil {
		t.Fatalf("改回原位置失败: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(orig, "a.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("改回原位置后文件应就位：%v %q", err, string(b))
	}
	after := fileSyncStatusSnapshot().Entries[0]
	if after.Status == "pending-upload" || after.Status == "error" {
		t.Fatalf("往返移动后不该卡在待同步/错误：%+v", after)
	}
	for _, f := range after.Files {
		if f.Status == "removed-local" {
			t.Fatalf("往返移动后不该显示已在本机移除：%+v", after.Files)
		}
	}
}

// TestFileSyncRelocateConflictOverwritesWithoutCopy 目标已有同名但内容不同：用搬过来的直接覆盖，
// **不再留「(冲突-本机)」副本**（用户决策 2026-10-06：用户决定覆盖就不必重命名保留原件）。
func TestFileSyncRelocateConflictOverwritesWithoutCopy(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())

	oldRoot := filepath.Join(dir, "old")
	fsWriteFile(t, filepath.Join(oldRoot, "a.txt"), "newer-local")
	newRoot := filepath.Join(dir, "new")
	fsWriteFile(t, filepath.Join(newRoot, "a.txt"), "existing-other")
	sha := fileSyncHashBytes([]byte("newer-local"))
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{Entries: []fileSyncEntry{{
		ID: "e1", Name: "notes", Kind: "dir", SourcePath: oldRoot, OriginDevice: "dev-peer",
		Files: map[string]fileSyncLocalFile{"a.txt": {Sha256: sha, SyncedSha: sha}},
	}}}
	fileSyncMu.Unlock()

	// 覆盖前会用它问用户一次：这里确认会覆盖（数量应为 1）
	if n := fileSyncMoveConflictCount(oldRoot, newRoot); n != 1 {
		t.Fatalf("应统计出 1 个会被覆盖的同名文件：%d", n)
	}
	moved, conflicts, _ := fileSyncMoveLocalContent("dir", oldRoot, newRoot)
	if moved != 1 || conflicts != 1 {
		t.Fatalf("应移动 1 个并覆盖 1 个冲突：moved=%d conflicts=%d", moved, conflicts)
	}
	if b, err := os.ReadFile(filepath.Join(newRoot, "a.txt")); err != nil || string(b) != "newer-local" {
		t.Fatalf("目标应被搬过来的文件覆盖：%v %q", err, string(b))
	}
	if copies, _ := filepath.Glob(filepath.Join(newRoot, "a (冲突-本机)*.txt")); len(copies) != 0 {
		t.Fatalf("覆盖后不该再留「(冲突-本机)」副本：%v", copies)
	}
}

// TestFileSyncRelocateAsksBeforeOverwrite 文件夹条目覆盖前先问一次：
// 用户确认 → 覆盖且不留副本；用户取消 → 一个文件都不动（源与目标都保持原样）。
func TestFileSyncRelocateAsksBeforeOverwrite(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())

	oldRoot := filepath.Join(dir, "old")
	fsWriteFile(t, filepath.Join(oldRoot, "a.txt"), "newer-local")
	newRoot := filepath.Join(dir, "new")
	fsWriteFile(t, filepath.Join(newRoot, "a.txt"), "existing-other")
	sha := fileSyncHashBytes([]byte("newer-local"))

	reset := func() {
		fileSyncMu.Lock()
		fileSyncCur = fileSyncState{Entries: []fileSyncEntry{{
			ID: "e1", Name: "notes", Kind: "dir", SourcePath: oldRoot, OriginDevice: "dev-peer",
			Files: map[string]fileSyncLocalFile{"a.txt": {Sha256: sha, SyncedSha: sha}},
		}}}
		fileSyncMu.Unlock()
		fsWriteFile(t, filepath.Join(oldRoot, "a.txt"), "newer-local")
		fsWriteFile(t, filepath.Join(newRoot, "a.txt"), "existing-other")
	}

	// 用户取消：报「已取消移动」，源文件仍在原处、目标内容不变
	reset()
	asked := 0
	askMoveOverwriteFn = func(n int) bool { asked = n; return false }
	t.Cleanup(func() { askMoveOverwriteFn = askMoveOverwriteLocal })
	if _, err := fileSyncRelocateEntry("e1", newRoot); err == nil {
		t.Fatal("用户取消覆盖时应返回「已取消移动」")
	}
	if asked != 1 {
		t.Fatalf("应带着冲突数量问一次，实际 n=%d", asked)
	}
	if b, err := os.ReadFile(filepath.Join(oldRoot, "a.txt")); err != nil || string(b) != "newer-local" {
		t.Fatalf("取消后源文件应原地不动：%v %q", err, string(b))
	}
	if b, err := os.ReadFile(filepath.Join(newRoot, "a.txt")); err != nil || string(b) != "existing-other" {
		t.Fatalf("取消后目标内容应保持原样：%v %q", err, string(b))
	}

	// 用户确认：移动完成、覆盖生效、不留副本
	reset()
	askMoveOverwriteFn = func(int) bool { return true }
	if _, err := fileSyncRelocateEntry("e1", newRoot); err != nil {
		t.Fatalf("确认覆盖后移动应成功：%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(newRoot, "a.txt")); err != nil || string(b) != "newer-local" {
		t.Fatalf("确认后目标应被覆盖：%v %q", err, string(b))
	}
	if _, err := os.Stat(filepath.Join(oldRoot, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("移动后源文件应被清理：%v", err)
	}
	if copies, _ := filepath.Glob(filepath.Join(newRoot, "a (冲突-本机)*.txt")); len(copies) != 0 {
		t.Fatalf("确认覆盖后不该留副本：%v", copies)
	}
}

// TestFileSyncRelocateFileEntryMovesFile 文件条目的移动：旧文件搬到新落点（含跨卷复制路径）。
func TestFileSyncRelocateFileEntryMovesFile(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())

	src := filepath.Join(dir, "single.txt")
	fsWriteFile(t, src, "single")
	dstDir := filepath.Join(dir, "dest")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	dst, err := fileSyncFileEntryTargetIn(dstDir, "single.txt")
	if err != nil {
		t.Fatalf("落点解析失败: %v", err)
	}
	sha := fileSyncHashBytes([]byte("single"))
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{Entries: []fileSyncEntry{{
		ID: "e1", Name: "single.txt", Kind: "file", SourcePath: src, OriginDevice: "dev-peer",
		Files: map[string]fileSyncLocalFile{"single.txt": {Sha256: sha, SyncedSha: sha}},
	}}}
	fileSyncMu.Unlock()

	if _, err := fileSyncRelocateEntry("e1", dst); err != nil {
		t.Fatalf("更改位置失败: %v", err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "single" {
		t.Fatalf("文件应被移动到新落点：%v %q", err, string(b))
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("移动后源文件应被清理：%v", err)
	}
	if after := fileSyncStatusSnapshot().Entries[0]; after.Status == "error" {
		t.Fatalf("不该报错：%+v", after)
	}
}

// TestFileSyncRelocateFileEntryIntoFolder 文件条目的落点与覆盖交给系统对话框决定：
// 用户可用系统「另存为」对话框把文件放进某个文件夹并沿用原名（`<文件夹>/<条目名>`），
// 也可以改名或指定已存在的文件——**是否覆盖由系统自己询问**，App 不再自建询问弹窗。
func TestFileSyncRelocateFileEntryIntoFolder(t *testing.T) {
	dir := setupFileSyncTest(t)
	target, err := fileSyncFileEntryTargetIn(dir, "note.txt")
	if err != nil {
		t.Fatalf("目录内落点不该报错：%v", err)
	}
	if target != filepath.Join(dir, "note.txt") {
		t.Fatalf("应保留文件名：%s", target)
	}
	if err := fileSyncCheckAdoptTarget(target, "file"); err != nil {
		t.Fatalf("目录内尚不存在的落点应被接受（等服务器内容落地）：%v", err)
	}
	// 文件名里的非法字符被替换（否则落点会跑到别的目录）
	weird, err := fileSyncFileEntryTargetIn(dir, `a/b:c*.txt`)
	if err != nil {
		t.Fatalf("应能规整文件名：%v", err)
	}
	if strings.ContainsAny(filepath.Base(weird), `/\:*?`) || filepath.Dir(weird) != dir {
		t.Fatalf("文件名应被规整到同目录内：%s", weird)
	}
	// 已有的同名文件（用户在系统对话框里确认替换）：接受该落点
	existing := filepath.Join(dir, "exists.txt")
	fsWriteFile(t, existing, "old")
	if err := fileSyncCheckAdoptTarget(existing, "file"); err != nil {
		t.Fatalf("已存在的文件应由系统询问后接受：%v", err)
	}
	// 目录条目仍然只能选目录
	fsWriteFile(t, filepath.Join(dir, "plain.txt"), "x")
	if err := fileSyncCheckAdoptTarget(filepath.Join(dir, "plain.txt"), "dir"); err == nil {
		t.Fatal("文件夹条目选到文件时应报错")
	}
	if err := fileSyncCheckAdoptTarget(dir, "dir"); err != nil {
		t.Fatalf("文件夹条目选目录应通过：%v", err)
	}
}

// TestFileSyncRelocateRegistersSourcePathForOrigin 改位置时：本机创建的条目把新位置登记到账号
// （日后清单丢失能回到新位置），其它设备创建的条目只改本机、不发 PATCH。
func TestFileSyncRelocateRegistersSourcePathForOrigin(t *testing.T) {
	dir := setupFileSyncTest(t)
	setAccountState(loggedInState())
	accountMu.Lock()
	accountCur.DeviceID = "dev-self"
	accountCur.Token = "tok"
	accountMu.Unlock()

	newRoot := filepath.Join(dir, "new-root")
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}

	patched := make(chan string, 1)
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/source-path") {
			var body struct {
				SourcePath string `json:"sourcePath"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			patched <- body.SourcePath
			_, _ = w.Write([]byte(`{"id":"e1","name":"notes","kind":"dir","sourcePath":""}`))
			return
		}
		switch r.URL.Path {
		case "/v1/files/quota":
			_, _ = w.Write([]byte(`{"used":0,"limit":10485760,"tier":"free"}`))
		case "/v1/files/sync":
			_, _ = w.Write([]byte(`{"entries":[],"actions":[],"quota":{"used":0,"limit":10485760,"tier":"free"}}`))
		case "/v1/files/objects":
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("未预期请求: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	setAccountAPIBase(client.base)
	t.Cleanup(func() { setAccountAPIBase("") })

	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{Entries: []fileSyncEntry{{
		ID: "e1", Name: "notes", Kind: "dir", SourcePath: filepath.Join(dir, "old"), OriginDevice: "dev-self",
		Files: map[string]fileSyncLocalFile{},
	}}}
	fileSyncMu.Unlock()
	if _, err := fileSyncRelocateEntry("e1", newRoot); err != nil {
		t.Fatalf("更改位置失败: %v", err)
	}
	select {
	case got := <-patched:
		if got != newRoot {
			t.Fatalf("应把新位置登记到账号，实际 %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("本机创建的条目应把新位置登记到账号（PATCH source-path）")
	}

	// 其它设备创建的条目：只改本机
	peerRoot := filepath.Join(dir, "peer-root")
	if err := os.MkdirAll(peerRoot, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	fileSyncMu.Lock()
	fileSyncCur = fileSyncState{Entries: []fileSyncEntry{{
		ID: "e2", Name: "peer", Kind: "dir", SourcePath: filepath.Join(dir, "old"), OriginDevice: "dev-peer",
		Files: map[string]fileSyncLocalFile{},
	}}}
	fileSyncMu.Unlock()
	if _, err := fileSyncRelocateEntry("e2", peerRoot); err != nil {
		t.Fatalf("更改位置失败: %v", err)
	}
	select {
	case got := <-patched:
		t.Fatalf("其它设备的条目不该登记到账号，却收到了 %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestFileSyncStateRoundTrip(t *testing.T) {
	dir := setupFileSyncTest(t)
	st := fileSyncState{
		Entries: []fileSyncEntry{{
			ID: "e1", Name: "notes", Kind: "dir", SourcePath: filepath.Join(dir, "src"),
			Files: map[string]fileSyncLocalFile{
				"a.txt": {Size: 5, Mtime: 1700000000, Sha256: "aa", SyncedSha: "aa", SyncedSize: 5, Rev: 1},
				"b.txt": {Size: 1, Mtime: 1700000001, Sha256: "bb", SyncedSha: "bb", Rev: 1, RemovedLocally: true},
			},
			Ignored: map[string]int64{"c.txt": 1700000002},
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
		!got.Entries[0].Files["b.txt"].RemovedLocally || got.Entries[0].Ignored["c.txt"] != 1700000002 {
		t.Fatalf("往返后条目状态丢失: %+v", got.Entries)
	}
	if got.QuotaUsed != 5 || got.QuotaLimit != 10*1024*1024 || got.LastSyncedAt != 1700000003 {
		t.Fatalf("往返后容量/时间丢失: %+v", got)
	}
	if len(got.PendingApply) != 1 || got.PendingApply[0].Sha256 != "bb" {
		t.Fatalf("往返后待应用动作丢失: %+v", got.PendingApply)
	}
}

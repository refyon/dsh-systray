// account_files.go：文件/文件夹同步引擎——本机扫描、增量上传、删除传播、对账与应用。
//
// 语义（用户已确认的推荐默认，见 docs/评估-dsh-systray-文件同步.md）：
//   - 源设备（添加条目的那台）：文件留在原位；接收设备：落到「接收目录/<显示名>/<相对路径>」；
//   - 本机改动**自动上传**（用户自己的动作，无需确认）；服务端下达的改动进待应用集合，
//     由用户点「应用」才落地——不自动覆盖本机文件；
//   - 冲突（两端都改了）：服务端较新时先把本机版本另存为「(冲突-本机)」副本再下载；
//   - 远端删除：源设备只停止同步（**不删用户原文件**），接收设备删除本地副本；
//   - 容量不足：拦在本地预检（服务端闸门仍是权威），状态标「容量不足」并提示清理。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	// maxFileSyncBytes 单文件上限（与服务端一致：免费档容量 10 MiB）。
	maxFileSyncBytes = 10 * 1024 * 1024
	// fileSyncLocalInterval 本机扫描周期（只 stat，不联网；有变更才上传）。
	fileSyncLocalInterval = 60 * time.Second
	// fileSyncRemoteInterval 远端对账最小间隔（无本机变更时的节流）。
	fileSyncRemoteInterval = 5 * time.Minute
	// fileSyncCheckTimeout 一次「扫描 + 上传 + 对账」的总预算。
	fileSyncCheckTimeout = 5 * time.Minute
	// fileSyncApplyTimeout 一次「应用待生效改动」的总预算（含下载全部文件）。
	fileSyncApplyTimeout = 10 * time.Minute
	// maxFileSyncItems 单次对账上报的条数上限（与服务端一致）。
	maxFileSyncItems = 2000
)

var (
	fileSyncMu        sync.Mutex
	fileSyncCur       fileSyncState
	fileSyncSyncing   bool
	fileSyncApplying  bool
	fileSyncLastCheck int64
)

// initFileSyncState 启动时载入清单（只读，不阻塞启动），并按本机现状重判待应用动作。
//
// 上次退出可能被强杀：已下载/已满足的动作不该继续挂着（否则下次点「应用」会重复下载）。
func initFileSyncState() {
	fileSyncMu.Lock()
	fileSyncCur = loadFileSyncState()
	entries := len(fileSyncCur.Entries)
	if entries > 0 {
		fileSyncScanAllLocked()
	}
	kept, dropped := fileSyncFilterActionsLocked(fileSyncCur.PendingApply)
	fileSyncCur.PendingApply = kept
	if dropped > 0 {
		_ = saveFileSyncStateLocked(fileSyncCur)
	}
	pending := len(kept)
	fileSyncMu.Unlock()
	if entries > 0 || pending > 0 || dropped > 0 {
		log.Printf("[files] 已载入文件同步清单：%d 个条目，%d 项待应用（启动重判丢弃 %d 项）", entries, pending, dropped)
	}
}

// resetFileSyncStateForAccountSwitch 换账号时作废文件同步清单：远端条目属于旧账号，
// 沿用会让新账号下的对账把本机条目判成「服务端已删除」而误删本地副本。
func resetFileSyncStateForAccountSwitch() {
	fileSyncMu.Lock()
	had := len(fileSyncCur.Entries)
	fileSyncCur = fileSyncState{}
	_ = saveFileSyncStateLocked(fileSyncCur)
	fileSyncMu.Unlock()
	if had > 0 {
		log.Printf("[files] 账号已切换：文件同步清单已清空（原 %d 个条目需在新账号下重新添加）", had)
	}
}

// ==================== 扫描 ====================

// fileSyncExcludedFile 扫描时跳过的系统/临时文件（不排除会让列表永远处于「有改动」）。
func fileSyncExcludedFile(name string) bool {
	switch strings.ToLower(name) {
	case "thumbs.db", "ehthumbs.db", "desktop.ini", ".ds_store", "icon\r":
		return true
	}
	return strings.HasPrefix(name, "~$") // Office 打开文档时生成的临时文件
}

// fileSyncExcludedDir 扫描时跳过的目录（系统回收站与卷信息；其余目录尊重用户选择）。
func fileSyncExcludedDir(name string) bool {
	switch strings.ToLower(name) {
	case "$recycle.bin", "system volume information", ".trash", ".trashes":
		return true
	}
	return false
}

// samePath 两个路径是否指向同一位置（Windows 大小写不敏感，统一小写比较）。
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// fileSyncHashFile 流式计算文件摘要（小写十六进制）。
func fileSyncHashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileSyncHashBytes 计算内存内容的摘要。
func fileSyncHashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fileSyncScanAllLocked 扫描全部条目（调用方须持有 fileSyncMu）。
func fileSyncScanAllLocked() {
	for i := range fileSyncCur.Entries {
		_ = fileSyncScanEntryLocked(&fileSyncCur.Entries[i])
	}
}

// fileSyncScanEntryLocked 扫描一个条目：刷新文件状态、登记本机删除、清理已恢复的删除台账。
func fileSyncScanEntryLocked(e *fileSyncEntry) error {
	root := fileSyncEntryRoot(*e)
	if root == "" {
		e.Error = T("本机路径不可用")
		return errors.New("entry root empty")
	}
	if e.Files == nil {
		e.Files = map[string]fileSyncLocalFile{}
	}
	if e.Deletions == nil {
		e.Deletions = map[string]int64{}
	}
	now := fileSyncNow()

	if e.Kind == "file" {
		st, err := os.Stat(root)
		if err != nil || st.IsDir() {
			rel := filepath.Base(root)
			if _, ok := e.Files[rel]; ok {
				e.Deletions[rel] = now // 源文件被删除/改名 → 传播删除
			}
			e.Error = ""
			return nil
		}
		fileSyncNoteFileLocked(e, filepath.Base(root), st)
		e.Error = ""
		return nil
	}

	seen := map[string]bool{}
	receive := fileSyncReceiveDir()
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个条目读不到（占用/权限）不中断整体扫描
		}
		if d.IsDir() {
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil || rel == "." {
				return nil
			}
			if receive != "" && samePath(p, receive) {
				return filepath.SkipDir // 接收目录自身不进同步（防自嵌套）
			}
			if fileSyncExcludedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil // 符号链接/junction 跳过（防环与越界）
		}
		if fileSyncExcludedFile(d.Name()) {
			return nil
		}
		st, serr := d.Info()
		if serr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		fileSyncNoteFileLocked(e, rel, st)
		return nil
	})
	if walkErr != nil {
		e.Error = T("读取目录失败：") + walkErr.Error()
		return walkErr
	}
	for rel := range e.Files {
		if !seen[rel] {
			e.Deletions[rel] = now // 本机删除 → 待传播
		}
	}
	for rel := range e.Deletions {
		if seen[rel] {
			delete(e.Deletions, rel) // 文件又出现了：撤销删除台账
		}
	}
	e.Error = ""
	return nil
}

// fileSyncNoteFileLocked 刷新单个文件的本机状态；大小与修改时间都没变时复用已算摘要。
func fileSyncNoteFileLocked(e *fileSyncEntry, rel string, st os.FileInfo) {
	// 远端已删除、本机保留的文件不进入同步；文件被改动（mtime 更新）视为用户重建 → 重新同步。
	if ignoredAt, ok := e.Ignored[rel]; ok {
		if st.ModTime().Unix() <= ignoredAt {
			return
		}
		delete(e.Ignored, rel)
	}
	prev := e.Files[rel]
	size, mtime := st.Size(), st.ModTime().Unix()
	if prev.Sha256 != "" && prev.Size == size && prev.Mtime == mtime {
		return
	}
	sha, err := fileSyncHashFile(fileSyncEntryLocalPath(*e, rel))
	if err != nil {
		prev.Size, prev.Mtime = size, mtime
		prev.Error = T("读取失败：") + err.Error()
		prev.Blocked = false
		e.Files[rel] = prev
		return
	}
	m := prev
	m.Size, m.Mtime, m.Sha256 = size, mtime, sha
	m.Error, m.Blocked = "", false // 内容变了：清掉上次的失败/拦下状态，重新尝试
	e.Files[rel] = m
}

// fileSyncHasLocalChangesLocked 是否有待上传内容或待传播删除（调用方须持有 fileSyncMu）。
func fileSyncHasLocalChangesLocked() bool {
	for _, e := range fileSyncCur.Entries {
		if len(e.Deletions) > 0 {
			return true
		}
		for _, m := range e.Files {
			if m.Sha256 != "" && m.Sha256 != m.SyncedSha {
				return true
			}
		}
	}
	return false
}

// ==================== 上传与删除传播 ====================

// fileSyncUploadEntryLocked 上传条目内需要同步的文件，返回（上传数, 容量拦下数）。
func fileSyncUploadEntryLocked(ctx context.Context, client *accountClient, token string, e *fileSyncEntry, q *fileQuota) (int, int) {
	uploaded, blocked := 0, 0
	rels := make([]string, 0, len(e.Files))
	for rel := range e.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		m := e.Files[rel]
		if _, gone := e.Deletions[rel]; gone {
			continue // 本机已删除：由删除传播处理
		}
		if m.Sha256 == "" || m.Sha256 == m.SyncedSha {
			continue // 读不到内容，或已同步
		}
		// 容量预检：替换文件时扣除它上次的占用（服务端闸门仍是权威，这里只是省一次 10 MiB 往返）
		projected := q.Used - m.SyncedSize + m.Size
		if m.Size > q.Limit || projected > q.Limit {
			m.Blocked, m.Error = true, T("可用容量不足")
			e.Files[rel] = m
			blocked++
			continue
		}
		data, err := os.ReadFile(fileSyncEntryLocalPath(*e, rel))
		if err != nil {
			m.Blocked, m.Error = false, T("读取失败：")+err.Error()
			e.Files[rel] = m
			continue
		}
		resp, err := client.FilesUpload(ctx, token, e.ID, rel, data, m.Sha256, m.Mtime)
		if err != nil {
			if accountErrorCode(err) == accErrQuotaExceeded {
				m.Blocked, m.Error = true, T("可用容量不足")
				blocked++
			} else {
				m.Blocked, m.Error = false, accountErrorText(err)
			}
			e.Files[rel] = m
			continue
		}
		m.SyncedSha, m.SyncedSize, m.Rev = m.Sha256, m.Size, resp.Rev
		m.Error, m.Blocked = "", false
		e.Files[rel] = m
		if resp.Limit > 0 {
			q.Used, q.Limit, q.Tier = resp.Used, resp.Limit, resp.Tier
		}
		uploaded++
	}
	return uploaded, blocked
}

// fileSyncPropagateDeletionsLocked 把本机删除传播到服务端（打墓碑），返回成功条数。
func fileSyncPropagateDeletionsLocked(ctx context.Context, client *accountClient, token string, e *fileSyncEntry, q *fileQuota) int {
	done := 0
	rels := make([]string, 0, len(e.Deletions))
	for rel := range e.Deletions {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		out, err := client.FilesDeleteObject(ctx, token, e.ID, rel)
		if err != nil {
			if accountErrorCode(err) == accErrNotFound {
				// 服务端本就没有这个文件：台账照样清掉
				delete(e.Deletions, rel)
				delete(e.Files, rel)
				done++
			}
			continue // 其它错误（网络）：保留台账，下次重试
		}
		delete(e.Deletions, rel)
		delete(e.Files, rel)
		if out.Limit > 0 {
			q.Used, q.Limit, q.Tier = out.Used, out.Limit, out.Tier
		}
		done++
	}
	return done
}

// ==================== 对账 ====================

// fileSyncResult 一次文件同步的结果（日志与测试断言用）。
type fileSyncResult struct {
	Uploaded int
	Deleted  int
	Pending  int
	Blocked  int
}

// fileSyncBuildRequestLocked 组装对账请求（调用方须持有 fileSyncMu）。
func fileSyncBuildRequestLocked() fileSyncRequest {
	req := fileSyncRequest{}
	for _, e := range fileSyncCur.Entries {
		req.Entries = append(req.Entries, fileSyncEntryBody{ID: e.ID, Name: e.Name, Kind: e.Kind})
		for rel, m := range e.Files {
			if _, gone := e.Deletions[rel]; gone {
				continue // 本机已删除：不进对账，由 DELETE 传播
			}
			if m.Sha256 == "" {
				continue
			}
			req.Files = append(req.Files, fileSyncLocalMeta{EntryID: e.ID, RelPath: rel, Size: m.Size, Sha256: m.Sha256, Mtime: m.Mtime})
		}
	}
	sort.Slice(req.Entries, func(i, j int) bool { return req.Entries[i].ID < req.Entries[j].ID })
	sort.Slice(req.Files, func(i, j int) bool {
		if req.Files[i].EntryID != req.Files[j].EntryID {
			return req.Files[i].EntryID < req.Files[j].EntryID
		}
		return req.Files[i].RelPath < req.Files[j].RelPath
	})
	if len(req.Files) > maxFileSyncItems {
		log.Printf("[files] 对账文件数 %d 超出上报上限 %d，本轮只报前 %d 个", len(req.Files), maxFileSyncItems, maxFileSyncItems)
		req.Files = req.Files[:maxFileSyncItems]
	}
	return req
}

// fileSyncFilterActionsLocked 过滤「本机已经满足」的动作，其余进待应用集合（调用方须持有 fileSyncMu）。
func fileSyncFilterActionsLocked(actions []fileSyncAction) ([]fileSyncAction, int) {
	out := make([]fileSyncAction, 0, len(actions))
	skipped := 0
	for _, a := range actions {
		idx := fileSyncFindEntryLocked(a.EntryID)
		switch a.Kind {
		case "upload":
			if idx >= 0 {
				if m, ok := fileSyncCur.Entries[idx].Files[a.RelPath]; ok && m.SyncedSha != "" && m.Sha256 == m.SyncedSha {
					skipped++
					continue
				}
			}
		case "download":
			if idx >= 0 {
				if m, ok := fileSyncCur.Entries[idx].Files[a.RelPath]; ok && m.Sha256 == a.Sha256 {
					skipped++
					continue
				}
			}
		case "remove_file":
			if idx >= 0 {
				if _, ok := fileSyncCur.Entries[idx].Files[a.RelPath]; !ok {
					skipped++
					continue
				}
			}
		case "create_entry":
			if idx >= 0 {
				skipped++
				continue
			}
		case "rename_entry":
			if idx >= 0 && fileSyncCur.Entries[idx].Name == a.Name {
				skipped++
				continue
			}
		case "remove_entry":
			if idx < 0 {
				skipped++
				continue
			}
		}
		out = append(out, a)
	}
	return out, skipped
}

// fileSyncCheck 一次完整同步：扫描 → 上传本机变更 → 传播本机删除 → 对账取待应用动作。
func fileSyncCheck(ctx context.Context, client *accountClient) (fileSyncResult, error) {
	var res fileSyncResult
	if !beginFileSync() {
		return res, errors.New(T("正在同步，请稍候再试"))
	}
	defer endFileSync()

	token := accountToken()
	if token == "" {
		return res, &accountError{Code: accErrUnauthorized, Message: "未登录"}
	}

	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()

	fileSyncScanAllLocked()

	q, err := client.FilesQuota(ctx, token)
	if err != nil {
		fileSyncCur.LastError = accountErrorText(err)
		_ = saveFileSyncStateLocked(fileSyncCur)
		if accountErrorCode(err) == accErrUnauthorized {
			accountInvalidateSession()
		}
		return res, err
	}

	for i := range fileSyncCur.Entries {
		up, blocked := fileSyncUploadEntryLocked(ctx, client, token, &fileSyncCur.Entries[i], &q)
		res.Uploaded += up
		res.Blocked += blocked
		res.Deleted += fileSyncPropagateDeletionsLocked(ctx, client, token, &fileSyncCur.Entries[i], &q)
	}

	resp, err := client.FilesSync(ctx, token, fileSyncBuildRequestLocked())
	if err != nil {
		fileSyncCur.LastError = accountErrorText(err)
		_ = saveFileSyncStateLocked(fileSyncCur)
		if accountErrorCode(err) == accErrUnauthorized {
			accountInvalidateSession()
		}
		return res, err
	}
	apply, _ := fileSyncFilterActionsLocked(resp.Actions)
	fileSyncCur.PendingApply = apply
	if resp.Quota.Limit > 0 {
		fileSyncCur.QuotaUsed, fileSyncCur.QuotaLimit, fileSyncCur.QuotaTier = resp.Quota.Used, resp.Quota.Limit, resp.Quota.Tier
	} else {
		fileSyncCur.QuotaUsed = q.Used
	}
	fileSyncCur.LastSyncedAt = fileSyncNow()
	fileSyncCur.LastError = ""
	fileSyncLastCheck = fileSyncCur.LastSyncedAt
	res.Pending = len(apply)
	_ = saveFileSyncStateLocked(fileSyncCur)
	return res, nil
}

// ==================== 应用待生效改动 ====================

// fileSyncApplyResult 应用结果（前端提示用）。
type fileSyncApplyResult struct {
	Applied int
	Failed  int
	Errors  []string
}

// fileSyncApplyAll 执行待应用动作：建条目 → 重命名 → 下载 → 删文件 → 移除条目。
//
// 安全边界：源设备（本机添加的条目）**不删用户的原始文件**——远端删除只让它停止同步；
// 接收设备（本机只有副本）按同步语义删除副本。失败项保留在待应用集合，可再次点击续做。
func fileSyncApplyAll(ctx context.Context, client *accountClient) (fileSyncApplyResult, error) {
	var res fileSyncApplyResult
	token := accountToken()
	if token == "" {
		return res, &accountError{Code: accErrUnauthorized, Message: "未登录"}
	}
	fileSyncMu.Lock()
	pending := append([]fileSyncAction(nil), fileSyncCur.PendingApply...)
	fileSyncMu.Unlock()
	if len(pending) == 0 {
		return res, nil
	}

	// 第一阶段：条目级（先建/改名，下载才有落点）
	rest := make([]fileSyncAction, 0, len(pending))
	for _, a := range pending {
		switch a.Kind {
		case "create_entry", "rename_entry":
			if err := fileSyncApplyEntryAction(a); err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("%s：%s", a.Name, err.Error()))
				continue
			}
			res.Applied++
		default:
			rest = append(rest, a)
		}
	}
	// 第二阶段：下载与删除
	kept := make([]fileSyncAction, 0, len(rest))
	for _, a := range rest {
		var err error
		switch a.Kind {
		case "download":
			err = fileSyncApplyDownload(a, client, ctx, token)
		case "remove_file":
			err = fileSyncApplyRemoveFile(a)
		case "remove_entry":
			err = fileSyncApplyRemoveEntry(a)
		case "upload":
			err = nil // 上传由同步阶段负责，不该出现在待应用集合；忽略
		default:
			err = nil
		}
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s/%s：%s", fileSyncEntryLabel(a.EntryID), a.RelPath, err.Error()))
			kept = append(kept, a)
			continue
		}
		res.Applied++
	}

	fileSyncMu.Lock()
	fileSyncCur.PendingApply = kept
	_ = saveFileSyncStateLocked(fileSyncCur)
	fileSyncMu.Unlock()
	return res, nil
}

// fileSyncApplyEntryAction 建条目 / 重命名条目（本地动作，不改服务端）。
func fileSyncApplyEntryAction(a fileSyncAction) error {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	idx := fileSyncFindEntryLocked(a.EntryID)
	switch a.Kind {
	case "create_entry":
		if idx >= 0 {
			return nil
		}
		fileSyncCur.Entries = append(fileSyncCur.Entries, fileSyncEntry{
			ID:    a.EntryID,
			Name:  a.Name,
			Kind:  firstNonEmpty(a.EntryKind, "dir"),
			Files: map[string]fileSyncLocalFile{},
		})
		return nil
	case "rename_entry":
		if idx < 0 {
			return nil
		}
		e := &fileSyncCur.Entries[idx]
		old := e.Name
		e.Name = a.Name
		if strings.TrimSpace(e.SourcePath) == "" && old != a.Name {
			oldRoot := fileSyncEntryRoot(fileSyncEntry{Name: old})
			newRoot := fileSyncEntryRoot(*e)
			if oldRoot != "" && newRoot != "" {
				if _, err := os.Stat(oldRoot); err == nil {
					if err := os.Rename(oldRoot, newRoot); err != nil {
						e.Error = T("重命名本机目录失败：") + err.Error()
					}
				}
			}
		}
		return nil
	}
	return nil
}

// fileSyncApplyDownload 下载一个文件到本机（必要时先保留冲突副本）。
func fileSyncApplyDownload(a fileSyncAction, client *accountClient, ctx context.Context, token string) error {
	fileSyncMu.Lock()
	idx := fileSyncFindEntryLocked(a.EntryID)
	if idx < 0 {
		fileSyncMu.Unlock()
		return errors.New(T("条目不存在"))
	}
	e := fileSyncCur.Entries[idx]
	meta := e.Files[a.RelPath]
	target := fileSyncEntryLocalPath(e, a.RelPath)
	fileSyncMu.Unlock()

	if target == "" {
		return errors.New(T("本机路径不可用"))
	}
	data, err := client.FilesDownload(ctx, token, a.EntryID, a.RelPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	// 冲突副本：本机文件内容与服务端不同，且本机有尚未上传的改动 → 先留副本再覆盖
	if st, serr := os.Stat(target); serr == nil && !st.IsDir() {
		localSha := meta.Sha256
		if localSha == "" {
			localSha, _ = fileSyncHashFile(target)
		}
		pendingLocal := meta.SyncedSha != "" && meta.Sha256 != meta.SyncedSha
		if localSha != a.Sha256 && pendingLocal {
			if _, cerr := fileSyncCopyFile(target, fileSyncConflictPath(target)); cerr != nil {
				return errors.New(T("保留冲突副本失败：") + cerr.Error())
			}
		}
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return err
	}
	if a.Mtime > 0 {
		mt := time.Unix(a.Mtime, 0)
		_ = os.Chtimes(target, mt, mt) // 保持服务端时间：下一次扫描不会误判为「本机改动」
	}
	fileSyncMu.Lock()
	if idx := fileSyncFindEntryLocked(a.EntryID); idx >= 0 {
		ee := &fileSyncCur.Entries[idx]
		if ee.Files == nil {
			ee.Files = map[string]fileSyncLocalFile{}
		}
		ee.Files[a.RelPath] = fileSyncLocalFile{
			Size:       int64(len(data)),
			Mtime:      a.Mtime,
			Sha256:     a.Sha256,
			SyncedSha:  a.Sha256,
			SyncedSize: int64(len(data)),
			Rev:        a.Rev,
		}
	}
	fileSyncMu.Unlock()
	return nil
}

// fileSyncApplyRemoveFile 远端删除一个文件：源设备保留原文件只停止同步，接收设备删除副本。
func fileSyncApplyRemoveFile(a fileSyncAction) error {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	idx := fileSyncFindEntryLocked(a.EntryID)
	if idx < 0 {
		return nil
	}
	e := &fileSyncCur.Entries[idx]
	if strings.TrimSpace(e.SourcePath) == "" {
		target := fileSyncEntryLocalPath(*e, a.RelPath)
		if target != "" {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			fileSyncPruneEmptyDirs(filepath.Dir(target), fileSyncEntryRoot(*e))
		}
	} else {
		// 源设备：不删用户原文件，记入忽略名单（文件被改动后会自动重新同步）
		if e.Ignored == nil {
			e.Ignored = map[string]int64{}
		}
		e.Ignored[a.RelPath] = fileSyncNow()
	}
	delete(e.Files, a.RelPath)
	delete(e.Deletions, a.RelPath)
	return nil
}

// fileSyncApplyRemoveEntry 远端移除条目：接收设备删副本目录，源设备保留原目录只停止同步。
func fileSyncApplyRemoveEntry(a fileSyncAction) error {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	idx := fileSyncFindEntryLocked(a.EntryID)
	if idx < 0 {
		return nil
	}
	e := fileSyncCur.Entries[idx]
	if strings.TrimSpace(e.SourcePath) == "" {
		if root := fileSyncEntryRoot(e); root != "" && fileSyncSafeToDelete(root) {
			if err := os.RemoveAll(root); err != nil {
				return err
			}
		}
	}
	fileSyncCur.Entries = append(fileSyncCur.Entries[:idx], fileSyncCur.Entries[idx+1:]...)
	return nil
}

// fileSyncConflictPath 冲突副本路径：`名字 (冲突-本机).扩展名`（已存在则追加序号）。
func fileSyncConflictPath(target string) string {
	dir := filepath.Dir(target)
	ext := filepath.Ext(target)
	base := strings.TrimSuffix(filepath.Base(target), ext)
	cand := filepath.Join(dir, fmt.Sprintf("%s (冲突-本机)%s", base, ext))
	for i := 2; i < 100; i++ {
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
		cand = filepath.Join(dir, fmt.Sprintf("%s (冲突-本机 %d)%s", base, i, ext))
	}
	return cand
}

// fileSyncCopyFile 复制文件（冲突副本用；上限 10 MiB，直接整读）。
func fileSyncCopyFile(src, dst string) (int64, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

// fileSyncPruneEmptyDirs 删除空目录（最多上溯到 root 为止）。
func fileSyncPruneEmptyDirs(dir, root string) {
	for dir != "" && root != "" && !samePath(dir, root) && strings.HasPrefix(strings.ToLower(filepath.Clean(dir)), strings.ToLower(filepath.Clean(root))) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// fileSyncEntryLabel 条目显示名（找不到返回 id）。
func fileSyncEntryLabel(entryID string) string {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	return fileSyncEntryNameLocked(entryID)
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// fileSyncSafeToDelete 危险删除前的保护：拒绝卷根、用户主目录与接收目录本身。
func fileSyncSafeToDelete(path string) bool {
	p := filepath.Clean(path)
	if p == "" || p == "." {
		return false
	}
	if filepath.Dir(p) == p {
		return false // 卷根（C:\ 或 /）
	}
	if home, err := os.UserHomeDir(); err == nil && samePath(p, home) {
		return false
	}
	if receive := fileSyncReceiveDir(); receive != "" && samePath(p, receive) {
		return false
	}
	return true
}

// ==================== 并发闸门与事件 ====================

// beginFileSync 置位「同步进行中」；已有同步/应用在跑时返回 false。
func beginFileSync() bool {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	if fileSyncSyncing || fileSyncApplying {
		return false
	}
	fileSyncSyncing = true
	return true
}

func endFileSync() {
	fileSyncMu.Lock()
	fileSyncSyncing = false
	fileSyncMu.Unlock()
}

// accountToken 当前登录令牌（未登录为空）。
func accountToken() string {
	accountMu.Lock()
	defer accountMu.Unlock()
	if !accountCur.loggedIn(time.Now()) {
		return ""
	}
	return accountCur.Token
}

// emitFilesChanged 广播文件同步状态变化（前端刷新文件卡与容量条）。
func emitFilesChanged() {
	if appCtx == nil {
		return
	}
	wruntime.EventsEmit(appCtx, "files:changed", fileSyncStatusSnapshot())
}

// fileSyncStatusSnapshot 组装快照（公共入口，内部加锁）。
func fileSyncStatusSnapshot() FileSyncStatusInfo {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	return fileSyncSnapshotLocked()
}

// fileSyncKick 异步触发一次同步（不阻塞调用方）。
func fileSyncKick() {
	if shotMode || !accountLoggedIn() {
		return
	}
	accountSyncWG.Add(1)
	go func() {
		defer accountSyncWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), fileSyncCheckTimeout)
		defer cancel()
		res, err := fileSyncCheck(ctx, newAccountClient(""))
		if err != nil {
			log.Printf("[files] 后台同步失败: %v", err)
		} else if res.Uploaded > 0 || res.Deleted > 0 || res.Pending > 0 || res.Blocked > 0 {
			log.Printf("[files] 后台同步完成：上传 %d、删除 %d、待应用 %d、容量拦下 %d", res.Uploaded, res.Deleted, res.Pending, res.Blocked)
		}
		emitFilesChanged()
	}()
}

// startFileSyncBackground 文件同步后台循环：60s 扫描本机；有变更或距上次对账超时即联网同步。
func startFileSyncBackground(ctx context.Context) {
	if ctx == nil || shotMode {
		return
	}
	go func() {
		ticker := time.NewTicker(fileSyncLocalInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !accountLoggedIn() {
					continue
				}
				fileSyncMu.Lock()
				fileSyncScanAllLocked()
				dirty := fileSyncHasLocalChangesLocked()
				due := fileSyncNow()-fileSyncLastCheck >= int64(fileSyncRemoteInterval/time.Second)
				fileSyncMu.Unlock()
				if !dirty && !due {
					continue
				}
				cctx, cancel := context.WithTimeout(ctx, fileSyncCheckTimeout)
				res, err := fileSyncCheck(cctx, newAccountClient(""))
				cancel()
				if err != nil {
					log.Printf("[files] 后台同步失败: %v", err)
				} else if res.Uploaded > 0 || res.Deleted > 0 || res.Pending > 0 {
					log.Printf("[files] 后台同步完成：上传 %d、删除 %d、待应用 %d", res.Uploaded, res.Deleted, res.Pending)
				}
				emitFilesChanged()
			}
		}
	}()
}

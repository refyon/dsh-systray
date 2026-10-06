// account_files.go：文件/文件夹同步引擎——本机扫描、增量上传、对账与应用。
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
	// 大文件夹（几百到上千个文件）即使 5 并发也可能超过几分钟——预算给足，
	// 否则中途 context 取消会让在传文件全部报「context deadline exceeded」（2026-10-05 现场）。
	fileSyncCheckTimeout = 15 * time.Minute
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
	// 上传进度（仅运行时，不落盘）：前端显示「正在上传 x/y · n KB/s」。
	fileSyncProg          fileSyncProgress
	fileSyncProgLastAt    time.Time
	fileSyncProgLastBytes int64
	// 「同步中」标记的推送节流（与进度推送分开，避免互相影响）
	fileSyncInFlightEmitAt time.Time
	// 本轮上传失败日志计数（只记前几条，避免几百个文件刷屏）。
	fileSyncErrLogCount int
)

// fileSyncProgress 本机上传进度。
type fileSyncProgress struct {
	Active   bool
	Done     int
	Total    int
	Bytes    int64
	SpeedBps int64
	// InFlight 本轮在传文件（entryID\x00rel → true）：前端据此把该行显示为「同步中」。
	InFlight map[string]bool
	// FileSpeeds 各文件上一次上传的实测速度（B/s，entryID\x00rel → 速度）。
	FileSpeeds map[string]int64
	// Canceled 本轮已取消的上传：键为条目 id（整条目移除）或 taskKey（单个文件/子目录移除）。
	Canceled map[string]bool
}

// fileSyncCancelUploads 取消某条目（rel==""）或条目内某个文件/子目录的在传任务：
// 移除同步条目/内容后不应继续上传（用户要求）。已在传的那个请求无法中断，
// 但排队中的任务会立刻跳过，结果也会被忽略。
func fileSyncCancelUploads(entryID, rel string) {
	if entryID == "" {
		return
	}
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	if fileSyncProg.Canceled == nil {
		fileSyncProg.Canceled = map[string]bool{}
	}
	if rel == "" {
		fileSyncProg.Canceled[entryID] = true
	} else {
		fileSyncProg.Canceled[fileSyncTaskKey(entryID, rel)] = true
	}
	for key := range fileSyncProg.InFlight {
		match := key == fileSyncTaskKey(entryID, rel)
		if rel == "" {
			match = strings.HasPrefix(key, entryID+"\x00")
		} else {
			// 子目录：rel 前缀下的所有文件
			match = strings.HasPrefix(key, fileSyncTaskKey(entryID, rel)) || strings.HasPrefix(key, fileSyncTaskKey(entryID, rel+"/"))
		}
		if match {
			delete(fileSyncProg.InFlight, key)
			if fileSyncProg.Total > fileSyncProg.Done {
				fileSyncProg.Total-- // 不再计入本轮分母，进度不会卡在 x/y
			}
		}
	}
}

// fileSyncUploadCanceled 该文件的上传是否已被取消（worker 上传前检查）。
func fileSyncUploadCanceled(entryID, rel string) bool {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	if fileSyncProg.Canceled == nil {
		return false
	}
	return fileSyncProg.Canceled[entryID] || fileSyncProg.Canceled[fileSyncTaskKey(entryID, rel)]
}

// fileSyncCountPendingUpload 该条目本轮需要上传的文件数（进度分母）。
func fileSyncCountPendingUpload(e *fileSyncEntry) int {
	n := 0
	for _, m := range e.Files {
		if m.Sha256 == "" || m.Sha256 == m.SyncedSha {
			continue
		}
		n++
	}
	return n
}

// fileSyncProgressTickLocked 推进进度、刷新已用容量，并按 300ms 节流推一次事件
// （调用方须持有 fileSyncMu）。
func fileSyncProgressTickLocked(size, used int64) {
	fileSyncProg.Done++
	fileSyncProg.Bytes += size
	if used > 0 {
		fileSyncCur.QuotaUsed = used
	}
	now := time.Now()
	if fileSyncProgLastAt.IsZero() {
		fileSyncProgLastAt, fileSyncProgLastBytes = now, 0
		return
	}
	elapsed := now.Sub(fileSyncProgLastAt)
	if elapsed < 300*time.Millisecond {
		return
	}
	if secs := elapsed.Seconds(); secs > 0 {
		fileSyncProg.SpeedBps = int64(float64(fileSyncProg.Bytes-fileSyncProgLastBytes) / secs)
	}
	fileSyncProgLastAt, fileSyncProgLastBytes = now, fileSyncProg.Bytes
	_ = saveFileSyncStateLocked(fileSyncCur)
	fileSyncEmitLocked()
}

// fileSyncEmitLocked 在持锁状态下把快照推给前端（EventsEmit 放到 goroutine，避免锁内做 IO）。
func fileSyncEmitLocked() {
	if appCtx == nil {
		return
	}
	snap := fileSyncSnapshotLocked()
	go wruntime.EventsEmit(appCtx, "files:changed", snap)
}

// initFileSyncState 启动时载入清单（只读，不阻塞启动），并按本机现状重判待应用动作。
//
// 上次退出可能被强杀：已下载/已满足的动作不该继续挂着（否则下次点「应用」会重复下载）。
func initFileSyncState() {
	fileSyncMu.Lock()
	fileSyncCur = loadFileSyncState()
	// 上一次会话的失败文案不带到本次启动（界面否则会在首轮同步前显示过期错误）
	fileSyncCur.LastError = ""
	entries := len(fileSyncCur.Entries)
	if entries > 0 {
		fileSyncScanAllLocked()
	}
	kept, dropped, _ := fileSyncFilterActionsLocked(fileSyncCur.PendingApply)
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

// fileSyncOnAccountLogin 登录后处理清单归属：
//
//   - 同一账号重新登录（退出再登录）→ **保留**清单，避免把账号上的文件全量重下一次；
//   - 换账号（或清单没有归属记录）→ 作废：远端条目属于旧账号，沿用会让新账号下的对账
//     把本机条目判成「服务端已删除」而误删本地副本。
func fileSyncOnAccountLogin(userID string) {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	if userID == "" {
		return
	}
	// 登录后不沿用上一次会话的失败文案：等本轮同步真的失败再显示（否则会把历史错误当成当前问题）
	fileSyncCur.LastError = ""
	if fileSyncCur.UserID == userID {
		_ = saveFileSyncStateLocked(fileSyncCur)
		return
	}
	if fileSyncCur.UserID == "" {
		// 旧版本写的清单没有归属记录：**认领**给当前账号（否则每次重新登录都会清空 → 全量重下）
		fileSyncCur.UserID = userID
		_ = saveFileSyncStateLocked(fileSyncCur)
		return
	}
	had := len(fileSyncCur.Entries)
	fileSyncCur = fileSyncState{UserID: userID}
	_ = saveFileSyncStateLocked(fileSyncCur)
	if had > 0 {
		log.Printf("[files] 账号已切换：文件同步清单已清空（原 %d 个条目需在新账号下重新添加）", had)
	}
}

// fileSyncReattachOwnEntriesLocked 自愈：对账响应里若某条目由**本机**创建、且带着原路径，
// 而本机这条却是接收端（老数据、或清单曾丢失后被当成接收端重建），就自动回源路径并重扫。
// 调用方须持有 fileSyncMu。返回回源条数。
func fileSyncReattachOwnEntriesLocked(remote []fileSyncRemoteEntry) int {
	dev := accountDeviceID()
	if dev == "" {
		return 0
	}
	done := 0
	for _, r := range remote {
		if r.OriginDevice != dev || strings.TrimSpace(r.SourcePath) == "" {
			continue
		}
		idx := fileSyncFindEntryLocked(r.ID)
		if idx < 0 {
			continue
		}
		e := &fileSyncCur.Entries[idx]
		if strings.TrimSpace(e.SourcePath) != "" {
			continue // 已经是源设备
		}
		root, ok := fileSyncUsableSourcePath(r.SourcePath, e.Kind)
		if !ok {
			// 原路径在本机不存在：记下来供界面说明，内容按接收端保存
			if e.OriginPath != r.SourcePath || !e.SourcePathMissing {
				e.OriginPath, e.SourcePathMissing = r.SourcePath, true
				log.Printf("[files] 条目 %s 的原路径 %s 在本机不存在，保持接收目录副本", e.Name, r.SourcePath)
			}
			continue
		}
		e.SourcePath, e.SourcePathMissing, e.OriginPath = root, false, ""
		e.Error = ""
		if e.Files == nil {
			e.Files = map[string]fileSyncLocalFile{}
		}
		_ = fileSyncScanEntryLocked(e) // 以原路径重扫（内容一致的文件保持已同步，不会重传）
		log.Printf("[files] 条目 %s 由本机创建，已自动同步回原路径 %s", e.Name, root)
		logUI("自动回源路径", fmt.Sprintf("%s → %s", e.Name, root))
		done++
	}
	return done
}

// fileSyncUsableSourcePath 校验服务端记下的来源路径在本机是否可用：
// 必须是绝对路径、存在，且类型与条目一致（file 对应文件、dir 对应目录）。
func fileSyncUsableSourcePath(path, kind string) (string, bool) {
	p := strings.TrimSpace(path)
	if p == "" || !filepath.IsAbs(p) {
		return "", false
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", false
	}
	if kind == "dir" && !st.IsDir() {
		return "", false
	}
	if kind == "file" && st.IsDir() {
		return "", false
	}
	return p, true
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

// fileSyncScanEntryLocked 扫描一个条目：刷新文件状态，并标记本机已删除的文件（只在本机停止同步）。
func fileSyncScanEntryLocked(e *fileSyncEntry) error {
	root := fileSyncEntryRoot(*e)
	if root == "" {
		e.Error = T("本机路径不可用")
		return errors.New("entry root empty")
	}
	if e.Files == nil {
		e.Files = map[string]fileSyncLocalFile{}
	}

	if e.Kind == "file" {
		st, err := os.Stat(root)
		if err != nil || st.IsDir() {
			rel := filepath.Base(root)
			if m, ok := e.Files[rel]; ok {
				// 原文件被删除/改名 → 只在本机停止同步（不传播），保留「重新同步」入口
				m.RemovedLocally = true
				e.Files[rel] = m
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
		// 重解析点（junction / 符号链接 / pnpm 的 node_modules 链接）一律不进同步：既防环、防越界，
		// 也避免悬空链接产生「读取失败」把整个条目拖成失败态。判定必须走 isReparsePoint——
		// Windows 的 junction **不会**被 Type() 标记成 ModeSymlink，且必须放在 d.IsDir() 之前。
		if info, ierr := d.Info(); ierr == nil && isReparsePoint(info) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
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
	for rel, m := range e.Files {
		if seen[rel] {
			continue
		}
		if m.SyncedSha == "" && m.Rev == 0 {
			delete(e.Files, rel) // 从未上传成功（如读取失败被跳过的链接）：本来就没进账号，直接去掉
			continue
		}
		// 本机文件被删除 → **只在本机停止同步**，不传播到账号/其它设备（源设备与接收端一致）；
		// 保留元数据，界面显示「已在本机移除」，可点「重新同步」拉回。
		m.RemovedLocally = true
		e.Files[rel] = m
	}
	e.Error = ""
	return nil
}

// fileSyncNoteFileLocked 刷新单个文件的本机状态；大小与修改时间都没变时复用已算摘要。
func fileSyncNoteFileLocked(e *fileSyncEntry, rel string, st os.FileInfo) {
	// 文件又出现在本机（用户恢复/重新同步）→ 撤销「已在本机移除」，重新纳入同步
	if prev, ok := e.Files[rel]; ok && prev.RemovedLocally {
		prev.RemovedLocally = false
		e.Files[rel] = prev
	}
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
	m.RemovedLocally = false       // 本机文件又在了（用户恢复/重新同步）：撤销「已在本机移除」
	e.Files[rel] = m
}

// fileSyncHasLocalChangesLocked 是否有待上传的本机改动（调用方须持有 fileSyncMu）。
func fileSyncHasLocalChangesLocked() bool {
	for _, e := range fileSyncCur.Entries {
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
// fileSyncUploadConcurrency 单轮并发上传线程数（用户决策：暂时固定最多 5 个）。
const fileSyncUploadConcurrency = 5

// fileSyncUploadTask 一个待上传文件（锁内构建，锁外执行）。
type fileSyncUploadTask struct {
	entryID    string
	entryName  string
	rel        string
	path       string
	sha        string
	size       int64
	mtime      int64
	syncedSize int64
}

// fileSyncUploadOutcome 单个文件的上传结果。
type fileSyncUploadOutcome struct {
	task     fileSyncUploadTask
	err      error
	readErr  error
	blocked  bool
	canceled bool
	rev      int64
	used     int64
	limit    int64
	tier     string
	elapsed  time.Duration
}

// fileSyncTaskKey 运行时进度表的键（entry + 相对路径）。
func fileSyncTaskKey(entryID, rel string) string { return entryID + "\x00" + rel }

// fileSyncMarkInFlight 标记/取消「该文件正在上传」。只有真正拿到上传线程（并发上限内）的文件
// 才算「同步中」，其余排队中的仍是「待同步」——否则几百个文件全显示同步中（2026-10-05 现场）。
func fileSyncMarkInFlight(entryID, rel string, on bool) {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	if fileSyncProg.InFlight == nil {
		fileSyncProg.InFlight = map[string]bool{}
	}
	key := fileSyncTaskKey(entryID, rel)
	if on {
		fileSyncProg.InFlight[key] = true
		// 让前端尽快看到「同步中」（独立节流，避免影响速度统计）
		now := time.Now()
		if now.Sub(fileSyncInFlightEmitAt) >= 300*time.Millisecond {
			fileSyncInFlightEmitAt = now
			_ = saveFileSyncStateLocked(fileSyncCur)
			fileSyncEmitLocked()
		}
	} else {
		delete(fileSyncProg.InFlight, key)
	}
}

// fileSyncBuildUploadTasksLocked 收集本轮需要上传的文件并做容量预检（调用方须持有 fileSyncMu）。
// 返回（任务列表, 预检拦下的文件数）。
func fileSyncBuildUploadTasksLocked(q *fileQuota) ([]fileSyncUploadTask, int) {
	tasks := make([]fileSyncUploadTask, 0, 16)
	blocked := 0
	if fileSyncProg.InFlight == nil {
		fileSyncProg.InFlight = map[string]bool{}
	}
	for _, key := range fileSyncSortedEntryIndexesLocked() {
		e := &fileSyncCur.Entries[key]
		rels := make([]string, 0, len(e.Files))
		for rel := range e.Files {
			rels = append(rels, rel)
		}
		sort.Strings(rels)
		for _, rel := range rels {
			m := e.Files[rel]
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
			tasks = append(tasks, fileSyncUploadTask{
				entryID: e.ID, entryName: e.Name, rel: rel,
				path: fileSyncEntryLocalPath(*e, rel),
				sha:  m.Sha256, size: m.Size, mtime: m.Mtime, syncedSize: m.SyncedSize,
			})
		}
	}
	return tasks, blocked
}

// fileSyncSortedEntryIndexesLocked 条目下标按名称排序（上传顺序稳定，便于阅读日志）。
func fileSyncSortedEntryIndexesLocked() []int {
	idx := make([]int, 0, len(fileSyncCur.Entries))
	for i := range fileSyncCur.Entries {
		idx = append(idx, i)
	}
	sort.SliceStable(idx, func(a, b int) bool { return fileSyncCur.Entries[idx[a]].Name < fileSyncCur.Entries[idx[b]].Name })
	return idx
}

// fileSyncRunUploads 并发执行上传（最多 fileSyncUploadConcurrency 个线程），结果按完成顺序送回。
func fileSyncRunUploads(ctx context.Context, client *accountClient, token string, tasks []fileSyncUploadTask) <-chan fileSyncUploadOutcome {
	out := make(chan fileSyncUploadOutcome, len(tasks))
	sem := make(chan struct{}, fileSyncUploadConcurrency)
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		go func(t fileSyncUploadTask) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			item := fileSyncUploadOutcome{task: t}
			if fileSyncUploadCanceled(t.entryID, t.rel) {
				item.canceled = true
				out <- item
				return
			}
			// 真正开始上传才算「同步中」（并发上限内的那几个）
			fileSyncMarkInFlight(t.entryID, t.rel, true)
			defer fileSyncMarkInFlight(t.entryID, t.rel, false)
			data, err := os.ReadFile(t.path)
			if err != nil {
				item.readErr = err
				out <- item
				return
			}
			start := time.Now()
			resp, err := client.FilesUpload(ctx, token, t.entryID, t.rel, data, t.sha, t.mtime)
			item.elapsed = time.Since(start)
			item.err = err
			item.rev, item.used, item.limit, item.tier = resp.Rev, resp.Used, resp.Limit, resp.Tier
			item.blocked = accountErrorCode(err) == accErrQuotaExceeded
			out <- item
		}(t)
	}
	go func() { wg.Wait(); close(out) }()
	return out
}

// fileSyncApplyUploadOutcomeLocked 应用单个上传结果（调用方须持有 fileSyncMu）。
func fileSyncApplyUploadOutcomeLocked(res *fileSyncResult, q *fileQuota, o fileSyncUploadOutcome) {
	// 已取消（条目/内容被移除）：不改状态、不计进度、不动容量
	if o.canceled {
		delete(fileSyncProg.InFlight, fileSyncTaskKey(o.task.entryID, o.task.rel))
		return
	}
	size := int64(0)
	if idx := fileSyncFindEntryLocked(o.task.entryID); idx >= 0 {
		e := &fileSyncCur.Entries[idx]
		m := e.Files[o.task.rel]
		switch {
		case o.readErr != nil:
			m.Blocked, m.Error = false, T("读取失败：")+o.readErr.Error()
		case o.err != nil:
			if o.blocked {
				m.Blocked, m.Error = true, T("可用容量不足")
				res.Blocked++
			} else {
				m.Blocked, m.Error = false, accountErrorText(o.err)
			}
			if fileSyncErrLogCount < 3 {
				log.Printf("[files] 上传失败 %s/%s: %v", o.task.entryName, o.task.rel, o.err)
				fileSyncErrLogCount++
			}
		default:
			m.SyncedSha, m.SyncedSize, m.Rev = o.task.sha, o.task.size, o.rev
			m.Error, m.Blocked = "", false
			res.Uploaded++
			size = o.task.size
			if o.elapsed > 0 {
				// Windows 计时器粒度下，毫秒级上传的 elapsed 可能为 0：该文件本轮不记速度（显示时省略）
				if fileSyncProg.FileSpeeds == nil {
					fileSyncProg.FileSpeeds = map[string]int64{}
				}
				fileSyncProg.FileSpeeds[fileSyncTaskKey(o.task.entryID, o.task.rel)] = int64(float64(o.task.size) / o.elapsed.Seconds())
			}
		}
		e.Files[o.task.rel] = m
	}
	delete(fileSyncProg.InFlight, fileSyncTaskKey(o.task.entryID, o.task.rel))
	if o.limit > 0 {
		q.Used, q.Limit, q.Tier = o.used, o.limit, o.tier
	}
	fileSyncProgressTickLocked(size, q.Used)
}

// ==================== 对账 ====================

// fileSyncResult 一次文件同步的结果（日志与测试断言用）。
// 说明：本机删除**不再传播**到服务端（源设备与接收端一致：只在本机停止同步，可「重新同步」拉回），
// 因此没有 Deleted 字段；从账号真正删除只走界面上的「移除」（那是由绑定直接调 DELETE 完成的）。
type fileSyncResult struct {
	Uploaded int
	Pending  int
	Blocked  int
	// Applied 本轮自动落地的远端改动项数（用户要求：远端改动直接更新到本地，不再等点击）
	Applied int
}

// fileSyncBuildRequestLocked 组装对账请求（调用方须持有 fileSyncMu）。
//
// 注意：两个列表**必须初始化为非 nil**——Go 会把 nil slice 序列化成 `null`，
// 而服务端 schema 只认数组（2026-10-05 v1.3.0 现场：空列表 → 400 → 容量读不出、界面报「操作失败」）。
func fileSyncBuildRequestLocked() fileSyncRequest {
	req := fileSyncRequest{Entries: []fileSyncEntryBody{}, Files: []fileSyncLocalMeta{}}
	for _, e := range fileSyncCur.Entries {
		req.Entries = append(req.Entries, fileSyncEntryBody{ID: e.ID, Name: e.Name, Kind: e.Kind})
		for rel, m := range e.Files {
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
func fileSyncFilterActionsLocked(actions []fileSyncAction) ([]fileSyncAction, int, []string) {
	out := make([]fileSyncAction, 0, len(actions))
	orphans := make([]string, 0, 2)
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
				// 内容一致就跳过。注意「已在本机移除」的文件 Sha256 仍是服务端那份摘要，
				// 因此不会被自动拉回；用户点「重新同步」会清掉摘要，下一轮才会真正下载。
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
			// 来源设备就是本机、而本机已无该条目 → 这是自家残留（本机删除时服务端没删干净），
			// 不该提示「来自其它设备的改动」，交给调用方去清理服务端（2026-10-05 现场）。
			if a.OriginDevice != "" && a.OriginDevice == accountDeviceID() {
				orphans = append(orphans, a.EntryID)
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
	return out, skipped, orphans
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

	fileSyncScanAllLocked()

	q, err := client.FilesQuota(ctx, token)
	if err != nil {
		fileSyncCur.LastError = accountErrorText(err)
		_ = saveFileSyncStateLocked(fileSyncCur)
		if accountErrorCode(err) == accErrUnauthorized {
			accountInvalidateSession()
		}
		fileSyncMu.Unlock()
		return res, err
	}
	// 容量即时落盘：后续步骤（上传/对账）失败也不影响前端把容量条显示出来
	fileSyncCur.QuotaUsed, fileSyncCur.QuotaTier = q.Used, q.Tier
	if q.Limit > 0 {
		fileSyncCur.QuotaLimit = q.Limit
	}
	_ = saveFileSyncStateLocked(fileSyncCur)

	// 收集本轮待上传文件（锁内构建，锁外并发执行——上传期间不长时间持锁，界面才能实时刷新）
	fileSyncProg = fileSyncProgress{InFlight: map[string]bool{}, FileSpeeds: map[string]int64{}, Canceled: map[string]bool{}}
	fileSyncProgLastAt, fileSyncProgLastBytes, fileSyncErrLogCount = time.Time{}, 0, 0
	tasks, preBlocked := fileSyncBuildUploadTasksLocked(&q)
	res.Blocked = preBlocked
	fileSyncProg.Active = len(tasks) > 0
	fileSyncProg.Total = len(tasks)
	fileSyncMu.Unlock()

	if len(tasks) > 0 {
		for o := range fileSyncRunUploads(ctx, client, token, tasks) {
			fileSyncMu.Lock()
			fileSyncApplyUploadOutcomeLocked(&res, &q, o)
			fileSyncMu.Unlock()
		}
	}

	fileSyncMu.Lock()
	fileSyncProg.Active = false
	fileSyncProg.InFlight = map[string]bool{}

	resp, err := client.FilesSync(ctx, token, fileSyncBuildRequestLocked())
	if err != nil {
		fileSyncCur.LastError = accountErrorText(err)
		_ = saveFileSyncStateLocked(fileSyncCur)
		if accountErrorCode(err) == accErrUnauthorized {
			accountInvalidateSession()
		}
		fileSyncMu.Unlock()
		return res, err
	}
	apply, _, orphans := fileSyncFilterActionsLocked(resp.Actions)
	fileSyncCur.PendingApply = apply
	// 自愈：服务端记着「本机创建 + 原路径」，而本机这条却是接收端（老数据 / 曾经丢过清单）
	// → 这里直接回源路径（用户要求：源设备自动同步回源路径，不靠人工指定）。
	fileSyncReattachOwnEntriesLocked(resp.Entries)
	// 自家孤儿条目（来源设备=本机、本机已删除）：不提示应用，直接清理服务端残留
	for _, id := range orphans {
		if derr := client.FilesDeleteEntry(ctx, token, id); derr != nil && accountErrorCode(derr) != accErrNotFound {
			log.Printf("[files] 清理孤儿条目 %s 失败（下轮重试）: %v", id, derr)
			continue
		}
		logUI("清理孤儿同步条目", id)
	}
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
	fileSyncMu.Unlock()

	// 远端改动**直接落地**（用户要求：不再等点「应用改动」）；失败项留在待应用集合，下轮自动重试。
	// 小字提示保留痕迹：什么时候应用了多少项（见 RemoteAppliedAt/Count）。
	if len(apply) > 0 && beginFileApplyAuto() {
		ar, aerr := fileSyncApplyAll(ctx, client)
		endFileApply()
		if aerr != nil {
			log.Printf("[files] 自动应用远端改动失败: %v", aerr)
		}
		if ar.Applied > 0 {
			// 小字只报「真正落地的文件数」；纯条目级改动（如远端改名）才回退用条目数
			count := ar.Files
			if count == 0 {
				count = ar.Entries
			}
			fileSyncMu.Lock()
			fileSyncCur.RemoteAppliedAt = fileSyncNow()
			fileSyncCur.RemoteAppliedCount = count
			_ = saveFileSyncStateLocked(fileSyncCur)
			fileSyncMu.Unlock()
			res.Applied = ar.Applied
			logUI("已应用远端改动", fmt.Sprintf("%d 项（文件 %d、条目 %d）", ar.Applied, ar.Files, ar.Entries))
		}
		if ar.Failed > 0 {
			log.Printf("[files] 远端改动有 %d 项未能应用（下轮重试）: %v", ar.Failed, ar.Errors)
		}
		// 剩余待应用项数（真正的「待应用」）——供日志与界面显示
		fileSyncMu.Lock()
		res.Pending = len(fileSyncCur.PendingApply)
		fileSyncMu.Unlock()
	}
	return res, nil
}

// ==================== 应用待生效改动 ====================

// fileSyncApplyResult 应用结果（前端提示用）。
//
// 计数口径：Applied 是所有成功项（含条目级）；小字提示只用 Files（真正落到本机/从本机删除的文件数）——
// 否则「远端新增 1 个文件」会因为「新建条目」也算一项而显示成「2 项」，对用户没有意义（2026-10-06 现场）。
type fileSyncApplyResult struct {
	Applied int
	Failed  int
	Errors  []string
	Files   int // 文件级：下载 / 删除文件
	Entries int // 条目级：新建 / 改名 / 移除条目
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
			res.Entries++
		default:
			rest = append(rest, a)
		}
	}
	// 第二阶段：下载与删除
	kept := make([]fileSyncAction, 0, len(rest))
	for _, a := range rest {
		// 上传由同步阶段负责（服务器可能仍下发 upload 动作）：不计入「远端更新」
		if a.Kind == "upload" || a.Kind == "" {
			continue
		}
		var err error
		switch a.Kind {
		case "download":
			err = fileSyncApplyDownload(a, client, ctx, token)
		case "remove_file":
			err = fileSyncApplyRemoveFile(a)
		case "remove_entry":
			err = fileSyncApplyRemoveEntry(a)
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
		if a.Kind == "download" || a.Kind == "remove_file" {
			res.Files++
		} else {
			res.Entries++
		}
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
		entry := fileSyncEntry{
			ID:    a.EntryID,
			Name:  a.Name,
			Kind:  firstNonEmpty(a.EntryKind, "dir"),
			Files: map[string]fileSyncLocalFile{},
		}
		// 先校验设备：如果这个条目本来就是**本机**创建的（来源设备 id = 本机），说明本机是源设备
		// （可能只是本机清单丢了）。此时用服务端记下的原路径自动同步回原位置，
		// 而不是按接收端把内容下成接收目录里的副本（2026-10-06 现场）。
		if dev := accountDeviceID(); dev != "" && a.OriginDevice == dev && strings.TrimSpace(a.SourcePath) != "" {
			if root, ok := fileSyncUsableSourcePath(a.SourcePath, entry.Kind); ok {
				entry.SourcePath = root
				log.Printf("[files] 条目 %s 由本机创建，自动同步回原路径 %s", entry.Name, root)
			} else {
				// 源路径文件/文件夹已不存在：保留记录以便界面说明，内容按接收端落到接收目录
				entry.OriginPath = a.SourcePath
				entry.SourcePathMissing = true
				log.Printf("[files] 条目 %s 的原路径 %s 已不存在，按接收端保存到接收目录", entry.Name, a.SourcePath)
			}
		}
		fileSyncCur.Entries = append(fileSyncCur.Entries, entry)
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
		} else if res.Uploaded > 0 || res.Pending > 0 || res.Blocked > 0 {
			log.Printf("[files] 后台同步完成：上传 %d、待应用 %d、容量拦下 %d", res.Uploaded, res.Pending, res.Blocked)
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
		// 启动**立即**检查一次：远端改动（下载/改名，以及「本机创建的条目自动回源路径」）
		// 不该等到第一个 tick（60 秒）才生效——2026-10-06 现场：用户以为只有点「立即同步」才恢复。
		// 账号状态在这之前已载入（main.go: initAccountState → initFileSyncState → 本函数）。
		fileSyncBackgroundCheck(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fileSyncBackgroundCheck(ctx)
			}
		}
	}()
}

// fileSyncBackgroundCheck 后台一轮：本机扫描 +（有改动或到了远端对账间隔）完整同步一次。
func fileSyncBackgroundCheck(ctx context.Context) {
	if ctx == nil || ctx.Err() != nil || !accountLoggedIn() {
		return
	}
	fileSyncMu.Lock()
	fileSyncScanAllLocked()
	dirty := fileSyncHasLocalChangesLocked()
	due := fileSyncNow()-fileSyncLastCheck >= int64(fileSyncRemoteInterval/time.Second)
	fileSyncMu.Unlock()
	if !dirty && !due {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, fileSyncCheckTimeout)
	res, err := fileSyncCheck(cctx, newAccountClient(""))
	cancel()
	if err != nil {
		log.Printf("[files] 后台同步失败: %v", err)
	} else if res.Uploaded > 0 || res.Applied > 0 || res.Pending > 0 {
		log.Printf("[files] 后台同步完成：上传 %d、已应用 %d、待应用 %d", res.Uploaded, res.Applied, res.Pending)
	}
	emitFilesChanged()
}

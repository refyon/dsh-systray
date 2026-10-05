// account_files_bindings.go：文件同步的 Wails 绑定（前端「数据同步 → 文件同步」卡调用）。
//
// 绑定清单：
//
//	FilesStatus        状态快照（容量、条目树、待应用、容量拦下数）
//	FilesAdd           添加文件/文件夹（系统对话框 → 建条目 → 立即同步）
//	FilesRemoveEntry   从同步列表移除整个条目（本机文件保持不动）
//	FilesRemovePath    移除条目内的一个文件/子目录（云端打墓碑传播，本机文件保持不动）
//	FilesOpenEntry     以系统默认方式打开文件（或条目根）
//	FilesApplyPending  应用服务端下达的改动（下载/删除/改名）
//	FilesSyncNow       立即同步一次（扫描 + 上传 + 对账）
//
// 所有写操作都会 emit `files:changed`，前端据此刷新列表与容量条。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// FilesStatus 返回文件同步状态快照。
func (a *App) FilesStatus() FileSyncStatusInfo {
	return fileSyncStatusSnapshot()
}

// FilesAdd 添加文件（kind=file）或文件夹（kind=dir）到同步列表。
func (a *App) FilesAdd(kind string) (FileSyncStatusInfo, error) {
	kind = strings.TrimSpace(kind)
	if kind != "file" && kind != "dir" {
		return fileSyncStatusSnapshot(), fmt.Errorf("不支持的条目类型：%s", kind)
	}
	if !accountLoggedIn() {
		return fileSyncStatusSnapshot(), errors.New(T("请先登录账号"))
	}
	if shotMode {
		return fileSyncStatusSnapshot(), nil // 截图模式不弹系统对话框
	}
	path, err := pickFileSyncPath(kind)
	if err != nil {
		return fileSyncStatusSnapshot(), err
	}
	if strings.TrimSpace(path) == "" {
		return fileSyncStatusSnapshot(), nil // 用户取消
	}
	return fileSyncAddPath(kind, path)
}

// pickFileSyncPath 让用户选择要同步的文件或文件夹。
func pickFileSyncPath(kind string) (string, error) {
	if kind == "dir" {
		return wruntime.OpenDirectoryDialog(appCtx, wruntime.OpenDialogOptions{Title: T("选择要同步的文件夹")})
	}
	return wruntime.OpenFileDialog(appCtx, wruntime.OpenDialogOptions{Title: T("选择要同步的文件")})
}

// fileSyncAddPath 建条目并立即同步（服务端登记失败时给出明确错误）。
func fileSyncAddPath(kind, path string) (FileSyncStatusInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fileSyncStatusSnapshot(), fmt.Errorf(T("路径不可用：")+"%v", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return fileSyncStatusSnapshot(), fmt.Errorf(T("路径不可用：")+"%v", err)
	}
	if kind == "dir" && !st.IsDir() {
		return fileSyncStatusSnapshot(), errors.New(T("请选择文件夹"))
	}
	if kind == "file" && st.IsDir() {
		return fileSyncStatusSnapshot(), errors.New(T("请选择文件"))
	}
	if err := fileSyncCheckOverlap(abs, ""); err != nil {
		return fileSyncStatusSnapshot(), err
	}
	receive := fileSyncReceiveDir()
	if receive != "" && (samePath(abs, receive) || strings.HasPrefix(strings.ToLower(abs), strings.ToLower(receive)+string(os.PathSeparator))) {
		return fileSyncStatusSnapshot(), errors.New(T("接收目录不能作为同步来源"))
	}

	name := fileSyncUniqueEntryName(filepath.Base(abs))
	if !fileSyncValidEntryName(name) {
		return fileSyncStatusSnapshot(), errors.New(T("名称不合法（不能包含 \\ / : * ? \" < > | 等字符，也不能是保留名）"))
	}
	entry := fileSyncEntry{
		ID:         newOpID(),
		Name:       name,
		Kind:       kind,
		SourcePath: abs,
		Files:      map[string]fileSyncLocalFile{},
	}

	if token := accountToken(); token != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		client := newAccountClient("")
		body := fileSyncEntryBody{ID: entry.ID, Name: entry.Name, Kind: entry.Kind}
		if _, err := client.FilesCreateEntry(ctx, token, body); err != nil {
			if accountErrorCode(err) == accErrUnauthorized {
				accountInvalidateSession()
				return fileSyncStatusSnapshot(), errors.New(accountErrorText(err))
			}
			// 显示名与账号下其它设备已有条目冲突：换后缀重试一次
			entry.Name = fileSyncUniqueEntryName(name + " 2")
			body.Name = entry.Name
			if _, err2 := client.FilesCreateEntry(ctx, token, body); err2 != nil {
				return fileSyncStatusSnapshot(), errors.New(accountErrorText(err2))
			}
		}
	}

	fileSyncMu.Lock()
	fileSyncCur.Entries = append(fileSyncCur.Entries, entry)
	if idx := fileSyncFindEntryLocked(entry.ID); idx >= 0 {
		_ = fileSyncScanEntryLocked(&fileSyncCur.Entries[idx])
	}
	_ = saveFileSyncStateLocked(fileSyncCur)
	snap := fileSyncSnapshotLocked()
	fileSyncMu.Unlock()

	logUI("添加文件同步条目", fmt.Sprintf("%s（%s）", entry.Name, kind))
	fileSyncKick()
	emitFilesChanged()
	return snap, nil
}

// fileSyncUniqueEntryName 本机显示名去重（`名字`、`名字 (2)`、`名字 (3)`…）。
func fileSyncUniqueEntryName(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "sync"
	}
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	name := base
	for i := 2; i < 1000; i++ {
		if fileSyncEntryByNameLocked(name) < 0 {
			return name
		}
		name = fmt.Sprintf("%s (%d)", base, i)
	}
	return fmt.Sprintf("%s (%d)", base, time.Now().Unix())
}

// fileSyncCheckOverlap 检查路径是否与既有条目嵌套（同一批文件被两个条目重复同步会互相打架）。
func fileSyncCheckOverlap(path, excludeID string) error {
	target := strings.ToLower(filepath.Clean(path))
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	for _, e := range fileSyncCur.Entries {
		if e.ID == excludeID {
			continue
		}
		root := fileSyncEntryRoot(e)
		if root == "" {
			continue
		}
		r := strings.ToLower(filepath.Clean(root))
		if target == r || strings.HasPrefix(target, r+string(os.PathSeparator)) {
			return fmt.Errorf(T("该路径已在同步条目「%s」内"), e.Name)
		}
		if strings.HasPrefix(r, target+string(os.PathSeparator)) {
			return fmt.Errorf(T("该路径包含已同步的条目「%s」"), e.Name)
		}
	}
	return nil
}

// FilesRemovePath 从同步中删除条目内的一个文件或一个子目录：
// 云端打墓碑（传播到其它设备）；本机文件一律保留（用户决策：不再支持删除本机文件）。
//
// 与 FilesRemoveEntry 的分工：本函数只处理条目内容（条目本身保留）。
func (a *App) FilesRemovePath(entryID, relPath string) (FileSyncStatusInfo, error) {
	token := accountToken()
	if token == "" {
		return fileSyncStatusSnapshot(), errors.New(T("请先登录账号"))
	}
	relPath = strings.TrimSpace(strings.ReplaceAll(relPath, "\\", "/"))
	if relPath == "" {
		return fileSyncStatusSnapshot(), errors.New(T("条目不存在"))
	}
	// 先取消该文件/子目录下排队中的上传（用户要求：移除后不再继续上传）
	fileSyncCancelUploads(entryID, relPath)

	fileSyncMu.Lock()
	idx := fileSyncFindEntryLocked(entryID)
	if idx < 0 {
		snap := fileSyncSnapshotLocked()
		fileSyncMu.Unlock()
		return snap, errors.New(T("条目不存在"))
	}
	entry := fileSyncCur.Entries[idx]
	targets := fileSyncPathsUnderLocked(entry, relPath)
	fileSyncMu.Unlock()

	if len(targets) == 0 {
		return fileSyncStatusSnapshot(), errors.New(T("文件不存在（可能已被移动或删除）"))
	}

	client := newAccountClient("")
	for _, rel := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := client.FilesDeleteObject(ctx, token, entryID, rel)
		cancel()
		if err != nil && accountErrorCode(err) != accErrNotFound {
			return fileSyncStatusSnapshot(), errors.New(accountErrorText(err))
		}
	}

	fileSyncMu.Lock()
	if idx := fileSyncFindEntryLocked(entryID); idx >= 0 {
		e := &fileSyncCur.Entries[idx]
		if e.Ignored == nil {
			e.Ignored = map[string]int64{}
		}
		for _, rel := range targets {
			// 本机文件保留 → 记入「忽略」台账，否则下一轮扫描会把它重新拉回同步并重传；
			// 用户之后改动该文件（mtime 变新）会自动重新纳入同步（见 fileSyncNoteFileLocked）。
			e.Ignored[rel] = fileSyncNow()
			delete(e.Files, rel)
		}
		_ = saveFileSyncStateLocked(fileSyncCur)
	}
	snap := fileSyncSnapshotLocked()
	fileSyncMu.Unlock()

	// 云端已随内容删除释放空间：立刻刷新容量（用户要求）
	fileSyncRefreshQuota()
	snap = fileSyncStatusSnapshot()

	logUI("移除同步内容", fmt.Sprintf("%s/%s（%d 个文件）", entry.Name, relPath, len(targets)))
	emitFilesChanged()
	return snap, nil
}

// FilesRestorePath 把「已在本机移除」的文件重新纳入同步（relPath 为空 = 整个条目）：
// 从本机清单移除该文件，下一轮对账即把它当「本机缺文件」重新下载。
// 本机删除只在本机生效（不传播到账号），恢复就靠这个入口。
func (a *App) FilesRestorePath(entryID, relPath string) (FileSyncStatusInfo, error) {
	relPath = strings.TrimSpace(strings.ReplaceAll(relPath, "\\", "/"))
	fileSyncMu.Lock()
	idx := fileSyncFindEntryLocked(entryID)
	if idx < 0 {
		snap := fileSyncSnapshotLocked()
		fileSyncMu.Unlock()
		return snap, errors.New(T("条目不存在"))
	}
	e := &fileSyncCur.Entries[idx]
	restored := 0
	for rel, m := range e.Files {
		if !m.RemovedLocally {
			continue
		}
		if relPath != "" && rel != relPath && !strings.HasPrefix(rel, relPath+"/") {
			continue
		}
		// 从本机清单移除该文件：对账时本机就是「缺这个文件」→ 服务端下发下载动作
		// （若只是清标志位，下一轮扫描发现文件仍不在本机，又会标记回「已在本机移除」）
		delete(e.Files, rel)
		restored++
	}
	for rel := range e.Ignored {
		if relPath == "" || rel == relPath || strings.HasPrefix(rel, relPath+"/") {
			delete(e.Ignored, rel) // 「移除」后本机仍保留的文件：重新纳入同步
		}
	}
	_ = saveFileSyncStateLocked(fileSyncCur)
	snap := fileSyncSnapshotLocked()
	fileSyncMu.Unlock()

	logUI("重新同步文件", fmt.Sprintf("%s/%s（%d 项）", e.Name, relPath, restored))
	emitFilesChanged()
	fileSyncKick()
	return snap, nil
}

// fileSyncPathsUnderLocked 条目内与 relPath 匹配的文件相对路径：自身，或该子目录下的全部文件。
func fileSyncPathsUnderLocked(e fileSyncEntry, relPath string) []string {
	out := make([]string, 0, 4)
	prefix := relPath + "/"
	for rel := range e.Files {
		if rel == relPath || strings.HasPrefix(rel, prefix) {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// fileSyncRefreshQuota 主动刷新容量：删除条目/文件后云端已释放空间，界面要立刻反映（用户要求）。
func fileSyncRefreshQuota() {
	token := accountToken()
	if token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	q, err := newAccountClient("").FilesQuota(ctx, token)
	if err != nil {
		return
	}
	fileSyncMu.Lock()
	fileSyncCur.QuotaUsed, fileSyncCur.QuotaTier = q.Used, q.Tier
	if q.Limit > 0 {
		fileSyncCur.QuotaLimit = q.Limit
	}
	_ = saveFileSyncStateLocked(fileSyncCur)
	fileSyncMu.Unlock()
}

// FilesRemoveEntry 从同步列表移除条目：云端删除（其它设备据此收敛）+ 本机清单移除；
// deleteLocal=true 时同时删除本机文件（前端必须二次确认）。
func (a *App) FilesRemoveEntry(id string) (FileSyncStatusInfo, error) {
	// 先取消该条目所有排队中的上传，再走云端删除（用户要求：移除后不再继续上传）
	fileSyncCancelUploads(id, "")
	fileSyncMu.Lock()
	idx := fileSyncFindEntryLocked(id)
	if idx < 0 {
		snap := fileSyncSnapshotLocked()
		fileSyncMu.Unlock()
		return snap, errors.New(T("条目不存在"))
	}
	entry := fileSyncCur.Entries[idx]
	fileSyncMu.Unlock()

	if token := accountToken(); token != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := newAccountClient("").FilesDeleteEntry(ctx, token, id)
		cancel()
		if err != nil && accountErrorCode(err) != accErrNotFound {
			return fileSyncStatusSnapshot(), errors.New(accountErrorText(err))
		}
	}

	fileSyncMu.Lock()
	idx = fileSyncFindEntryLocked(id)
	if idx >= 0 {
		// 只从同步清单移除：本机文件保持不动（用户决策：不再支持移除时删除本机文件）
		fileSyncCur.Entries = append(fileSyncCur.Entries[:idx], fileSyncCur.Entries[idx+1:]...)
		fileSyncCur.PendingApply = fileSyncDropEntryActionsLocked(id)
		_ = saveFileSyncStateLocked(fileSyncCur)
	}
	snap := fileSyncSnapshotLocked()
	fileSyncMu.Unlock()

	// 云端已随条目删除释放空间：立刻刷新容量，界面剩余容量随即变大（用户要求）
	fileSyncRefreshQuota()
	snap = fileSyncStatusSnapshot()

	logUI("移除文件同步条目", entry.Name)
	emitFilesChanged()
	return snap, nil
}

// fileSyncDropEntryActionsLocked 丢弃某条目的待应用动作（调用方须持有 fileSyncMu）。
func fileSyncDropEntryActionsLocked(entryID string) []fileSyncAction {
	out := make([]fileSyncAction, 0, len(fileSyncCur.PendingApply))
	for _, a := range fileSyncCur.PendingApply {
		if a.EntryID != entryID {
			out = append(out, a)
		}
	}
	return out
}

// errOpenCanceled 「打开方式」对话框被用户取消（Windows ERROR_CANCELLED）：
// 这不是失败，绑定层直接吞掉，界面不报错（2026-10-05 现场）。
var errOpenCanceled = errors.New("open canceled by user")

// 打开函数做成变量，便于用例注入（真实 ShellExecuteW 无法在单测里跑）。
var (
	openFileFn     = openFile
	openFileWithFn = openFileWith
)

// FilesOpenEntry 以系统默认方式打开条目内某个文件（relPath 为空 = 打开条目根）。
// 用户在「打开方式」对话框里取消时静默返回成功——那不是错误。
func (a *App) FilesOpenEntry(id, relPath string) error {
	return a.filesOpen(id, relPath, false)
}

// FilesOpenEntryWith 让用户重新选择打开方式（默认方式启动失败时由界面提供）。
func (a *App) FilesOpenEntryWith(id, relPath string) error {
	return a.filesOpen(id, relPath, true)
}

func (a *App) filesOpen(id, relPath string, chooseApp bool) error {
	fileSyncMu.Lock()
	idx := fileSyncFindEntryLocked(id)
	target := ""
	if idx >= 0 {
		target = fileSyncEntryLocalPath(fileSyncCur.Entries[idx], relPath)
	}
	fileSyncMu.Unlock()
	if target == "" {
		return errors.New(T("本机路径不可用"))
	}
	if _, err := os.Stat(target); err != nil {
		return errors.New(T("文件不存在（可能已被移动或删除）"))
	}
	var err error
	if chooseApp {
		err = openFileWithFn(target)
	} else {
		err = openFileFn(target)
	}
	if errors.Is(err, errOpenCanceled) {
		return nil // 用户取消：静默
	}
	return err
}

// FilesApplyPending 应用服务端下达的改动（用户显式动作，不自动覆盖本机文件）。
func (a *App) FilesApplyPending() (FileSyncStatusInfo, error) {
	if !beginFileApply() {
		return fileSyncStatusSnapshot(), errors.New(T("正在应用同步改动，请稍候再试"))
	}
	defer endFileApply()
	if accountToken() == "" {
		return fileSyncStatusSnapshot(), errors.New(T("请先登录账号"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), fileSyncApplyTimeout)
	defer cancel()
	res, err := fileSyncApplyAll(ctx, newAccountClient(""))

	fileSyncMu.Lock()
	if err != nil {
		fileSyncCur.LastError = accountErrorText(err)
	}
	fileSyncScanAllLocked()
	_ = saveFileSyncStateLocked(fileSyncCur)
	fileSyncMu.Unlock()
	emitFilesChanged()

	switch {
	case err != nil:
		return fileSyncStatusSnapshot(), errors.New(accountErrorText(err))
	case res.Failed > 0:
		return fileSyncStatusSnapshot(), fmt.Errorf(T("有 %d 项应用失败：%s"), res.Failed, strings.Join(res.Errors, "；"))
	case res.Applied > 0:
		logUI("应用文件同步改动", fmt.Sprintf("应用 %d 项", res.Applied))
	}
	return fileSyncStatusSnapshot(), nil
}

// FilesSyncNow 立即同步一次（扫描 + 上传本机改动 + 传播删除 + 对账）。
func (a *App) FilesSyncNow() (FileSyncStatusInfo, error) {
	if !accountLoggedIn() {
		return fileSyncStatusSnapshot(), errors.New(T("请先登录账号"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), fileSyncCheckTimeout)
	defer cancel()
	res, err := fileSyncCheck(ctx, newAccountClient(""))
	emitFilesChanged()
	if err != nil {
		if accountErrorCode(err) == accErrUnauthorized {
			return fileSyncStatusSnapshot(), errors.New(T("登录已失效，请重新登录"))
		}
		return fileSyncStatusSnapshot(), errors.New(accountErrorText(err))
	}
	log.Printf("[files] 手动同步完成：上传 %d、待应用 %d、容量拦下 %d", res.Uploaded, res.Pending, res.Blocked)
	return fileSyncStatusSnapshot(), nil
}

// beginFileApply 置位「应用进行中」；已有同步/应用在跑时返回 false。
func beginFileApply() bool {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	if fileSyncApplying || fileSyncSyncing {
		return false
	}
	fileSyncApplying = true
	return true
}

func endFileApply() {
	fileSyncMu.Lock()
	fileSyncApplying = false
	fileSyncMu.Unlock()
}

// fileSyncValidEntryName 条目显示名校验：与服务端同一套规则，另拒 Windows 保留名
// （接收设备要按显示名建目录，保留名会让下载失败）。
func fileSyncValidEntryName(name string) bool {
	if name == "" || len([]rune(name)) > 128 || name != strings.TrimSpace(name) {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `\/:*?"<>|`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 {
			return false
		}
	}
	stem := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	switch stem {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return false
	}
	return true
}

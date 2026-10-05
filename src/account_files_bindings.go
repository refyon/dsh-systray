// account_files_bindings.go：文件同步的 Wails 绑定（前端「数据同步 → 文件同步」卡调用）。
//
// 绑定清单：
//
//	FilesStatus        状态快照（容量、条目树、待应用、容量拦下数）
//	FilesAdd           添加文件/文件夹（系统对话框 → 建条目 → 立即同步）
//	FilesRemoveEntry   从同步列表移除整个条目（deleteLocal 决定是否同时删除本机文件）
//	FilesRemovePath    删除条目内的一个文件/子目录（云端打墓碑传播，可选删除本机）
//	FilesRenameEntry   重命名条目（改云端显示名；接收设备同步改目录名）
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
	entry := fileSyncEntry{
		ID:         newOpID(),
		Name:       name,
		Kind:       kind,
		SourcePath: abs,
		Files:      map[string]fileSyncLocalFile{},
		Deletions:  map[string]int64{},
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
// 云端打墓碑（传播到其它设备），deleteLocal=true 时同时删除本机对应文件/目录。
//
// 与 FilesRemoveEntry 的分工：本函数只处理条目内容（条目本身保留）。
func (a *App) FilesRemovePath(entryID, relPath string, deleteLocal bool) (FileSyncStatusInfo, error) {
	token := accountToken()
	if token == "" {
		return fileSyncStatusSnapshot(), errors.New(T("请先登录账号"))
	}
	relPath = strings.TrimSpace(strings.ReplaceAll(relPath, "\\", "/"))
	if relPath == "" {
		return fileSyncStatusSnapshot(), errors.New(T("条目不存在"))
	}

	fileSyncMu.Lock()
	idx := fileSyncFindEntryLocked(entryID)
	if idx < 0 {
		snap := fileSyncSnapshotLocked()
		fileSyncMu.Unlock()
		return snap, errors.New(T("条目不存在"))
	}
	entry := fileSyncCur.Entries[idx]
	targets := fileSyncPathsUnderLocked(entry, relPath)
	root := fileSyncEntryRoot(entry)
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
		if deleteLocal {
			for _, rel := range targets {
				p := fileSyncEntryLocalPath(*e, rel)
				if p == "" {
					continue
				}
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					fileSyncMu.Unlock()
					return fileSyncStatusSnapshot(), fmt.Errorf(T("删除本机文件失败：")+"%v", err)
				}
			}
			if p := fileSyncEntryLocalPath(*e, relPath); p != "" {
				fileSyncPruneEmptyDirs(p, root)
			}
		}
		for _, rel := range targets {
			delete(e.Files, rel)
			delete(e.Deletions, rel)
		}
		_ = saveFileSyncStateLocked(fileSyncCur)
	}
	snap := fileSyncSnapshotLocked()
	fileSyncMu.Unlock()

	logUI("删除同步内容", fmt.Sprintf("%s/%s（%d 个文件，删除本机=%v）", entry.Name, relPath, len(targets), deleteLocal))
	emitFilesChanged()
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

// FilesRemoveEntry 从同步列表移除条目：云端删除（其它设备据此收敛）+ 本机清单移除；
// deleteLocal=true 时同时删除本机文件（前端必须二次确认）。
func (a *App) FilesRemoveEntry(id string, deleteLocal bool) (FileSyncStatusInfo, error) {
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
		e := fileSyncCur.Entries[idx]
		if deleteLocal {
			if r := fileSyncEntryRoot(e); r != "" {
				if !fileSyncSafeToDelete(r) {
					fileSyncMu.Unlock()
					return fileSyncStatusSnapshot(), errors.New(T("该路径受保护，未删除本机文件"))
				}
				if err := os.RemoveAll(r); err != nil {
					fileSyncMu.Unlock()
					return fileSyncStatusSnapshot(), fmt.Errorf(T("删除本机文件失败：")+"%v", err)
				}
			}
		}
		fileSyncCur.Entries = append(fileSyncCur.Entries[:idx], fileSyncCur.Entries[idx+1:]...)
		fileSyncCur.PendingApply = fileSyncDropEntryActionsLocked(id)
		_ = saveFileSyncStateLocked(fileSyncCur)
	}
	snap := fileSyncSnapshotLocked()
	fileSyncMu.Unlock()

	logUI("移除文件同步条目", fmt.Sprintf("%s（删除本机文件=%v）", entry.Name, deleteLocal))
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

// FilesRenameEntry 重命名条目：改云端显示名；接收设备把本机目录一并改名（源设备不动原路径）。
func (a *App) FilesRenameEntry(id, name string) (FileSyncStatusInfo, error) {
	name = strings.TrimSpace(name)
	if !fileSyncValidEntryName(name) {
		return fileSyncStatusSnapshot(), errors.New(T("名称不合法（不能包含 \\ / : * ? \" < > | 等字符，也不能是保留名）"))
	}
	token := accountToken()
	if token == "" {
		return fileSyncStatusSnapshot(), errors.New(T("请先登录账号"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := newAccountClient("").FilesRenameEntry(ctx, token, id, name); err != nil {
		return fileSyncStatusSnapshot(), errors.New(accountErrorText(err))
	}

	fileSyncMu.Lock()
	idx := fileSyncFindEntryLocked(id)
	if idx < 0 {
		snap := fileSyncSnapshotLocked()
		fileSyncMu.Unlock()
		return snap, errors.New(T("条目不存在"))
	}
	for i := range fileSyncCur.Entries {
		if i != idx && strings.EqualFold(fileSyncCur.Entries[i].Name, name) {
			snap := fileSyncSnapshotLocked()
			fileSyncMu.Unlock()
			return snap, errors.New(T("已有同名条目"))
		}
	}
	e := &fileSyncCur.Entries[idx]
	old := e.Name
	e.Name = name
	if strings.TrimSpace(e.SourcePath) == "" && old != name {
		oldRoot := fileSyncEntryRoot(fileSyncEntry{Name: old})
		newRoot := fileSyncEntryRoot(*e)
		if oldRoot != "" && newRoot != "" {
			if _, err := os.Stat(oldRoot); err == nil {
				if err := os.Rename(oldRoot, newRoot); err != nil {
					e.Error = T("重命名本机目录失败：") + err.Error()
				} else {
					e.Error = ""
				}
			}
		}
	}
	_ = saveFileSyncStateLocked(fileSyncCur)
	snap := fileSyncSnapshotLocked()
	fileSyncMu.Unlock()

	logUI("重命名文件同步条目", fmt.Sprintf("%s → %s", old, name))
	emitFilesChanged()
	return snap, nil
}

// FilesOpenEntry 以系统默认方式打开条目内某个文件（relPath 为空 = 打开条目根）。
func (a *App) FilesOpenEntry(id, relPath string) error {
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
	return openFile(target)
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
	log.Printf("[files] 手动同步完成：上传 %d、删除 %d、待应用 %d、容量拦下 %d", res.Uploaded, res.Deleted, res.Pending, res.Blocked)
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

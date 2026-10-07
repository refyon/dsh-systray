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
		body := fileSyncEntryBody{ID: entry.ID, Name: entry.Name, Kind: entry.Kind, SourcePath: entry.SourcePath}
		created, err := client.FilesCreateEntry(ctx, token, body)
		if err != nil {
			if accountErrorCode(err) == accErrUnauthorized {
				accountInvalidateSession()
				return fileSyncStatusSnapshot(), errors.New(accountErrorText(err))
			}
			// 显示名与账号下其它设备已有条目冲突：换后缀重试一次
			entry.Name = fileSyncUniqueEntryName(name + " 2")
			body.Name = entry.Name
			created, err = client.FilesCreateEntry(ctx, token, body)
			if err != nil {
				return fileSyncStatusSnapshot(), errors.New(accountErrorText(err))
			}
		}
		// 记下创建设备（服务端回执）：本机创建 → 「移动」时同步登记到账号
		entry.OriginDevice = created.OriginDevice
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

// FileSyncRelocateResult 「移动」的结果：Canceled=true 表示用户在选择对话框里取消了，
// 一个文件都没动——前端据此不再提示「已移动同步位置」（2026-10-07 用户反馈：
// 点「移动」后在系统对话框点取消，界面仍报「已移动」）。
type FileSyncRelocateResult struct {
	Status   FileSyncStatusInfo `json:"status"`
	Canceled bool               `json:"canceled"`
}

// FilesSetLocalPath 把条目移动到本机其它位置（「移动」）：
//
//   - 文件夹条目：选一个**文件夹**，条目内容按原文件名铺进去；
//   - 文件条目：选**文件夹** → 文件放进该文件夹并保留文件名；选**文件** → 覆盖它（系统对话框已问过替换）；
//   - **旧位置的文件搬走**（系统移动语义：搬完清理源文件与空目录）；
//   - 目标已有同名且内容不同的文件时先问一次（覆盖后不留副本，见 fileSyncRelocateEntry）；
//   - 用户取消对话框 → Canceled=true、未改动任何文件；
//   - 若本机正是该条目的创建者，同时把新路径登记到账号（日后清单丢失能回到新位置），
//     否则只改本机（创建者的路径不该被我们覆盖）。
func (a *App) FilesSetLocalPath(id string) (FileSyncRelocateResult, error) {
	if !accountLoggedIn() {
		return FileSyncRelocateResult{Status: fileSyncStatusSnapshot()}, errors.New(T("请先登录账号"))
	}
	fileSyncMu.Lock()
	idx := fileSyncFindEntryLocked(id)
	if idx < 0 {
		snap := fileSyncSnapshotLocked()
		fileSyncMu.Unlock()
		return FileSyncRelocateResult{Status: snap}, errors.New(T("条目不存在"))
	}
	entry := fileSyncCur.Entries[idx]
	kind, entryName := entry.Kind, entry.Name
	fileSyncMu.Unlock()

	if shotMode {
		// 截图模式不弹系统对话框：没有移动即成，按「取消」处理
		return FileSyncRelocateResult{Status: fileSyncStatusSnapshot(), Canceled: true}, nil
	}
	abs, err := pickRelocateTargetFn(kind, entryName)
	if err != nil {
		return FileSyncRelocateResult{Status: fileSyncStatusSnapshot()}, err
	}
	if abs == "" { // 用户在系统对话框里取消
		return FileSyncRelocateResult{Status: fileSyncStatusSnapshot(), Canceled: true}, nil
	}
	if err := fileSyncCheckAdoptTarget(abs, kind); err != nil {
		return FileSyncRelocateResult{Status: fileSyncStatusSnapshot()}, err
	}
	st, err := fileSyncRelocateEntry(id, abs)
	return FileSyncRelocateResult{Status: st}, err
}

// pickRelocateTargetFn 选移动目标（可替换：测试里模拟用户取消/选定目录，见 account_files_test.go）。
var pickRelocateTargetFn = pickRelocateTarget

// pickRelocateTarget 选移动目标，全部借用**系统对话框**：
//   - 文件夹条目：系统「选择文件夹」；
//   - 文件条目：系统「保存文件」对话框（预填条目文件名）——放进某个文件夹并沿用该名字，
//     或改名/换位置都由用户决定；目标已存在时由**系统自己**弹「是否替换」，App 不再多问一次
//     （所以文件条目覆盖时不再确认；文件夹条目没有这一问，覆盖前由 fileSyncRelocateEntry 补问）。
func pickRelocateTarget(kind, entryName string) (string, error) {
	if kind == "dir" {
		p, err := wruntime.OpenDirectoryDialog(appCtx, wruntime.OpenDialogOptions{Title: T("移动到哪个文件夹")})
		if err != nil || strings.TrimSpace(p) == "" {
			return "", err
		}
		return filepath.Abs(p)
	}
	defaultName := sanitizeRelName(entryName)
	if defaultName == "" {
		defaultName = "sync"
	}
	p, err := wruntime.SaveFileDialog(appCtx, wruntime.SaveDialogOptions{
		Title:           T("移动到哪里"),
		DefaultFilename: defaultName,
	})
	if err != nil || strings.TrimSpace(p) == "" {
		return "", err
	}
	return filepath.Abs(p)
}

// fileSyncFileEntryTargetIn 文件条目选了文件夹时的落点：`<文件夹>/<条目名>`（保留文件名）。
func fileSyncFileEntryTargetIn(dir, entryName string) (string, error) {
	name := sanitizeRelName(entryName)
	if name == "" {
		return "", errors.New(T("条目名不可用作文件名，请选择具体文件"))
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", errors.New(T("文件不存在（可能已被移动或删除）"))
	}
	return filepath.Join(dir, name), nil
}

// sanitizeRelName 去掉文件名里不允许的路径分隔与保留字符（作为目录内的落点文件名用）。
func sanitizeRelName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_").Replace(name)
	name = strings.Trim(name, ". ")
	return name
}

// fileSyncRelocateEntry 「移动」的核心：把本机内容**移动**到新位置（不是复制、也不是重下），
// 再按新位置重新扫描；本机是创建者时把新路径登记到账号。
//
// 语义（用户要求，按操作系统"移动"的行为）：
//   - 旧位置存在的文件搬到新位置（保留相对结构/文件名），**搬完源头即清理**（含留空的目录）；
//   - 目标已有同名文件：内容相同视为同一份（丢掉源那份）；内容不同则用搬过来的覆盖，
//     覆盖前**问一次**（文件夹条目；文件条目的「另存为」系统对话框已经问过替换），
//     确认后直接覆盖、**不再留「(冲突-本机)」副本**（用户决策：决定覆盖就不必保留原件）；
//   - 旧位置本来就没有的文件（未同步/已删除）不动，由服务器按正常同步补下来；
//   - 因此"改过去再改回来"也只是再移动一次，不会卡住。
func fileSyncRelocateEntry(id, newRoot string) (FileSyncStatusInfo, error) {
	fileSyncMu.Lock()
	idx := fileSyncFindEntryLocked(id)
	if idx < 0 {
		snap := fileSyncSnapshotLocked()
		fileSyncMu.Unlock()
		return snap, errors.New(T("条目不存在"))
	}
	e := &fileSyncCur.Entries[idx]
	oldRoot := fileSyncEntryRoot(*e)
	kind, name := e.Kind, e.Name
	dev := accountDeviceID()
	isOrigin := dev != "" && e.OriginDevice == dev
	fileSyncMu.Unlock()

	moved, conflicts, skipped := 0, 0, 0
	if !samePath(oldRoot, newRoot) {
		// 会覆盖目标同名文件（内容不同）时先确认：文件夹条目选的是目录，系统对话框不会替我们问，
		// 而覆盖后目标原内容不再保留副本——用户不确认就整体不动。
		if n := fileSyncMoveConflictCount(oldRoot, newRoot); n > 0 && !askMoveOverwriteFn(n) {
			return fileSyncStatusSnapshot(), errors.New(T("已取消移动（目标位置有同名文件，未改动任何文件）"))
		}
		moved, conflicts, skipped = fileSyncMoveLocalContent(kind, oldRoot, newRoot)
	}

	fileSyncMu.Lock()
	idx = fileSyncFindEntryLocked(id)
	if idx < 0 {
		snap := fileSyncSnapshotLocked()
		fileSyncMu.Unlock()
		return snap, errors.New(T("条目不存在"))
	}
	e = &fileSyncCur.Entries[idx]
	e.SourcePath = newRoot
	e.SourcePathMissing, e.OriginPath, e.Error = false, "", ""
	if e.Files == nil {
		e.Files = map[string]fileSyncLocalFile{}
	}
	// 搬过来的文件内容没变：清掉「已在本机移除」标记并重扫（同一份内容继续显示已同步）
	for rel, m := range e.Files {
		if m.RemovedLocally {
			delete(e.Files, rel)
		}
	}
	_ = fileSyncScanEntryLocked(e)
	_ = saveFileSyncStateLocked(fileSyncCur)
	snap := fileSyncSnapshotLocked()
	fileSyncMu.Unlock()

	if isOrigin {
		if token := accountToken(); token != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, perr := newAccountClient("").FilesSetSourcePath(ctx, token, id, newRoot)
			cancel()
			if perr != nil {
				log.Printf("[files] 登记新同步位置到账号失败（本机已生效）: %v", perr)
			}
		}
	}

	logUI("移动同步位置", fmt.Sprintf("%s（%s）：%s → %s，移动 %d 个文件、覆盖 %d、跳过 %d",
		name, kind, oldRoot, newRoot, moved, conflicts, skipped))
	emitFilesChanged()
	fileSyncKick()
	return snap, nil
}

// fileSyncMoveLocalContent 把 oldRoot 下的内容移动到 newRoot（同卷 rename，跨卷复制后删除），
// 返回 (移动的文件数, 内容不同被覆盖的冲突数, 内容相同直接去重的跳过数)。
//
// 冲突规则按操作系统"移动"：目标同名且内容相同 = 同一份（丢源）；内容不同 = 搬过来的覆盖目标。
// 覆盖前已由调用方（fileSyncRelocateEntry）问过用户，因此这里**不再留「(冲突-本机)」副本**
// （用户决策 2026-10-06：用户决定覆盖就不必重命名保留原件）。
// 文件条目 oldRoot 就是那个文件本身。
func fileSyncMoveLocalContent(kind, oldRoot, newRoot string) (int, int, int) {
	if oldRoot == "" || newRoot == "" || samePath(oldRoot, newRoot) {
		return 0, 0, 0
	}
	moved, conflicts, skipped := 0, 0, 0
	moveOne := func(src, dst string) {
		if samePath(src, dst) {
			return
		}
		st, err := os.Stat(src)
		if err != nil || st.IsDir() {
			return
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			log.Printf("[files] 移动 %s 失败（建目录）: %v", src, err)
			return
		}
		if dstInfo, derr := os.Stat(dst); derr == nil && !dstInfo.IsDir() {
			same, herr := fileSyncSameContent(src, dst)
			if herr == nil && same {
				// 内容相同：同一份文件，丢掉源那份即为“移动”完成
				if rmErr := os.Remove(src); rmErr == nil {
					skipped++
				}
				return
			}
			// 内容不同：覆盖目标（用户已确认；不留副本）
			conflicts++
		}
		if err := os.Rename(src, dst); err == nil {
			moved++
			return
		}
		// 跨卷：复制后删除
		if _, cerr := fileSyncCopyFile(src, dst); cerr != nil {
			log.Printf("[files] 移动 %s → %s 失败: %v", src, dst, cerr)
			return
		}
		if rmErr := os.Remove(src); rmErr != nil {
			log.Printf("[files] 移动后删除源文件失败 %s: %v", src, rmErr)
			return
		}
		moved++
	}

	if kind == "file" {
		moveOne(oldRoot, newRoot)
		return moved, conflicts, skipped
	}
	// 目录条目：非递归遍历源目录（含未纳入同步的本地文件，与系统移动一致），只搬文件
	_ = filepath.WalkDir(oldRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(oldRoot, path)
		if rerr != nil {
			return nil
		}
		moveOne(path, filepath.Join(newRoot, rel))
		return nil
	})
	// 搬空的旧目录清理掉（含嵌套空目录；留一个空壳目录没有意义）
	fileSyncPruneTreeEmpty(oldRoot)
	return moved, conflicts, skipped
}

// fileSyncPruneTreeEmpty 自底向上删除 root 下的空目录，最后若 root 也空了就一并删除。
func fileSyncPruneTreeEmpty(root string) {
	for i := 0; i < 64; i++ {
		removed := false
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() || samePath(path, root) {
				return nil
			}
			if entries, rerr := os.ReadDir(path); rerr == nil && len(entries) == 0 {
				if os.Remove(path) == nil {
					removed = true
				}
			}
			return nil
		})
		if !removed {
			break
		}
	}
	if entries, err := os.ReadDir(root); err == nil && len(entries) == 0 {
		_ = os.Remove(root)
	}
}

// fileSyncMoveConflictCount 统计移动会覆盖掉的目标同名文件数（同名且内容不同）。
// 覆盖后目标原内容不再保留副本，因此覆盖前要用它问用户一次。
// 内容相同的不计（那是同一份，直接去重）；目标不存在的也不计（新增落点）。
func fileSyncMoveConflictCount(oldRoot, newRoot string) int {
	if oldRoot == "" || newRoot == "" || samePath(oldRoot, newRoot) {
		return 0
	}
	n := 0
	check := func(src, dst string) {
		if samePath(src, dst) {
			return
		}
		st, err := os.Stat(src)
		if err != nil || st.IsDir() {
			return
		}
		dstInfo, derr := os.Stat(dst)
		if derr != nil || dstInfo.IsDir() {
			return
		}
		if same, herr := fileSyncSameContent(src, dst); herr != nil || !same {
			n++
		}
	}
	// 文件条目：oldRoot 就是那个文件本身
	if st, err := os.Stat(oldRoot); err == nil && !st.IsDir() {
		check(oldRoot, newRoot)
		return n
	}
	// 目录条目：与移动同一套遍历（含未纳入同步的本地文件）
	_ = filepath.WalkDir(oldRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(oldRoot, path)
		if rerr != nil {
			return nil
		}
		check(path, filepath.Join(newRoot, rel))
		return nil
	})
	return n
}

// askMoveOverwriteFn 移动前询问「目标已有同名不同内容的文件，是否覆盖」（平台弹窗；测试可替换）。
var askMoveOverwriteFn = askMoveOverwriteLocal

// fileSyncSameContent 两个文件内容是否一致（大小不同直接判定不同）。
func fileSyncSameContent(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if ai.Size() != bi.Size() {
		return false, nil
	}
	ah, err := fileSyncHashFile(a)
	if err != nil {
		return false, err
	}
	bh, err := fileSyncHashFile(b)
	if err != nil {
		return false, err
	}
	return ah == bh, nil
}

// fileSyncCheckAdoptTarget 校验「移动」选中的目标：
// 文件夹条目必须是目录；文件条目可以是文件（覆盖）或目录下的同名文件落点；不能是接收目录。
func fileSyncCheckAdoptTarget(abs, kind string) error {
	st, err := os.Stat(abs)
	if err != nil {
		// 文件条目选文件夹时，落点是「文件夹/文件名」——该文件还不存在是正常的
		if kind == "file" && os.IsNotExist(err) {
			if pst, perr := os.Stat(filepath.Dir(abs)); perr == nil && pst.IsDir() {
				return nil
			}
		}
		return errors.New(T("文件不存在（可能已被移动或删除）"))
	}
	if kind == "dir" && !st.IsDir() {
		return errors.New(T("该条目同步的是文件夹，请选择文件夹"))
	}
	receive := fileSyncReceiveDir()
	if receive != "" && samePath(abs, receive) {
		return errors.New(T("接收目录本身不能作为同步位置"))
	}
	if err := fileSyncCheckOverlap(abs, ""); err != nil {
		return err
	}
	return nil
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
	// 先记移除墓碑：服务端那次删除万一失败，下次同步才能区分「用户真的移除了」与
	// 「本机清单丢了」——两者都表现为「来源设备=本机、本机没有该条目」。
	fileSyncCur.noteEntryRemoved(id, fileSyncNow())
	_ = saveFileSyncStateLocked(fileSyncCur)
	fileSyncMu.Unlock()

	if token := accountToken(); token != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := newAccountClient("").FilesDeleteEntry(ctx, token, id)
		cancel()
		if err != nil && accountErrorCode(err) != accErrNotFound {
			return fileSyncStatusSnapshot(), errors.New(accountErrorText(err))
		}
		fileSyncMu.Lock()
		fileSyncCur.clearEntryRemoved(id) // 服务端已确认删除，墓碑使命完成
		_ = saveFileSyncStateLocked(fileSyncCur)
		fileSyncMu.Unlock()
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
		// 手动同步失败也要留日志（此前只在界面提示，排障时无迹可循——2026-10-06 现场）
		log.Printf("[files] 手动同步失败: %v", err)
		if accountErrorCode(err) == accErrUnauthorized {
			return fileSyncStatusSnapshot(), errors.New(T("登录已失效，请重新登录"))
		}
		return fileSyncStatusSnapshot(), errors.New(accountErrorText(err))
	}
	log.Printf("[files] 手动同步完成：上传 %d、已应用 %d、待应用 %d、容量拦下 %d", res.Uploaded, res.Applied, res.Pending, res.Blocked)
	return fileSyncStatusSnapshot(), nil
}

// beginFileApply 置位「应用进行中」；已有同步/应用在跑时返回 false（用户手动「重试」路径）。
func beginFileApply() bool {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	if fileSyncApplying || fileSyncSyncing {
		return false
	}
	fileSyncApplying = true
	return true
}

// beginFileApplyAuto 自动应用专用守卫：同步轮次内部调用，**不受「同步进行中」影响**
// （用 beginFileApply 会永远返回 false，远端改动就落不了地——2026-10-06 现场）。
func beginFileApplyAuto() bool {
	fileSyncMu.Lock()
	defer fileSyncMu.Unlock()
	if fileSyncApplying {
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

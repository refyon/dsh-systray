// account_files_store.go：文件同步的本机清单（filesync.json，0600）与服务端契约类型。
//
// 模型（详见 plugins/dsh-systray/docs/评估-dsh-systray-文件同步.md）：
//   - 一个**条目**= 用户添加的一个文件或文件夹；跨设备身份 = 条目 id + 条目内相对路径；
//   - **绝对路径永不上传**：源设备把文件留在原位（SourcePath 只存本机），接收设备落到
//     「接收目录/<显示名>/<相对路径>」；
//   - 本机删除文件**不传播**（源设备与接收端一致）：该文件只在本机停止同步、显示「已在本机移除」，
//     元数据保留，可随时点「重新同步」从账号拉回；要把内容从账号删除只能走界面的「移除」；
//   - 服务端下达的改动先进 PendingApply，等用户点「应用」才落地（不自动覆盖本机文件）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ==================== 服务端契约类型（字段名逐字对齐 docs/API.md 端点 13-20） ====================

// fileSyncEntryBody 条目（新建/对账入参）。
type fileSyncEntryBody struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// SourcePath 本机原路径：随创建条目一起登记，供来源设备丢失清单后自动同步回原路径
	// （非来源设备只拿到相对路径，见 docs/API.md 端点 15）。
	SourcePath string `json:"sourcePath,omitempty"`
}

// fileSyncLocalMeta 对账入参里的本机文件状态。
type fileSyncLocalMeta struct {
	EntryID string `json:"entryId"`
	RelPath string `json:"relPath"`
	Size    int64  `json:"size"`
	Sha256  string `json:"sha256"`
	Mtime   int64  `json:"mtime"`
}

// fileSyncRequest 对账请求。
type fileSyncRequest struct {
	Entries []fileSyncEntryBody `json:"entries"`
	Files   []fileSyncLocalMeta `json:"files"`
}

// fileSyncAction 对账动作（服务端只计算，不产生副作用）。
type fileSyncAction struct {
	Kind      string `json:"kind"` // create_entry | rename_entry | remove_entry | download | upload | remove_file
	EntryID   string `json:"entryId"`
	Name      string `json:"name,omitempty"`
	EntryKind string `json:"entryKind,omitempty"`
	// OriginDevice create_entry 的创建设备 id（迁移 0004 起）：等于本机时说明是自家残留，直接清理
	OriginDevice string `json:"originDevice,omitempty"`
	// SourcePath create_entry 的来源设备原路径（迁移 0005 起）：来源设备据此自动同步回原路径
	SourcePath string `json:"sourcePath,omitempty"`
	RelPath    string `json:"relPath,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Sha256     string `json:"sha256,omitempty"`
	Mtime      int64  `json:"mtime,omitempty"`
	Rev        int64  `json:"rev,omitempty"`
}

// fileQuota 容量快照。
type fileQuota struct {
	Used  int64  `json:"used"`
	Limit int64  `json:"limit"`
	Tier  string `json:"tier"`
}

// fileSyncRemoteEntry 服务端条目。
type fileSyncRemoteEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	// OriginDevice 创建设备 id（迁移 0004 起；旧条目为空串 = 来源未知）
	OriginDevice string `json:"originDevice,omitempty"`
	// SourcePath 来源设备上的原路径（迁移 0005 起；空串 = 未知，按接收端处理）
	SourcePath string `json:"sourcePath,omitempty"`
}

// fileSyncResponse 对账响应。
type fileSyncResponse struct {
	Entries []fileSyncRemoteEntry `json:"entries"`
	Actions []fileSyncAction      `json:"actions"`
	Quota   fileQuota             `json:"quota"`
}

// fileUploadResponse 上传响应（含最新容量）。
type fileUploadResponse struct {
	Rev   int64  `json:"rev"`
	Used  int64  `json:"used"`
	Limit int64  `json:"limit"`
	Tier  string `json:"tier"`
}

// ==================== 本机状态（filesync.json） ====================

// fileSyncLocalFile 条目内某个文件的本机状态。
type fileSyncLocalFile struct {
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime"`
	Sha256 string `json:"sha256"`
	// SyncedSha 服务端已确认的内容摘要（空 = 从未上传成功）；与 Sha256 相同即「已同步」。
	SyncedSha string `json:"syncedSha,omitempty"`
	// SyncedSize 上次上传时的大小：替换文件的容量预检据此扣除旧占用（避免假性超限）。
	SyncedSize int64 `json:"syncedSize,omitempty"`
	Rev        int64 `json:"rev,omitempty"`
	// Error 最近一次同步失败原因（展示用；下次成功清空）。
	Error string `json:"error,omitempty"`
	// Blocked 因容量不足被拦下（前端据此弹「容量不足」提示；释放空间后自动重试）。
	Blocked bool `json:"blocked,omitempty"`
	// RemovedLocally 接收设备上用户删掉了本机副本：**只在本机停止同步，不传播删除**，
	// 服务端与其它设备保持不变；文件保留在清单里显示「已在本机移除」，可点「重新同步」恢复。
	RemovedLocally bool `json:"removedLocally,omitempty"`
}

// fileSyncEntry 一个同步条目。
type fileSyncEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // file | dir
	// SourcePath 本机绝对路径：只有「添加该条目的设备」有值（源设备文件留在原位）；
	// 接收设备为空，文件落在「接收目录/<显示名>」。
	SourcePath string `json:"sourcePath,omitempty"`
	// Files 条目内文件的本机状态（相对路径 → 状态）。
	Files map[string]fileSyncLocalFile `json:"files,omitempty"`
	// Ignored 「仅本机停止同步」的文件（相对路径 → 时间）：本机删除（保留元数据可重新同步）或用
	// 「移除」删掉云端副本后本机仍保留的文件都记在这里；文件修改时间晚于该时间视为用户改动 → 重新同步。
	Ignored map[string]int64 `json:"ignored,omitempty"`
	// OriginPath / SourcePathMissing：本机曾是来源设备，但服务端记下的原路径在本机已不存在
	// （文件被删/移走）——此时按接收端保存副本，界面说明原委。
	OriginPath        string `json:"originPath,omitempty"`
	SourcePathMissing bool   `json:"sourcePathMissing,omitempty"`
	// Error 条目级错误（如本机路径不存在、目录重命名失败）。
	Error string `json:"error,omitempty"`
}

// fileSyncState filesync.json 的内容。
type fileSyncState struct {
	// UserID 清单归属账号：同一账号重新登录时继续用（不再全量重传），换账号才作废。
	UserID       string           `json:"userId,omitempty"`
	Entries      []fileSyncEntry  `json:"entries,omitempty"`
	PendingApply []fileSyncAction `json:"pendingApply,omitempty"`
	QuotaUsed    int64            `json:"quotaUsed,omitempty"`
	QuotaLimit   int64            `json:"quotaLimit,omitempty"`
	QuotaTier    string           `json:"quotaTier,omitempty"`
	LastSyncedAt int64            `json:"lastSyncedAt,omitempty"`
	LastError    string           `json:"lastError,omitempty"`
	// 远端改动直接落地后的小字提示：什么时候、应用了多少项（用户要求保留可见痕迹）
	RemoteAppliedAt    int64 `json:"remoteAppliedAt,omitempty"`
	RemoteAppliedCount int   `json:"remoteAppliedCount,omitempty"`
}

// ==================== 状态视图（前端列表渲染的数据源） ====================

// FileSyncFileView 列表里的一个文件行。
type FileSyncFileView struct {
	RelPath string `json:"relPath"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Mtime   int64  `json:"mtime"`
	// Status synced | pending-upload（待同步）| uploading（同步中）| pending-download | error
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// Blocked 容量不足（前端据此弹提示）
	Blocked bool `json:"blocked"`
	// SpeedBps 该文件上一次上传的实测速度（B/s；前端显示「1.2 MB/s」）
	SpeedBps int64 `json:"speedBps,omitempty"`
}

// FileSyncEntryView 列表里的一个条目行（文件夹可展开为 Files）。
type FileSyncEntryView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	IsSource bool   `json:"isSource"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	// Mtime 条目内文件的最新修改时间（列表按「修改时间」排序用；空条目为 0）
	Mtime  int64              `json:"mtime"`
	Status string             `json:"status"`
	Error  string             `json:"error,omitempty"`
	Files  []FileSyncFileView `json:"files"`
	// SourcePathMissing 本机曾是来源设备、但原路径已不存在：内容按接收端保存，界面说明原委
	SourcePathMissing bool   `json:"sourcePathMissing,omitempty"`
	OriginPath        string `json:"originPath,omitempty"`
}

// FileSyncStatusInfo 「数据同步 → 文件同步」卡的状态快照（JSON 字段名即前端读取名）。
type FileSyncStatusInfo struct {
	LoggedIn     bool                `json:"loggedIn"`
	Syncing      bool                `json:"syncing"`
	Applying     bool                `json:"applying"`
	LastError    string              `json:"lastError"`
	LastSyncedAt int64               `json:"lastSyncedAt"`
	QuotaUsed    int64               `json:"quotaUsed"`
	QuotaLimit   int64               `json:"quotaLimit"`
	QuotaTier    string              `json:"quotaTier"`
	ReceiveDir   string              `json:"receiveDir"`
	Entries      []FileSyncEntryView `json:"entries"`
	PendingCount int                 `json:"pendingCount"`
	PendingFiles []string            `json:"pendingFiles"`
	BlockedCount int                 `json:"blockedCount"`
	// 上传进度（仅运行时）：前端显示「正在上传 12/605 · 345 KB/s」，并让容量随上传实时增长。
	Uploading      bool  `json:"uploading"`
	UploadDone     int   `json:"uploadDone"`
	UploadTotal    int   `json:"uploadTotal"`
	UploadBytes    int64 `json:"uploadBytes"`
	UploadSpeedBps int64 `json:"uploadSpeedBps"`
	// 远端改动自动落地的小字提示：时间 + 项数（0 = 从未）
	RemoteAppliedAt    int64 `json:"remoteAppliedAt"`
	RemoteAppliedCount int   `json:"remoteAppliedCount"`
}

// ==================== 路径与持久化 ====================

// fileSyncStatePath filesync.json 路径（与 config.json / account.json 同目录）。
func fileSyncStatePath() string {
	if dir := fileSyncStateDirValue(); dir != "" {
		return filepath.Join(dir, "filesync.json")
	}
	p := configFilePath()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "filesync.json")
}

// 可注入覆盖值（测试用；与 account_store.go 的 accountStateDirOverride 同一套做法）。
var (
	fileSyncCfgMu         sync.Mutex
	fileSyncStateDirOvr   string
	fileSyncReceiveDirOvr string
)

// setFileSyncStateDir 设置清单目录（测试注入；空 = 与 config.json 同目录）。
func setFileSyncStateDir(v string) {
	fileSyncCfgMu.Lock()
	fileSyncStateDirOvr = v
	fileSyncCfgMu.Unlock()
}

func fileSyncStateDirValue() string {
	fileSyncCfgMu.Lock()
	defer fileSyncCfgMu.Unlock()
	return fileSyncStateDirOvr
}

// setFileSyncReceiveDir 设置接收目录（测试注入；空 = 默认位置）。
func setFileSyncReceiveDir(v string) {
	fileSyncCfgMu.Lock()
	fileSyncReceiveDirOvr = v
	fileSyncCfgMu.Unlock()
}

func fileSyncReceiveDirValue() string {
	fileSyncCfgMu.Lock()
	defer fileSyncCfgMu.Unlock()
	return fileSyncReceiveDirOvr
}

// loadFileSyncState 读取清单；缺失或损坏返回零值（不阻塞启动）。
func loadFileSyncState() fileSyncState {
	var st fileSyncState
	p := fileSyncStatePath()
	if p == "" {
		return st
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return st
	}
	if json.Unmarshal(data, &st) != nil {
		return fileSyncState{}
	}
	return st
}

// saveFileSyncStateLocked 原子写入（临时文件 + rename），权限 0600（调用方须持有 fileSyncMu）。
func saveFileSyncStateLocked(st fileSyncState) error {
	p := fileSyncStatePath()
	if p == "" {
		return fmt.Errorf("no config dir for file sync state")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// fileSyncReceiveDir 本机接收目录（接收设备下载落点）：`<用户文档目录>/DeepSeekSync`，
// 文档目录不存在时退回用户主目录。接收目录自身在扫描时被排除（避免与源目录自嵌套）。
func fileSyncReceiveDir() string {
	if v := fileSyncReceiveDirValue(); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	if st, err := os.Stat(filepath.Join(home, "Documents")); err == nil && st.IsDir() {
		return filepath.Join(home, "Documents", "DeepSeekSync")
	}
	return filepath.Join(home, "DeepSeekSync")
}

// fileSyncEntryRoot 条目在本机的根路径。
//
//   - 单文件条目（kind=file）：本机路径就是那次选中的文件本身（SourcePath）；
//   - 目录条目（源设备）：选中的目录原位置；
//   - 接收设备：接收目录 / 云端显示名。
func fileSyncEntryRoot(e fileSyncEntry) string {
	if strings.TrimSpace(e.SourcePath) != "" {
		return e.SourcePath
	}
	dir := fileSyncReceiveDir()
	if dir == "" || strings.TrimSpace(e.Name) == "" {
		return ""
	}
	return filepath.Join(dir, e.Name)
}

// fileSyncEntryLocalPath 条目内某个文件的本机路径（relPath 为空 = 条目根）。
//
// 单文件条目必须返回 SourcePath 本身：它的 relPath 只是云端的显示名（相对路径），
// 再拼一次会得到 `H:\DOC\info.txt\info.txt` 这种错误路径（2026-10-05 现场）。
func fileSyncEntryLocalPath(e fileSyncEntry, relPath string) string {
	root := fileSyncEntryRoot(e)
	if root == "" {
		return ""
	}
	if e.Kind == "file" {
		return root
	}
	if strings.TrimSpace(relPath) == "" {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(relPath))
}

// ==================== 状态派生与快照 ====================

// fileSyncEntryIsSource 本机是否为该条目的来源设备（本机添加 = 有本机原路径）。
func fileSyncEntryIsSource(e fileSyncEntry) bool {
	return strings.TrimSpace(e.SourcePath) != ""
}

// fileSyncStatusRank 状态优先级：错误 > 同步中 > 待同步/待下载 > 已在本机移除 > 已同步。
// 目录与条目的状态都取子树/文件里优先级最高者（只要有后代在传就显示「同步中」）。
func fileSyncStatusRank(status string) int {
	switch status {
	case "error":
		return 4
	case "uploading":
		return 3
	case "pending", "pending-upload", "pending-download":
		return 2
	case "removed-local":
		return 1
	default:
		return 0
	}
}

// fileSyncNormalizeStatus 文件级状态 → 条目级状态（待上传/待下载统一显示为「待同步」）。
func fileSyncNormalizeStatus(status string) string {
	switch status {
	case "pending-upload", "pending-download":
		return "pending"
	default:
		return status
	}
}

// fileSyncFileStatus 单个文件的状态：有错误 → error；未上传或内容已变 → pending-upload；否则 synced。
func fileSyncFileStatus(m fileSyncLocalFile) string {
	switch {
	case m.Error != "" || m.Blocked:
		return "error"
	case m.RemovedLocally:
		return "removed-local"
	case m.SyncedSha == "" || m.Sha256 != m.SyncedSha:
		return "pending-upload"
	default:
		return "synced"
	}
}

// fileSyncSnapshotLocked 组装前端快照（调用方须持有 fileSyncMu）。
//
// 排序：条目按显示名；条目内文件**文件夹（含子路径）在前**，再按相对路径升序——
// 与需求「文件夹排在文件前面」一致（层级仅一级，展开/折叠由前端按 relPath 前缀分组）。
func fileSyncSnapshotLocked() FileSyncStatusInfo {
	out := FileSyncStatusInfo{
		Syncing:            fileSyncSyncing,
		Applying:           fileSyncApplying,
		LastError:          fileSyncCur.LastError,
		LastSyncedAt:       fileSyncCur.LastSyncedAt,
		QuotaUsed:          fileSyncCur.QuotaUsed,
		QuotaLimit:         fileSyncCur.QuotaLimit,
		QuotaTier:          fileSyncCur.QuotaTier,
		ReceiveDir:         fileSyncReceiveDir(),
		PendingCount:       len(fileSyncCur.PendingApply),
		PendingFiles:       fileSyncPendingLabelsLocked(),
		Uploading:          fileSyncProg.Active,
		UploadDone:         fileSyncProg.Done,
		UploadTotal:        fileSyncProg.Total,
		UploadBytes:        fileSyncProg.Bytes,
		UploadSpeedBps:     fileSyncProg.SpeedBps,
		RemoteAppliedAt:    fileSyncCur.RemoteAppliedAt,
		RemoteAppliedCount: fileSyncCur.RemoteAppliedCount,
	}
	out.LoggedIn = accountLoggedIn()
	// 未登录：文件卡清空内容（容量、列表、进度一律不外露），由前端显示「登录后同步可查看」提示。
	// 本机清单仍留在 filesync.json 里，重新登录后继续用（不会重传所有文件）。
	if !out.LoggedIn {
		return FileSyncStatusInfo{
			LoggedIn:   false,
			ReceiveDir: fileSyncReceiveDir(),
			Entries:    []FileSyncEntryView{},
		}
	}

	for _, e := range fileSyncCur.Entries {
		view := FileSyncEntryView{
			ID:                e.ID,
			Name:              e.Name,
			Kind:              e.Kind,
			IsSource:          strings.TrimSpace(e.SourcePath) != "",
			Path:              fileSyncEntryRoot(e),
			Status:            "synced",
			Error:             e.Error,
			SourcePathMissing: e.SourcePathMissing,
			OriginPath:        e.OriginPath,
		}
		if e.Error != "" {
			view.Status = "error"
		}
		rels := make([]string, 0, len(e.Files))
		for rel := range e.Files {
			rels = append(rels, rel)
		}
		sort.Strings(rels)
		for _, rel := range rels {
			m := e.Files[rel]
			view.Size += m.Size
			if m.Mtime > view.Mtime {
				view.Mtime = m.Mtime // 条目按「修改时间」排序取最新
			}
			fv := FileSyncFileView{
				RelPath: rel,
				Name:    fileSyncDisplayName(rel),
				Size:    m.Size,
				Mtime:   m.Mtime,
				Status:  fileSyncFileStatus(m),
				Error:   m.Error,
				Blocked: m.Blocked,
			}
			key := fileSyncTaskKey(e.ID, rel)
			if fileSyncProg.InFlight[key] {
				fv.Status = "uploading" // 本轮在传：行内显示「同步中」
			}
			if sp := fileSyncProg.FileSpeeds[key]; sp > 0 {
				fv.SpeedBps = sp
			}
			if m.Blocked {
				out.BlockedCount++
			}
			// 条目状态 = 文件状态的最高优先级（错误 > 同步中 > 待同步 > 已在本机移除 > 已同步）：
			// 只要有文件在传，条目就该显示「同步中」，而不是只显示「待同步」。
			if fileSyncStatusRank(fv.Status) > fileSyncStatusRank(view.Status) {
				view.Status = fileSyncNormalizeStatus(fv.Status)
			}
			view.Files = append(view.Files, fv)
		}
		if fileSyncEntryHasPendingDownloadLocked(e.ID) && fileSyncStatusRank(view.Status) < 2 {
			view.Status = "pending"
		}
		out.Entries = append(out.Entries, view)
	}
	sort.SliceStable(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	return out
}

// fileSyncDisplayName 相对路径的最后一段（列表主文本）。
func fileSyncDisplayName(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

// fileSyncEntryHasPendingDownloadLocked 该条目是否有待应用的下载动作。
func fileSyncEntryHasPendingDownloadLocked(entryID string) bool {
	for _, a := range fileSyncCur.PendingApply {
		if a.EntryID != entryID {
			continue
		}
		if a.Kind == "download" || a.Kind == "remove_file" || a.Kind == "remove_entry" || a.Kind == "rename_entry" {
			return true
		}
	}
	return false
}

// fileSyncPendingLabelsLocked 待应用动作的展示标签（「条目/文件」形态）。
func fileSyncPendingLabelsLocked() []string {
	out := make([]string, 0, len(fileSyncCur.PendingApply))
	for _, a := range fileSyncCur.PendingApply {
		switch a.Kind {
		case "create_entry":
			out = append(out, fmt.Sprintf("%s（新条目）", a.Name))
		case "rename_entry":
			out = append(out, fmt.Sprintf("%s（重命名）", a.Name))
		case "remove_entry":
			out = append(out, fmt.Sprintf("%s（已在其它设备移除）", a.Name))
		case "download":
			out = append(out, fmt.Sprintf("%s/%s", fileSyncEntryNameLocked(a.EntryID), a.RelPath))
		case "remove_file":
			out = append(out, fmt.Sprintf("%s/%s（删除）", fileSyncEntryNameLocked(a.EntryID), a.RelPath))
		}
	}
	sort.Strings(out)
	return out
}

// fileSyncEntryNameLocked 条目显示名（找不到返回 id）。
func fileSyncEntryNameLocked(entryID string) string {
	for _, e := range fileSyncCur.Entries {
		if e.ID == entryID {
			return e.Name
		}
	}
	return entryID
}

// fileSyncFindEntryLocked 按 id 找条目（返回下标；-1 = 不存在）。
func fileSyncFindEntryLocked(entryID string) int {
	for i := range fileSyncCur.Entries {
		if fileSyncCur.Entries[i].ID == entryID {
			return i
		}
	}
	return -1
}

// fileSyncEntryByNameLocked 按显示名找条目（返回下标；-1 = 不存在）。
func fileSyncEntryByNameLocked(name string) int {
	for i := range fileSyncCur.Entries {
		if strings.EqualFold(fileSyncCur.Entries[i].Name, name) {
			return i
		}
	}
	return -1
}

// fileSyncNow 当前时间（Unix 秒；测试可替换）。
var fileSyncNow = func() int64 { return time.Now().Unix() }

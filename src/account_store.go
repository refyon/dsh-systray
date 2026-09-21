// account_store.go：登录态持久化（account.json，0600）与本地 30 天有效期策略。
//
// 与 config.json 分开存放：令牌属于凭据，权限收紧到 0600；config.json 会被多处整份覆盖写，
// 且可能随导出包分享（导出包不含本文件）。存储接口化（本文件的 load/save/clear 三个函数），
// 后续要换成 macOS Keychain / Windows DPAPI 只需替换实现。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// accountSessionTTLDays 本地登录有效期（用户决策：30 天）。
// 服务端令牌有效期是 90 天，本地更短，用于「过期后明确提示重新登录」。
const accountSessionTTLDays = 30

// 可注入覆盖值：启动时写入，之后会被后台同步 goroutine 读取，故用读写锁保护
// （-race 下曾暴露「测试复位 vs 后台上报读」的竞态）。
var (
	accountCfgMu            sync.RWMutex
	accountAPIBaseOverride  string // config.json 的 accountApiBase（空 = 默认正式域名）
	accountStateDirOverride string // account.json 所在目录（空 = 与 config.json 同目录）
)

// setAccountAPIBase 设置服务地址覆盖值（空 = 用默认正式域名）。
func setAccountAPIBase(v string) {
	accountCfgMu.Lock()
	accountAPIBaseOverride = v
	accountCfgMu.Unlock()
}

func accountAPIBaseValue() string {
	accountCfgMu.RLock()
	defer accountCfgMu.RUnlock()
	return accountAPIBaseOverride
}

// setAccountStateDir 设置登录态文件目录（测试注入）。
func setAccountStateDir(v string) {
	accountCfgMu.Lock()
	accountStateDirOverride = v
	accountCfgMu.Unlock()
}

func accountStateDirValue() string {
	accountCfgMu.RLock()
	defer accountCfgMu.RUnlock()
	return accountStateDirOverride
}

// appliedRecord 一条已应用到本机的服务器记录（来源序号 + 目标值）。
// 用途见 accountState.AppliedVals：游标推进后仍能按本机现状重判是否需要重新应用。
type appliedRecord struct {
	Seq   int64           `json:"seq"`
	Value json.RawMessage `json:"value"`
}

// accountState 登录态与同步进度（account.json）。
type accountState struct {
	Token          string `json:"token"`
	TokenExpiresAt int64  `json:"tokenExpiresAt"`
	// IssuedAt 本地记录的令牌签发时间：30 天策略的依据（不依赖服务端时钟）。
	IssuedAt int64  `json:"issuedAt"`
	DeviceID string `json:"deviceId"`
	UserID   string `json:"userId"`
	Email    string `json:"email"`
	// Cursor 已拉取到的服务器游标（下次 ops/since 的起点）。
	Cursor int64 `json:"cursor"`
	// LastSyncedAt 最近一次成功同步完成的时间（Unix 秒）。
	LastSyncedAt int64 `json:"lastSyncedAt"`
	// BaselineDone 是否已完成首次同步（上报本地基线，或拉取到服务器记录）。
	BaselineDone bool `json:"baselineDone"`
	// PendingOps 待上报的操作记录队列（上报成功即出队；登出时丢弃）。
	PendingOps []accountPendingOp `json:"pendingOps,omitempty"`
	// PendingRemote 已从服务器拉到、但**尚未应用**到本机的操作记录（点「重启生效」后合并并清空）。
	// 跨托盘重启保留：提示必须一直存在，且不允许自动生效。启动时会重校验（见
	// revalidatePendingApplyOnStartup）：应用中途退出进程时不能把这份集合原样当作事实。
	PendingRemote []accountPendingOp `json:"pendingRemote,omitempty"`
	// PendingApply 是否存在待生效改动（= len(PendingRemote) > 0，随同一份状态持久化）。
	PendingApply bool `json:"pendingApply,omitempty"`
	// AppliedSeqs 各 key 已成功应用到本机的服务器记录序号（key → seq）。
	// 拉取阶段的判定依据之一是「已应用 **且 本机当前状态仍满足**」——序号只表示曾经应用过，
	// 不能单独当作「已生效」的凭据（手工删除 .dsh / harness 目录后序号还在，插件却已不在）。
	// 本机状态读取口径与服务端目标值形态不完全一致时（如版本范围 vs 已装版本），
	// 满足性判定负责避免同一条记录被反复重装（2026-09-21 现场问题：每次重启生效都重装
	// dsh-cost-meter）。目标值更新（seq 更大）时自然重新进入待生效。
	AppliedSeqs map[string]int64 `json:"appliedSeqs,omitempty"`
	// AppliedVals 各 key 已应用的服务器记录目标值（key → {seq, value}）。
	//
	// 与 AppliedSeqs 并存、含义互补：序号只说明「曾经应用过」，而手工删除 .dsh /
	// harness 目录后本机被重置、服务器游标却已推进到这些记录之后——后续增量拉取再也拿不到
	// 它们，只靠拉取阶段的「已应用序号 + 本机现状」重判会整条漏掉（表现为重启托盘后
	// 「显示已同步、插件列表却是空的」，2026-09-22 现场问题①）。保存目标值后，
	// 启动与每次同步都能离线重判漂移并重新入队（见 accountReenqueueDriftedApplied）。
	AppliedVals map[string]appliedRecord `json:"appliedVals,omitempty"`
	// SyncInvariantOK 「游标恒小于未生效记录」不变量已建立（2026-09-22 存量迁移标记）。
	// 旧版本会在「上报确认」时无条件把游标推进到服务器流头部，且已应用记录只存序号不存目标值，
	// 存量 account.json 因而可能带着「游标已越过未生效记录」的坏状态；见 accountMigrateCursorInvariant。
	SyncInvariantOK bool `json:"syncInvariantOK,omitempty"`
	// LastReportedHarnessVersion 最近一次与服务端**对账**过的本机 Harness 版本
	// （不一定已上报：版本下降属意外回退，只记对账、不覆盖账号上的「最后选用版本」）。
	// 供两种情况补报：①首次基线时版本尚未可知（harness 仍在安装/识别失败）；
	// ②版本在 dsh-systray 之外被**升高**（源码 checkout 切换、外部 npm 安装）。
	LastReportedHarnessVersion string `json:"lastReportedHarnessVersion,omitempty"`
}

// accountPendingOp 待上报的操作记录（本地队列项，见 account_ops.go）。
type accountPendingOp struct {
	// OpID 客户端幂等键（UUID）：重试沿用同一条，服务端按 (user_id, op_id) 去重。
	OpID string `json:"opId"`
	// Key 合并键：setting:<name> / plugin:<profile>:<name>。
	Key string `json:"key"`
	// Value 该 key 的目标值（JSON）。
	Value json.RawMessage `json:"value"`
	// CreatedAt 本地产生时间（仅用于展示与排障；合并以服务器时间为准）。
	CreatedAt int64 `json:"createdAt"`
	// Seq 服务器序号（历史遗留字段：本地队列为 0，待生效集合记录来源记录的序号，
	// 用于把同步游标停在未应用记录之前）。
	Seq int64 `json:"seq,omitempty"`
}

// accountStatePath account.json 路径（与 config.json 同目录）。
func accountStatePath() string {
	dir := accountStateDirValue()
	if dir == "" {
		if p := configFilePath(); p != "" {
			dir = filepath.Dir(p)
		}
	}
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "account.json")
}

// loadAccountState 读取登录态；文件缺失或损坏返回零值（视为未登录），不阻塞启动。
func loadAccountState() accountState {
	var st accountState
	p := accountStatePath()
	if p == "" {
		return st
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return st
	}
	if json.Unmarshal(data, &st) != nil {
		return accountState{}
	}
	return st
}

// saveAccountState 原子写入（临时文件 + rename），权限 0600。
func saveAccountState(st accountState) error {
	p := accountStatePath()
	if p == "" {
		return fmt.Errorf("no config dir for account state")
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

// clearAccountState 删除登录态（登出或本地失效）。
func clearAccountState() error {
	p := accountStatePath()
	if p == "" {
		return nil
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// loggedIn 是否处于本地有效登录期。
func (st accountState) loggedIn(now time.Time) bool {
	return st.expireReason(now) == ""
}

// expireReason 登录失效原因："" 有效 / "session_expired"（本地 30 天到期）/
// "token_expired"（服务端有效期已过）/ "no_token"（从未登录）。
func (st accountState) expireReason(now time.Time) string {
	if st.Token == "" {
		return "no_token"
	}
	if st.IssuedAt > 0 && now.Unix() >= st.IssuedAt+int64(accountSessionTTLDays)*86400 {
		return "session_expired"
	}
	if st.TokenExpiresAt > 0 && now.Unix() >= st.TokenExpiresAt {
		return "token_expired"
	}
	return ""
}

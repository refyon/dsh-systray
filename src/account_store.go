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
	"time"
)

// accountSessionTTLDays 本地登录有效期（用户决策：30 天）。
// 服务端令牌有效期是 90 天，本地更短，用于「过期后明确提示重新登录」。
const accountSessionTTLDays = 30

// accountAPIBaseOverride config.json 的 accountApiBase（空 = 用默认正式域名）。
var accountAPIBaseOverride string

// accountStateDirOverride 测试注入：account.json 所在目录（空 = 与 config.json 同目录）。
var accountStateDirOverride string

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
}

// accountStatePath account.json 路径（与 config.json 同目录）。
func accountStatePath() string {
	dir := accountStateDirOverride
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

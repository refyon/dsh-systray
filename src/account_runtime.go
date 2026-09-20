// account_runtime.go：账号登录态的进程内状态与 Wails 绑定（设置页「数据同步」页的数据源）。
//
// 分工：account.go = HTTP 客户端；account_store.go = 0600 持久化；本文件 = 运行时状态 + 前端接口。
// 同步逻辑（op 队列、拉取、合并）在后续文件中实现，通过 accountMu 读写同一份状态。
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// AccountStatusInfo 「数据同步」页的状态快照（JSON 字段名即前端读取名）。
type AccountStatusInfo struct {
	LoggedIn     bool   `json:"loggedIn"`
	Email        string `json:"email"`
	DeviceID     string `json:"deviceId"`
	ExpireReason string `json:"expireReason"` // "" 有效 | session_expired | token_expired | no_token
	LastSyncedAt int64  `json:"lastSyncedAt"`
	BaselineDone bool   `json:"baselineDone"`
	Syncing      bool   `json:"syncing"`
	SyncError    string `json:"syncError"`
	PendingOps   int    `json:"pendingOps"`
	// PendingApply 是否已有「拉到本机但尚未生效」的改动（前端据此常驻提示「重启生效」）。
	PendingApply bool   `json:"pendingApply"`
	APIBase      string `json:"apiBase"`
}

// AccountCodeResult 验证码请求结果（前端据此做重发倒计时）。
type AccountCodeResult struct {
	ExpiresInSec   int `json:"expiresInSec"`
	ResendAfterSec int `json:"resendAfterSec"`
}

var (
	accountMu      sync.Mutex
	accountCur     accountState
	accountSyncing bool
	accountSyncErr string
)

// initAccountState 启动时载入登录态（只读，不阻塞启动）。
func initAccountState() {
	accountMu.Lock()
	defer accountMu.Unlock()
	accountCur = loadAccountState()
	if accountCur.Token != "" {
		log.Printf("[account] 已载入登录态 email=%s cursor=%d baseline=%v",
			maskEmail(accountCur.Email), accountCur.Cursor, accountCur.BaselineDone)
	}
}

// accountLoggedIn 是否已登录且本地有效期未过（埋点上报的前置判断）。
func accountLoggedIn() bool {
	accountMu.Lock()
	defer accountMu.Unlock()
	return accountCur.loggedIn(time.Now())
}

// accountSetSyncError 记录最近一次同步失败原因（状态页展示；下次成功时清除）。
func accountSetSyncError(msg string) {
	accountMu.Lock()
	accountSyncErr = msg
	accountMu.Unlock()
}

// accountClearSyncError 清除同步失败状态。
func accountClearSyncError() {
	accountMu.Lock()
	accountSyncErr = ""
	accountMu.Unlock()
}

// clearAccountRuntime 复位进程内登录态（登出与测试用）。
func clearAccountRuntime() {
	accountMu.Lock()
	accountCur = accountState{}
	accountSyncing = false
	accountSyncErr = ""
	accountMu.Unlock()
}

// accountSnapshot 组装状态快照。
func accountSnapshot() AccountStatusInfo {
	accountMu.Lock()
	defer accountMu.Unlock()
	return accountStatusLocked()
}

func accountStatusLocked() AccountStatusInfo {
	now := time.Now()
	return AccountStatusInfo{
		LoggedIn:     accountCur.loggedIn(now),
		Email:        accountCur.Email,
		DeviceID:     accountCur.DeviceID,
		ExpireReason: accountCur.expireReason(now),
		LastSyncedAt: accountCur.LastSyncedAt,
		BaselineDone: accountCur.BaselineDone,
		Syncing:      accountSyncing,
		SyncError:    accountSyncErr,
		PendingOps:   len(accountCur.PendingOps),
		PendingApply: accountCur.PendingApply,
		APIBase:      accountAPIBase(),
	}
}

// accountDeviceSelf 上报给服务端的设备信息（不采集硬件 UUID）。
func accountDeviceSelf() accountDeviceInfo {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		name = "unknown"
	}
	return accountDeviceInfo{Name: name, Platform: runtime.GOOS, AppVersion: appVersion}
}

// maskEmail 日志脱敏：`user@example.com` → `u***@example.com`。
func maskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return "***"
	}
	return email[:1] + "***" + email[at:]
}

// accountErrorText 把错误映射成用户可读文案（中文为 i18n 键，英文见 i18n.go）。
func accountErrorText(err error) string {
	switch accountErrorCode(err) {
	case accErrInvalidEmail:
		return T("邮箱格式不正确")
	case accErrRateLimited:
		return T("操作过于频繁，请稍后再试")
	case accErrOTPInvalid:
		return T("验证码不正确")
	case accErrOTPExpired:
		return T("验证码已过期，请重新获取")
	case accErrOTPTooMany:
		return T("验证码尝试次数过多，请重新获取")
	case accErrMailFailed:
		return T("验证码邮件发送失败，请稍后重试")
	case accErrUnauthorized:
		return T("登录已失效，请重新登录")
	case accErrNetwork:
		return T("网络连接失败，请检查网络后重试")
	default:
		return T("操作失败，请稍后重试")
	}
}

// ---- Wails 绑定（前端 window.go.main.App.*） ----

// AccountStatus 返回「数据同步」页状态快照。
func (a *App) AccountStatus() AccountStatusInfo {
	return accountSnapshot()
}

// AccountRequestCode 发送邮箱验证码。
func (a *App) AccountRequestCode(email string) (AccountCodeResult, error) {
	email = strings.TrimSpace(email)
	client := newAccountClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := client.RequestCode(ctx, email, currentLang())
	if err != nil {
		logUI("请求登录验证码失败", accountErrorText(err))
		return AccountCodeResult{}, errors.New(accountErrorText(err))
	}
	logUI("已发送登录验证码", maskEmail(email))
	return AccountCodeResult{ExpiresInSec: res.ExpiresInSec, ResendAfterSec: res.ResendAfterSec}, nil
}

// AccountVerify 校验验证码；成功即登记登录态并落盘（换账号时重置游标与基线标记）。
func (a *App) AccountVerify(email, code string) (AccountStatusInfo, error) {
	email = strings.TrimSpace(email)
	client := newAccountClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := client.Verify(ctx, email, code, accountDeviceSelf())
	if err != nil {
		logUI("登录失败", accountErrorText(err))
		return accountSnapshot(), errors.New(accountErrorText(err))
	}

	accountMu.Lock()
	if accountCur.Email != "" && !strings.EqualFold(accountCur.Email, sess.User.Email) {
		accountCur = accountState{} // 换账号：游标与基线作废
	}
	accountCur.Token = sess.Token
	accountCur.TokenExpiresAt = sess.TokenExpiresAt
	accountCur.IssuedAt = time.Now().Unix()
	accountCur.DeviceID = sess.DeviceID
	accountCur.UserID = sess.User.ID
	accountCur.Email = sess.User.Email
	saved := accountCur
	accountSyncing = false
	accountSyncErr = ""
	accountMu.Unlock()

	if err := saveAccountState(saved); err != nil {
		log.Printf("[account] 保存登录态失败: %v", err)
	}
	logUI("登录成功", maskEmail(sess.User.Email))
	return accountSnapshot(), nil
}

// AccountLogout 撤销服务端令牌并清除本地登录态（网络失败不阻断本地登出）。
func (a *App) AccountLogout() (AccountStatusInfo, error) {
	accountMu.Lock()
	st := accountCur
	accountMu.Unlock()
	clearAccountRuntime()

	var revokeErr error
	if st.Token != "" {
		client := newAccountClient("")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := client.Logout(ctx, st.Token); err != nil && accountErrorCode(err) != accErrUnauthorized {
			revokeErr = err // 令牌已失效不算错误；其余（网络/服务端）提示但不回滚本地登出
		}
	}
	if err := clearAccountState(); err != nil && revokeErr == nil {
		revokeErr = err
	}
	logUI("退出登录", maskEmail(st.Email))

	status := accountSnapshot()
	if revokeErr != nil {
		return status, errors.New(accountErrorText(revokeErr))
	}
	return status, nil
}

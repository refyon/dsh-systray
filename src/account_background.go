// account_background.go：启动自动登录与后台定期同步（需求④⑤）。
//
// 模型：
//   - 启动时若本地登录态有效（0600 文件 + 服务端令牌未过期），先用服务端校验令牌是否仍然有效——
//     401/已撤销 → 清本地登录态并提示重新登录；网络错误 → 保留登录态（离线不误登出）；
//   - 校验通过后立即做一次同步检查，此后每 20 分钟一次（需求⑤）；
//   - 每次校验顺带检查令牌剩余有效期，临近到期（<30 天）时静默续期，用户无感知；
//   - 后台同步只「检查」：拉到的改动进入待生效集合，等用户点「重启生效」才落地；
//   - 每次状态变化广播 account:changed，前端据此刷新左侧小字与页面。
package main

import (
	"context"
	"log"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	// accountSyncInterval 后台定期同步周期（用户决策：20 分钟）。
	accountSyncInterval = 20 * time.Minute
	// accountStartupGrace 启动后首次同步检查的延迟：只等托盘与界面就位。
	//
	// 不能等太久：这段时间里界面还没有本次会话的同步结果，而 account.json 里的
	// lastSyncedAt 是**上一会话**留下的——旧值会让状态行显示绿色「已同步」，而服务器上
	// 可能早已有本机没拉到的记录（2026-09-22 现场：启动显示已同步，手点「立即同步」
	// 才弹出「重启生效」）。首次检查本身只读本机文件 + 一次网络请求，与服务启动无依赖，
	// 因此这里只留够界面就绪的时间；等待期间界面显示「正在检查同步…」。
	accountStartupGrace = 5 * time.Second
	// accountRenewAhead 令牌剩余有效期低于该值时静默续期（用户无感知）。
	//
	// 服务端令牌 90 天、续期从续期时刻重算：提前 30 天续期 → 正常使用下约每 60 天一次后台请求；
	// 只有连续 90 天完全没有续期（应用长期未运行）或服务端已撤销，才会要求重新登录。
	accountRenewAhead = 30 * 24 * time.Hour
)

// startAccountBackground 启动后台循环（由 onStartup 调用；截图模式与绑定生成进程不启动）。
func startAccountBackground(ctx context.Context) {
	if ctx == nil || shotMode {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(accountStartupGrace):
		}
		// 启动自动登录：仅当本地登录态有效时才校验与同步
		if accountLoggedIn() {
			accountVerifySession(ctx)
			if accountLoggedIn() { // 校验可能因令牌失效而清空登录态
				accountBackgroundTick(ctx) // 内部无论成败都会标记「本会话已检查」
			}
		}
		ticker := time.NewTicker(accountSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !accountLoggedIn() {
					continue
				}
				accountVerifySession(ctx)
				if accountLoggedIn() {
					accountBackgroundTick(ctx)
				}
			}
		}
	}()
}

// accountVerifySession 用服务端确认本地令牌是否仍然有效（自动登录的服务端校验）。
//
// 与本地过期判定互补：本地令牌未到期但服务端已撤销（换机登出、设备撤销）时，
// 这里会清掉本地登录态并让前端提示重新登录；校验通过后再按剩余有效期决定是否静默续期。
func accountVerifySession(ctx context.Context) {
	accountMu.Lock()
	token := accountCur.Token
	accountMu.Unlock()
	if token == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	me, err := newAccountClient("").Me(cctx, token)
	if err != nil {
		if accountErrorCode(err) == accErrUnauthorized {
			accountInvalidateSession()
			log.Printf("[account] 服务端令牌已失效，已停用本地登录态")
			return
		}
		log.Printf("[account] 令牌校验失败（保留登录态，离线可用）: %v", err)
		return
	}
	accountMu.Lock()
	accountCur.Email = me.User.Email
	accountCur.UserID = me.User.ID
	if err := saveAccountState(accountCur); err != nil {
		log.Printf("[account] 保存账号信息失败: %v", err)
	}
	accountMu.Unlock()

	// 令牌临近过期时顺手续期：静默进行，不弹任何提示（用户要求无感知）。
	accountMaybeRenewToken(ctx)
}

// accountMaybeRenewToken 令牌剩余有效期不足 accountRenewAhead 时静默续期。
//
// 续期不换令牌值、可安全重试（见 dsh-connect 的 POST /v1/auth/refresh）。
// 失败处理：401（服务端已撤销/拒绝）→ 停用本地登录态并提示重新登录；
// 网络或服务端错误 → 保留登录态，等下一轮后台检查重试（离线不误登出）。
func accountMaybeRenewToken(ctx context.Context) {
	accountMu.Lock()
	token, exp := accountCur.Token, accountCur.TokenExpiresAt
	accountMu.Unlock()
	if token == "" || exp == 0 {
		return
	}
	remaining := time.Until(time.Unix(exp, 0))
	if remaining > accountRenewAhead {
		return
	}

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sess, err := newAccountClient("").Refresh(cctx, token)
	if err != nil {
		if accountErrorCode(err) == accErrUnauthorized {
			accountInvalidateSession()
			log.Printf("[account] 令牌续期被服务端拒绝，已停用本地登录态")
			return
		}
		log.Printf("[account] 令牌续期失败（保留登录态，下轮重试，剩余 %s）: %v", remaining.Truncate(time.Hour), err)
		return
	}

	accountMu.Lock()
	if accountCur.Token != token {
		// 续期期间发生了重新登录/登出：不要用旧会话覆盖新状态
		accountMu.Unlock()
		return
	}
	accountCur.Token = sess.Token
	accountCur.TokenExpiresAt = sess.TokenExpiresAt
	accountCur.IssuedAt = time.Now().Unix()
	saved := accountCur
	accountMu.Unlock()

	if err := saveAccountState(saved); err != nil {
		log.Printf("[account] 保存续期后的登录态失败: %v", err)
	}
	log.Printf("[account] 令牌已静默续期至 %s", time.Unix(saved.TokenExpiresAt, 0).Format("2006-01-02 15:04"))
}

// accountBackgroundTickFn 触发一次完整同步检查（测试可替换：避免真实联网与协程竞态）。
var accountBackgroundTickFn = accountBackgroundTick

// accountBackgroundTick 一次后台同步检查（不应用任何改动）。
func accountBackgroundTick(ctx context.Context) {
	// 应用流程进行中：两边都会改写待生效集合，本轮直接跳过（应用结束后的下次 tick 自然补上）
	if accountApplyBusy() {
		log.Printf("[account] 后台同步跳过：正在应用同步改动")
		return
	}
	if !beginAccountSync() {
		log.Printf("[account] 后台同步跳过：已有同步在进行")
		return
	}
	defer endAccountSync()

	// 120 秒：单请求 20 秒 × 最多 3 次重试（见 accountRequestTimeout），60 秒会在重试途中被掐断。
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	res, err := accountSyncNow(cctx, newAccountClient(""))

	// 本次会话已经检查过（无论成败）：界面据此把「尚未检查」与「已同步」分开显示
	// （失败会走 SyncError 分支，不会因为标记了已检查就显示成绿色）。
	accountMarkStartupChecked()

	if err != nil {
		accountSetSyncError(accountErrorText(err))
		log.Printf("[account] 后台同步失败: %v", err)
	} else {
		accountClearSyncError()
		log.Printf("[account] 后台同步完成：上报 %d 项，拉到 %d 条，待生效 %d 项，重入队 %d 项，插件补报 %d 项", res.Uploaded, res.Pulled, len(res.Pending), res.Reenqueued, res.PluginsReported)
	}
	emitAccountChanged()
}

// emitAccountChanged 广播账号/同步状态变化（前端刷新左侧小字与「数据同步」页）。
func emitAccountChanged() {
	if appCtx == nil {
		return
	}
	wruntime.EventsEmit(appCtx, "account:changed", accountSnapshot())
}

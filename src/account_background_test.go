package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// ---------- 启动自动登录的服务端校验 ----------

func TestAccountVerifySessionClearsOnUnauthorized(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())
	if err := saveAccountState(accountCur); err != nil {
		t.Fatalf("写入登录态失败: %v", err)
	}

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/me" {
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"登录状态无效，请重新登录"}}`))
	})
	setAccountAPIBase(client.base)

	accountVerifySession(context.Background())

	accountMu.Lock()
	token := accountCur.Token
	accountMu.Unlock()
	if token != "" {
		t.Fatalf("服务端 401 时应清除本地登录态，实际仍有令牌")
	}
	if st := loadAccountState(); st.Token != "" {
		t.Fatalf("account.json 也应被清除，实际 %+v", st)
	}
	if st := accountSnapshot(); st.LoggedIn || st.ExpireReason != "no_token" {
		t.Fatalf("快照应回到未登录：%+v", st)
	}
}

func TestAccountVerifySessionKeepsStateOnNetworkError(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())
	setAccountAPIBase("http://127.0.0.1:1") // 死地址：模拟断网

	accountVerifySession(context.Background())

	accountMu.Lock()
	token := accountCur.Token
	accountMu.Unlock()
	if token != "tok-1" {
		t.Fatal("网络失败时应保留登录态（离线可用，不误登出）")
	}
	if st := accountSnapshot(); !st.LoggedIn {
		t.Fatalf("离线时仍应视为已登录：%+v", st)
	}
}

func TestAccountVerifySessionRefreshesProfile(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user":{"id":"u-9","email":"new@example.com","createdAt":1},"devices":[]}`))
	})
	setAccountAPIBase(client.base)

	accountVerifySession(context.Background())

	accountMu.Lock()
	email, uid := accountCur.Email, accountCur.UserID
	accountMu.Unlock()
	if email != "new@example.com" || uid != "u-9" {
		t.Fatalf("账号信息应随服务端刷新：email=%s uid=%s", email, uid)
	}
}

// ---------- 令牌静默续期 ----------

func TestAccountRenewsTokenNearExpiry(t *testing.T) {
	setupAccountTest(t)
	st := loggedInState()
	st.TokenExpiresAt = time.Now().Add(10 * 24 * time.Hour).Unix() // 剩余 10 天：进入续期窗口
	setAccountState(st)
	if err := saveAccountState(st); err != nil {
		t.Fatalf("写入登录态失败: %v", err)
	}

	newExpiry := time.Now().Add(90 * 24 * time.Hour).Unix()
	var paths []string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v1/me":
			_, _ = w.Write([]byte(`{"user":{"id":"u-1","email":"user@example.com","createdAt":1},"devices":[]}`))
		case "/v1/auth/refresh":
			if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
				t.Errorf("续期请求应带当前令牌，实际 Authorization=%q", got)
			}
			_, _ = fmt.Fprintf(w, `{"token":"tok-1","tokenExpiresAt":%d,"deviceId":"dev-1",`+
				`"user":{"id":"u-1","email":"user@example.com","createdAt":1}}`, newExpiry)
		default:
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
	})
	setAccountAPIBase(client.base)

	accountVerifySession(context.Background())

	if len(paths) != 2 || paths[0] != "/v1/me" || paths[1] != "/v1/auth/refresh" {
		t.Fatalf("应先校验再续期，实际请求: %v", paths)
	}
	accountMu.Lock()
	gotExp := accountCur.TokenExpiresAt
	accountMu.Unlock()
	if gotExp != newExpiry {
		t.Fatalf("续期后内存有效期应更新: got %d want %d", gotExp, newExpiry)
	}
	if st := loadAccountState(); st.TokenExpiresAt != newExpiry {
		t.Fatalf("续期后的有效期应落盘，实际 %d", st.TokenExpiresAt)
	}
	if snap := accountSnapshot(); !snap.LoggedIn {
		t.Fatalf("续期后仍应是已登录: %+v", snap)
	}
}

func TestAccountSkipsRenewalFarFromExpiry(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState()) // 剩余 90 天：不需要续期

	renewed := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/refresh" {
			renewed++
		}
		_, _ = w.Write([]byte(`{"user":{"id":"u-1","email":"user@example.com","createdAt":1},"devices":[]}`))
	})
	setAccountAPIBase(client.base)

	accountVerifySession(context.Background())

	if renewed != 0 {
		t.Fatalf("距到期还早不应调用续期，实际 %d 次", renewed)
	}
	if snap := accountSnapshot(); !snap.LoggedIn {
		t.Fatalf("校验后应保持已登录: %+v", snap)
	}
}

func TestAccountRenewalUnauthorizedInvalidatesSession(t *testing.T) {
	setupAccountTest(t)
	st := loggedInState()
	st.TokenExpiresAt = time.Now().Add(5 * 24 * time.Hour).Unix()
	setAccountState(st)

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me" {
			_, _ = w.Write([]byte(`{"user":{"id":"u-1","email":"user@example.com","createdAt":1},"devices":[]}`))
			return
		}
		// 服务端已撤销/过期：续期被拒，客户端必须停止拿旧令牌重试
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"登录状态无效，请重新登录"}}`))
	})
	setAccountAPIBase(client.base)

	accountVerifySession(context.Background())

	accountMu.Lock()
	token := accountCur.Token
	accountMu.Unlock()
	if token != "" {
		t.Fatalf("续期被拒（401）应清除令牌，实际仍有 %q", token)
	}
	if snap := accountSnapshot(); snap.LoggedIn || snap.ExpireReason != "no_token" {
		t.Fatalf("快照应回到未登录: %+v", snap)
	}
}

func TestAccountRenewalErrorKeepsSession(t *testing.T) {
	setupAccountTest(t)
	st := loggedInState()
	st.TokenExpiresAt = time.Now().Add(5 * 24 * time.Hour).Unix()
	setAccountState(st)

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me" {
			_, _ = w.Write([]byte(`{"user":{"id":"u-1","email":"user@example.com","createdAt":1},"devices":[]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"服务器内部错误"}}`))
	})
	setAccountAPIBase(client.base)

	accountVerifySession(context.Background())

	accountMu.Lock()
	token := accountCur.Token
	accountMu.Unlock()
	if token != "tok-1" {
		t.Fatalf("续期临时失败不应登出（下轮重试），实际令牌 %q", token)
	}
	if snap := accountSnapshot(); !snap.LoggedIn {
		t.Fatalf("续期临时失败仍应保持已登录: %+v", snap)
	}
}

// ---------- 后台定期检查 ----------

func TestAccountSyncIntervalIsTwentyMinutes(t *testing.T) {
	if accountSyncInterval != 20*time.Minute {
		t.Fatalf("后台同步周期应为 20 分钟（用户决策），实际 %s", accountSyncInterval)
	}
}

func TestAccountBackgroundTickChecksWithoutApplying(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())

	opposite := "true"
	if isAutostartEnabled() {
		opposite = "false"
	}
	client := syncTestServer(t, `[{"seq":3,"opId":"r1","key":"setting:autostart","value":`+opposite+`,"deviceId":"d","updatedAt":1}]`)
	setAccountAPIBase(client.base)

	// 后台检查不应触碰真实系统：把应用钩子换成"被调用即失败"
	oldAuto := applyAutostartFn
	applyAutostartFn = func(bool) error {
		t.Error("后台检查不应应用改动（必须等用户点「重启生效」）")
		return nil
	}
	t.Cleanup(func() { applyAutostartFn = oldAuto })

	accountBackgroundTick(context.Background())

	if st := accountSnapshot(); !st.PendingApply {
		t.Fatalf("后台检查应把差异放入待生效集合：%+v", st)
	}
	if st := accountSnapshot(); st.SyncError != "" {
		t.Fatalf("成功的检查不应留下失败状态：%+v", st)
	}
	if st := accountSnapshot(); st.Syncing {
		t.Fatal("检查结束后 syncing 应复位")
	}
}

func TestAccountBackgroundTickReportsFailure(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"服务器内部错误"}}`))
	})
	setAccountAPIBase(client.base)

	accountBackgroundTick(context.Background())

	if st := accountSnapshot(); st.SyncError == "" {
		t.Fatalf("失败应记录到状态里供界面展示：%+v", st)
	}
}

// ---------- 启动同步检查的「尚未检查 / 已检查」语义 ----------

// TestAccountStartupCheckedLifecycle 启动检查标记：载入时为未检查（界面显示「正在检查同步…」
// 而不是拿上一会话的 lastSyncedAt 显示「已同步」），首次后台检查完成即置位，登出复位。
func TestAccountStartupCheckedLifecycle(t *testing.T) {
	setupAccountTest(t)
	st := loggedInState()
	st.LastSyncedAt = 1 // 上一会话留下的同步时间：未检查时不得据此显示「已同步」
	setAccountState(st)

	if snap := accountSnapshot(); snap.StartupChecked {
		t.Fatalf("启动载入时不应是「本会话已检查」：%+v", snap)
	}
	if snap := accountSnapshot(); snap.LastSyncedAt == 0 {
		t.Fatal("用例前提：应有上一会话的 lastSyncedAt")
	}

	client := syncTestServer(t, `[]`)
	setAccountAPIBase(client.base)
	accountBackgroundTick(context.Background())

	if snap := accountSnapshot(); !snap.StartupChecked {
		t.Fatalf("首次后台检查完成后应标记已检查：%+v", snap)
	}

	clearAccountRuntime() // 登出
	if snap := accountSnapshot(); snap.StartupChecked {
		t.Fatalf("登出后应复位为未检查：%+v", snap)
	}
}

// TestAccountStartupCheckFailureStillMarksChecked 启动检查失败也要标记「已检查」：
// 否则界面会一直停在「正在检查同步…」，看不到失败原因（失败走 SyncError 分支）。
func TestAccountStartupCheckFailureStillMarksChecked(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())
	setAccountAPIBase("http://127.0.0.1:1") // 死地址：模拟断网

	accountBackgroundTick(context.Background())

	snap := accountSnapshot()
	if !snap.StartupChecked {
		t.Fatalf("检查失败同样算已检查（否则界面卡在检查中）：%+v", snap)
	}
	if snap.SyncError == "" {
		t.Fatalf("失败原因应显示在状态里：%+v", snap)
	}
}

// TestStartupTickSurfacesPendingRecordWithoutManualSync 启动检查就把服务器记录变成待生效：
// 现场问题是「启动显示已同步，手点立即同步才弹重启生效」——本用例锁定启动检查的结果
// 必须直接进入待生效集合（界面据此显示「待生效 N 项」与「重启生效」）。
func TestStartupTickSurfacesPendingRecordWithoutManualSync(t *testing.T) {
	setupAccountTest(t)
	st := loggedInState()
	st.LastSyncedAt = 1 // 上一会话的时间戳：正确实现不得据此显示「已同步」
	setAccountState(st)

	// 服务器上有本机没有的插件记录（本机 web profile 里确实没装该包）
	client := syncTestServer(t, `[{"seq":14,"opId":"r14","key":"plugin:web:not-installed-here",`+
		`"value":{"action":"install","spec":"^1.0.0","source":"npm","version":"1.0.0"},"deviceId":"other","updatedAt":1}]`)
	setAccountAPIBase(client.base)

	accountBackgroundTick(context.Background())

	snap := accountSnapshot()
	if !snap.StartupChecked {
		t.Fatalf("启动检查完成应标记已检查：%+v", snap)
	}
	if !snap.PendingApply || snap.PendingApplyCount != 1 {
		t.Fatalf("启动检查应把服务器记录放入待生效集合（界面才有「重启生效」）：%+v", snap)
	}
	if snap.SyncError != "" {
		t.Fatalf("成功的启动检查不应留下失败状态：%+v", snap)
	}
}

// TestAccountSyncNowMarksStartupChecked 手动同步成功同样算已检查：启动检查前手点同步后，
// 状态行应立刻按本次结果渲染，而不是继续显示「正在检查同步…」。
func TestAccountSyncNowMarksStartupChecked(t *testing.T) {
	app := &App{}
	setupAccountTest(t)
	setAccountState(loggedInState())
	setAccountAPIBase(syncTestServer(t, `[]`).base)

	if _, err := app.AccountSyncNow(); err != nil {
		t.Fatalf("手动同步失败: %v", err)
	}
	if snap := accountSnapshot(); !snap.StartupChecked {
		t.Fatalf("手动同步成功后应标记已检查：%+v", snap)
	}
}

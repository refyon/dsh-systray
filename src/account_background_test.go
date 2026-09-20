package main

import (
	"context"
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

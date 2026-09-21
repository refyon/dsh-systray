package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// account_e2e_test.go：真机端到端联调（默认跳过；需要真实邮箱验证码）。
//
// 这条用例走**真实网络**（默认服务地址，或 config 的 accountApiBase 覆盖），覆盖客户端代码的完整链路：
//
//	请求验证码 → verify 取令牌 → 同步检查（首次会按服务器是否为空决定上报基线/拉取）→
//	上报一条设置 op → 从服务器拉回并断言可见 → 登出清本地登录态
//
// 用法（两步，验证码 10 分钟内有效；**必须带 -count=1**，否则 go test 会用缓存结果而不真的发信/联调）：
//
//	DSH_SYSTRAY_E2E=1 DSH_SYSTRAY_E2E_EMAIL=you@example.com go test -count=1 -run TestAccountE2ELiveServer -v ./
//	  → 第一次运行只发送验证码并跳过，按提示带上验证码重跑：
//	DSH_SYSTRAY_E2E=1 DSH_SYSTRAY_E2E_EMAIL=you@example.com DSH_SYSTRAY_E2E_CODE=123456 go test -count=1 -run TestAccountE2ELiveServer -v ./
func TestAccountE2ELiveServer(t *testing.T) {
	if os.Getenv("DSH_SYSTRAY_E2E") == "" {
		t.Skip("真机联调默认跳过：设置 DSH_SYSTRAY_E2E=1 启用（需要真实邮箱验证码）")
	}
	email := strings.TrimSpace(os.Getenv("DSH_SYSTRAY_E2E_EMAIL"))
	if email == "" {
		t.Fatal("需要 DSH_SYSTRAY_E2E_EMAIL=<你的邮箱>")
	}
	code := strings.TrimSpace(os.Getenv("DSH_SYSTRAY_E2E_CODE"))

	setupAccountTest(t) // 隔离 account.json 到临时目录；服务地址用默认正式域名
	client := newAccountClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if code == "" {
		res, err := client.RequestCode(ctx, email, "zh")
		if err != nil {
			t.Fatalf("请求验证码失败: %v", err)
		}
		t.Skipf("验证码已发送到 %s（%d 秒内有效，%d 秒后可重发）；带上 DSH_SYSTRAY_E2E_CODE=xxxxxx 重跑本用例",
			maskEmail(email), res.ExpiresInSec, res.ResendAfterSec)
	}

	// 1) 登录
	sess, err := client.Verify(ctx, email, code, accountDeviceInfo{Name: "e2e-test", Platform: "darwin", AppVersion: appVersion})
	if err != nil {
		t.Fatalf("verify 失败: %v", err)
	}
	if len(sess.Token) < 20 || sess.User.ID == "" {
		t.Fatalf("会话异常: token 长度=%d user=%+v", len(sess.Token), sess.User)
	}
	t.Logf("登录成功：user=%s device=%s 令牌 %d 字符", sess.User.ID, sess.DeviceID, len(sess.Token))

	accountMu.Lock()
	accountCur = accountState{
		Token: sess.Token, TokenExpiresAt: sess.TokenExpiresAt, IssuedAt: time.Now().Unix(),
		DeviceID: sess.DeviceID, UserID: sess.User.ID, Email: sess.User.Email,
	}
	saved := accountCur
	accountMu.Unlock()
	if err := saveAccountState(saved); err != nil {
		t.Fatalf("保存登录态失败: %v", err)
	}
	t.Cleanup(func() { _ = clearAccountState() })

	// 2) 同步检查（首次：服务器为空则上报基线，否则拉到待生效集合；都不应报错）
	res, err := accountSyncNow(ctx, client)
	if err != nil {
		t.Fatalf("同步检查失败: %v", err)
	}
	t.Logf("同步检查：上报 %d 项，拉到 %d 条，基线=%v，待生效 %v", res.Uploaded, res.Pulled, res.Baseline, res.Pending)

	// 3) 上报一条设置变更，并从服务器拉回验证
	probe := !harnessPrereleaseOverride
	if err := accountEnqueueOp(opKeyHarnessPrerelease, probe); err != nil {
		t.Fatalf("登记操作记录失败: %v", err)
	}
	sent, err := accountFlushOps(ctx, client)
	if err != nil || sent == 0 {
		t.Fatalf("上报失败：sent=%d err=%v", sent, err)
	}

	page, err := client.OpsSince(ctx, sess.Token, 0, 200)
	if err != nil {
		t.Fatalf("增量拉取失败: %v", err)
	}
	var found bool
	for _, op := range page.Ops {
		if op.Key != opKeyHarnessPrerelease {
			continue
		}
		var v bool
		if json.Unmarshal(op.Value, &v) == nil && v == probe {
			found = true
		}
	}
	if !found {
		t.Fatalf("服务器上应能拉到刚上报的 %s=%v（共 %d 条记录）", opKeyHarnessPrerelease, probe, len(page.Ops))
	}
	t.Logf("上报闭环成功：服务器可见 %s=%v（游标 %d，共 %d 条）", opKeyHarnessPrerelease, probe, page.Cursor, len(page.Ops))

	// 4) 用本地策略再走一次「应用」路径的空转（待生效集合为空时不应有任何动作）
	applied, err := accountApplyPending(ctx, client, nil)
	if err != nil {
		t.Fatalf("应用路径失败: %v", err)
	}
	t.Logf("应用路径空转：应用 %d 项，本就一致 %d 项", applied.Applied, applied.Unchanged)

	// 5) 登出并清理本地登录态
	if err := client.Logout(ctx, sess.Token); err != nil {
		t.Fatalf("登出失败: %v", err)
	}
	if _, err := client.Me(ctx, sess.Token); accountErrorCode(err) != accErrUnauthorized {
		t.Fatalf("登出后令牌应失效，实际 err=%v", err)
	}
	clearAccountRuntime()
	if st := accountSnapshot(); st.LoggedIn {
		t.Fatalf("登出后不应再是已登录：%+v", st)
	}
	t.Log("端到端联调通过：登录 → 同步检查 → 上报 → 拉回 → 应用空转 → 登出")
}

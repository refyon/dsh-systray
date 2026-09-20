package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// newTestClient 起一个假 dsh-connect 服务并返回客户端（退避置 0，测试不等待）。
func newTestClient(t *testing.T, handler http.HandlerFunc) (*accountClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := newAccountClient(srv.URL)
	c.backoff = func(int) time.Duration { return 0 }
	return c, srv
}

// readJSONBody 读取并解析请求体（测试断言用）。
func readJSONBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("读取请求体失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v (%s)", err, string(data))
	}
	return m
}

func TestAccountRequestCodeAndVerify(t *testing.T) {
	var gotPath string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/v1/auth/otp/request":
			body := readJSONBody(t, r)
			if body["email"] != "user@example.com" {
				t.Errorf("email 字段错误: %v", body["email"])
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"expiresInSec":600,"resendAfterSec":60}`))
		case "/v1/auth/otp/verify":
			body := readJSONBody(t, r)
			if body["code"] != "123456" {
				t.Errorf("code 字段错误: %v", body["code"])
			}
			dev, _ := body["device"].(map[string]any)
			if dev["platform"] != "darwin" {
				t.Errorf("device.platform 未透传: %v", dev)
			}
			_, _ = w.Write([]byte(`{"token":"tok-1","tokenExpiresAt":1800000000,"deviceId":"dev-1",` +
				`"user":{"id":"u-1","email":"user@example.com","createdAt":1750000000}}`))
		default:
			t.Errorf("未预期的路径: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	ctx := context.Background()
	res, err := client.RequestCode(ctx, "user@example.com", "zh")
	if err != nil {
		t.Fatalf("RequestCode 失败: %v", err)
	}
	if res.ExpiresInSec != 600 || res.ResendAfterSec != 60 {
		t.Fatalf("验证码响应解析错误: %+v", res)
	}
	if gotPath != "/v1/auth/otp/request" {
		t.Fatalf("请求路径错误: %s", gotPath)
	}

	sess, err := client.Verify(ctx, "user@example.com", "123456", accountDeviceInfo{Name: "Mac", Platform: "darwin", AppVersion: "0.9.3"})
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if sess.Token != "tok-1" || sess.DeviceID != "dev-1" || sess.User.ID != "u-1" || sess.TokenExpiresAt != 1800000000 {
		t.Fatalf("会话解析错误: %+v", sess)
	}
}

func TestAccountErrorEnvelopeAndNoRetryOn4xx(t *testing.T) {
	attempts := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"登录状态无效，请重新登录"}}`))
	})

	_, err := client.Me(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("期望错误，实际成功")
	}
	if code := accountErrorCode(err); code != accErrUnauthorized {
		t.Fatalf("错误码解析错误: %q", code)
	}
	if attempts != 1 {
		t.Fatalf("4xx 不应重试，实际请求 %d 次", attempts)
	}

	// 429 限流同样不重试（交由 UI 提示等待）
	attempts = 0
	client2, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"请求过于频繁"}}`))
	})
	if _, err := client2.RequestCode(context.Background(), "a@b.com", ""); accountErrorCode(err) != accErrRateLimited {
		t.Fatalf("限流错误码解析错误: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("429 不应重试，实际请求 %d 次", attempts)
	}
}

func TestAccountRetryOn5xxAndNetwork(t *testing.T) {
	attempts := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"服务器内部错误"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"user":{"id":"u-1","email":"a@b.com","createdAt":1},"devices":[]}`))
	})

	me, err := client.Me(context.Background(), "tok")
	if err != nil {
		t.Fatalf("第三次应成功: %v", err)
	}
	if me.User.ID != "u-1" || attempts != 3 {
		t.Fatalf("重试行为错误: attempts=%d me=%+v", attempts, me)
	}

	// 连接失败（服务已关闭）→ 稳定返回 network 错误码
	dead := newAccountClient("http://127.0.0.1:1")
	dead.backoff = func(int) time.Duration { return 0 }
	if _, err := dead.Me(context.Background(), "tok"); accountErrorCode(err) != accErrNetwork {
		t.Fatalf("网络错误码解析错误: %v", err)
	}
}

func TestAccountOpsReportAndSince(t *testing.T) {
	var reported map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ops/report":
			if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
				t.Errorf("缺少/错误 Authorization: %q", got)
			}
			reported = readJSONBody(t, r)
			_, _ = w.Write([]byte(`{"accepted":1,"duplicates":0,"cursor":7}`))
		case "/v1/ops/since":
			if r.URL.Query().Get("cursor") != "7" {
				t.Errorf("cursor 未透传: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"ops":[{"seq":7,"opId":"op-1","key":"setting:autostart","value":true,` +
				`"deviceId":"dev-1","updatedAt":1750000000}],"cursor":7,"hasMore":false}`))
		default:
			t.Errorf("未预期的路径: %s", r.URL.Path)
		}
	})

	ctx := context.Background()
	rep, err := client.ReportOps(ctx, "tok-1", []accountOp{{OpID: "op-1", Key: "setting:autostart", Value: json.RawMessage(`true`)}})
	if err != nil {
		t.Fatalf("ReportOps 失败: %v", err)
	}
	if rep.Accepted != 1 || rep.Cursor != 7 {
		t.Fatalf("上报响应解析错误: %+v", rep)
	}
	ops, _ := reported["ops"].([]any)
	if len(ops) != 1 {
		t.Fatalf("上报条数错误: %v", reported["ops"])
	}
	first, _ := ops[0].(map[string]any)
	if first["key"] != "setting:autostart" || first["opId"] != "op-1" {
		t.Fatalf("上报字段错误: %v", first)
	}
	if v, ok := first["value"].(bool); !ok || !v {
		t.Fatalf("value 应为 JSON true: %#v", first["value"])
	}

	page, err := client.OpsSince(ctx, "tok-1", 7, 0)
	if err != nil {
		t.Fatalf("OpsSince 失败: %v", err)
	}
	if len(page.Ops) != 1 || page.Ops[0].Seq != 7 || string(page.Ops[0].Value) != "true" {
		t.Fatalf("增量拉取解析错误: %+v", page)
	}

	if _, err := client.ReportOps(ctx, "tok-1", make([]accountOp, accountOpsMaxBatch+1)); err == nil {
		t.Fatal("超过单次上报上限应报错")
	}
}

func TestAccountStateStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	oldDir, oldBase := accountStateDirValue(), accountAPIBaseValue()
	setAccountStateDir(dir)
	setAccountAPIBase("")
	t.Cleanup(func() {
		waitAccountSync()
		setAccountStateDir(oldDir)
		setAccountAPIBase(oldBase)
	})

	if got := loadAccountState(); got.Token != "" {
		t.Fatalf("文件不存在时应为零值: %+v", got)
	}

	st := accountState{
		Token: "tok-1", TokenExpiresAt: 1800000000, IssuedAt: 1750000000,
		DeviceID: "dev-1", UserID: "u-1", Email: "user@example.com",
		Cursor: 7, LastSyncedAt: 1750000100, BaselineDone: true,
	}
	if err := saveAccountState(st); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 权限 0600（Windows 的 NTFS 不保证 mode，故只在类 Unix 断言）
	if info, err := os.Stat(filepath.Join(dir, "account.json")); err == nil && os.PathSeparator == '/' {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("account.json 权限应为 0600，实际 %o", perm)
		}
	}

	got := loadAccountState()
	if !reflect.DeepEqual(got, st) {
		t.Fatalf("往返不一致:\n got %+v\nwant %+v", got, st)
	}

	if err := clearAccountState(); err != nil {
		t.Fatalf("清除失败: %v", err)
	}
	if got := loadAccountState(); got.Token != "" {
		t.Fatalf("清除后应回到零值: %+v", got)
	}
	if err := clearAccountState(); err != nil {
		t.Fatalf("重复清除应幂等: %v", err)
	}
}

func TestAccountSessionExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	day := int64(86400)

	cases := []struct {
		name string
		st   accountState
		want string
	}{
		{"未登录", accountState{}, "no_token"},
		{"有效", accountState{Token: "t", IssuedAt: now.Unix() - 5*day, TokenExpiresAt: now.Unix() + 60*day}, ""},
		{"本地 30 天到期", accountState{Token: "t", IssuedAt: now.Unix() - 31*day, TokenExpiresAt: now.Unix() + 60*day}, "session_expired"},
		{"服务端令牌过期", accountState{Token: "t", IssuedAt: now.Unix() - day, TokenExpiresAt: now.Unix() - 1}, "token_expired"},
		{"29 天仍有效", accountState{Token: "t", IssuedAt: now.Unix() - 29*day, TokenExpiresAt: now.Unix() + 61*day}, ""},
	}
	for _, tc := range cases {
		if got := tc.st.expireReason(now); got != tc.want {
			t.Errorf("%s: expireReason=%q want %q", tc.name, got, tc.want)
		}
		if tc.want == "" && !tc.st.loggedIn(now) {
			t.Errorf("%s: loggedIn 应为 true", tc.name)
		}
	}
}

func TestAccountAPIBasePrecedence(t *testing.T) {
	oldBase := accountAPIBaseValue()
	t.Cleanup(func() { setAccountAPIBase(oldBase) })

	setAccountAPIBase("")
	if got := accountAPIBase(); got != defaultAccountAPIBase {
		t.Fatalf("默认地址错误: %s", got)
	}
	setAccountAPIBase("https://example.test/")
	if got := accountAPIBase(); got != "https://example.test" {
		t.Fatalf("覆盖地址应去掉尾部斜杠: %s", got)
	}
	if c := newAccountClient(""); c.base != "https://example.test" {
		t.Fatalf("客户端未采用覆盖地址: %s", c.base)
	}
}

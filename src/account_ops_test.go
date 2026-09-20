package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
	"time"
)

// setupAccountTest 隔离 account.json 目录并复位进程内状态。
func setupAccountTest(t *testing.T) {
	t.Helper()
	oldDir, oldBase := accountStateDirOverride, accountAPIBaseOverride
	accountStateDirOverride = t.TempDir()
	accountAPIBaseOverride = ""
	t.Cleanup(func() {
		accountStateDirOverride, accountAPIBaseOverride = oldDir, oldBase
		accountMu.Lock()
		accountCur = accountState{}
		accountSyncing, accountSyncErr = false, ""
		accountMu.Unlock()
	})
}

// setAccountState 直接注入登录态（不经 HTTP）。
func setAccountState(st accountState) {
	accountMu.Lock()
	accountCur = st
	accountMu.Unlock()
}

func loggedInState() accountState {
	now := time.Now().Unix()
	return accountState{
		Token: "tok-1", IssuedAt: now, TokenExpiresAt: now + 90*86400,
		DeviceID: "dev-1", UserID: "u-1", Email: "user@example.com",
	}
}

func TestNewOpIDFormat(t *testing.T) {
	pattern := regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newOpID()
		if !pattern.MatchString(id) {
			t.Fatalf("opId 不符合服务端格式: %q", id)
		}
		if seen[id] {
			t.Fatalf("opId 重复: %q", id)
		}
		seen[id] = true
	}
	if len(newOpID()) != 36 {
		t.Fatalf("opId 应为 UUID 形态（36 字符）: %q", newOpID())
	}
}

func TestAccountPluginKeyUsesWebProfile(t *testing.T) {
	if got := accountPluginKey("restrict-discipline"); got != "plugin:web:restrict-discipline" {
		t.Fatalf("插件合并键错误: %s", got)
	}
}

func TestAccountEnqueueReplacesSameKeyAndPersists(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())

	if err := accountEnqueueOp(opKeyAutostart, true); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	if err := accountEnqueueOp(opKeyAutostart, false); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	if err := accountEnqueueOp(opKeyHarnessPrerelease, true); err != nil {
		t.Fatalf("登记失败: %v", err)
	}

	if n := accountPendingCount(); n != 2 {
		t.Fatalf("同 key 应被替换，待上报数应为 2，实际 %d", n)
	}

	// 落盘：重新载入应保留队列
	st := loadAccountState()
	if len(st.PendingOps) != 2 {
		t.Fatalf("队列未持久化: %+v", st.PendingOps)
	}
	var found bool
	for _, op := range st.PendingOps {
		if op.Key == opKeyAutostart {
			found = true
			if string(op.Value) != "false" {
				t.Fatalf("同 key 应保留最新值，实际 %s", op.Value)
			}
		}
	}
	if !found {
		t.Fatal("队列中缺少 autostart 记录")
	}
}

func TestAccountEnqueueSkippedWhenLoggedOut(t *testing.T) {
	setupAccountTest(t)
	setAccountState(accountState{})

	accountEnqueueOpIfLoggedIn(opKeyAutostart, true)
	if n := accountPendingCount(); n != 0 {
		t.Fatalf("未登录不应产生本地残队，实际 %d", n)
	}
}

func TestAccountFlushOpsReportsAndDrains(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())

	var gotKeys []string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ops/report" {
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
		body := readJSONBody(t, r)
		ops, _ := body["ops"].([]any)
		for _, raw := range ops {
			op, _ := raw.(map[string]any)
			gotKeys = append(gotKeys, op["key"].(string))
		}
		_, _ = w.Write([]byte(`{"accepted":3,"duplicates":0,"cursor":12}`))
	})

	for _, kv := range []struct {
		key string
		val any
	}{{opKeyAutostart, true}, {opKeyHarnessPrerelease, false}, {accountPluginKey("pkg-a"), map[string]any{"action": "install", "spec": "^1.0.0", "source": "npm", "version": "1.0.2"}}} {
		if err := accountEnqueueOp(kv.key, kv.val); err != nil {
			t.Fatalf("登记失败: %v", err)
		}
	}

	sent, err := accountFlushOps(context.Background(), client)
	if err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	if sent != 3 || len(gotKeys) != 3 {
		t.Fatalf("上报条数错误: sent=%d keys=%v", sent, gotKeys)
	}
	if n := accountPendingCount(); n != 0 {
		t.Fatalf("上报成功后队列应清空，实际 %d", n)
	}
	st := loadAccountState()
	if st.Cursor != 12 || st.LastSyncedAt == 0 {
		t.Fatalf("游标/同步时间未推进: cursor=%d lastSynced=%d", st.Cursor, st.LastSyncedAt)
	}
}

func TestAccountFlushOpsFailureKeepsQueue(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"服务器内部错误"}}`))
	})

	if err := accountEnqueueOp(opKeyAutostart, true); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	sent, err := accountFlushOps(context.Background(), client)
	if err == nil {
		t.Fatal("服务端 5xx 时应返回错误")
	}
	if sent != 0 || accountPendingCount() != 1 {
		t.Fatalf("失败时队列必须保留: sent=%d pending=%d", sent, accountPendingCount())
	}
}

func TestAccountFlushOpsRequiresToken(t *testing.T) {
	setupAccountTest(t)
	setAccountState(accountState{}) // 无令牌
	accountMu.Lock()
	accountCur.PendingOps = []accountPendingOp{{OpID: "op-1", Key: opKeyAutostart, Value: json.RawMessage(`true`)}}
	accountMu.Unlock()

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("未登录不应发起请求")
	})
	if _, err := accountFlushOps(context.Background(), client); accountErrorCode(err) != accErrUnauthorized {
		t.Fatalf("应返回 unauthorized，实际 %v", err)
	}
	if n := accountPendingCount(); n != 1 {
		t.Fatalf("未登录时队列应保留（换账号登录后可继续上报）: %d", n)
	}
}

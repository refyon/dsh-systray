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
	oldDir, oldBase := accountStateDirValue(), accountAPIBaseValue()
	setAccountStateDir(t.TempDir())
	setAccountAPIBase("")
	clearAccountRuntime()
	t.Cleanup(func() {
		waitAccountSync() // 等在跑的上报结束，避免与下面的复位竞态（-race 曾暴露）
		setAccountStateDir(oldDir)
		setAccountAPIBase(oldBase)
		clearAccountRuntime()
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

// TestFlushAckClampsCursorBeforePending 上报确认不得把游标推进到未生效记录之后：
// 服务器游标是账号全流位置（含其它设备的更高 seq），无条件采用会让待生效记录「落在游标之前」
// ——增量拉取再也取不到，下一次同步的整体替换把它们静默丢弃
// （2026-09-22 现场问题②：插件同步失败后再点「立即同步」永远不再弹「重启生效」按钮）。
func TestFlushAckClampsCursorBeforePending(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())

	key := accountPluginKey("pkg-fail")
	accountSetPendingApply(map[string]accountSyncTarget{
		key: {
			Key:   key,
			Value: json.RawMessage(`{"action":"update","spec":"^1.0.0","source":"npm","version":"1.0.2"}`),
			Seq:   30,
		},
	})

	if err := accountEnqueueOp(opKeyAutostart, true); err != nil {
		t.Fatalf("登记本地改动失败: %v", err)
	}
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ops/report" {
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"accepted":1,"duplicates":0,"cursor":50}`))
	})
	if n, err := accountFlushOps(context.Background(), client); err != nil || n != 1 {
		t.Fatalf("上报失败: n=%d err=%v", n, err)
	}

	accountMu.Lock()
	cursor := accountCur.Cursor
	accountMu.Unlock()
	if cursor != 29 {
		t.Fatalf("游标应钳在未生效记录之前（29），实际 %d", cursor)
	}
	if st := accountSnapshot(); !st.PendingApply || st.PendingApplyCount != 1 {
		t.Fatalf("待生效提示不得因上报推进游标而丢失：%+v", st)
	}
}

// TestFlushAckRecordsReportedValues 上报确认后记下「本机最近一次上报成功的值」，并在该 key 的
// 服务器记录被确认/应用（appliedVals 追上）后清除。漂移重判据此排除「本机改动就是自己刚上报
// 的新值」——否则会把被自己新记录取代的旧记录重新入队（见 accountReenqueueDriftedApplied）。
func TestFlushAckRecordsReportedValues(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())

	if err := accountEnqueueOp(opKeyHarnessVersion, "1.2.3"); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"accepted":1,"duplicates":0,"cursor":9}`))
	})
	if n, err := accountFlushOps(context.Background(), client); err != nil || n != 1 {
		t.Fatalf("上报失败: n=%d err=%v", n, err)
	}

	accountMu.Lock()
	got, ok := accountCur.ReportedVals[opKeyHarnessVersion]
	accountMu.Unlock()
	if !ok || string(got) != `"1.2.3"` {
		t.Fatalf("上报确认后应记下自报值，实际 ok=%v value=%s", ok, got)
	}
	if loaded := loadAccountState(); string(loaded.ReportedVals[opKeyHarnessVersion]) != `"1.2.3"` {
		t.Fatalf("自报值应随 account.json 持久化（跨托盘重启仍能豁免伪漂移）：%+v", loaded.ReportedVals)
	}

	// 该 key 的服务器记录被确认/应用后，appliedVals 已与账号一致，自报值不再需要。
	accountMarkApplied(opKeyHarnessVersion, json.RawMessage(`"1.2.3"`), 9, 1700000000)
	accountMu.Lock()
	_, still := accountCur.ReportedVals[opKeyHarnessVersion]
	accountMu.Unlock()
	if still {
		t.Fatal("该 key 的服务器记录已应用：应清除自报值，避免长期保留失效值")
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

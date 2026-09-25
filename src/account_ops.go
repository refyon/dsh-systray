// account_ops.go：本地操作记录队列（待上报 op）与合并键约定。
//
// 语义（设计见 Docs/dsh-systray-connect-sync-plan.md）：
//   - 每个「用户可见的设置/在线插件变更」生成一条 op（opId 幂等），先落本地队列；
//   - 上报成功（服务端确认）后出队；失败保留并退避重试，状态页显示「同步失败」；
//   - 同一 key 的待上报记录只保留最新值：目标状态是标量，离线期间反复改动无需逐条回放；
//   - 队列随 account.json（0600）持久化，跨托盘重启保留；登出时随登录态一起丢弃。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// 设置项合并键（与 dsh-connect docs/API.md §11 一致）。
const (
	opKeyAutostart         = "setting:autostart"
	opKeyHarnessPrerelease = "setting:harness_prerelease"
	opKeyHarnessVersion    = "setting:harness_version"
	// accountPluginProfile 当前只同步 web profile（用户决策）。
	accountPluginProfile = "web"
)

// accountPluginKey 在线插件的合并键：plugin:<profile>:<name>。
func accountPluginKey(name string) string {
	return "plugin:" + accountPluginProfile + ":" + strings.TrimSpace(name)
}

// newOpID 生成 UUID v4 形态的幂等键（服务端 opId 格式：≤64 个 [A-Za-z0-9._-]）。
func newOpID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("op-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// accountEnqueueOp 把一条变更追加到本地队列并落盘；同一 key 的旧待上报记录被替换。
func accountEnqueueOp(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	accountMu.Lock()
	st := accountCur
	ops := make([]accountPendingOp, 0, len(st.PendingOps)+1)
	for _, op := range st.PendingOps {
		if op.Key != key {
			ops = append(ops, op)
		}
	}
	ops = append(ops, accountPendingOp{OpID: newOpID(), Key: key, Value: raw, CreatedAt: time.Now().Unix()})
	st.PendingOps = ops
	accountCur = st
	accountMu.Unlock()
	return saveAccountState(st)
}

// accountEnqueueOpIfLoggedIn 仅在已登录时登记变更（未登录不产生本地残队）。
func accountEnqueueOpIfLoggedIn(key string, value any) {
	accountMu.Lock()
	loggedIn := accountCur.loggedIn(time.Now())
	accountMu.Unlock()
	if !loggedIn {
		return
	}
	if err := accountEnqueueOp(key, value); err != nil {
		log.Printf("[account] 登记操作记录失败 key=%s: %v", key, err)
	}
}

// accountFlushMu 串行化上报：埋点（设置改动）与后台同步可能同时触发，串行避免重复提交同一批。
var accountFlushMu sync.Mutex

// accountSyncWG 追踪在跑的上报 goroutine：测试在复位全局状态前等它们结束（-race 要求），
// 进程退出前也可等待，保证最后一笔改动不会因为退出而丢失。
var accountSyncWG sync.WaitGroup

// waitAccountSync 等待在跑的上报结束。
func waitAccountSync() { accountSyncWG.Wait() }

// accountFlushOps 上报队列中的待上报记录；成功的条目出队并推进游标。
//
// 返回本次成功上报的条数。失败时队列保持不变，由调用方决定退避重试。
func accountFlushOps(ctx context.Context, client *accountClient) (int, error) {
	accountFlushMu.Lock()
	defer accountFlushMu.Unlock()

	accountMu.Lock()
	pending := append([]accountPendingOp(nil), accountCur.PendingOps...)
	token := accountCur.Token
	accountMu.Unlock()

	if len(pending) == 0 {
		return 0, nil
	}
	if token == "" {
		return 0, &accountError{Code: accErrUnauthorized, Message: "未登录"}
	}

	sent := 0
	var cursor int64
	var acked []accountPendingOp
	for start := 0; start < len(pending); start += accountOpsMaxBatch {
		end := start + accountOpsMaxBatch
		if end > len(pending) {
			end = len(pending)
		}
		chunk := pending[start:end]
		ops := make([]accountOp, 0, len(chunk))
		for _, op := range chunk {
			ops = append(ops, accountOp{OpID: op.OpID, Key: op.Key, Value: op.Value})
		}
		res, err := client.ReportOps(ctx, token, ops)
		if err != nil {
			if sent > 0 {
				accountFlushAck(acked, cursor) // 已确认的部分照常出队，其余保留待重试
			}
			return sent, err
		}
		acked = append(acked, chunk...)
		cursor = res.Cursor
		sent += len(chunk)
	}
	accountFlushAck(acked, cursor)
	return sent, nil
}

// accountFlushAck 把已确认的条目出队并推进游标（未被确认的条目保留）。
func accountFlushAck(acked []accountPendingOp, cursor int64) {
	done := make(map[string]bool, len(acked))
	for _, op := range acked {
		done[op.OpID] = true
	}
	accountMu.Lock()
	defer accountMu.Unlock()
	if len(done) > 0 {
		rest := make([]accountPendingOp, 0, len(accountCur.PendingOps))
		for _, op := range accountCur.PendingOps {
			if !done[op.OpID] {
				rest = append(rest, op)
			}
		}
		accountCur.PendingOps = rest
		// 记下「本机最近一次上报成功的值」：该 key 的漂移重判据此排除自报改动
		// （见 accountReenqueueDriftedApplied；同 key 只留最新一条）。
		for _, op := range acked {
			if len(op.Value) == 0 {
				continue
			}
			if accountCur.ReportedVals == nil {
				accountCur.ReportedVals = map[string]json.RawMessage{}
			}
			accountCur.ReportedVals[op.Key] = append(json.RawMessage(nil), op.Value...)
		}
	}
	if cursor > accountCur.Cursor {
		accountCur.Cursor = cursor
	}
	// 服务器游标是账号全流位置（含其它设备的更高 seq）：无条件采用会越过仍未生效的记录，
	// 增量拉取再也取不到它们，下一次 accountSetPendingApply 的整体替换会把它们静默丢弃
	// （2026-09-22 现场问题②：插件同步失败后再点「立即同步」永远不再弹「重启生效」）。
	accountClampCursorLocked()
	accountCur.LastSyncedAt = time.Now().Unix()
	accountSyncErr = ""
	_ = saveAccountState(accountCur)
}

// accountPendingCount 待上报条数（状态页展示）。
func accountPendingCount() int {
	accountMu.Lock()
	defer accountMu.Unlock()
	return len(accountCur.PendingOps)
}

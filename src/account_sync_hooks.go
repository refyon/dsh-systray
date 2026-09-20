// account_sync_hooks.go：把用户可见的设置 / 在线插件变更登记为操作记录（op）并立即尝试上报。
//
// 冻结决策（见 Docs/dsh-systray-connect-sync-plan.md）：
//   - 只同步 web profile 的**在线**插件（npm/github/tarball 等）；file/local 来源的本地插件不上报；
//   - 安装位置由本机派生，绝对路径一律不上报（只报 spec/source/version）；
//   - 「仅改本地记录」类变更（待重指定、自动禁用记录清理）不算插件变动，不上报；
//   - 上报是异步的：登记入队后立刻尝试一次，失败留在队列里由后台重试（状态页显示「同步失败」）。
package main

import (
	"context"
	"strings"
	"time"
)

// pluginOpValue 插件操作记录的 value 形状（与 dsh-connect docs/API.md §11 一致）。
type pluginOpValue struct {
	Action  string `json:"action"` // update | remove（install 由服务端/其它客户端产生）
	Spec    string `json:"spec"`
	Source  string `json:"source"`
	Version string `json:"version"`
}

// isOnlinePluginSource 是否为在线来源（本地插件不上报）。
func isOnlinePluginSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "", "file", "local":
		return false
	default:
		return true
	}
}

// accountSyncKick 触发一次异步上报：不阻塞调用方；失败只记录状态，由后台重试。
func accountSyncKick() {
	accountMu.Lock()
	loggedIn := accountCur.loggedIn(time.Now())
	accountMu.Unlock()
	if !loggedIn {
		return
	}
	accountSyncWG.Add(1) // 必须在 go 之前 Add：否则测试里 Wait 可能先于计数器自增返回
	go func() {
		defer accountSyncWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := accountFlushOps(ctx, newAccountClient("")); err != nil {
			accountSetSyncError(accountErrorText(err))
			return
		}
		accountClearSyncError()
	}()
}

// reportSettingAutostart 上报开机自启动开关：值取后端**实际注册状态**（登记失败时不会上报错值）。
func reportSettingAutostart() {
	if !accountLoggedIn() {
		return
	}
	if err := accountEnqueueOp(opKeyAutostart, isAutostartEnabled()); err != nil {
		return
	}
	accountSyncKick()
}

// reportSettingPrerelease 上报 Harness 预发布通道开关。
func reportSettingPrerelease(on bool) {
	if !accountLoggedIn() {
		return
	}
	if err := accountEnqueueOp(opKeyHarnessPrerelease, on); err != nil {
		return
	}
	accountSyncKick()
}

// reportHarnessVersionIfChanged 上报最后选用的 Harness 版本。
//
// 在「更新 / 重置 Harness」流程结束时调用：与流程开始前的版本比较，只有真的变了才上报——
// 这样不必在长流程里找成功点，失败回滚时版本未变，自然不会产生错误记录。
func reportHarnessVersionIfChanged(prev string) {
	cur := strings.TrimPrefix(strings.TrimSpace(installedHarnessVersion()), "v")
	if cur == "" || cur == strings.TrimPrefix(strings.TrimSpace(prev), "v") {
		return
	}
	if !accountLoggedIn() {
		return
	}
	if err := accountEnqueueOp(opKeyHarnessVersion, cur); err != nil {
		return
	}
	accountSyncKick()
}

// reportPluginBatchChanges 批处理结果确定后（含自愈/回退）上报成功的**在线**插件变更。
func reportPluginBatchChanges(tasks []*pluginOpTask) {
	if !accountLoggedIn() {
		return
	}
	changed := 0
	for _, t := range tasks {
		if t == nil || !t.ok || t.recordOnly {
			continue
		}
		if t.op != "update" && t.op != "remove" { // enable 只改本地激活状态，不在同步范围
			continue
		}
		if !isOnlinePluginSource(t.row.Source) {
			continue
		}
		val := pluginOpValue{Action: t.op, Spec: t.row.Spec, Source: t.row.Source}
		if t.op == "update" {
			val.Version = t.newVer
			if val.Version == "" {
				val.Version = t.target
			}
		}
		if err := accountEnqueueOp(accountPluginKey(t.name), val); err != nil {
			continue
		}
		changed += 1
	}
	if changed > 0 {
		accountSyncKick()
	}
}

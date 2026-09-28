// plugin_install_times.go：本机插件安装时刻台账（`plugin:<profile>:<name>` → Unix 秒）。
//
// 为什么独立记台账：账号同步的「删除墓碑」判定要回答「本机这份安装发生在墓碑之前还是之后」
// （见 accountKeyTargetSatisfiedAt 的 remove 分支、accountReconcileLocalPlugins 的墓碑分支）。
// 原实现取 node_modules 里包目录的创建时间，而 Windows 下 pnpm 每次 add 都会重建 node_modules
// 的链接层，把所有包目录的创建时间刷成同一次操作的时刻——2026-09-28 实测 web profile 下五个
// 插件目录 CreationTime 全为 21:28:54/55，而它们的真实安装时刻跨前一天到当天。于是「服务端已
// 删除的插件」被判成本机装得更新，对账补报 install 把删除推翻，插件删了又回来。
//
// 写入时机只有两个真实安装点：同步应用插件变更（applyPluginOp → installPluginIntoProfile）
// 与托盘内的插件批处理（reportPluginBatchChanges）。已记录的插件在版本更新时不刷新时刻——
// 与「重新安装才算新安装、更新不算」的既有语义一致。读取时没有记录（功能引入前就装着、或在
// 外部用 pnpm/npm 安装）退回文件系统时间并固化一次，此后不再随 pnpm 重建 node_modules 漂移。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// pluginInstallTimesFile 台账文件名（与 account.json / config.json 同目录）。
const pluginInstallTimesFile = "plugin-installs.json"

// pluginInstallTimes 台账的磁盘形态。
type pluginInstallTimes struct {
	// Times `plugin:<profile>:<name>` → 本机安装该插件的时刻（Unix 秒）。
	Times map[string]int64 `json:"times,omitempty"`
}

var (
	// pluginInstallTimesMu 保护内存台账与加载标记。
	pluginInstallTimesMu sync.Mutex
	// pluginInstallTimesPathMu 单独保护路径覆盖：路径解析会在持有数据锁时被调用，两者共用一把锁会自锁。
	pluginInstallTimesPathMu     sync.Mutex
	pluginInstallTimesCur        = &pluginInstallTimes{Times: map[string]int64{}}
	pluginInstallTimesLoaded     bool
	pluginInstallTimesLoadedPath string // 上次载入所用路径：路径变化即重新载入（见 loadPluginInstallTimesLocked）
	// pluginInstallTimesFileOverride 台账路径覆盖（测试用；空 = 按默认位置解析）。
	pluginInstallTimesFileOverride string
	// pluginInstallTimesNoPersist 未显式指定路径时禁止落盘（由测试的 TestMain 置位）：
	// 单测会大量构造临时 profile 并查询安装时刻，落盘会写进用户真实的配置目录。
	// 显式用 setPluginInstallTimesFileOverride 指定路径的用例不受影响（见 *_test.go 的 TestMain）。
	pluginInstallTimesNoPersist bool
)

// pluginInstallTimesFilePath 台账文件路径：与 account.json / config.json 同目录。
// 解析不出目录时返回空串——调用方按「不落盘」处理，内存台账仍然可用（本次运行内有效）。
func pluginInstallTimesFilePath() string {
	pluginInstallTimesPathMu.Lock()
	override, noPersist := pluginInstallTimesFileOverride, pluginInstallTimesNoPersist
	pluginInstallTimesPathMu.Unlock()
	if override != "" {
		return override
	}
	if noPersist {
		return ""
	}
	if p := accountStatePath(); p != "" {
		return filepath.Join(filepath.Dir(p), pluginInstallTimesFile)
	}
	if p := configFilePath(); p != "" {
		return filepath.Join(filepath.Dir(p), pluginInstallTimesFile)
	}
	return ""
}

// loadPluginInstallTimesLocked 载入台账（调用方须持数据锁）。
// 文件缺失或损坏按空台账处理：这是本机自用的辅助台账，读不出来不该阻塞同步。
//
// 按路径缓存而不是「只加载一次」：路径变了就重新载入并丢弃旧的内存台账。生产环境路径恒定
// （实际只加载一次），而测试用例会把 account.json 目录换到各自的临时目录——沿用上一个用例的
// 内存台账会把它的安装记录泄漏进来（曾使「应用顺序」用例把插件误判为已满足而跳过应用）。
func loadPluginInstallTimesLocked() {
	p := pluginInstallTimesFilePath()
	if pluginInstallTimesLoaded && pluginInstallTimesLoadedPath == p {
		return
	}
	pluginInstallTimesLoaded = true
	pluginInstallTimesLoadedPath = p
	pluginInstallTimesCur = &pluginInstallTimes{Times: map[string]int64{}}
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var t pluginInstallTimes
	if json.Unmarshal(data, &t) != nil || t.Times == nil {
		logWarn("plugin-installs", "安装时刻台账无法解析，按空台账继续（下次写入即重建）：%s", p)
		return
	}
	pluginInstallTimesCur = &t
}

// savePluginInstallTimesLocked 原子写入台账（临时文件 + rename，0600）。
func savePluginInstallTimesLocked() {
	p := pluginInstallTimesFilePath()
	if p == "" {
		return
	}
	data, err := json.MarshalIndent(pluginInstallTimesCur, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
	}
}

// pluginInstallTimeLookup 取台账里的安装时刻（无记录返回 0）。
func pluginInstallTimeLookup(profile, name string) int64 {
	key := accountPluginKeyFor(profile, name)
	pluginInstallTimesMu.Lock()
	defer pluginInstallTimesMu.Unlock()
	loadPluginInstallTimesLocked()
	return pluginInstallTimesCur.Times[key]
}

// pluginInstallTimeNoteInstalled 记一次「本机安装」：**已有记录不刷新**——版本更新不算重新安装
// （与既有语义一致：只有从无到有的安装才是新的安装事件）。也用于把文件系统兜底值固化一次。
func pluginInstallTimeNoteInstalled(profile, name string, at int64) {
	if at <= 0 {
		return
	}
	key := accountPluginKeyFor(profile, name)
	pluginInstallTimesMu.Lock()
	defer pluginInstallTimesMu.Unlock()
	loadPluginInstallTimesLocked()
	if _, exists := pluginInstallTimesCur.Times[key]; exists {
		return
	}
	pluginInstallTimesCur.Times[key] = at
	savePluginInstallTimesLocked()
	logInfo("plugin-installs", "记录本机安装时刻：%s = %s", key, formatAccountTime(at))
}

// pluginInstallTimeForget 卸载后清除记录（下次安装重新计时）。
func pluginInstallTimeForget(profile, name string) {
	key := accountPluginKeyFor(profile, name)
	pluginInstallTimesMu.Lock()
	defer pluginInstallTimesMu.Unlock()
	loadPluginInstallTimesLocked()
	if _, exists := pluginInstallTimesCur.Times[key]; !exists {
		return
	}
	delete(pluginInstallTimesCur.Times, key)
	savePluginInstallTimesLocked()
	logInfo("plugin-installs", "清除安装时刻记录：%s", key)
}

// pluginInstallTimeNow 当前时刻（Unix 秒），供安装动作写入台账。
func pluginInstallTimeNow() int64 { return time.Now().Unix() }

// pluginInstallTimesFileOverrideValue 取台账路径覆盖（测试隔离用）。
func pluginInstallTimesFileOverrideValue() string {
	pluginInstallTimesPathMu.Lock()
	defer pluginInstallTimesPathMu.Unlock()
	return pluginInstallTimesFileOverride
}

// setPluginInstallTimesFileOverride 设置台账路径覆盖（测试隔离用；空 = 回到默认位置解析）。
func setPluginInstallTimesFileOverride(p string) {
	pluginInstallTimesPathMu.Lock()
	pluginInstallTimesFileOverride = p
	pluginInstallTimesPathMu.Unlock()
}

// setPluginInstallTimesNoPersist 关闭/恢复默认落盘（测试的 TestMain 置位，见 pluginInstallTimesNoPersist）。
func setPluginInstallTimesNoPersist(v bool) {
	pluginInstallTimesPathMu.Lock()
	pluginInstallTimesNoPersist = v
	pluginInstallTimesPathMu.Unlock()
}

// resetPluginInstallTimes 丢弃内存台账并复位加载标记（测试隔离用：上一个用例残留的安装记录
// 不该参与下一个用例的删除墓碑判定）。
func resetPluginInstallTimes() {
	pluginInstallTimesMu.Lock()
	defer pluginInstallTimesMu.Unlock()
	pluginInstallTimesCur = &pluginInstallTimes{Times: map[string]int64{}}
	pluginInstallTimesLoaded = false
	pluginInstallTimesLoadedPath = ""
}

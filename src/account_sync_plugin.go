// account_sync_plugin.go：把同步来的插件变更落到本机（只作用于 web profile）。
//
// 复用既有插件链路的安全语义：停服 → 快照（package.json / pnpm-lock.yaml 复制 + node_modules 改名）
// → pnpm 操作 → 重启校验 → 失败回退；成功则提升 LKG 并清理快照。
// 与用户手动操作的区别：不经过「待应用」队列（用户点「重启生效」本就是一次显式确认）。
package main

import (
	"errors"
	"fmt"
	"strings"
)

// webProfileDir 定位 web profile 目录（同步只作用于 web profile，见冻结决策）。
func webProfileDir() (string, bool) {
	for _, pf := range enumeratePluginProfiles() {
		if pf.label == accountPluginProfile {
			return pf.dir, true
		}
	}
	return "", false
}

// pluginDepArg 构造 pnpm 依赖参数：
// 带协议前缀的 spec（github:/git+/http(s)/file:/npm:）与 GitHub owner/repo 简写原样使用，
// 其余（版本范围、固定版本）拼成 name@spec。
func pluginDepArg(name, spec string) string {
	s := strings.TrimSpace(spec)
	if s == "" {
		return name
	}
	// 自建中转 Worker 启用时：github: spec 改写为固定 commit 的中转 tarball 地址
	//（解析失败则原样回退，不影响安装）。
	if mirrorBase != "" {
		if u, err := mirrorTarballSpec(s); err == nil && u != "" {
			return u
		}
	}
	if specGitHubShorthandRe.MatchString(s) {
		return s
	}
	lower := strings.ToLower(s)
	for _, prefix := range []string{"github:", "git+", "http://", "https://", "file:", "link:", "workspace:", "npm:"} {
		if strings.HasPrefix(lower, prefix) {
			return s
		}
	}
	return name + "@" + s
}

// applyPluginOp 应用一条插件变更（install/update → 安装到 spec；remove → 卸载）。
func applyPluginOp(name string, v pluginOpValue) error {
	dir, ok := webProfileDir()
	if !ok {
		return fmt.Errorf("未找到 %s profile 目录，无法同步插件 %s", accountPluginProfile, name)
	}
	if v.Action == "remove" {
		return removePluginFromProfile(dir, name)
	}
	spec := strings.TrimSpace(v.Spec)
	if spec == "" {
		spec = strings.TrimSpace(v.Version)
	}
	if spec == "" {
		return fmt.Errorf("插件 %s 缺少 spec/version，无法安装", name)
	}
	// GitHub 来源 + 已有凭据：幂等补齐 git 凭据助手与 npmrc tokenHelper（与插件批处理路径
	// 同口径，见 plugin_batch.go：563）——缺失时私有仓库必然解析失败。未授权过的机器不会
	// 在此触发授权流程（ghAuthToken 为空则跳过）。
	if source, _, _ := classifyPluginSpec(spec); source == "github" && ghAuthToken() != "" {
		ensureGitHubPrivateRepoCreds()
	}
	return installPluginIntoProfile(dir, name, pluginDepArg(name, spec))
}

// installPluginIntoProfile 安装依赖到 profile：快照 → pnpm add → 登记激活清单 → 入口预检 →
// 重启校验 → 失败回退。
//
// 必须同时登记 dsh.profile.bundles：harness 只加载激活清单里的插件，而 pnpm add 只写
// dependencies——只装不登记就会出现「关于页插件齐全、harness 会话设置→插件里找不到」
// （2026-09-21 现场问题；用户靠手动导入备份才恢复，导入路径正是会合并 bundles 的那条）。
func installPluginIntoProfile(dir, name, dep string) error {
	killServer() // 运行中的 node 占用文件，快照改名会失败
	hadNM := snapshotPluginProfile(dir)
	rollback := func(reason string) error {
		restorePluginProfileSnapshot(dir, hadNM)
		restartAndVerifyServer()
		return errors.New(reason)
	}
	if err := runProfileCmd(dir, pnpmCmd(), "add", dep); err != nil {
		return rollback(fmt.Sprintf("安装 %s 失败：%v", dep, err))
	}
	if err := activateSyncedPlugin(dir, name); err != nil {
		return rollback(fmt.Sprintf("%s %v，已回退", name, err))
	}
	if !restartAndVerifyServer() {
		return rollback(fmt.Sprintf("%s 与当前服务不兼容，已回退", dep))
	}
	promoteProfileLkg(dir)
	cleanupPluginProfileSnapshot(dir)
	return nil
}

// activateSyncedPlugin 安装成功后的落地登记：写进 dsh.profile.bundles（harness 只加载激活
// 清单里的插件，pnpm add 不写它）+ 入口预检。任一步失败由调用方回退快照。
func activateSyncedPlugin(dir, name string) error {
	if err := enablePluginInProfile(dir, name); err != nil {
		return fmt.Errorf("登记激活清单失败：%v", err)
	}
	return verifySyncedPluginLoadable(dir, name)
}

// pluginImportCheckFn 插件入口预检（测试可替换，避免单测依赖真实 node/profile 环境）。
var pluginImportCheckFn = pluginImportCheck

// verifySyncedPluginLoadable 同步安装后的确定性验收：插件入口能被 node 解析并 import
// （与导入流程的预检同一实现）。「解析类」错误不算失败：宿主包由 harness 运行时从自己的
// 依赖树提供、不在 profile 树，预检环境必然报错而运行时正常（见 preflightResolveErrorRe）。
func verifySyncedPluginLoadable(dir, name string) error {
	kind, reason := pluginImportCheckFn(dir, name)
	switch kind {
	case "", "no-apply", "no-result":
		if kind == "no-result" {
			logWarn("account", "插件入口预检未能执行（跳过验收）：%s", name)
		}
		return nil
	case "resolve-error":
		if preflightDeferMissingDep(name, reason) {
			return nil
		}
		return fmt.Errorf("入口解析失败：%s", reason)
	default:
		return fmt.Errorf("入口加载失败：%s", reason)
	}
}

// removePluginFromProfile 从 profile 卸载插件：快照 → pnpm remove → 摘除激活声明 →
// 重启校验 → 失败回退。
func removePluginFromProfile(dir, name string) error {
	killServer()
	hadNM := snapshotPluginProfile(dir)
	rollback := func(reason string) error {
		restorePluginProfileSnapshot(dir, hadNM)
		restartAndVerifyServer()
		return errors.New(reason)
	}
	if err := runProfileCmd(dir, pnpmCmd(), "remove", name); err != nil {
		return rollback(fmt.Sprintf("卸载 %s 失败：%v", name, err))
	}
	// pnpm remove 不感知 dsh.profile.bundles：残留激活声明会让服务启动报
	// 「cannot resolve profile bundle」硬失败（与插件批处理路径同口径）。
	if err := stripProfileBundleEntry(dir, name); err != nil {
		return rollback(fmt.Sprintf("摘除 %s 激活清单失败：%v", name, err))
	}
	_ = clearProfileDisabledRecord(dir, name)
	if !restartAndVerifyServer() {
		return rollback("卸载后服务启动失败，已回退")
	}
	clearLkgInDir(dir)
	cleanupPluginProfileSnapshot(dir)
	return nil
}

// account_sync_plugin.go：把同步来的插件变更落到本机（按 key 里的 profile 落到对应环境）。
//
// 复用既有插件链路的安全语义：停服 → 快照（package.json / pnpm-lock.yaml 复制 + node_modules 改名）
// → pnpm 操作 → 重启校验 → 失败回退；成功则提升 LKG 并清理快照。
// 与用户手动操作的区别：不经过「待应用」队列（用户点「重启生效」本就是一次显式确认）。
//
// 两个例外（见 profileNeedsServiceVerify）：
//   - desktop profile：其加载由官方桌面端自己负责，托盘无法驱动桌面端重启 → 只做文件变更；
//   - desktop 启动方式：托盘自带的 Web 服务本就不该在跑 → 同样不重启、不做启动校验。
package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
)

// webProfileDir 定位 web profile 目录。
func webProfileDir() (string, bool) {
	return pluginProfileDir(accountPluginProfile)
}

// pluginProfileDir 定位某个命名 profile 的目录（不存在返回 false）。
func pluginProfileDir(profile string) (string, bool) {
	for _, pf := range enumeratePluginProfiles() {
		if pf.label == profile {
			return pf.dir, true
		}
	}
	return "", false
}

// profileNeedsServiceVerify 该 profile 的插件变更是否需要「重启后台服务并做启动校验」。
//
//   - desktop 启动方式：托盘自带的 Web 服务不在运行（其引擎属于官方桌面端），无从校验；
//   - desktop profile：插件由官方桌面端加载，托盘无法重启它，变更在桌面端重启后生效；
//   - 其余（web profile + web 启动方式）：保持原有的停服/重启/校验/自愈语义。
func profileNeedsServiceVerify(profile string) bool {
	if launchTargetIsDesktop() {
		return false
	}
	return profile == accountPluginProfile
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

// applyPluginOp 应用一条插件变更到 key 指定的 profile（install/update → 安装到目标版本；remove → 卸载）。
func applyPluginOp(profile, name string, v pluginOpValue) error {
	dir, ok := pluginProfileDir(profile)
	if !ok {
		return fmt.Errorf("未找到 %s profile 目录，无法同步插件 %s", profile, name)
	}
	if v.Action == "remove" {
		return removePluginFromProfile(dir, profile, name)
	}
	spec, exact, skip := pluginApplySpec(dir, name, v)
	if skip {
		return nil // 版本维度已达标、记录 spec 又自相矛盾：本次无需改动（原因见 pluginApplySpec）
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
	if err := installPluginIntoProfile(dir, profile, name, pluginDepArg(name, spec)); err != nil {
		return err
	}
	if exact != "" {
		return verifyInstalledTargetVersion(dir, name, exact)
	}
	return nil
}

// pluginApplySpec 本次应用要安装的依赖 spec；exact 非空表示走了「按精确目标版本安装」；
// skip=true 表示本次不做任何安装动作（版本维度已达标，且记录的 spec 与它自己的 Version 矛盾）。
//
// 为什么不能直接用账号里的 spec：跨机的 spec 常是范围（如 ^1.7.35），而 `pnpm add <name>@<范围>`
// 在锁定文件里的版本已满足该范围时**不升级**（pnpm 11 实测：先装 1.7.35，再 add ^1.7.35 仍是
// 1.7.35，且连 package.json 的 spec 都不改写）。于是另一台机器升到 1.7.40 后，本机反复点
// 「重启生效」仍是 1.7.35，却报应用成功——30 秒后漂移重判又把它放回待生效，表现为「一直提示
// 1 项待同步」（2026-09-28 现场）。目标版本比本机已装的**新**时按精确版本安装才能真正追平；
// 目标更旧时不改判（本机更新的情况由补报路径上报账号，这里不能把新版本降级）。
//
// skip 的由来（2026-09-30 现场）：账号记录 spec=1.7.44 / version=1.7.45（老客户端把升级前的
// spec 写进了记录）。本机已是 1.7.45 时，版本维度无需变更，照搬 spec 反而 `pnpm add name@1.7.44`
// 把版本装回 1.7.44——记录自相矛盾时以 Version 为准，本次不动。
func pluginApplySpec(dir, name string, v pluginOpValue) (spec, exact string, skip bool) {
	spec = strings.TrimSpace(v.Spec)
	target := strings.TrimSpace(v.Version)
	if v.Source == "npm" && target != "" {
		cur := installedPluginVersion(dir, name)
		if !versionTargetSatisfied(cur, target) && compareVersions(target, cur) > 0 {
			return target, target, false
		}
		if versionTargetSatisfied(cur, target) && pluginRecordSpecStale(v) {
			logInfo("account", "插件 %s 的账号记录自相矛盾（spec=%s / version=%s），本机 %s 已满足目标版本：本次不改动",
				name, spec, target, cur)
			return "", "", true
		}
	}
	if spec == "" {
		spec = target
	}
	return spec, "", false
}

// verifyInstalledTargetVersion 精确安装后的校验：本机版本必须达到账号目标，否则如实报错。
// 宁可让这次「重启生效」以失败收场并给出原因，也不要静默成功、把同一项无限交回待生效。
func verifyInstalledTargetVersion(dir, name, target string) error {
	got := installedPluginVersion(dir, name)
	if versionTargetSatisfied(got, target) {
		return nil
	}
	return fmt.Errorf("插件 %s 安装后版本为 %s，未达到账号目标 %s", name, orDash(got), target)
}

// installPluginIntoProfile 安装依赖到 profile：快照 → pnpm add → 登记激活清单 → 入口预检 →
// 重启校验 → 失败回退。
//
// 必须同时登记 dsh.profile.bundles：harness 只加载激活清单里的插件，而 pnpm add 只写
// dependencies——只装不登记就会出现「关于页插件齐全、harness 会话设置→插件里找不到」
// （2026-09-21 现场问题；用户靠手动导入备份才恢复，导入路径正是会合并 bundles 的那条）。
//
// verify=false（desktop profile / desktop 启动方式）时不重启服务、不做启动校验，也不提升 LKG
// ——没有校验过就不能把当前状态当成新基线（见 profileNeedsServiceVerify）。
func installPluginIntoProfile(dir, profile, name, dep string) error {
	killServer() // 运行中的 node 占用文件，快照改名会失败（服务未运行时为 no-op）
	verify := profileNeedsServiceVerify(profile)
	// 台账判据：安装前该包是否已在——只有「从无到有」才算新的安装事件（版本更新不刷新安装
	// 时刻，与删除墓碑判定依赖的「重新安装」语义一致，见 plugin_install_times.go）。
	wasInstalled := installedPluginVersion(dir, name) != ""
	hadNM := snapshotPluginProfile(dir)
	rollback := func(reason string) error {
		restorePluginProfileSnapshot(dir, hadNM)
		if verify {
			restartAndVerifyServer()
		}
		return errors.New(reason)
	}
	if err := runProfileCmd(dir, pnpmCmd(), "add", dep); err != nil {
		return rollback(fmt.Sprintf("安装 %s 失败：%v", dep, err))
	}
	if err := activateSyncedPlugin(dir, name); err != nil {
		return rollback(fmt.Sprintf("%s %v，已回退", name, err))
	}
	if !verify {
		log.Printf("[sync] %s：跳过重启校验（该环境的加载由用户侧客户端负责），改动重启后生效", profile)
		cleanupPluginProfileSnapshot(dir)
		notePluginInstalled(profile, name, wasInstalled)
		return nil
	}
	if !restartAndVerifyServer() {
		return rollback(fmt.Sprintf("%s 与当前服务不兼容，已回退", dep))
	}
	promoteProfileLkg(dir)
	cleanupPluginProfileSnapshot(dir)
	notePluginInstalled(profile, name, wasInstalled)
	return nil
}

// notePluginInstalled 安装成功后的台账登记：只有「包此前不在」才算新的安装事件
// （版本更新不刷新已有记录——删除墓碑的「本机是否装得更晚」判定依赖这条语义）。
func notePluginInstalled(profile, name string, wasInstalled bool) {
	if wasInstalled {
		return
	}
	pluginInstallTimeNoteInstalled(profile, name, pluginInstallTimeNow())
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
// 重启校验 → 失败回退（verify=false 时只做文件变更，见 profileNeedsServiceVerify）。
func removePluginFromProfile(dir, profile, name string) error {
	killServer()
	verify := profileNeedsServiceVerify(profile)
	hadNM := snapshotPluginProfile(dir)
	rollback := func(reason string) error {
		restorePluginProfileSnapshot(dir, hadNM)
		if verify {
			restartAndVerifyServer()
		}
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
	if !verify {
		log.Printf("[sync] %s：跳过卸载后的启动校验（该环境的加载由用户侧客户端负责），重启后生效", profile)
		clearLkgInDir(dir) // 删除不会让启动变坏；旧基线与新状态不一致，直接清掉
		cleanupPluginProfileSnapshot(dir)
		pluginInstallTimeForget(profile, name)
		return nil
	}
	if !restartAndVerifyServer() {
		return rollback("卸载后服务启动失败，已回退")
	}
	clearLkgInDir(dir)
	cleanupPluginProfileSnapshot(dir)
	pluginInstallTimeForget(profile, name)
	return nil
}

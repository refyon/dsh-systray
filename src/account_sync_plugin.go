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
	return installPluginIntoProfile(dir, pluginDepArg(name, spec))
}

// installPluginIntoProfile 安装依赖到 profile：快照 → pnpm add → 重启校验 → 失败回退。
func installPluginIntoProfile(dir, dep string) error {
	killServer() // 运行中的 node 占用文件，快照改名会失败
	hadNM := snapshotPluginProfile(dir)
	if err := runProfileCmd(dir, pnpmCmd(), "add", dep); err != nil {
		restorePluginProfileSnapshot(dir, hadNM)
		restartAndVerifyServer()
		return fmt.Errorf("安装 %s 失败：%v", dep, err)
	}
	if !restartAndVerifyServer() {
		restorePluginProfileSnapshot(dir, hadNM)
		restartAndVerifyServer()
		return fmt.Errorf("%s 与当前服务不兼容，已回退", dep)
	}
	promoteProfileLkg(dir)
	cleanupPluginProfileSnapshot(dir)
	return nil
}

// removePluginFromProfile 从 profile 卸载插件：快照 → pnpm remove → 重启校验 → 失败回退。
func removePluginFromProfile(dir, name string) error {
	killServer()
	hadNM := snapshotPluginProfile(dir)
	if err := runProfileCmd(dir, pnpmCmd(), "remove", name); err != nil {
		restorePluginProfileSnapshot(dir, hadNM)
		restartAndVerifyServer()
		return fmt.Errorf("卸载 %s 失败：%v", name, err)
	}
	_ = clearProfileDisabledRecord(dir, name)
	if !restartAndVerifyServer() {
		restorePluginProfileSnapshot(dir, hadNM)
		restartAndVerifyServer()
		return errors.New("卸载后服务启动失败，已回退")
	}
	clearLkgInDir(dir)
	cleanupPluginProfileSnapshot(dir)
	return nil
}

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ==================== 重置 DeepSeek Harness ====================
// 常规页「重置 DeepSeek Harness」：物理删除用户通过 dsh add 安装的插件
// （排除插件导致的服务启动失败），并把 harness 回退到官方最后发布的稳定版本。
// 弹窗警告后执行（前端 confirmDialog），全程 splash 进度，失败自动回滚版本快照。

// removeInstalledPlugins 物理删除用户安装的插件（profiles 下各 profile）：
//  1. package.json：dependencies 与 dsh.profile.bundles 移除非 @deepseek-ai/* 的条目
//     （保留 harness 官方 bundle，插件注册信息一并清除，使 harness 不再加载它们）；
//  2. node_modules：删除其中所有非 @deepseek-ai 的包目录与 .pnpm 私有存储
//     （用户插件与其依赖闭包被物理移除；@deepseek-ai 官方包目录保留）。
//
// 返回被清理的 profile 目录数。调用前须已 killServer()（避免运行中占用文件）。
// 注意：本函数不可逆，符合「重置会丢失所有已安装插件」的警告语义。
func removeInstalledPlugins() (int, error) {
	home := dshHomeDir()
	if home == "" {
		return 0, fmt.Errorf("无法确定 harness 数据目录（DSH_HOME）")
	}
	profilesRoot := filepath.Join(home, "profiles")
	if _, err := os.Stat(profilesRoot); err != nil {
		return 0, nil // 从未安装任何 profile/插件
	}
	var dirs []string
	if entries, err := os.ReadDir(profilesRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(profilesRoot, e.Name(), "package.json")); err == nil {
				dirs = append(dirs, filepath.Join(profilesRoot, e.Name()))
			}
		}
	}
	// 兼容旧布局：profiles/package.json（profiles 本身即 profile 根）
	if _, err := os.Stat(filepath.Join(profilesRoot, "package.json")); err == nil {
		dirs = append(dirs, profilesRoot)
	}
	cleaned := 0
	for _, dir := range dirs {
		if err := cleanProfilePlugins(dir); err != nil {
			return cleaned, fmt.Errorf("清理 profile %s 失败：%w", dir, err)
		}
		cleaned++
	}
	log.Printf("reset: removed installed plugins from %d profile(s)", cleaned)
	return cleaned, nil
}

// cleanProfilePlugins 清理单个 profile 目录中的用户插件（package.json 注册 + node_modules 文件）。
func cleanProfilePlugins(dir string) error {
	// 1) package.json：去掉非官方插件注册
	pj := filepath.Join(dir, "package.json")
	if data, err := os.ReadFile(pj); err == nil {
		var root map[string]interface{}
		if json.Unmarshal(data, &root) == nil {
			changed := false
			if deps, ok := root["dependencies"].(map[string]interface{}); ok {
				for k := range deps {
					if !isOfficialHarnessPkg(k) {
						delete(deps, k)
						changed = true
					}
				}
			}
			if dsh, ok := root["dsh"].(map[string]interface{}); ok {
				if prof, ok := dsh["profile"].(map[string]interface{}); ok {
					if bundles, ok := prof["bundles"].([]interface{}); ok {
						var keep []interface{}
						for _, b := range bundles {
							if s, ok := b.(string); ok && isOfficialHarnessPkg(s) {
								keep = append(keep, b)
							} else {
								changed = true
							}
						}
						prof["bundles"] = keep
					}
				}
			}
			if changed {
				if out, err := json.MarshalIndent(root, "", "  "); err == nil {
					if err := os.WriteFile(pj, append(out, '\n'), 0o644); err != nil {
						return err
					}
				}
			}
		}
	}
	// 2) node_modules：删除非 @deepseek-ai 的顶层目录（用户插件/依赖闭包）与 .pnpm 私有存储
	nm := filepath.Join(dir, "node_modules")
	entries, err := os.ReadDir(nm)
	if err != nil {
		return nil // 无 node_modules（从未装或已清）
	}
	for _, e := range entries {
		name := e.Name()
		// scoped 目录（@scope）整体按 scope 判断：@deepseek-ai 保留，其它 scope 全删
		del := true
		if strings.HasPrefix(name, "@deepseek-ai") {
			del = false
		}
		if del {
			target := filepath.Join(nm, name)
			log.Printf("reset: removing plugin dir %s", target)
			if err := os.RemoveAll(target); err != nil {
				return fmt.Errorf("删除 %s 失败：%w", target, err)
			}
		}
	}
	return nil
}

// isOfficialHarnessPkg @deepseek-ai/* 为 harness 官方自带，重置时保留。
func isOfficialHarnessPkg(name string) bool {
	return strings.HasPrefix(name, "@deepseek-ai/")
}

// harnessGitTagForVersion 源码形态下把版本号还原为仓库 tag（优先 dsh-vX.Y.Z，兼容 dsh-X.Y.Z）。
func harnessGitTagForVersion(version string) (string, error) {
	candidates := []string{"dsh-v" + version, "dsh-" + version}
	for _, tag := range candidates {
		if out := runHarnessCmdCapture("git", "rev-parse", "--verify", "--quiet", tag+"^{commit}"); out != "" {
			return tag, nil
		}
	}
	return "", fmt.Errorf("harness 仓库中未找到版本 %s 对应的 tag（dsh-v%s / dsh-%s）", version, version, version)
}

// runHarnessReset 重置 DeepSeek Harness：停服务 → 删插件 → 回退官方最新稳定版 → 重启校验。
// 失败自动回退到重置前的可运行快照并弹窗报告。异步执行（按钮触发后 go 调用）。
func runHarnessReset() {
	splash := startSplash("正在重置 DeepSeek Harness…")
	defer splash.Close()

	// 0) 先停止服务（否则运行中的 node 占用文件，删除/快照会失败）
	splash.Update("正在停止后台服务…", 0.1)
	killServer()
	time.Sleep(1 * time.Second)

	// 1) 解析官方最后发布的稳定版本（不跟随预发布通道开关）
	splash.Update("正在查询官方最新稳定版本…", 0.2)
	latest, err := fetchLatestStableHarnessVersion()
	if err != nil {
		splash.Close()
		showMessageBox("重置失败：无法获取官方稳定版本。\n"+err.Error()+"\n\n请检查网络后重试。", appName)
		return
	}
	cur := installedHarnessVersion()
	log.Printf("reset: target stable version v%s (current %s)", latest, cur)

	// 2) 物理删除用户安装的插件（不可逆；弹窗已警告）
	splash.Update("正在清除已安装的插件…", 0.35)
	n, err := removeInstalledPlugins()
	if err != nil {
		// 插件清理失败：尝试恢复服务后报错
		restartAndVerifyServer()
		splash.Close()
		showMessageBox("重置失败："+err.Error(), appName)
		return
	}

	// 3) 版本回退（快照当前可运行版本，失败回滚）
	if cur == latest && isNpmHarnessReady() {
		// 已是官方最新稳定版：无需重装，仅重启生效（插件已清）
		log.Printf("reset: harness already at latest stable v%s", latest)
	} else {
		hadNM := snapshotHarness()
		var rerr error
		switch {
		case isNpmHarnessReady():
			// npm 预构建形态：重装稳定版（reconcile 依赖树避免新旧混装）
			splash.Update(fmt.Sprintf("正在安装官方稳定版本 v%s…", latest), 0.5)
			rerr = runHarnessCmd(pnpmCmd(), "add", "@deepseek-ai/dsh@"+latest, "--save-exact")
			if rerr == nil {
				splash.Update("正在安装依赖…", 0.6)
				rerr = runHarnessCmd(pnpmCmd(), "install")
			}
		case isSourceHarnessDir():
			// 源码形态：fetch tags 后切到稳定 tag
			if _, err := os.Stat(filepath.Join(harnessDir, ".git")); err != nil {
				rerr = fmt.Errorf("源码目录不是 git 仓库，无法自动回退版本，请手动处理")
				break
			}
			splash.Update("正在拉取官方稳定版本代码…", 0.5)
			if rerr = runHarnessCmd("git", "fetch", "--tags", "--force"); rerr == nil {
				var tag string
				if tag, rerr = harnessGitTagForVersion(latest); rerr == nil {
					splash.Update(fmt.Sprintf("正在切换代码到 %s…", tag), 0.55)
					rerr = runHarnessCmd("git", "reset", "--hard", tag)
				}
			}
			if rerr == nil {
				splash.Update("正在安装 harness 依赖…", 0.7)
				rerr = runHarnessCmd(pnpmCmd(), "install")
			}
			if rerr == nil {
				splash.Update("正在构建 harness 前端…", 0.8)
				rerr = runHarnessCmd(pnpmCmd(), "run", "build")
			}
		default:
			rerr = fmt.Errorf("无法识别 harness 安装形态，跳过版本回退（插件已清除）")
		}
		if rerr != nil {
			// 回滚到快照版本并尝试重启
			rollbackUpdate(splash, cur, hadNM, "重置回退失败")
			return
		}
		cleanupHarnessSnapshot()
	}

	// 4) 重启并健康校验（就绪 + 启动日志无加载报错）
	splash.Update("正在重启服务…", 0.9)
	if !restartAndVerifyServer() {
		splash.Close()
		showMessageBox("重置后服务未能正常启动，请查看日志：\n"+filepath.Join(logDir, "server.log"), appName)
		return
	}
	splash.Close()
	if cur == latest {
		showMessageBox(fmt.Sprintf("重置完成：已清除 %d 个 profile 中的全部已安装插件。\n当前已是官方最新稳定版本 v%s，服务已重启。", n, latest), appName)
	} else {
		showMessageBox(fmt.Sprintf("重置完成：已清除 %d 个 profile 中的全部已安装插件，\nDeepSeek Harness 已回退到官方最新稳定版本 v%s，服务已重启。", n, latest), appName)
	}
}

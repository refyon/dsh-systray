//go:build windows

package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestShotGitHubAuthDialog 站点/README 截图用：弹出真实的 GitHub 授权确认弹窗，
// 供 capture_github_dialog.ps1 截取后关闭。仅设置 DSH_TMP_SHOT_DIALOG=1 时运行。
func TestShotGitHubAuthDialog(t *testing.T) {
	if os.Getenv("DSH_TMP_SHOT_DIALOG") != "1" {
		t.Skip("shot dialog disabled")
	}
	appCtx = context.Background() // 复现真实调用环境（检查更新时 appCtx 已就绪）
	// 测试二进制不走 loadConfig，界面语言在这里按同一约定解析（脚本用 -Lang en 生成英文图）
	langPref = "zh"
	if v := os.Getenv("DSH_SYSTRAY_LANG"); v != "" {
		langPref = normalizeLang(v)
	}
	curLang = resolveLang(langPref)
	// 脱敏：插件名与来源仓库一律用虚构示例（与 plugin_update.go shotPlugins 的示例集一致），
	// 截图会进公开站点/README，不能暴露开发者真实的私有仓库标识。
	msg := ghAuthPromptMsg("prompt-assistant", "example/prompt-assistant")
	runModernDialog(appName, msg, []string{T("取消"), T(ghLoginLabel)}, 1)
	time.Sleep(500 * time.Millisecond)
}

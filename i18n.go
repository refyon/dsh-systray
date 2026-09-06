package main

import "fmt"

// ==================== 界面语言（zh / en） ====================
// langPref 用户偏好：config.json 的 language 字段，取值 auto | zh | en，缺省 auto（跟随系统）。
// curLang  解析后的生效语言：zh | en。托盘菜单 / 原生弹窗 / splash 文案据此渲染
// （设置窗口内文案由前端 i18n 渲染，见 frontend/dist/main.js 的 tr()/fmt() 与 lang:changed）。
var (
	langPref = "auto"
	curLang  = "zh"
)

// normalizeLang 规整语言偏好；非法值回退 auto。
func normalizeLang(s string) string {
	switch s {
	case "zh", "en":
		return s
	default:
		return "auto"
	}
}

// resolveLang 把偏好解析为生效语言：auto 走系统检测（各平台实现 detectSystemLang）。
func resolveLang(pref string) string {
	if normalizeLang(pref) == "auto" {
		return detectSystemLang()
	}
	return normalizeLang(pref)
}

// currentLang 当前生效语言（zh/en）。
func currentLang() string { return curLang }

// ==================== Go 侧文案翻译（zh 字面量 → en 字典；命中失败回退原 zh） ====================
// 覆盖托盘菜单 / 服务状态 / 更新询问等高频原生 UI；其余文案随需求 3c 持续推进逐步补键。
var i18nEnMap = map[string]string{
	"服务启动失败":     "Service failed to start",
	"服务已停止":      "Service stopped",
	"服务启动中…":     "Service starting…",
	"后台服务状态":     "Background service status",
	"打开 Web UI":  "Open Web UI",
	"打开网页端界面":    "Open the web interface",
	"设置":         "Settings",
	"打开设置窗口":     "Open settings window",
	"退出":         "Quit",
	"退出并关闭后台服务器": "Quit and stop the background server",
}

// T 按当前生效语言翻译 zh 文案；无映射时回退原文（zh）。
func T(s string) string {
	if curLang != "en" {
		return s
	}
	if e, ok := i18nEnMap[s]; ok {
		return e
	}
	return s
}

// TF 同 T，但支持 fmt.Sprintf 风格占位（先取译文模板再格式化）。
func TF(tpl string, a ...interface{}) string {
	return fmt.Sprintf(T(tpl), a...)
}

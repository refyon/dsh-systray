package main

// ==================== 界面语言（zh / en） ====================
// langPref 用户偏好：config.json 的 language 字段，取值 auto | zh | en，缺省 auto（跟随系统）。
// curLang  解析后的生效语言：zh | en。托盘菜单 / 原生弹窗 / splash 文案据此渲染
// （设置窗口内文案由前端 i18n 渲染，见 frontend/dist/main.js 的 t() 与 lang:changed）。
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

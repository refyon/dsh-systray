package main

import "fmt"

// ==================== 界面语言（zh / en） ====================
// langPref 用户偏好：config.json 的 language 字段，取值 auto | zh | en。
//
//	config 未写该字段（旧版本升级 / 全新安装）时缺省 zh（简体中文），
//	仅在用户显式选择 auto（跟随系统）或 en 时才按对应语义生效。
//
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
	"服务启动失败":                      "Service failed to start",
	"服务已停止":                       "Service stopped",
	"服务启动中…":                      "Service starting…",
	"后台服务状态":                      "Background service status",
	"打开 Web UI":                   "Open Web UI",
	"打开网页端界面":                     "Open the web interface",
	"设置":                          "Settings",
	"打开设置窗口":                      "Open settings window",
	"退出":                          "Quit",
	"退出并关闭后台服务器":                  "Quit and stop the background server",
	"打开":                          "Open",
	"立即更新":                        "Update now",
	"稍后":                          "Later",
	"更新 Harness":                  "Update Harness",
	"保留后台服务":                      "Keep running",
	"重新启动":                        "Restart",
	"是否取消更新？":                     "Cancel the update?",
	"取消更新":                        "Cancel update",
	"继续更新":                        "Continue update",
	"所有历史会话":                      "All sessions",
	"已安装的插件":                      "Installed plugins",
	"文件目录":                        "File folders",
	"已安装的插件（%d 个）":                "Installed plugins (%d)",
	"正在更新插件 %s…":                  "Updating plugin %s…",
	"正在删除插件 %s…":                  "Removing plugin %s…",
	"正在更新本地插件 %s…":                "Updating local plugin %s…",
	"正在备份当前版本…":                   "Backing up current version…",
	"正在备份当前状态…":                   "Backing up current state…",
	"正在重启服务…":                     "Restarting service…",
	"正在尝试重新启用插件…":                 "Trying to re-enable plugin…",
	"更新失败，正在回退插件版本…":              "Update failed — rolling back plugin version…",
	"删除失败，正在回退…":                  "Remove failed — rolling back…",
	"正在重置 DeepSeek Harness…":      "Resetting DeepSeek Harness…",
	"DeepSeek Harness 已重置：\n":     "DeepSeek Harness has been reset:\n",
	"正在更新 DeepSeek Harness…":      "Updating DeepSeek Harness…",
	"正在更新 DeepSeek Harness 依赖…":   "Updating DeepSeek Harness dependencies…",
	"正在安装依赖…":                     "Installing dependencies…",
	"正在拉取 DeepSeek Harness 最新代码…": "Pulling latest DeepSeek Harness code…",
	"正在安装 harness 依赖…":            "Installing harness dependencies…",
	"正在构建 harness 前端…":            "Building harness frontend…",
	"正在回退代码…":                     "Rolling back code…",
	"启动校验失败，正在排查不兼容插件…":           "Boot check failed — inspecting incompatible plugins…",
	"正在停止后台服务…":                   "Stopping background service…",
	"正在启动后台服务…":                   "Starting background service…",
	"启动未通过健康校验，正在修复插件依赖并重试…":               "Boot failed health check — repairing plugin dependencies and retrying…",
	"正在检查解压工具…":                            "Checking archive tool…",
	"正在启动服务…":                              "Starting service…",
	"运行环境就绪":                               "Runtime ready",
	"正在解压 Node.js 运行时…":                    "Extracting Node.js runtime…",
	"正在下载 Node.js 运行时（来源：%s，%.0f%%）…":      "Downloading Node.js runtime (source: %s, %.0f%%)…",
	"正在安装 pnpm 包管理器（registry %d/%d：%s）…":   "Installing pnpm (registry %d/%d: %s)…",
	"正在解压安装…":                              "Extracting & installing…",
	"正在校验更新包…":                            "Verifying update package…",
	"正在更新程序…":                              "Updating the app…",
	"正在重启后台服务…":                            "Restarting background service…",
	"正在准备更新…":                              "Preparing update…",
	"启动失败，正在回退到上次正常状态…":                    "Startup failed — rolling back to the last working state…",
	"正在准备全新安装…":                            "Preparing a fresh install…",
	"正在清空原 harness 目录…":                    "Clearing the harness directory…",
	"正在清除会话记录…":                            "Clearing session history…",
	"正在清除已安装的插件…":                          "Removing installed plugins…",
	"正在准备运行环境…":                            "Preparing runtime environment…",
	"正在下载 Node.js / pnpm 运行时（首次约 1-3 分钟）…": "Downloading the Node.js / pnpm runtime (first run ~1-3 min)…",
	"正在安装 harness 依赖（首次约 2-5 分钟）…":         "Installing harness dependencies (first run ~2-5 min)…",
	"正在构建 harness 前端产物（首次约 1-3 分钟）…":       "Building harness frontend assets (first run ~1-3 min)…",
	"正在安装 DeepSeek Harness（首次约 2-5 分钟）…":   "Installing DeepSeek Harness (first run ~2-5 min)…",
	"正在查询最新版本…":                            "Querying the latest version…",
	"正在重试启动后台服务…":                          "Retrying to start the background service…",
	"更新失败，正在回退到上一可用版本…":                    "Update failed — rolling back to the last working version…",
	"正在下载 %s…":                             "Downloading %s…",
	"历史会话记录":                               "Session history",
	"已安装插件":                                "Installed plugins",
	"自选文件目录":                               "File folders",
	"未找到该插件，可能已被移除。":                       "Plugin not found — it may have been removed.",
	"检测到缺少运行依赖，但无法写入安装脚本。":                 "Missing runtime dependencies, but the installer script could not be written.",
	"未找到适用于当前系统的更新包。":                      "No update package found for this system.",
	"当前为开发版本（dev），未启用自动更新。":                "This is a development build (dev) — automatic updates are disabled.",
	"DeepSeek Harness 已在运行中，请使用系统托盘图标操作。":  "DeepSeek Harness is already running — use the tray icon to manage it.",
	"是否停止后台 Web 服务？确定将停止服务并退出；保留后台服务仅关闭托盘，服务继续运行。":           "Stop the background web service? OK stops the service and quits; keeping it running only closes the tray while the service continues.",
	"发现新版本 %s（当前版本 %s）。\n是否立即下载并更新？":                         "New version %s available (current %s).\nDownload and update now?",
	"DeepSeek Harness 有新版本 %s（当前 %s）。\n是否先更新 Harness？":       "DeepSeek Harness %s is available (current %s).\nUpdate Harness first?",
	"是否停止后台 Web 服务？\n\n「确定」将停止服务并退出；\n「保留后台服务」仅关闭托盘，服务继续运行。": "Stop the background web service?\n\n“OK” stops the service and quits;\n“Keep running” closes the tray only and keeps the service.",
	"是否重启后台 Web 服务？\n重启期间 Web UI 会短暂不可用。":                    "Restart the background web service?\nThe Web UI will be briefly unavailable during restart.",

	"取消": "Cancel",
	"DeepSeek Harness 服务已就绪。\n是否立即打开 Web UI？": "DeepSeek Harness service is ready.\nOpen the Web UI now?",
	"DeepSeek Harness 服务已就绪。是否立即打开 Web UI？":   "DeepSeek Harness service is ready. Open the Web UI now?",
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

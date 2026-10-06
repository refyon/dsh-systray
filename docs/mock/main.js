/* dsh-systray · Wails 前端逻辑（原生 JS，无构建步骤） */
"use strict";

// ---------- Wails 运行时 ----------
const { EventsOn } = window.runtime;

/** 绑定方法（wails build 自动生成，位于 frontend/wailsjs/） */
let GoApp = null;
function bindings() {
  if (GoApp) return GoApp;
  // wails build 生成的绑定：window.go.main.App
  if (window.go && window.go.main && window.go.main.App) {
    GoApp = window.go.main.App;
    return GoApp;
  }
  return null;
}

const $ = (id) => document.getElementById(id);

const state = {
  page: "general",
  cfg: null,
  svc: null,
  logName: "dsh-systray.log", // 统一日志：所有行为与子进程输出合并到单文件（路径见日志页 log-path）
  logOffset: 0,
  logArchiveLoaded: false, // 轮转归档（.3/.2/.1）是否已加载：首次与轮转重置后重载，保证完整历史可见
  logTimer: null,
  expDirs: [],          // 已选打包目录
  expSelected: { sessions: true, plugins: false, files: false },
  impItems: [],
  impDone: {},          // kind → true：该项已恢复完成（显示 ✓ 徽标）
  imp: {},              // kind → {busy,text,pct,pending,watch}：逐项恢复运行态
  impHealAll: false,    // 共享自愈进行中：所有「恢复」按钮暂时禁用（不可打断）
  plugRows: [],         // 全量插件（Go PluginRow），供过滤渲染与事件委托按索引取用
  plugFilter: "",       // 插件过滤关键字（输入防抖后）
  plugState: {},        // name → {note,noteTone,upLatest,upShow}：滚动/过滤重渲染后恢复行内状态
  plugPending: [],      // [{id,name,op}]：已登记待应用的插件变更（需重启服务才生效）
  plugTimer: null,      // 过滤防抖计时器
  updateProgress: null, // {text, pct} 更新进度
  splashMode: "startup", // startup | update
  shotPage: "",         // 截图模式当前页（GetShotPage 返回；空=正常模式）
  shotScroll: "",       // 截图模式内容区滚动量（bottom/像素/空）
  launchResolved: "",   // 当前生效的启动方式（web|desktop）：切换后插件清单要按新环境重拉
  filesEntries: [],     // 文件同步条目快照（Go FileSyncStatusInfo.entries）：行内操作按 id 取显示名
};

// ==================== 界面语言（en 字典；默认 DOM 为中文，zh↔en 就地双向切换） ====================
const I18N_EN = {
  splashStatus: "Preparing runtime environment…",
  splashCancel: "Cancel update",
  navGeneral: "General", navAbout: "About", navLogs: "Logs", navExport: "Export", navImport: "Import", navHelp: "Help",
  navSync: "Data sync",
  syncLoginTitle: "Sign in",
  syncLoginSub: "Signing in with an email code keeps your start-at-login switch, Harness version and online plugins in sync across machines; the first sign-in with an email registers it. Machine-specific settings (directories, ports) and local plugins are never uploaded.",
  syncEmailTitle: "Email",
  syncEmailSub: "The code is sent to this address and stays valid for 10 minutes",
  syncEmailPh: "you@example.com",
  syncCodeTitle: "Verification code",
  syncCodeSub: "Click “Send code”, then enter the 6 digits you received",
  btnSyncSend: "Send code",
  btnSyncLogin: "Sign in",
  syncScopeTitle: "Service sync",
  syncScopeSub: "Start-at-login, the selected Harness version (including the prerelease channel) and every online plugin (one record per environment — Web and Desktop are kept separately). Directories, ports and local plugins are not synced.",
  btnSyncNow: "Sync now",
  btnSyncLogout: "Sign out",
  syncRestartTitle: "Synced changes are waiting to take effect",
  syncRestartSub: "To avoid interrupting your work the changes are saved but not applied yet. Click the button to merge and apply them.",
  btnSyncApply: "Restart to apply",
  filesTitle: "File sync",
  filesSub: "Sync the files and folders you pick with your account",
  btnFilesAddFile: "Add file",
  btnFilesAddDir: "Add folder",
  btnFilesSync: "Sync now",
  filesPendingTitle: "Some remote changes could not be applied",
  btnFilesApply: "Retry",
  filesLoggedOutHint: "Sign in to sync files — what you sync and how much room is left will show up here.",
  filesSortLabel: "Sort",
  filesSortName: "Name",
  filesSortSize: "Size",
  filesSortTime: "Modified",
  filesEmpty: "No files or folders synced yet — use “Add file” or “Add folder” above to start.",
  stAutoTitle: "Start at login", stAutoSub: "Start the background service and keep it in the tray after login",
  stAutoSubDesktop: "Start the tray at login and keep it running; the Desktop UI does not start the background service",
  stLangTitle: "Interface language", stLangSub: "Tray menu and native dialogs switch with it; “Follow system” picks the OS language",
  langAuto: "Follow system (auto)",
  svcText: "Background service: starting…",
  btnRestart: "Restart service", btnOpenWeb: "Open Web UI",
  stPortTitle: "Service port", stHarnessTitle: "Harness directory",
  btnChoose: "Choose…",
  stResetTitle: "Reset DeepSeek Harness",
  stResetSub: "Stops the service and reinstalls Harness fresh from the selected official version",
  btnReset: "Reset service",
  abAppVerTitle: "dsh-systray version", abAppVerSub: "Desktop tray app",
  abHarnessVerTitle: "DeepSeek Harness version", abHarnessVerSub: "Background service engine",
  abPreTitle: "Enable prerelease channel", abPreSub: "alpha / beta / rc builds",
  btnCheckUpdate: "Check for updates",
  btnUpdateApp: "Update dsh-systray", btnUpdateHarness: "Update Harness",
  abPluginsTitle: "Installed plugins",
  plugFilterPh: "Filter plugins (name / source / version)…",
  plugEmpty: "No plugins installed",
  btnDiscardAllPending: "Undo all", btnApplyPending: "Apply now",
  btnRefresh: "Refresh", btnClear: "Clear",
  logPathCopyHint: "Click to copy the log file path",
  btnAddDir: "Add folders…", btnExport: "Export…",
  expHintDefault: "0 items selected",
  btnOpenDir: "Open export folder",
  impTitle: "Import dsh-systray export bundle",
  impSub: "Pick a dsh-systray-export-*.zip to restore sessions, installed plugins or file folders.",
  impDesktopNote: "The default launch method is the official Desktop app: restoring only writes files (no background service is started for boot verification), so plugin changes take effect after the Desktop app restarts.",
  btnAddZip: "Add archive…",
  btnCancelRestore: "Cancel restore",
  impBusyHint: "Restoring an import item — you can't add another archive until it finishes or is canceled.",
  dlgConfirm: "Confirm", btnCancel: "Cancel", btnSkip: "Skip", btnOk: "OK",
  expModalTitle: "Exporting", expModalText: "Preparing export…", btnDone: "Done",
  rstTitle: "Reset DeepSeek Harness",
  rstMsg: "Stops the background service and clears the harness directory, then installs the selected version fresh. Checked data is erased and cannot be recovered.",
  rstTargetLabel: "Reset target version", rstLoading: "Querying available versions…",
  rstOptHarness: "Harness service <em>(required)</em>",
  rstOptSessions: "Sessions", rstOptPlugins: "Installed plugins",
  rstSessionsSub: "Will clear 0 sessions", rstPluginsSub: "Will clear 0 plugins",
  btnStartReset: "Start reset",
  helpWebAuthTitle: "Web UI asks for authentication",
  // 帮助页 · desktop 模式专用：桌面端异常时改用 Web UI
  helpFallbackTitle: "When the Desktop UI doesn't work",
  helpFallbackSymp: "If the official Desktop app won't open or can't run an AI session (engine errors, incompatible plugins, a frozen window…), you can switch to the tray-managed Web UI and keep working — both are interfaces on the same machine, and saved sessions and settings are not lost by switching.",
  helpFallbackStep1: "Click “Switch to Web UI” below: the tray starts the background service and points the tray's “Open” at the web interface (the first start may download the runtime and take several minutes).",
  helpFallbackStep2: "Once the service is ready, click “Open Web UI” in the tray menu, or come back to this page and click it here (this page shows that button after switching).",
  helpFallbackStep3: "To go back to the Desktop app later, switch “Default launch method” back to Desktop UI on the General page (the background service stops again).",
  btnSwitchToWeb: "Switch to Web UI",
  helpWebAuthSymp: "Opening the harness Web UI in the browser shows: dsh web authentication required; reopen the URL printed by dsh web.",
  helpWebAuthWhy: "Why: the background service issues a new access token each time it starts (by design), so addresses kept in bookmarks or history stop working; and the browser has no valid credential yet (first visit, cleared cookies, or older than 30 days).",
  helpWebAuthStep1: "Click “Open Web UI” below — it always uses the latest access link.",
  helpWebAuthStep2: "Or click “Copy access link” and paste it into the browser address bar.",
  helpWebAuthStep3: "Open the link exactly as given (the address is 127.0.0.1) — the credential is bound to that address, so don't rewrite it as localhost.",
  helpWebAuthStep4: "If it still fails, click “Restart service” to generate a new access link, then try again.",
  btnCopyLink: "Copy access link",
  helpStopped: "The background service isn't running: start it to open the Web UI or copy the access link.",
  helpNoToken: "No access link with a token was found: the service wasn't started by this app (it was kept running after the last exit, or survived a reboot), and log rotation can drop that line. If this browser signed in before, just click “Open Web UI”; for another browser or a private window, click “Restart service” first to generate a new link.",
  helpRestarting: "Restarting the background service… “Copy access link” becomes available once it's ready (the current Web UI session drops briefly).",
  // ---- 启动方式（Web UI / Desktop UI）----
  stLaunchTitle: "Default launch method",
  stLaunchSub: "Which interface the tray opens by default; it also decides which Harness engine the version, update and reset actions below apply to",
  launchAuto: "Auto-detect (prefer Desktop)",
  launchWeb: "Web UI (tray-managed service)",
  launchDesktop: "Desktop UI (official Desktop app)",
  dtTitle: "Official Desktop app",
  dtPathTitle: "Install location",
  btnOpenDesktop: "Open Desktop UI",
  btnInstallDesktop: "Install Desktop app",
  syncScopeDesktopNote: "The default launch method is the official Desktop app: the synced Harness version and prerelease channel only affect the tray-managed Web service, not the Desktop app (whose update channel is fixed to Nightly).",
};
const PAGE_I18N_KEY = { general: "navGeneral", sync: "navSync", about: "navAbout", logs: "navLogs", export: "navExport", import: "navImport", help: "navHelp" };

function curLangCode() {
  return (state.cfg && state.cfg.curLang === "en") ? "en" : "zh";
}

// ============ 动态文案：zh 字面量 → en；tr() 直译，fmt() 支持 {0}{1} 插值。zh 无命中回退原样 ============
const I18N_DYN = {
  "正在准备运行环境…": "Preparing runtime environment…",
  "正在准备更新…": "Preparing update…",
  "正在更新…": "Updating…",
  "正在取消…": "Cancelling…",
  "正在准备导出…": "Preparing export…",
  "正在检查更新…": "Checking for updates…",
  "检查失败：{0}": "Check failed: {0}",
  "有新版本 {0}": "New version {0} available",
  "已是最新（当前 {0}）": "Up to date (current {0})",
  "已是最新版本（{0}）": "Already on the latest version ({0})",
  "后台服务：运行中": "Background service: running",
  "后台服务：启动中": "Background service: starting",
  "后台服务：已停止": "Background service: stopped",
  "后台服务：启动失败": "Background service: failed to start",
  "请查看日志": "See logs",
  "已复制": "Copied",
  "已复制访问链接": "Access link copied",
  "复制失败": "Copy failed",
  "重启失败，请查看日志": "Restart failed — see logs",
  "正在重启后台服务…": "Restarting the background service…",
  "端口已修改为 {0}，当前服务仍运行于 {1}——重启后台服务后生效。": "Port changed to {0}, but the service still runs on {1} — effective after restarting the service.",
  "服务端口 {0} 被系统保留（Windows 排除端口段），后台服务无法启动。": "Service port {0} is reserved by Windows (excluded port range), so the background service cannot start.",
  "服务端口 {0} 已被其它程序占用，后台服务无法启动。": "Service port {0} is already used by another program, so the background service cannot start.",
  "改用端口 {0} 并启动": "Use port {0} and start",
  "请手动指定其它端口后重启服务": "Pick another port below, then restart the service",
  "正在改用新端口并启动后台服务…": "Switching port and starting the background service…",
  "请到「常规 → 服务端口」改用其它端口后重启服务。": "Go to General → Service port, switch ports, then restart the service.",
  "注意：所选为预发布版本，可能与已安装插件不兼容；若重置后服务无法启动，请查看日志。": "Note: the selected build is a prerelease and may be incompatible with installed plugins; if the service fails to start after reset, check the logs.",
  "已关闭预发布通道，检查更新将仅显示稳定版本": "Prerelease channel off — update checks now only show stable releases",
  "已开启预发布通道，可重新检查更新（含预发布版）": "Prerelease channel on — re-check for updates to see prereleases",
  "导出失败：{0}": "Export failed: {0}",
  "导出完成：{0}": "Export finished: {0}",
  "导出完成 ✓": "Export finished ✓",
  "已取消恢复{0}": "Restore cancelled{0}",
  "，已回退到恢复前状态": " — rolled back to the pre-restore state",
  "恢复失败：{0}": "Restore failed: {0}",
  "恢复完成 ✓": "Restore finished ✓",
  "将清除 {0} 条会话记录": "Will clear {0} sessions",
  "将清除 {0} 个已安装插件": "Will clear {0} plugins",
  "正在查询可用版本…": "Querying available versions…",
  "正在更新插件…": "Updating plugin…",
  "已加入批量队列，等待执行…": "Queued — waiting for the batch to run…",
  "已登记为待应用变更（{0}）": "Registered as a pending change ({0})",
  "撤销": "Undo",
  "登记变更": "Register change",
  "登记插件更新？": "Register plugin update?",
  "登记更新并启用？": "Register update and enable?",
  "登记插件删除？": "Register plugin removal?",
  "立即应用": "Apply now",
  "全部撤销": "Undo all",
  "撤销全部待应用变更？": "Undo all pending changes?",
  "将撤销 {0} 项尚未生效的变更（更新/删除/启用），已安装的插件不受影响。确认撤销吗？":
    "This undoes {0} pending change(s) (updates/removals/enables). Installed plugins are not affected. Undo them all?",
  "有 {0} 项变更尚未生效（{1}）": "{0} change(s) not applied yet ({1})",
  "{0} 项更新": "{0} update(s)",
  "{0} 项删除": "{0} removal(s)",
  // 插件删除的会话数据风险（该插件写入的自定义事件会让历史会话在删除后打不开）
  "修复会话": "Fix sessions",
  "删除后 {0} 个历史会话将无法打开（该插件写入了 {1} 条自定义记录），可点「修复会话」后删除":
    "Deleting it makes {0} past session(s) unopenable (this plugin wrote {1} custom record(s)) — use \"Fix sessions\" first",
  "（{0} 项删除会导致历史会话无法打开，可在行内先「修复会话」）":
    " ({0} removal(s) will make past sessions unopenable — fix them inline first)",
  "正在修复历史会话记录…": "Fixing past session records…",
  "修复失败：{0}": "Fix failed: {0}",
  "未知原因": "unknown reason",
  "已修复 {0} 条记录；该插件仍在写入，还有 {1} 个会话存在风险":
    "Fixed {0} record(s); the plugin is still writing — {1} session(s) remain at risk",
  "已修复 {0} 个会话（{1} 条记录），删除后不再影响历史会话":
    "Fixed {0} session(s) ({1} record(s)) — removal no longer affects past sessions",
  "{0} 项启用": "{0} enable(s)",
  "已登记：更新到 {0}（{1}）": "Registered: update to {0} ({1})",
  "已登记：删除该插件（{0}）": "Registered: remove this plugin ({0})",
  "已登记：移除「待重指定」记录": "Registered: drop the pending-respec record",
  "已登记：启用该插件（{0}）": "Registered: enable this plugin ({0})",
  "（更新）": " (update)",
  "（删除）": " (remove)",
  "（启用）": " (enable)",
  "删除该插件": "remove this plugin",
  "更新到最新版本": "update to the latest version",
  "启用该插件": "enable this plugin",
  "重启服务后生效": "— takes effect after the service restarts",
  "桌面端重启后生效": "— takes effect after the Desktop app restarts",
  "有 {0} 项变更尚未生效：{1}{2}": "{0} change(s) not applied yet: {1}{2}",
  "等 {0} 项": " and {0} in total",
  "待应用：{0}（{1}）": "Pending: {0} ({1})",
  "正在删除插件…": "Removing plugin…",
  "无法更新：{0}": "Update failed: {0}",
  "更新失败：{0}": "Update failed: {0}",
  "取消恢复": "Cancel restore",
  "更新中不可取消": "Update in progress — cannot cancel",
  "确认操作": "Confirm",
  "确定": "OK",
  "更新并启用插件？": "Update and enable plugin?",
  "更新插件？": "Update plugin?",
  "开始更新": "Start update",
  "开始重置": "Start reset",
  "覆盖更新本地插件？": "Overwrite-update local plugin?",
  "将本地插件改为所选版本？": "Point local plugin to the selected version?",
  "覆盖更新": "Overwrite & update",
  "覆盖为所选版本": "Overwrite to selected version",
  "删除插件？": "Remove plugin?",
  "移除待重指定插件？": "Remove pending-respec plugin?",
  "移除": "Remove",
  "删除": "Delete",
  "检测到数据冲突": "Data conflicts detected",
  "覆盖并恢复": "Overwrite & restore",
  "跳过": "Skip",
  "已选择跳过 {0} 项冲突，现有内容将保留。": "Skipped {0} conflicts — existing content is kept.",
  "正在准备恢复…": "Preparing restore…",
  "插件列表加载失败：{0}": "Failed to load plugin list: {0}",
  "所有历史会话": "All sessions",
  "已安装的插件": "Installed plugins",
  "需要打包的文件目录": "Folders to include",
  "plugins.zip": "plugins.zip",
  "files.zip": "files.zip",
  "移除": "Remove",
  "已选 {0} 项{1}": "Selected {0} item(s){1}",
  "（含 {0} 个目录）": " (incl. {0} folder(s))",
  "请至少勾选一项，或为「文件目录」添加目录": "Select at least one item, or add folders under “Folders to include”",
  "无法恢复：{0}": "Cannot restore: {0}",
  "服务正在启动校验，不可取消…": "Service boot is being verified — cannot cancel yet…",
  "当前没有进行中的恢复任务": "No restore task in progress",
  "已请求取消，正在回退到恢复前状态…": "Cancel requested — rolling back to the pre-restore state…",
  "共 {0} 个可恢复项": "{0} restorable item(s)",
  "正在恢复导入项，恢复期间不能重新添加压缩包。": "Restoring an import item — you can't add another archive until it finishes or is canceled.",
  "上一项恢复仍在收尾，请稍候再试。": "The previous restore is still finishing up — please try again in a moment.",
  "仍在回退到恢复前状态，请稍候…": "Still rolling back to the pre-restore state — please wait…",
  "服务端仍在处理，请稍候…": "The service is still working on it — please wait…",
  "恢复": "Restore",
  "✓ 已完成": "✓ Restored",
  "正在启动服务并校验插件兼容性…": "Starting the service and verifying plugin compatibility…",
  "检查更新": "Check for updates",
  "更新": "Update",
  "更新…": "Update…",
  "启用": "Enable",
  "删除": "Delete",
  "已禁用": "Disabled",
  "已禁用（{0}）": "Disabled ({0})",
  "已被跳过": "Skipped",
  "harness 已跳过（与当前 dsh 版本不兼容）：{0}": "Skipped by harness (incompatible with the running dsh version): {0}",
  "与当前版本不兼容": "incompatible with current version",
  "未安装": "not installed",
  "当前版本 {0}": "Version {0}",
  " · 环境 {0}": " · profile {0}",
  // 插件环境隔离（当前启动方式）：清单与行内操作只针对当前环境；导出/导入按包内环境各回各家
  "仅显示 Web UI 环境的插件": "Showing only Web UI environment plugins",
  "仅显示 Desktop UI 环境的插件": "Showing only Desktop UI environment plugins",
  "Web UI 环境还没有安装插件": "No plugins installed in the Web UI environment",
  "Desktop UI 环境还没有安装插件": "No plugins installed in the Desktop UI environment",
  "plugins.zip · 全部环境（Web UI / Desktop UI）": "plugins.zip · all environments (Web UI / Desktop UI)",
  "插件按包内环境分别恢复到对应环境（Web UI / Desktop UI 各自独立）；包里没有的环境不受影响。":
    "Plugins go back to the environments recorded in the bundle (Web UI and Desktop UI stay separate); environments not in the bundle are untouched.",
  "本地路径：{0}": "Local path: {0}",
  "没有匹配“{0}”的插件": "No plugins match “{0}”",
  "本地": "Local",
  "压缩包": "Archive",
  "未知来源": "Unknown source",
  "开启预发布通道？": "Enable prerelease channel?",
  "预发布版可能不稳定，可能导致服务启动失败。确定开启吗？": "Prerelease builds may be unstable and could break the service. Enable now?",
  "确定开启": "Enable",
  "更新 dsh-systray？": "Update dsh-systray?",
  "将下载并安装新版本并自动重启。确认开始更新吗？": "A new version will be downloaded, installed and the app restarted. Start now?",
  "更新 DeepSeek Harness？": "Update DeepSeek Harness?",
  "更新期间服务会短暂重启，失败会自动回退。确认开始更新吗？": "The service restarts briefly and failures auto-rollback. Start now?",
  "更新官方桌面端？": "Update the official Desktop app?",
  "将下载官方安装包（校验后）并启动安装向导；桌面端正在运行时需先退出它。确认开始吗？": "The official installer is downloaded (and verified) and its wizard is started; the Desktop app must be closed first if it is running. Continue?",
  "尝试启用": "Try enabling",

  // 数据同步（账号）
  "未登录": "Not signed in",
  "登录已过期，请重新登录": "Your sign-in expired — please sign in again",
  "同步中": "Syncing",
  "同步中…": "Syncing…",
  "已同步": "Synced",
  "待同步 {0} 项": "{0} pending",
  "同步失败": "Sync failed",
  "已登录，尚未同步": "Signed in — not synced yet",
  "正在检查同步…": "Checking sync…",
  "已同步 · 最后同步 {0}": "Synced · last {0}",
  "已同步 · 最后同步 {0} · 远端更新 {1} 项于 {2}": "Synced · last {0} · {1} remote change(s) applied at {2}",
  "同步失败：{0}": "Sync failed: {0}",
  "请先填写邮箱": "Enter your email first",
  "请输入 6 位验证码": "Enter the 6-digit code",
  "验证码已发送，{0} 分钟内有效": "Code sent — valid for {0} minutes",
  "正在发送…": "Sending…",
  "登录中…": "Signing in…",
  "正在同步…": "Syncing…",
  "同步已完成": "Sync completed",
  "同步检查完成，本机已是最新": "Sync check complete — this machine is up to date",
  "同步检查完成，{0} 项改动等待重启生效": "Sync check complete — {0} change(s) are waiting to be applied",
  "待生效 {0} 项": "{0} waiting to apply",
  "待生效": "Waiting to apply",
  "有待生效的同步改动": "Changes waiting to apply",
  "，点「重启生效」应用": " — click “Restart to apply”",
  "上次应用失败：{0}": "Last apply failed: {0}",
  "上次应用失败：{0}。剩余改动已保留，可再次点击应用续做。": "Last apply failed: {0}. The remaining changes are kept — click apply again to continue.",
  "共 {0} 项改动已保存但尚未生效；点击右侧按钮后合并并生效。": "{0} change(s) saved but not applied yet; click the button to merge and apply them.",
  "正在应用…": "Applying…",
  "重启生效": "Restart to apply",
  "正在应用同步改动…": "Applying synced changes…",
  "取消应用": "Cancel apply",
  "已取消应用，剩余改动留待生效": "Apply canceled — the remaining changes stay pending",
  "同步改动已生效：应用 {0} 项": "Synced changes applied: {0} item(s)",
  "已退出登录": "Signed out",
  "{0} 秒后可重发": "Resend in {0}s",
  "同步功能尚未就绪": "Sync is not available in this build yet",
  // ---- 启动方式（Web UI / Desktop UI）联动文案，见 applyLaunchMode ----
  "打开 Web UI": "Open Web UI",
  "打开 Desktop UI": "Open Desktop UI",
  "更新 Harness": "Update Harness",
  "更新桌面端": "Update Desktop app",
  "官方桌面端内置引擎": "Bundled with the official Desktop app",
  "运行中": "Running",
  "未运行": "Not running",
  "更新通道 {0}": "Update channel {0}",
  "未检测到官方桌面端，Desktop UI 选项不可用。": "Official Desktop app not detected — the Desktop UI option is unavailable.",
  "当前生效：Desktop UI。后台服务已停止；与 Web 服务相关的设置已隐藏，切回 Web UI 会重新启动服务。": "Currently in effect: Desktop UI. The background service is stopped and Web-service settings are hidden — switching back to Web UI starts it again.",
  "当前生效：Web UI。": "Currently in effect: Web UI.",
  "登录后自动启动托盘并常驻；Desktop UI 不启动后台服务": "Start the tray at login and keep it running; the Desktop UI does not start the background service",
  "登录后自动启动后台服务并常驻托盘": "Start the background service and keep it in the tray after login",
  "仅影响托盘自带的 Web 服务；当前默认启动方式为 Desktop UI。": "Only affects the tray-managed Web service; the default launch method is Desktop UI.",
  // 启动方式切换确认（切换会启停后台服务）
  "切换启动方式": "Switch launch method",
  "确认切换": "Switch",
  "切换到 Desktop UI 会停止后台服务，正在进行的网页端会话会中断。确认切换？": "Switching to Desktop UI stops the background service; any running Web UI session will be interrupted. Switch anyway?",
  "切换到 Desktop UI 后，托盘「打开」将指向官方桌面端，后台服务保持停止。确认切换？": "After switching to Desktop UI the tray opens the official Desktop app and the background service stays stopped. Switch?",
  "切换到 Web UI 后，托盘「打开」将指向网页端界面；后台服务已在运行。确认切换？": "After switching to Web UI the tray opens the Web interface; the background service is already running. Switch?",
  "切换到 Web UI 会启动后台服务（首次可能需要下载运行环境，耗时数分钟）。确认切换？": "Switching to Web UI starts the background service (the first run may download the runtime and take several minutes). Switch?",
  // 帮助页 · desktop 模式专用：桌面端异常时一键切换到 Web UI
  "切换到 Web UI？": "Switch to Web UI?",
  "将启动托盘自带的 Web 服务（首次启动可能需要下载运行环境，耗时数分钟），并把托盘「打开」改为指向 Web UI。确认切换？": "This starts the tray-managed Web service (the first start may download the runtime and take several minutes) and points the tray's “Open” at the Web UI. Switch?",
  "切换": "Switch",
  "正在切换到 Web UI 并启动后台服务…": "Switching to Web UI and starting the background service…",
  // 补登此前遗漏的动态文案（英文界面原本会回退中文，check-frontend-i18n 一直告警）
  "已取消恢复": "Restore cancelled",
  "将撤销 {0} 项尚未生效的变更（更新/删除），已安装的插件不受影响。确认撤销吗？": "This will undo {0} pending change(s) (update/remove); installed plugins are unaffected. Continue?",

  // 文件/文件夹同步（文件卡）
  // 注：「已同步 / 同步失败 / 同步失败：{0} / 删除 / 移除 / 正在同步… / 正在应用… / 正在应用同步改动…」
  // 已在账号同步与插件文案里登记过，这里不重复定义（同名键会被后者覆盖，容易改错一处）。
  "待上传": "To upload",
  "待下载": "To download",
  "待同步": "Pending",
  "已在本机移除": "Removed locally",
  "重新同步": "Sync again",
  "选择打开方式": "Choose an app",
  "移动": "Move",
  "移动到本机其它位置": "Move to another location on this device",
  // 排序键标签：renderFilesCard 每次渲染都按 tr() 重写按钮文字，缺这三条动态译文时
  // 会把静态层已译好的 Name/Size/Modified 又写回中文（2026-10-06 英文截图里发现）。
  "名称": "Name",
  "大小": "Size",
  "修改时间": "Modified",
  "已移动同步位置；本机文件已搬到新位置": "Location updated — local files were moved to the new location.",
  "该条目同步的是文件夹，请选择文件夹": "This item syncs a folder — please pick a folder",
  "该条目同步的是单个文件，请选择文件": "This item syncs a single file — please pick a file",
  "接收目录本身不能作为同步位置": "The receive folder itself cannot be used as a sync location",
  "源设备原路径 {0} 在本机已不存在，内容已保存为接收目录副本": "The original path {0} no longer exists on this device — the content is kept as a copy under the receive folder.",
  "已重新纳入同步，稍后可从账号拉回": "Back in sync — it will be pulled from your account shortly",
  "展开或折叠": "Expand or collapse",
  "打开": "Open",
  "重命名": "Rename",
  "本机原位置": "Original location",
  "接收目录": "Receive folder",
  "{0} 个文件": "{0} file(s)",
  "已用 {0} / 共 {1} · 剩余 {2}": "Using {0} of {1} · {2} left",
  "正在读取容量…": "Reading capacity…",
  "共 {0} 项：{1}{2}": "{0} item(s): {1}{2}",
  "可用容量不足": "Not enough space",
  "有 {0} 个文件因容量不足未同步。请删除部分已同步文件或移除条目后重试；已同步的内容不受影响。": "{0} file(s) could not be synced because the account is out of space. Delete some synced files or remove an entry and try again; already-synced content is unaffected.",
  "知道了": "Got it",
  "正在读取所选内容…": "Reading the selection…",
  "正在上传 {0}/{1}{2}": "Uploading {0}/{1}{2}",
  "{0}：{1}": "{0}: {1}",
  "已加入同步，正在上传…": "Added to sync — uploading…",
  "同步完成": "Sync finished",
  "改动已应用": "Changes applied",
  "名称不能为空": "Name cannot be empty",
  "移除同步条目？": "Remove this synced item?",
  "移除该文件夹的同步内容？": "Remove this folder from sync?",
  "移除该文件的同步内容？": "Remove this file from sync?",
  "「{0}」将从账号同步中移除（其它设备上的同一条目也会移除），本机文件保持不动。": "“{0}” will be removed from account sync (the same entry is removed on your other devices); local files stay untouched.",
  "「{0}」会从账号同步中删除（其它设备上的副本也会删除），本机文件保持不动。": "“{0}” will be deleted from account sync (copies on your other devices are deleted too); local files stay untouched.",
};
function tr(s) { return (curLangCode() === "en" && I18N_DYN[s]) || s; }
function fmt(s) {
  let t = tr(s);
  for (let i = 1; i < arguments.length; i++) t = t.split("{" + (i - 1) + "}").join(String(arguments[i]));
  return t;
}

// 静态层：zh↔en 就地双向切换。index.html 默认 DOM 为中文文案，首次应用语言前对其快照
// （ZH_SNAP）；en 时以 I18N_EN 覆盖，切回 zh 时从快照还原。动态 JS 文案（弹层/提示/行模板）
// 由 rerenderDynamicText 重渲染块级内容。语言切换不再整页 reload——reload 后 init 无条件
// 显示 splash 而设置视图仅由 Go 事件驱动出现，会永久卡在 splash（0.8.0 切换中文反馈的根因）。
let ZH_SNAP = null;
function snapshotStaticZh() {
  if (ZH_SNAP) return;
  ZH_SNAP = {};
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    ZH_SNAP[el.getAttribute("data-i18n")] = el.innerHTML;
  });
  document.querySelectorAll("[data-i18n-ph]").forEach((el) => {
    ZH_SNAP["ph:" + el.getAttribute("data-i18n-ph")] = el.getAttribute("placeholder") || "";
  });
  document.querySelectorAll("[data-i18n-title]").forEach((el) => {
    ZH_SNAP["title:" + el.getAttribute("data-i18n-title")] = el.getAttribute("title") || "";
  });
}
function applyStaticI18n() {
  const en = curLangCode() === "en";
  document.documentElement.lang = en ? "en" : "zh-CN";
  document.title = en ? "dsh-systray · Settings" : "dsh-systray · 设置";
  if (!ZH_SNAP) snapshotStaticZh(); // 防御：首次调用必然发生在未被覆盖的原始 zh DOM 上，快照安全
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    const k = el.getAttribute("data-i18n");
    const txt = en ? I18N_EN[k] : ZH_SNAP[k];
    if (txt !== undefined) el.innerHTML = txt;
  });
  document.querySelectorAll("[data-i18n-ph]").forEach((el) => {
    const k = el.getAttribute("data-i18n-ph");
    const v = en ? I18N_EN[k] : ZH_SNAP["ph:" + k];
    if (v !== undefined) el.setAttribute("placeholder", v);
  });
  document.querySelectorAll("[data-i18n-title]").forEach((el) => {
    const k = el.getAttribute("data-i18n-title");
    const v = en ? I18N_EN[k] : ZH_SNAP["title:" + k];
    if (v !== undefined) el.setAttribute("title", v);
  });
  rerenderDynamicText(); // 服务状态/插件/导出/导入等动态区块按当前语言重渲染（en 与 zh 恢复都执行）
}

// 动态区块统一重渲染（EN 生效后调用；各函数内部以 curLangCode() 决定语言）
function rerenderDynamicText() {
  // applyLaunchMode 排在其后：它写入的是动态文案（「打开 Desktop UI」「更新桌面端」等），
  // 必须在静态层用 ZH_SNAP/I18N_EN 覆盖过 DOM 之后再按当前启动方式重刷一次。
  [refreshService, renderPlugins, renderExportRows, renderImportRows, refreshSync,
    // 文件卡（条目名/状态徽标/容量行/按钮）也是动态文案，且首屏渲染早于语言就绪——
    // 不在这里重刷，英文界面会留下中文行（2026-10-06 截图里发现）。
    () => refreshFiles(true),
    () => applyLaunchMode(state.cfg)].forEach((fn) => {
    if (typeof fn === "function") { try { fn(); } catch (e) { console.error("rerenderDynamicText", fn && fn.name, e); } }
  });
}

// ==================== 页面路由 ====================

const PAGE_TITLES = { general: "常规", sync: "数据同步", about: "关于", logs: "日志", export: "导出", import: "导入", help: "帮助" };

// 页标题按当前语言重刷：showPage 与「语言生效后」两条路径都要调——启动时 showPage(shotPage) 早于
// GetConfig，此刻 curLangCode() 还是默认 zh，若只在这里设一次，英文配置的窗口会一直显示中文标题，
// 直到用户点一下左侧导航（2026-09-18 英文截图实拍暴露）。
function refreshPageTitle() {
  const name = state.page || "general";
  const key = PAGE_I18N_KEY[name];
  $("page-title").textContent = (curLangCode() === "en" && key && I18N_EN[key])
    ? I18N_EN[key] : PAGE_TITLES[name];
}

function showPage(name) {
  state.page = name;
  document.querySelectorAll(".nav-item").forEach((b) => b.classList.toggle("active", b.dataset.page === name));
  refreshPageTitle();
  document.querySelectorAll(".page").forEach((p) => p.classList.add("hidden"));
  $("page-" + name).classList.remove("hidden");
  if (name === "logs") startLogPolling();
  else stopLogPolling();
  // 导入页：恢复进行中时「添加压缩包…」保持禁用（窗口隐藏期间可能已有恢复在跑）
  if (name === "import") syncImportPickBtn();
  // 关于页每次进入刷新版本号与插件清单（更新/导入等操作后保持最新）
  if (name === "about") {
    refreshVersions();
    loadPlugins();
  }
  // 帮助页：进入即刷新服务状态（按钮可用性与警告提示按运行态/令牌可用性渲染）
  if (name === "help") refreshService();
  // 数据同步页：进入即拉取账号状态与同步进度
  if (name === "sync") { refreshSync(); refreshFiles(); }
}

/** 截图模式：把内容区滚动到 DSH_SYSTRAY_SHOT_SCROLL 指定位置（bottom=最底；数字=像素）。
 *  用于同一页面按不同滚动位置各截一张（如关于页上/下区域）。 */
function applyShotScroll() {
  if (!state.shotScroll) return;
  const c = document.querySelector(".content");
  if (!c) return;
  if (state.shotScroll === "bottom") c.scrollTop = c.scrollHeight;
  else {
    const n = parseInt(state.shotScroll, 10);
    if (n > 0) c.scrollTop = n;
  }
}

function showSplash(mode, statusText) {
  state.splashMode = mode || "startup";
  const cancelBtn = $("splash-cancel");
  // 可取消的进度视图：更新（下载/安装）与应用同步改动（取消后剩余项仍待生效）
  const cancellable = state.splashMode === "update" || state.splashMode === "sync";
  cancelBtn.classList.toggle("hidden", !cancellable);
  cancelBtn.textContent = tr(state.splashMode === "sync" ? "取消应用" : "取消更新");
  cancelBtn.disabled = false; // 每次进入进度视图重置可取消状态
  $("splash-status").textContent = statusText || tr("正在准备运行环境…");
  $("splash-fill").style.width = "0%";
  $("splash").classList.remove("hidden");
  $("settings").classList.add("hidden");
}

function showSettings() {
  $("splash").classList.add("hidden");
  $("settings").classList.remove("hidden");
}

// ==================== 常规页 ====================

async function refreshConfig() {
  const a = bindings();
  if (!a) return;
  try {
    state.cfg = await a.GetConfig();
    const on = state.cfg.autostart;
    $("sw-autostart").setAttribute("aria-checked", String(on));
    $("cfg-port-sub").textContent = state.cfg.webURL;
    $("inp-port").value = state.cfg.port;
    $("cfg-harness-dir").textContent = state.cfg.harnessDir;
    $("sw-prerelease").setAttribute("aria-checked", String(state.cfg.harnessPrerelease));
    const ls = $("sel-lang");
    if (ls) ls.value = state.cfg.language || "auto";
    applyStaticI18n(); // 语言生效后重刷静态文案（en 时覆盖默认中文 DOM）
    refreshPageTitle(); // 页标题不在 data-i18n 静态层里，需在此按已生效的语言重刷
    updatePortHint();
    applyLaunchMode(state.cfg); // 启动方式联动（「打开」按钮、web 专有项显隐/禁用、桌面端卡片文案）
  } catch (e) { console.error("GetConfig", e); }
}

/** 端口修改提示：设置端口 ≠ 服务实际运行端口（含服务停止、仅记录旧端口）时，
 *  持续显示「重启后台服务后生效」，直到两者一致才隐藏。 */
function updatePortHint() {
  const hint = $("port-hint");
  if (!hint) return;
  const p = state.cfg && state.cfg.port;
  const rp = state.svc && state.svc.runningPort;
  if (p && rp !== undefined && rp !== 0 && rp !== p) {
    hint.classList.remove("hidden");
    hint.textContent = fmt("端口已修改为 {0}，当前服务仍运行于 {1}——重启后台服务后生效。", p, rp);
  } else {
    hint.classList.add("hidden");
  }
}

// ==================== 启动方式（Web UI / Desktop UI）联动 ====================
//
// 「启动方式」决定托盘「打开」默认指向哪个界面，也决定设置页显示哪个板块（Desktop 板块 /
// Web 板块）以及托盘自带后台服务的启停：web = 本程序拉起的 dsh web；desktop = 官方桌面端
// 内置引擎（后台服务不启动；切换为 desktop 时后端会停止服务）。
// 后端见 desktop_app.go；解析结果在 GetConfig().launchResolved（auto 或桌面端缺失时已回退 web）。
//
// 声明式约定（index.html）：
//   data-only-web="hide"    仅 Web UI 启动方式成立的条目 → desktop 模式下整块隐藏
//   data-only-web="disable" 仅影响托盘自带 Web 服务的控件 → desktop 模式下禁用（保留可见值 + tooltip）
//   data-only-desktop       仅在 desktop 启动方式下适用的板块 → 其它模式隐藏
// 新增条目时优先用这套属性声明，而不是在 JS 里逐个写 id。

/** resolveTargetFor 与后端 resolveLaunchTargetPref 同口径：auto 装了桌面端即 desktop，桌面端缺失回退 web。 */
function resolveTargetFor(pref, installed) {
  if (pref === "web") return "web";
  if (pref === "desktop") return installed ? "desktop" : "web";
  return installed ? "desktop" : "web";
}

/** pendingEffectText 待应用插件变更的生效时机：desktop 启动方式下托盘不重启后台服务，
 *  改动在官方桌面端重新启动后生效（web 模式仍是重启后台服务）。 */
function pendingEffectText() {
  return (state.cfg && state.cfg.launchResolved === "desktop") ? tr("桌面端重启后生效") : tr("重启服务后生效");
}

/** applyLaunchMode 按解析出的启动方式调整设置页：板块显隐/禁用、按钮文案与动作、桌面端卡片。 */
function applyLaunchMode(cfg) {
  if (!cfg) return;
  const desktop = cfg.launchResolved === "desktop";
  document.querySelectorAll("[data-only-web]").forEach((el) => {
    if (el.getAttribute("data-only-web") === "hide") {
      el.classList.toggle("hidden", desktop);
      return;
    }
    el.classList.toggle("row-disabled", desktop);
    el.querySelectorAll("input, select, button, textarea").forEach((c) => {
      c.disabled = desktop;
      if (desktop) c.setAttribute("title", tr("仅影响托盘自带的 Web 服务；当前默认启动方式为 Desktop UI。"));
      else c.removeAttribute("title");
    });
  });
  document.querySelectorAll("[data-only-desktop]").forEach((el) => {
    el.classList.toggle("hidden", !desktop);
  });
  // 当前页在本启动方式下已隐藏（如 desktop 模式的帮助页）：切回「常规」，不要停在空白页。
  // showPage 与 data-only-* 的显隐互斥，因此页面 <section> 不加标记、只隐藏导航项（见 index.html）。
  const activeNav = document.querySelector('.nav-item[data-page="' + (state.page || "general") + '"]');
  if (activeNav && activeNav.classList.contains("hidden")) showPage("general");
  // 「打开」按钮：desktop 下文案与动作都切到官方桌面端（动作分派在后端 OpenDefaultUI）
  const owb = $("btn-open-webui");
  if (owb) {
    owb.textContent = desktop ? tr("打开 Desktop UI") : tr("打开 Web UI");
    if (desktop) owb.disabled = !cfg.desktopInstalled;
  }
  // Harness 更新按钮：desktop 下更新的是官方桌面端（下载官方安装包 + 启动安装向导）
  const hup = $("btn-harness-update");
  if (hup) hup.textContent = desktop ? tr("更新桌面端") : tr("更新 Harness");
  // 版本行副标题：desktop 下说明版本来自桌面端内置引擎（其它情况由静态层自动还原）
  const hsub = $("harness-ver-sub");
  if (hsub && desktop) hsub.textContent = tr("官方桌面端内置引擎");
  // 开机自启动说明：desktop 形态下不启动后台服务，文案随之调整
  const asub = $("autostart-sub");
  if (asub) asub.textContent = desktop ? tr("登录后自动启动托盘并常驻；Desktop UI 不启动后台服务") : tr("登录后自动启动后台服务并常驻托盘");
  // 「安装桌面端」入口：只在未检测到官方桌面端时显示（安装后由 refreshDesktopCard 收起）
  const installBtn = $("btn-install-desktop");
  if (installBtn) installBtn.classList.toggle("hidden", !!cfg.desktopInstalled);
  // 已安装插件清单说明：清单/行内操作只针对当前启动方式对应的环境（后端按环境过滤，
  // 见 plugin_env.go；导入/恢复同样只作用于该环境）
  const psub = $("ab-plugins-sub");
  if (psub) psub.textContent = desktop ? tr("仅显示 Desktop UI 环境的插件") : tr("仅显示 Web UI 环境的插件");
  // 启动方式切换会换一整套插件环境：重新拉取清单（首次赋值不拉，进入关于页时本就会加载）
  const prevLaunch = state.launchResolved;
  state.launchResolved = cfg.launchResolved || "";
  if (prevLaunch && prevLaunch !== state.launchResolved) loadPlugins();
  syncLaunchSelect(cfg);
  syncLaunchHint(cfg);
}

/** 启动方式下拉：显示用户偏好（auto 恒显示 auto），未安装桌面端时禁用 desktop 选项。 */
function syncLaunchSelect(cfg) {
  const sel = $("sel-launch");
  if (!sel) return;
  const opt = sel.querySelector('option[value="desktop"]');
  if (opt) opt.disabled = !cfg.desktopInstalled;
  sel.value = cfg.launchTarget || "auto";
}

/** 启动方式说明行：讲清「当前生效哪个」与服务启停；未检测到桌面端时给警示色。 */
function syncLaunchHint(cfg) {
  const el = $("launch-hint");
  if (!el) return;
  if (!cfg.desktopInstalled) {
    el.textContent = tr("未检测到官方桌面端，Desktop UI 选项不可用。");
    el.classList.add("warn");
    return;
  }
  el.classList.remove("warn");
  el.textContent = cfg.launchResolved === "desktop"
    ? tr("当前生效：Desktop UI。后台服务已停止；与 Web 服务相关的设置已隐藏，切回 Web UI 会重新启动服务。")
    : tr("当前生效：Web UI。");
}

/** 官方桌面端卡片：安装位置、版本、运行状态与「打开 Desktop UI」；未安装时整卡隐藏。 */
async function refreshDesktopCard() {
  const a = bindings();
  if (!a || typeof a.GetDesktopApp !== "function") return;
  const card = $("card-desktop");
  if (!card) return;
  try {
    const d = await a.GetDesktopApp();
    // 回填配置快照：说明行与下拉可用性都依赖安装状态（刚装/刚卸载时不必等下一次 GetConfig）
    state.cfg = Object.assign({}, state.cfg, {
      desktopInstalled: !!d.installed,
      desktopVersion: d.version || "",
      desktopPath: d.path || "",
      desktopRunning: !!d.running,
      desktopChannel: d.channel || "",
      desktopFeedURL: d.feedURL || "",
    });
    card.classList.toggle("hidden", !state.cfg.desktopInstalled || state.cfg.launchResolved !== "desktop");
    // 「安装桌面端」入口与安装状态互斥（刚装完/刚卸载即时反映，不必等下一次 GetConfig）
    const installBtn = $("btn-install-desktop");
    if (installBtn) installBtn.classList.toggle("hidden", !!state.cfg.desktopInstalled);
    if (!state.cfg.desktopInstalled) {
      syncLaunchHint(state.cfg);
      syncLaunchSelect(state.cfg);
      return;
    }
    const parts = [vtag(state.cfg.desktopVersion) || "—", state.cfg.desktopRunning ? tr("运行中") : tr("未运行")];
    if (state.cfg.desktopChannel) parts.push(fmt("更新通道 {0}", state.cfg.desktopChannel));
    $("dt-sub").textContent = parts.join(" · ");
    $("dt-path").textContent = state.cfg.desktopPath || "—";
    const open = $("btn-open-desktop");
    if (open) open.disabled = false;
    // desktop 启动方式下服务卡片里的「打开」按钮由桌面端安装状态决定可点性（与后台服务无关）
    if (state.cfg.launchResolved === "desktop") {
      const owb = $("btn-open-webui");
      if (owb) owb.disabled = false;
    }
    syncLaunchHint(state.cfg);
    syncLaunchSelect(state.cfg);
  } catch (e) { console.error("GetDesktopApp", e); }
}

async function refreshService() {
  const a = bindings();
  if (!a) return;
  try {
    state.svc = await a.GetServiceState();
    const dot = $("svc-dot");
    dot.className = "dot dot-" + state.svc.state;
    const labels = {
      running: "后台服务：运行中",
      starting: "后台服务：启动中",
      stopped: "后台服务：已停止",
      failed: "后台服务：启动失败",
    };
    $("svc-text").textContent = tr(labels[state.svc.state] || state.svc.state);
    // 服务状态由标题行（圆点 + 文案）表达；副标题只承载失败原因等必要反馈，不再堆说明文字
    $("svc-sub").textContent = state.svc.state === "failed" ? (state.svc.reason || tr("请查看日志")) : "";
    updatePortBlockedHint(state.svc);
    // 「打开」按钮：web 启动方式下要求服务运行；desktop 启动方式下与后台服务无关（由 applyLaunchMode
    // 在桌面端卡片上单独控制可点性），故只在 web 路径下按服务状态禁用。
    const owb = $("btn-open-webui");
    if (owb && (state.cfg && state.cfg.launchResolved) !== "desktop") {
      owb.disabled = state.svc.state !== "running";
    }
    updatePortHint();
    refreshDesktopCard(); // 桌面端安装/运行状态（3 秒轮询随服务状态一起刷新，运行状态才跟手）
    refreshHelpState();
    // 兜底（仅截图模式）：若 splash:done 在页面就绪前已发出（快速就绪 + 慢 WebView），
    // 周期轮询发现服务 running 且 splash 未收起时自动切回设置页。
    // 正常模式绝不在此处切页——窗口由 Go 在就绪后统一隐藏，避免启动瞬间闪现设置页。
    if (state.shotPage && state.svc.state === "running" && !$("splash").classList.contains("hidden")) {
      showSettings();
      refreshVersions();
      loadPlugins();
    }
  } catch (e) { console.error("GetServiceState", e); }
}

// 帮助页「Web UI 需要重新鉴权」条目的状态：服务未运行时两个操作禁用并给出说明；服务运行但
// 拿不到带令牌的链接（服务由先前进程启动/终端手动启动、日志轮转丢失）时禁用复制、显示警告，
// 并把「重启后台服务」（重新生成链接）摆出来。helpRestarting 期间锁住全部操作，由收尾统一刷新。
let helpRestarting = false;

// portFailReasonText 端口不可用的原因文案（常规页卡片与帮助页共用，与后端 ServiceState.Reason
// 同一口径；后端已按 failKind 分类透出，见 portcheck.go）。
function portFailReasonText(kind, port) {
  if (kind === "port-in-use") return fmt("服务端口 {0} 已被其它程序占用，后台服务无法启动。", port);
  return fmt("服务端口 {0} 被系统保留（Windows 排除端口段），后台服务无法启动。", port);
}

function isPortFailKind(kind) {
  return kind === "port-blocked" || kind === "port-in-use";
}

// 端口不可用时的常规页提示 + 「改用端口 N 并启动」一键恢复。这类失败与插件/版本无关
// （后端已据 failKind 跳过插件自愈与版本回退），所以必须给出一条明确出路：只把原因说出来
// 会让用户以为要自己排查端口（2026-09-28 现场：3080 落在 Windows 排除端口段 3004-3103）。
function updatePortBlockedHint(svc) {
  const box = $("svc-port-warn");
  if (!box) return;
  const kind = svc && svc.failKind;
  const show = !!svc && svc.state === "failed" && isPortFailKind(kind);
  box.classList.toggle("hidden", !show);
  if (!show) return;
  const p = (state.cfg && state.cfg.port) || 0;
  $("svc-port-warn-text").textContent = portFailReasonText(kind, p);
  const btn = $("btn-svc-use-port");
  if (!btn) return;
  const suggest = svc.suggestedPort || 0;
  btn.disabled = !suggest;
  btn.textContent = suggest ? fmt("改用端口 {0} 并启动", suggest) : tr("请手动指定其它端口后重启服务");
}

function refreshHelpState() {
  const open = $("btn-help-open");
  const copy = $("btn-help-copy");
  const restart = $("btn-help-restart");
  if (!open || !copy || !restart) return;
  const svc = state.svc || {};
  const running = svc.state === "running";
  const hasToken = running && !!svc.tokenFound;
  const warn = running && !hasToken;
  $("help-state").classList.toggle("hidden", running);
  // 「重启后台服务」嵌在警告框内：警告框显隐即按钮显隐
  $("help-warn").classList.toggle("hidden", !warn);
  // 端口不可用（含 desktop→web 切换失败）时的帮助页说明：换端口入口在常规页，这里只指路
  const portHint = $("help-port");
  if (portHint) {
    const portFailed = !running && svc.state === "failed" && isPortFailKind(svc.failKind);
    portHint.classList.toggle("hidden", !portFailed);
    if (portFailed) {
      portHint.textContent = portFailReasonText(svc.failKind, (state.cfg && state.cfg.port) || 0) +
        " " + tr("请到「常规 → 服务端口」改用其它端口后重启服务。");
    }
  }
  if (helpRestarting) {
    open.disabled = true;
    copy.disabled = true;
    restart.disabled = true;
    return;
  }
  open.disabled = !running;
  copy.disabled = !hasToken;
  restart.disabled = false;
}

// 帮助页状态提示行（重启中/重启失败）：空文本 = 隐藏。
function setHelpBusy(text, isError) {
  const el = $("help-busy");
  if (!el) return;
  if (!text) {
    el.classList.add("hidden");
    el.classList.remove("help-busy-error");
    return;
  }
  el.textContent = text;
  el.classList.toggle("help-busy-error", !!isError);
  el.classList.remove("hidden");
}

function wireGeneral() {
  const selLang = $("sel-lang");
  if (selLang) {
    selLang.addEventListener("change", async (e) => {
      try {
        await bindings().SetLanguage(e.target.value);
      } catch (err) { console.error("SetLanguage", err); }
      refreshConfig(); // 读回生效偏好（auto 时下拉仍显示 auto）
    });
  }
  // 默认启动方式：切换会启停后台服务（切 web 启动服务 / 切 desktop 停止服务），先弹确认，
  // 确认后才写入并生效；取消则把下拉回退到当前偏好。
  const selLaunch = $("sel-launch");
  if (selLaunch) {
    selLaunch.addEventListener("change", async (e) => {
      const want = e.target.value;
      const cfg = state.cfg || {};
      const willDesktop = resolveTargetFor(want, !!cfg.desktopInstalled) === "desktop";
      const svcRunning = !!(state.svc && state.svc.state === "running");
      let msg;
      if (willDesktop) {
        msg = svcRunning
          ? tr("切换到 Desktop UI 会停止后台服务，正在进行的网页端会话会中断。确认切换？")
          : tr("切换到 Desktop UI 后，托盘「打开」将指向官方桌面端，后台服务保持停止。确认切换？");
      } else {
        msg = svcRunning
          ? tr("切换到 Web UI 后，托盘「打开」将指向网页端界面；后台服务已在运行。确认切换？")
          : tr("切换到 Web UI 会启动后台服务（首次可能需要下载运行环境，耗时数分钟）。确认切换？");
      }
      const ok = await confirmDialog(tr("切换启动方式"), msg, tr("确认切换"));
      if (!ok) {
        syncLaunchSelect(cfg); // 取消：下拉显示回当前偏好
        return;
      }
      try {
        await bindings().SetLaunchTarget(want);
      } catch (err) { console.error("SetLaunchTarget", err); }
      refreshConfig();
      refreshService();
    });
  }
  $("sw-autostart").addEventListener("click", async () => {
    const on = $("sw-autostart").getAttribute("aria-checked") !== "true";
    await bindings().SetAutostart(on);
    // 以后端实际注册状态为准刷新开关（注册失败弹窗后开关回弹，不与真实状态脱节）
    try {
      const cfg = await bindings().GetConfig();
      if (cfg && typeof cfg.autostart === "boolean")
        $("sw-autostart").setAttribute("aria-checked", String(cfg.autostart));
    } catch (e) { console.error("refresh autostart state", e); }
  });
  $("inp-port").addEventListener("change", async (e) => {
    const v = parseInt(e.target.value, 10);
    if (v > 0 && v <= 65535) await bindings().SetPort(v);
    refreshConfig();
  });
  // 端口不可用（被系统保留 / 被占用）时的一键恢复：改用推荐端口 → 走既有 RestartService
  // （内含端口预检、进度与失败提示），成功后就地刷新状态。
  const usePortBtn = $("btn-svc-use-port");
  if (usePortBtn) {
    usePortBtn.addEventListener("click", async () => {
      const suggest = (state.svc && state.svc.suggestedPort) || 0;
      if (!suggest || usePortBtn.disabled) return;
      usePortBtn.disabled = true;
      try { await bindings().SetPort(suggest); } catch (e) { console.error("SetPort", e); }
      const portInput = $("inp-port");
      if (portInput) portInput.value = suggest;
      refreshConfig();
      $("svc-sub").textContent = tr("正在改用新端口并启动后台服务…");
      let ok = false;
      try { ok = await bindings().RestartService(); } catch (e) { console.error("RestartService", e); }
      if (!ok) $("svc-sub").textContent = tr("重启失败，请查看日志");
      refreshService();
      usePortBtn.disabled = false;
    });
  }
  $("btn-pick-harness").addEventListener("click", async () => {
    // 防重复弹窗：对话框打开期间禁用按钮，避免连点开出多个目录选择窗口
    const btn = $("btn-pick-harness");
    btn.disabled = true;
    try {
      const dir = await bindings().PickHarnessDir();
      if (dir) await bindings().SetHarnessDir(dir);
      refreshConfig();
    } finally {
      btn.disabled = false;
    }
  });
  $("btn-restart").addEventListener("click", async () => {
    $("btn-restart").disabled = true;
    $("svc-sub").textContent = "正在重启后台服务…";
    const ok = await bindings().RestartService();
    if (!ok) $("svc-sub").textContent = tr("重启失败，请查看日志");
    setTimeout(() => { $("btn-restart").disabled = false; refreshService(); }, 2000);
  });
  // 「安装桌面端」：下载官方安装包并启动安装向导（进度走 splash，可取消）
  const btnInstallDesktop = $("btn-install-desktop");
  if (btnInstallDesktop) {
    btnInstallDesktop.addEventListener("click", async () => {
      btnInstallDesktop.disabled = true;
      try {
        await bindings().InstallDesktopApp();
      } catch (e) {
        console.error("InstallDesktopApp", e);
      } finally {
        setTimeout(() => { btnInstallDesktop.disabled = false; refreshDesktopCard(); }, 1500);
      }
    });
  }
  // 「打开」按钮：按当前启动方式打开（desktop → 官方桌面端；web → 带最新 token 的 Web UI）。
  // 由后端 OpenDefaultUI 统一分派，前端只负责文案（见 applyLaunchMode）。
  $("btn-open-webui").addEventListener("click", () => bindings().OpenDefaultUI());
  // 桌面端卡片：打开 / 拉起官方桌面端
  const btnOpenDesktop = $("btn-open-desktop");
  if (btnOpenDesktop) {
    btnOpenDesktop.addEventListener("click", async () => {
      btnOpenDesktop.disabled = true;
      try {
        await bindings().LaunchDesktopApp();
      } catch (e) {
        console.error("LaunchDesktopApp", e);
      } finally {
        setTimeout(() => { btnOpenDesktop.disabled = false; refreshDesktopCard(); }, 1200);
      }
    });
  }
  // 重置：打开勾选弹层（harness 必选；会话/插件按需勾选，展示将清除的数量；
  // 目标版本下拉异步填充——仅早于当前运行版本的官方版本）
  $("btn-reset-harness").addEventListener("click", async () => {
    const btn = $("btn-reset-harness");
    btn.disabled = true;
    try {
      const stats = await bindings().GetResetStats();
      const sc = (stats && stats.sessionCount) || 0;
      const pc = (stats && stats.pluginCount) || 0;
      $("reset-sessions-sub").textContent = fmt("将清除 {0} 条会话记录", sc);
      $("reset-plugins-sub").textContent = fmt("将清除 {0} 个已安装插件", pc);
      // 默认勾选插件（重置将物理删除已装插件，谨慎起见默认勾选）；会话默认不勾选（数据谨慎）
      $("reset-c-sessions").checked = false;
      $("reset-c-plugins").checked = pc > 0;
      loadResetVersions(); // 异步填充目标版本（不阻塞弹窗打开：下拉先显示 loading 态）
      $("reset-modal").classList.remove("hidden");
    } catch (e) {
      $("svc-sub").textContent = "获取重置统计失败：" + (e && e.message ? e.message : e);
    } finally {
      setTimeout(() => { btn.disabled = false; }, 800);
    }
  });
  $("reset-cancel").addEventListener("click", () => $("reset-modal").classList.add("hidden"));
  $("reset-confirm").addEventListener("click", async () => {
    $("reset-modal").classList.add("hidden");
    const clearSessions = $("reset-c-sessions").checked;
    const clearPlugins = $("reset-c-plugins").checked;
    const target = $("reset-target").value || "";
    showSplash("startup", target ? "正在重置 DeepSeek Harness 到 " + vtag(target) + "…" : "正在重置 DeepSeek Harness…");
    await bindings().ResetHarness(clearSessions, clearPlugins, target);
  });
  $("reset-target").addEventListener("change", updateResetTargetWarn);
  // 点遮罩等同取消
  $("reset-modal").querySelector(".modal-mask").addEventListener("click", () => $("reset-modal").classList.add("hidden"));
}

/**
 * 填充「重置目标版本」下拉。候选来自 GetResetVersions（npm 全部已发布版本：含高于当前版本
 * 与预发布，按新→旧；默认选中当前版本=同版本重装）。边界语义：
 *  - 查询失败 → 说明原因并保持「开始重置」禁用（无法确定目标，勿盲目重置）；
 *  - 源码形态 → 说明并禁用（Go 侧会拦截执行）；
 *  - 无候选（registry 返回空）→ 按 Go 侧 Default 放行。
 */
async function loadResetVersions() {
  const tok = (state.resetVersionToken = (state.resetVersionToken || 0) + 1); // 防连点/快速重开时的过期响应覆盖
  const sel = $("reset-target");
  const note = $("reset-target-note");
  const curEl = $("reset-target-cur");
  const confirm = $("reset-confirm");
  sel.disabled = true;
  confirm.disabled = true;
  note.textContent = "";
  note.classList.add("hidden");
  curEl.textContent = "";
  sel.innerHTML = '<option value="">' + tr("正在查询可用版本…") + '</option>';
  try {
    const info = await bindings().GetResetVersions();
    if (tok !== state.resetVersionToken) return; // 已有更新的查询在跑，丢弃本次结果
    if (info && info.current) curEl.textContent = "当前版本 " + vtag(info.current);
    if (info && info.form === "source") {
      // 源码形态：Go 侧会拦截重置执行，直接禁用并说明
      sel.innerHTML = "";
      showResetTargetNote((info && info.note) || "当前为源码 checkout 形态，不支持自动重置。");
      return;
    }
    if (info && info.note) showResetTargetNote(info.note);
    const opts = (info && info.options) || [];
    if (!opts.length) {
      // 无「不高于当前版本」候选 → 降级放行：采用 Go 侧算好的具体默认目标
      // （弹窗打开时已查证，避免确认后再触网查询最新版本）
      sel.innerHTML = "";
      const fb = (info && info.default) || "";
      if (fb) {
        sel.innerHTML = '<option value="' + fb + '" data-pre="0">官方默认目标 ' + vtag(fb) + "</option>";
        sel.disabled = false;
        confirm.disabled = false;
      }
      updateResetTargetWarn();
      return;
    }
    let html = "";
    let defIdx = 0;
    opts.forEach((o, i) => {
      if (o.version === (info && info.default)) defIdx = i;
      const cur = o.version === (info && info.current) ? "（当前）" : "";
      const label = vtag(o.version) + cur + (o.prerelease ? "（预发布）" : "");
      html += '<option value="' + o.version + '" data-pre="' + (o.prerelease ? "1" : "0") + '"' +
        (o.prerelease ? ' class="opt-pre"' : "") + ">" + label + "</option>";
    });
    sel.innerHTML = html;
    sel.selectedIndex = defIdx;
    sel.disabled = false;
    confirm.disabled = false;
    updateResetTargetWarn();
  } catch (e) {
    if (tok !== state.resetVersionToken) return; // 过期响应的失败同样丢弃
    sel.innerHTML = "";
    showResetTargetNote("查询可用版本失败：" + (e && e.message ? e.message : e) + "，请检查网络后重试。");
  }
}

/** 弹窗内说明行（警示色；空文本隐藏）。loadResetVersions 专用。 */
function showResetTargetNote(text) {
  const note = $("reset-target-note");
  note.textContent = tr(text || "");
  note.classList.toggle("hidden", !note.textContent);
}

/** 所选目标为预发布时提示兼容性风险（下拉 change / 填充完成后调用；非预发布不改动既有说明）。 */
function updateResetTargetWarn() {
  const sel = $("reset-target");
  const opt = sel && sel.options[sel.selectedIndex];
  const isPre = opt && opt.dataset && opt.dataset.pre === "1";
  if (!isPre) return; // 保留 loadResetVersions 写入的边界/降级说明
  const note = $("reset-target-note");
  note.textContent = tr("注意：所选为预发布版本，可能与已安装插件不兼容；若重置后服务无法启动，请查看日志。");
  note.classList.remove("hidden");
}

// ==================== 关于页（按模块单独检查更新 + 插件列表） ====================

/** 版本号统一 v 前缀展示；dev/空原样。 */
function vtag(x) {
  x = String(x || "").replace(/^v/, "");
  return x ? "v" + x : "";
}

async function refreshVersions() {
  const a = bindings();
  if (!a) return;
  try {
    const v = await a.GetVersions();
    $("ver-app").textContent = v.app || "dev";
    $("ver-harness").textContent = vtag(v.harness) || "—";
  } catch (e) { console.error("GetVersions", e); }
}

/**
 * 单模块「检查更新」（dsh-systray / harness）。
 * which: "systray" | "harness" —— 按钮/提示/更新按钮按此约定命名。
 */
async function runModuleCheck(which) {
  const a = bindings();
  if (!a) return;
  const tok = (state.checkToken = state.checkToken || {});
  const myTok = (tok[which] = (tok[which] || 0) + 1); // 防竞态：开关切换/连点后过期响应不覆盖新状态
  const checkBtn = $("btn-check-" + which);
  const hintEl = $("hint-" + which);
  const upBtn = $(which === "systray" ? "btn-systray-update" : "btn-harness-update");
  checkBtn.disabled = true;
  hintEl.className = "update-note";
  hintEl.textContent = tr("正在检查更新…");
  upBtn.classList.add("hidden");
  try {
    const m = which === "systray" ? await a.CheckSystrayUpdate() : await a.CheckHarnessUpdate();
    if (myTok !== tok[which]) return; // 已有更新的检查/开关复位，丢弃本次过期结果
    if (m.error) {
      hintEl.classList.add("err");
      hintEl.textContent = fmt("检查失败：{0}", m.error);
      return;
    }
    // 检查结果只给结论（不再附详细说明），细节看下方版本号与日志
    if (m.hasUpdate) {
      hintEl.classList.add("ok");
      hintEl.textContent = fmt("有新版本 {0}", vtag(m.latest));
      upBtn.classList.remove("hidden");
    } else {
      hintEl.textContent = fmt("已是最新（当前 {0}）", vtag(m.current || m.latest) || "—");
    }
  } catch (e) {
    if (myTok !== tok[which]) return;
    hintEl.classList.add("err");
    hintEl.textContent = fmt("检查失败：{0}", e && e.message ? e.message : e);
  } finally {
    if (myTok === tok[which]) checkBtn.disabled = false;
  }
}

/** 复位单模块检查结果（通道开关切换后调用，避免残留过期状态误导）。systray 与 harness 各自独立。 */
function resetModuleCheck(which) {
  const tok = (state.checkToken = state.checkToken || {});
  tok[which] = (tok[which] || 0) + 1; // 使在途检查响应失效
  const hintEl = $("hint-" + which);
  if (hintEl) { hintEl.className = "update-note"; hintEl.textContent = ""; }
  const upBtn = $(which === "systray" ? "btn-systray-update" : "btn-harness-update");
  if (upBtn) upBtn.classList.add("hidden");
}

function wireAbout() {
  $("sw-prerelease").addEventListener("click", async () => {
    const on = $("sw-prerelease").getAttribute("aria-checked") !== "true";
    // 开启预发布通道前提示风险：可能导致服务启动失败
    if (on) {
      const ok = await confirmDialog(
        "开启预发布通道？",
        "预发布版可能不稳定，可能导致服务启动失败。确定开启吗？",
        "确定开启"
      );
      if (!ok) return;
    }
    await bindings().SetHarnessPrerelease(on);
    $("sw-prerelease").setAttribute("aria-checked", String(on));
    // 通道语义变化后必须复位 harness 检查结果：残留的「发现新版本（预发布）」提示与已关闭的通道矛盾
    resetModuleCheck("harness");
    const hint = $("hint-harness");
    hint.textContent = tr(on
      ? "已开启预发布通道，可重新检查更新（含预发布版）"
      : "已关闭预发布通道，检查更新将仅显示稳定版本");
    if (on) runModuleCheck("harness"); // 开启后按新通道自动重查一次（关闭则留给用户手动复查）
  });
  // dsh-systray / Harness：各自的检查按钮
  $("btn-check-systray").addEventListener("click", () => runModuleCheck("systray"));
  $("btn-check-harness").addEventListener("click", () => runModuleCheck("harness"));
  // dsh-systray 自身更新：确认后再下载（下载/安装/重启走 splash，窗口自动显示）
  $("btn-systray-update").addEventListener("click", async () => {
    const ok = await confirmDialog(
      "更新 dsh-systray？",
      "将下载并安装新版本并自动重启。确认开始更新吗？",
      "开始更新"
    );
    if (!ok) return;
    $("btn-systray-update").disabled = true;
    bindings().StartUpdate();
    showSplash("update", tr("正在准备更新…"));
  });
  // Harness 更新：确认后执行（进度走 splash，失败自动回退）。
  // desktop 启动方式下更新的是官方桌面端（下载官方安装包 + 启动安装向导），措辞与后果都不同。
  $("btn-harness-update").addEventListener("click", async () => {
    const desktop = !!(state.cfg && state.cfg.launchResolved === "desktop");
    const ok = await confirmDialog(
      desktop ? "更新官方桌面端？" : "更新 DeepSeek Harness？",
      desktop
        ? "将下载官方安装包（校验后）并启动安装向导；桌面端正在运行时需先退出它。确认开始吗？"
        : "更新期间服务会短暂重启，失败会自动回退。确认开始更新吗？",
      "开始更新"
    );
    if (!ok) return;
    $("btn-harness-update").disabled = true;
    await bindings().StartHarnessUpdate();
  });

  // 插件过滤：输入防抖后重渲染（大量插件时快速定位）
  const filterEl = $("plug-filter");
  if (filterEl) {
    filterEl.addEventListener("input", () => {
      clearTimeout(state.plugTimer);
      state.plugTimer = setTimeout(() => {
        state.plugFilter = filterEl.value || "";
        renderPlugins();
      }, 200);
    });
    filterEl.addEventListener("keydown", (e) => {
      if (e.key === "Escape") { filterEl.value = ""; state.plugFilter = ""; renderPlugins(); }
    });
  }

  // 插件行按钮：事件委托（300+ 行也只挂一个监听），行索引对应全量 rows
  const plugList = $("plug-list");
  if (plugList) {
    plugList.addEventListener("click", (e) => {
      const item = e.target.closest(".plug-item");
      const btn = e.target.closest("button");
      if (!item || !btn) return;
      const p = state.plugRows[Number(item.dataset.idx)];
      if (!p) return;
      if (btn.dataset.check !== undefined) doPluginCheck(p, item, btn);
      else if (btn.dataset.localupdate !== undefined) doLocalPluginUpdate(p, item, btn);
      else if (btn.dataset.enable !== undefined) doPluginEnable(p, item, btn);
      else if (btn.dataset.discard !== undefined) doPluginDiscard(p, item, btn);
      else if (btn.dataset.update !== undefined) doPluginUpdate(p, item, btn);
      else if (btn.dataset.del !== undefined) doPluginRemove(p, item, btn);
      else if (btn.dataset.repair !== undefined) doPluginRiskRepair(p, item, btn);
    });
  }
}

// ==================== 插件列表（每行单独检查 / 更新） ====================

const PLUG_SRC_LABEL = { npm: "npm", github: "GitHub", file: "本地", tarball: "压缩包", unknown: "未知来源" };
function srcLabel(src) {
  const z = PLUG_SRC_LABEL[src];
  return tr(z || src || "未知来源");
}

/** 行内小号状态文字：tone = ok | err | warn | muted */
function setNote(item, text, tone) {
  const note = item.querySelector("[data-note]");
  if (!note) return;
  const toned = tone === "ok" || tone === "err" || tone === "warn";
  note.className = "plug-note" + (toned ? " " + tone : "");
  note.textContent = text || "";
}

async function loadPlugins() {
  const a = bindings();
  if (!a) return;
  let rows = [];
  try {
    rows = (await a.GetInstalledPlugins()) || [];
  } catch (e) {
    console.error("GetInstalledPlugins", e);
    $("plug-empty").classList.remove("hidden");
    $("plug-empty").textContent = "插件列表加载失败：" + (e && e.message ? e.message : e);
    $("plug-count").textContent = "";
    return;
  }
  state.plugRows = rows;
  renderPlugins();
  loadPendingChanges();
}

/** 拉取待应用变更并刷新提示条（变更需重启服务才生效，未应用则常驻提示）。 */
async function loadPendingChanges() {
  const a = bindings();
  if (!a) return;
  try {
    state.plugPending = (await a.GetPendingPluginChanges()) || [];
  } catch (e) {
    state.plugPending = [];
  }
  renderPendingBanner();
}

/**
 * 关于页「待应用变更」提示条：只显示待生效数量（不列条目——大量增删时列表会挤占卡片高度，
 * 逐条状态与「撤销」在下方插件行内看），右侧「全部撤销」/「立即应用」。
 */
function renderPendingBanner() {
  const box = $("plug-pending");
  const text = $("plug-pending-text");
  if (!box || !text) return;
  const items = state.plugPending || [];
  if (!items.length) {
    box.classList.add("hidden");
    text.textContent = "";
    return;
  }
  const updates = items.filter((p) => p.op === "update").length;
  const removes = items.filter((p) => p.op === "remove").length;
  const enables = items.length - updates - removes;
  const parts = [];
  if (updates) parts.push(fmt("{0} 项更新", updates));
  if (removes) parts.push(fmt("{0} 项删除", removes));
  if (enables) parts.push(fmt("{0} 项启用", enables));
  let line = fmt("有 {0} 项变更尚未生效（{1}）", items.length, parts.join(" · ")) + " " + pendingEffectText();
  // 删除类变更若检测到会话数据风险，横幅只报数量（逐条警示与「修复会话」在下方插件行内）
  const risky = items.filter((p) => p.op === "remove" && p.risk && p.risk.sessions > 0).length;
  if (risky) line += " " + fmt("（{0} 项删除会导致历史会话无法打开，可在行内先「修复会话」）", risky);
  text.textContent = line;
  box.classList.remove("hidden");
}

/**
 * 按过滤关键字渲染插件列表。只负责“显示哪些行”；行内状态（检查结果/更新按钮）
 * 由 state.plugState 恢复——过滤/刷新重渲染后不丢失。事件统一委托给 #plug-list。
 */
function renderPlugins() {
  const list = $("plug-list");
  const empty = $("plug-empty");
  const count = $("plug-count");
  const q = (state.plugFilter || "").trim().toLowerCase();
  const byName = new Map();
  const shown = [];
  state.plugRows.forEach((p, i) => {
    byName.set(p.name, i);
    if (!q) { shown.push(p); return; }
    const hay = ((p.name || "") + " " + (PLUG_SRC_LABEL[p.source] || p.source || "") + " " + (p.version || "")).toLowerCase();
    if (hay.includes(q)) shown.push(p);
  });
  const total = state.plugRows.length;
  count.textContent = total ? (shown.length + " / " + total + (curLangCode() === "en" ? "" : " 个")) : "";
  const desktopEnv = state.cfg && state.cfg.launchResolved === "desktop";
  const emptyEnv = desktopEnv ? "Desktop UI 环境还没有安装插件" : "Web UI 环境还没有安装插件";
  empty.textContent = total
    ? (shown.length ? "" : fmt("没有匹配“{0}”的插件", state.plugFilter))
    : tr(emptyEnv);
  empty.classList.toggle("hidden", shown.length > 0);
  list.textContent = "";
  const frag = document.createDocumentFragment();
  for (const p of shown) frag.appendChild(renderPluginRow(p, byName.get(p.name)));
  list.appendChild(frag);
}

/** 组装一个插件行；p 为 Go 返回的 PluginRow，idx 为全量 rows 中的索引（事件委托取数据用）。 */
function renderPluginRow(p, idx) {
  const item = document.createElement("div");
  item.className = "plug-item";
  item.dataset.idx = String(idx);

  const name = document.createElement("div");
  name.className = "plug-name";
  name.textContent = p.name;
  const badge = document.createElement("span");
  badge.className = "plug-badge";
  badge.textContent = srcLabel(p.source);
  name.appendChild(badge);
  if (p.disabled) {
    const disBadge = document.createElement("span");
    disBadge.className = "plug-badge-dis";
    disBadge.textContent = tr("已禁用");
    name.appendChild(disBadge);
  }
  // 「已被跳过」：harness 启动期因 peer 版本不兼容主动跳过（未加载、未落盘），与本程序的
  // 「已禁用」（摘除 bundles + 记 disabledPlugins）是两种状态，故用不同徽标与配色区分。
  if (p.skippedByHarness) {
    const skipBadge = document.createElement("span");
    skipBadge.className = "plug-badge-skip";
    skipBadge.textContent = tr("已被跳过");
    if (p.skippedReason) skipBadge.title = p.skippedReason;
    name.appendChild(skipBadge);
  }

  const sub = document.createElement("div");
  sub.className = "plug-sub";
  sub.textContent = fmt("当前版本 {0}", p.version ? vtag(p.version) : tr("未安装")) +
    (p.profile ? fmt(" · 环境 {0}", p.profile) : "");
  // 本地插件：仅当存在「用户已重指定/生效」的本地路径时才展示（原路径不出现，保护隐私）
  if (p.localDir) {
    sub.textContent += " · " + fmt("本地路径：{0}", p.localDir);
    sub.title = p.localDir;
  }

  const note = document.createElement("div");
  note.className = "plug-note";
  note.dataset.note = "";

  const main = document.createElement("div");
  main.className = "plug-main";
  main.append(name, sub, note);

  const actions = document.createElement("div");
  actions.className = "plug-actions";

  // 远程来源：提供「检查更新」按钮（描边主色，比 ghost 醒目）；检查出新版本后启用「更新」
  if (p.canUpdate) {
    const checkBtn = document.createElement("button");
    checkBtn.className = "btn btn-outline btn-xs";
    checkBtn.textContent = tr("检查更新");
    checkBtn.dataset.check = "";
    actions.appendChild(checkBtn);
  }
  // 「更新」按钮（所有行同尺寸同文本位置，仅颜色/行为区分）：
  //  - 远程来源（canUpdate）：初始隐藏，检查出新版本后显示可用（primary）；
  //  - 本地来源（file/link/workspace：本地目录安装）：直接可用——点击弹目录选择框，
  //    比较所选与当前版本后提示覆盖更新或已是最新；
  //  - 其余不可更新来源（tarball 等）：灰置表达不可用。
  const upBtn = document.createElement("button");
  upBtn.className = "btn btn-primary btn-xs";
  upBtn.textContent = tr("更新");
  if (p.source === "file") {
    upBtn.className = "btn btn-outline btn-xs";
    upBtn.textContent = tr("更新…");
    upBtn.dataset.localupdate = "";
  } else {
    upBtn.dataset.update = "";
    if (p.canUpdate) {
      upBtn.classList.add("hidden");
    } else {
      upBtn.classList.add("btn-muted"); // 与 btn-primary 相同版式，仅颜色表达不可用
    }
  }
  upBtn.disabled = !(p.canUpdate || p.source === "file");
  actions.appendChild(upBtn);

  // 「启用」：仅禁用（不兼容自愈）行出现——登记一条启用变更，与更新/删除共用一次服务重启；
  // 若仍与当前版本不兼容，批末自愈会自动重新禁用（服务保持可用）。
  // 无依赖声明的「已自动禁用」行（ghostDisabled）不可直接启用，只提供删除。
  let enBtn = null;
  if (p.disabled && !p.ghostDisabled) {
    enBtn = document.createElement("button");
    enBtn.className = "btn btn-outline btn-xs";
    enBtn.textContent = tr("启用");
    enBtn.dataset.enable = "";
    actions.appendChild(enBtn);
  }

  // 「删除」：确认后物理移除该插件（其全部环境），失败自动回退；完成后 Go 端发 plugins:changed 刷新。
  // 用“安静危险”样式（透明底 + 描边），避免整块红底在行内过于突兀。
  const delBtn = document.createElement("button");
  delBtn.className = "btn btn-danger-ghost btn-xs";
  delBtn.textContent = tr("删除");
  delBtn.dataset.del = "";
  actions.appendChild(delBtn);

  // 待应用变更行：隐藏变更类按钮，只留「撤销」——变更已登记但未执行（需重启服务生效），
  // 重复点击更新/删除/启用会造成「已登记还想再登记」的困惑与并发登记。
  // 待删除且检测到会话数据风险时，另给「修复会话」入口（删除后这些历史会话会打不开）。
  const risk = p.pendingOp === "remove" ? p.pendingRisk : null;
  if (p.pendingOp) {
    const hide = [upBtn, delBtn];
    if (enBtn) hide.push(enBtn);
    for (const b of hide) b.classList.add("hidden");
    if (risk && risk.sessions > 0) {
      const fixBtn = document.createElement("button");
      fixBtn.className = "btn btn-outline btn-xs";
      fixBtn.textContent = tr("修复会话");
      fixBtn.dataset.repair = "";
      actions.appendChild(fixBtn);
    }
    const undoBtn = document.createElement("button");
    undoBtn.className = "btn btn-outline btn-xs";
    undoBtn.textContent = tr("撤销");
    undoBtn.dataset.discard = "";
    actions.appendChild(undoBtn);
  }

  item.append(main, actions);
  // 不可更新行的小号原因：本地来源已有「更新…」选目录入口，不再显示旧的“无远程来源”说明；
  // 但「待重指定」的本地行（pendingLocal，原依赖路径不存在）需显示重新指定指引
  if (!p.canUpdate && p.reason && (p.source !== "file" || p.pendingLocal)) setNote(item, p.reason, "muted");
  // 重渲染后恢复行内状态（检查结果 / 更新可用性），避免过滤/刷新丢失
  const st = applyPlugState(item, state.plugState[p.name], p);
  // 待应用变更：常驻提示「需重启服务生效」（优先于检查/禁用等行内状态）；
  // 若刚刚修复过会话记录，保留修复结论（st.fromRiskFix，见 doPluginRiskRepair）。
  if (p.pendingOp) {
    if (st && st.fromRiskFix && st.note) {
      // 修复结论已由 applyPlugState 写入，此处不覆盖
    } else if (risk && risk.sessions > 0) {
      setNote(item, fmt("删除后 {0} 个历史会话将无法打开（该插件写入了 {1} 条自定义记录），可点「修复会话」后删除",
        risk.sessions, risk.events), "warn");
    } else {
      setNote(item, fmt("待应用：{0}（{1}）",
        p.pendingOp === "remove" ? tr("删除该插件")
          : (p.pendingOp === "enable" ? tr("启用该插件") : tr("更新到最新版本")), pendingEffectText()), "muted");
    }
  } else if (p.disabled && !(st && st.note)) {
    // 禁用行默认原因行（无动态检查状态时显示）
    setNote(item, fmt("已禁用（{0}）", p.disabledReason || tr("与当前版本不兼容")), "err");
  } else if (p.skippedByHarness && !(st && st.note)) {
    // harness 跳过行：只告知（未加载的原因），不提供「启用」——它仍在激活清单里，重启即可重试；
    // 真正的出路是「检查更新」装到兼容版本（harness 自带 allow-version 授权也可恢复）。
    setNote(item, fmt("harness 已跳过（与当前 dsh 版本不兼容）：{0}",
      p.skippedReason || tr("与当前版本不兼容")), "warn");
  }
  return item;
}

/**
 * 恢复一行在检查/更新后留下的状态：note 文案语气 + 更新按钮可用性。
 * 过期清理：行内状态记录的是「产生时的版本」（atVersion），当前行版本已变（外部更新、重新导入、
 * 回滚、删除后又装回）时旧结论不再成立——丢弃整条缓存，避免行内长期停留在「已删除」「已更新 vX」
 * 这类与现状不符的提示。删除类结论以 atVersion="" 记录（只有该行不存在时才成立）。
 * 返回实际生效的状态（无=null）。
 */
function applyPlugState(item, st, p) {
  if (st && st.atVersion !== undefined && (p ? p.version || "" : "") !== st.atVersion) {
    if (p) delete state.plugState[p.name];
    st = null;
  }
  if (!st) return null;
  if (st.note) setNote(item, st.note, st.noteTone || "muted");
  const upBtn = item.querySelector("button[data-update]");
  if (upBtn) {
    if (st.upShow) {
      upBtn.disabled = false;
      upBtn.classList.remove("hidden");
      upBtn.textContent = tr("更新") + (st.upLatest ? " v" + st.upLatest : "");
    } else if (st.upShow === false) {
      upBtn.disabled = true;
      upBtn.classList.add("hidden");
    }
  }
}

/** 单插件检查（事件委托触发，仅影响该行；结果写入 plugState 供重渲染恢复）。 */
async function doPluginCheck(p, item, btn) {
  const a = bindings();
  if (!a) return;
  btn.disabled = true;
  setNote(item, "正在检查更新…", "muted");
  const st = state.plugState[p.name] || (state.plugState[p.name] = {});
  st.atVersion = p.version || ""; // 检查结论对应「当时版本」：版本变化即过期（见 applyPlugState）
  try {
    const r = await a.CheckPluginUpdate(p.name);
    if (r.error) {
      st.note = "无法检查更新：" + r.error; st.noteTone = "err";
      st.upShow = false; st.upLatest = "";
    } else if (r.hasUpdate) {
      st.note = "有新版本 " + vtag(r.latest) + "，可更新"; st.noteTone = "ok";
      st.upLatest = r.latest || ""; st.upShow = true;
    } else {
      st.note = fmt("已是最新版本（{0}）", vtag(r.latest)); st.noteTone = "muted";
      st.upShow = false; st.upLatest = "";
    }
  } catch (e) {
    st.note = fmt("检查失败：{0}", e && e.message ? e.message : e); st.noteTone = "err";
  }
  // 检查完成后刷新插件列表：版本列以实读 node_modules 为准（本地开发目录改动 / 导入副本 /
  // 外部更新都可能让行内「当前版本」过期）；行内状态（提示语、更新按钮）由 plugState 恢复，不丢失。
  await loadPlugins();
}

/**
 * 单插件更新：点击即登记为待应用变更（**不弹确认框**——登记不执行、可随时「撤销」，
 * 连续登记多个插件再统一应用，弹窗确认纯属多余）。
 */
function doPluginUpdate(p, item, upBtn) {
  item.querySelectorAll("button").forEach((b) => { b.disabled = true; });
  const ver = (state.plugState[p.name] || {}).upLatest || "";
  // 只登记、不执行：变更进入待应用区（多项合并为一次服务重启），由关闭设置窗口时确认或
  // 关于页「立即应用」统一执行。行状态由 Go 端 plugins:changed 重渲染（含「撤销」按钮）。
  setNote(item, ver ? fmt("已登记：更新到 {0}（{1}）", vtag(ver), pendingEffectText()) : fmt("已登记为待应用变更（{0}）", pendingEffectText()), "muted");
  bindings().StartPluginUpdate(p.name);
}

/**
 * 手动启用被禁用的插件：点击即登记为待应用变更（**不弹确认框**——登记不执行、可随时「撤销」，
 * 与更新/删除共用同一个待应用区与一次服务重启；启用后仍不兼容时由批末自愈自动重新禁用）。
 */
function doPluginEnable(p, item, enBtn) {
  item.querySelectorAll("button").forEach((b) => { b.disabled = true; });
  setNote(item, fmt("已登记：启用该插件（{0}）", pendingEffectText()), "muted");
  bindings().EnablePlugin(p.id); // 结果由 Go 端 plugin:op:done / plugins:changed 刷新行状态
}

/** 本地插件更新：弹目录选择 → Go 端比较所选/当前版本 → 有差异确认后覆盖更新，相同提示已是最新。 */
async function doLocalPluginUpdate(p, item, upBtn) {
  item.querySelectorAll("button").forEach((b) => { b.disabled = true; });
  try {
    const r = await bindings().PickLocalPluginPath(p.id);
    if (!r) return; // 对话框取消
    if (r.error) { setNote(item, fmt("无法更新：{0}", r.error), "err"); return; }
    const curTxt = vtag(r.current) || "未安装";
    const verTxt = vtag(r.version) || "未知版本";
    // 待重指定行：所选目录版本与残留副本一致并不等于「无需操作」——真正要做的是把
    // 安装来源重链接到所选目录（清 pending 记录 + 恢复激活 + 改写 link: spec），
    // 因此跳过 same 短路，照常确认并调用 Apply。版本差异仅在普通本地更新时才有意义。
    if (r.relation === "same" && !p.pendingLocal) {
      setNote(item, "已经是最新（所选目录版本与当前一致：" + verTxt + "）", "muted");
      return;
    }
    const isNewer = r.relation === "newer";
    const relinkOnly = !!p.pendingLocal;
    const ok = await confirmDialog(
      relinkOnly ? "重新指定本地插件目录？" : (isNewer ? "覆盖更新本地插件？" : "将本地插件改为所选版本？"),
      "所选目录中插件 " + p.name + " 的版本为 " + verTxt +
        (relinkOnly ? "（当前为待重指定，尚未安装）" : "（当前 " + curTxt + "）") + "。\n\n" +
        (relinkOnly
          ? "将把该插件的安装来源重新指定为所选目录并恢复激活，期间服务短暂重启，失败会自动回退到待重指定状态。确认继续吗？"
          : "更新会改写该插件的安装来源为所选目录，期间服务短暂重启，失败会自动回退到更新前版本。确认继续吗？"),
      relinkOnly ? "重新指定目录" : (isNewer ? "覆盖更新" : "覆盖为所选版本")
    );
    if (!ok) return;
    setNote(item, "正在更新本地插件…", "muted");
    bindings().ApplyLocalPluginUpdate(p.id, r.path); // 完成/失败由 Go 弹窗提示，成功后 plugins:changed 刷新
  } catch (e) {
    setNote(item, fmt("更新失败：{0}", e && e.message ? e.message : e), "err");
  } finally {
    setTimeout(() => { item.querySelectorAll("button").forEach((b) => { b.disabled = false; }); }, 1500);
  }
}

/**
 * 单插件删除：点击即登记为待应用变更（**不弹确认框**——登记不执行、可随时「撤销」，
 * 真正不可逆的是「立即应用」，那一步仍有服务重启与回退兜底）。
 */
function doPluginRemove(p, item, delBtn) {
  item.querySelectorAll("button").forEach((b) => { b.disabled = true; });
  const label = p.pendingLocal ? tr("已登记：移除「待重指定」记录") : fmt("已登记：删除该插件（{0}）", pendingEffectText());
  setNote(item, label, "muted");
  bindings().RemovePlugin(p.id); // 结果由 Go 端 plugins:changed / plugin:op:done 刷新行状态
}

/** 撤销一条待应用变更（行内「撤销」）：变更未执行，撤销即从待应用区移除。 */
async function doPluginDiscard(p, item, btn) {
  btn.disabled = true;
  bindings().DiscardPendingPluginChange(p.id);
  await loadPlugins();
}

/**
 * 修复待删除插件的会话数据风险：给该插件写入的自定义事件补上「可跳过」标记，修复后这些历史会话
 * 在任何构建下都能正常打开（原日志逐个备份）。只动会话日志，不停服、不需要重启；插件仍在运行时
 * 可能继续写入新记录，此时提示残留数量，可在应用变更前再修复一次。
 */
async function doPluginRiskRepair(p, item, btn) {
  const a = bindings();
  if (!a) return;
  item.querySelectorAll("button").forEach((b) => { b.disabled = true; });
  setNote(item, "正在修复历史会话记录…", "muted");
  let r = null;
  try {
    r = await a.RepairPluginSessionEvents(p.id);
  } catch (e) {
    setNote(item, fmt("修复失败：{0}", e && e.message ? e.message : e), "err");
    setTimeout(() => { item.querySelectorAll("button").forEach((b) => { b.disabled = false; }); }, 1500);
    return;
  }
  const st = state.plugState[p.name] || (state.plugState[p.name] = {});
  st.atVersion = p.version || ""; // 结论对应「当时版本」：版本变化即过期（与 applyPlugState 同口径）
  st.fromRiskFix = true;
  if (!r || !r.ok) {
    st.note = fmt("修复失败：{0}", (r && r.reason) || tr("未知原因"));
    st.noteTone = "err";
  } else if ((r.remainingSessions || 0) > 0) {
    st.note = fmt("已修复 {0} 条记录；该插件仍在写入，还有 {1} 个会话存在风险", r.events, r.remainingSessions);
    st.noteTone = "warn";
  } else {
    st.note = fmt("已修复 {0} 个会话（{1} 条记录），删除后不再影响历史会话", r.files, r.events);
    st.noteTone = "ok";
  }
  await loadPlugins();
}

// ==================== 日志页 ====================

// 日志行时间戳/级别识别：时间戳统一显示为行头（muted），兼容斜杠（Go log / 子进程前缀
// "2026/09/04 14:00:13"）与横杠+T 两种写法；级别词着色便于扫读。
const LOG_TS_RE = /^\s*(\d{4}[-\/]\d{2}[-\/]\d{2}[ T]\d{2}:\d{2}:\d{2})\s+(.*)$/;

// 截图模式(EN)下展示的样例日志（真实运行日志为诊断内容，按 i18n 边界保留原文）
const SAMPLE_EN_LOG = [
  "2026/09/06 12:00:01 [INFO] dsh-systray v0.7.3 starting (pid 12345)",
  "2026/09/06 12:00:02 [INFO] runtime ready: node v24.9.0 / pnpm 10.34.5",
  "2026/09/06 12:00:03 [server] starting DeepSeek Harness web service on 127.0.0.1:3080",
  "2026/09/06 12:00:04 [server] service ready — open the Web UI",
  "2026/09/06 12:00:05 [INFO] tray menu refreshed (language: en)",
  "2026/09/06 12:00:06 [WARN] background update check skipped (dev build)",
  "2026/09/06 12:00:07 [INFO] latest records follow automatically; clear with one click",
];
let sampleLogInjected = false;
const LOG_LVL_RE = /^\[?(INFO|WARN|ERROR|DEBUG)\]?\s+(.*)$/;

function renderLog(lines) {
  const view = $("log-view");
  const hint = $("log-empty-hint");
  if (hint) hint.remove(); // 有新内容时移除"暂无日志"占位
  const atBottom = view.scrollHeight - view.scrollTop - view.clientHeight < 40;
  for (const ln of lines) {
    const div = document.createElement("div");
    div.className = "log-line";
    let rest = ln;
    let ts = "";
    const mTs = ln.match(LOG_TS_RE);
    if (mTs) {
      ts = mTs[1];
      rest = mTs[2];
    }
    let html = ts ? '<span class="log-ts">' + esc(ts) + " </span>" : "";
    const mLvl = rest.match(LOG_LVL_RE);
    if (mLvl) {
      html += '<span class="lvl-' + mLvl[1].toLowerCase() + '">' + esc(mLvl[1]) + "</span> " + esc(mLvl[2]);
    } else {
      html += esc(rest);
    }
    div.innerHTML = html;
    view.appendChild(div);
  }
  // 不再裁剪 DOM 行数：日志页须完整显示所有已写入内容（轮转归档 + 基础文件在重置时
  // 整体重载，视图规模受轮转窗口约束；裁剪会把最早写入的行从页面上静默丢掉）
  if (atBottom) view.scrollTop = view.scrollHeight;
}

function esc(s) {
  const d = document.createElement("div");
  d.textContent = s;
  return d.innerHTML;
}

function escAttr(s) { return esc(s).replace(/"/g, "&quot;"); }

async function pollLog() {
  if (state.shotPage && curLangCode() === "en") {
    // 截图模式 EN：渲染样例日志而非真实中文运行日志（真实日志内容非 UI 文案）
    if (!sampleLogInjected) {
      const view = $("log-view");
      view.textContent = "";
      renderLog(SAMPLE_EN_LOG);
      sampleLogInjected = true;
    }
    return;
  }
  const a = bindings();
  if (!a || state.page !== "logs" || !state.logName) return;
  // 首次加载：先渲染轮转归档（.3/.2/.1 由旧到新），再尾读基础文件——完整历史一次可见
  if (!state.logArchiveLoaded) {
    state.logArchiveLoaded = true;
    try {
      const arch = await a.ReadLogArchives();
      if (arch && arch.lines && arch.lines.length) renderLog(arch.lines);
    } catch (e) { console.error("ReadLogArchives", e); }
  }
  try {
    const tail = await a.ReadLogTail(state.logName, state.logOffset);
    if (tail.reset) {
      // 基础文件被轮转/清空：清空视图，重载归档并从头读基础文件（旧内容经归档完整保留）
      $("log-view").textContent = "";
      state.logOffset = 0;
      state.logArchiveLoaded = false;
      pollLog();
      return;
    }
    if (tail.lines && tail.lines.length) renderLog(tail.lines);
    state.logOffset = tail.nextOffset;
  } catch (e) { console.error("ReadLogTail", e); }
}

/** 日志页固定查看统一日志文件（下拉切换已移除：所有日志合并为 dsh-systray.log）。 */
function showLogHint(text) {
  const view = $("log-view");
  const old = $("log-empty-hint");
  if (old) old.remove();
  const div = document.createElement("div");
  div.id = "log-empty-hint";
  div.className = "log-line";
  div.style.opacity = ".6";
  div.textContent = text;
  view.appendChild(div);
}

function setLogFile(name) {
  state.logName = name || "";
  $("log-view").textContent = "";
  state.logOffset = 0;
  state.logArchiveLoaded = false; // 重新加载归档 + 基础文件（完整历史）
  (async () => {
    const a = bindings();
    if (!a) return;
    if (!state.logName) { $("log-path").textContent = ""; return; }
    $("log-path").textContent = await a.GetLogPath(state.logName);
    // 空态提示：文件不存在（尚未创建）或为空时给出明确说明，避免误判为 bug
    try {
      const files = await a.GetLogFiles();
      const f = files && files[0];
      if (f && !f.exists) showLogHint("日志文件尚未创建");
      else if (f && f.size === 0) showLogHint("日志文件暂无内容");
    } catch (e) { console.error("GetLogFiles", e); }
  })();
  pollLog();
}

function startLogPolling() {
  stopLogPolling();
  const a = bindings();
  if (!a) return;
  setLogFile(state.logName);
  state.logTimer = setInterval(pollLog, 2000);
}

function stopLogPolling() {
  if (state.logTimer) { clearInterval(state.logTimer); state.logTimer = null; }
}

function wireLogs() {
  $("btn-log-refresh").addEventListener("click", () => {
    $("log-view").textContent = "";
    state.logOffset = 0;
    state.logArchiveLoaded = false;
    pollLog();
  });
  $("btn-log-clear").addEventListener("click", async () => {
    await bindings().ClearLog(state.logName); // Go 侧同时清除轮转归档
    $("log-view").textContent = "";
    state.logOffset = 0;
    state.logArchiveLoaded = true; // 归档已删除，无需重载
  });
  // 日志路径点击复制：成功/失败时文案短暂变“已复制/复制失败”，1.6s 后还原原路径
  const pathEl = $("log-path");
  let copyTimer = null;
  pathEl.addEventListener("click", async () => {
    const txt = pathEl.textContent || "";
    if (!txt || txt === "—" || pathEl.dataset.copy) return; // 占位符 / 正在显示提示时不再复制
    let ok = false;
    try { await bindings().CopyToClipboard(txt); ok = true; }
    catch (e) { console.error("CopyToClipboard", e); }
    const mark = tr(ok ? "已复制" : "复制失败");
    pathEl.dataset.copy = txt; // 保存原路径（刷新会重写文本并覆盖该标记，还原时校验）
    pathEl.textContent = mark;
    pathEl.classList.add(ok ? "copied" : "copied-fail");
    clearTimeout(copyTimer);
    copyTimer = setTimeout(() => {
      const orig = pathEl.dataset.copy || "";
      delete pathEl.dataset.copy;
      pathEl.classList.remove("copied", "copied-fail");
      if (orig && pathEl.textContent === mark) pathEl.textContent = orig; // 仅当仍显示提示才还原，避免覆盖刷新后的新路径
    }, 1600);
  });
}

// ==================== 导出页 ====================

/** 目录去重键：Windows 忽略大小写、去尾部分隔符，避免同一目录被重复添加。 */
function normalizeDirKey(p) {
  let s = p.replace(/[\\/]+$/, "");
  if (/win/i.test(navigator.platform || navigator.userAgent)) s = s.toLowerCase();
  return s;
}

function renderExportRows() {
  const a = bindings();
  const wrap = $("exp-rows");
  wrap.innerHTML = "";
  const opts = [
    { kind: "sessions", label: "所有历史会话", sub: "sessions.zip · ~/.dsh/sessions" },
    { kind: "plugins", label: "已安装的插件", sub: "plugins.zip · 全部环境（Web UI / Desktop UI）" },
    { kind: "files", label: "需要打包的文件目录", sub: "files.zip" },
  ];
  for (const o of opts) {
    const div = document.createElement("div");
    div.className = "exp-item" + (state.expSelected[o.kind] ? " selected" : "");
    div.dataset.kind = o.kind;
    div.innerHTML =
      '<div class="check">✓</div>' +
      "<div><div class=\"exp-label\">" + tr(o.label) + "</div><div class=\"exp-sub\">" + tr(o.sub) + "</div></div>";
    div.addEventListener("click", () => {
      state.expSelected[o.kind] = !state.expSelected[o.kind];
      renderExportRows();
      updateExportHint();
    });
    wrap.appendChild(div);
    // 文件目录二级列表：依附在「需要打包的文件目录」选项下方，无勾选框、保留移除按钮
    if (o.kind === "files") {
      const sub = document.createElement("div");
      sub.className = "exp-dirs" + (state.expSelected.files ? "" : " dim");
      for (const d of state.expDirs) {
        const row = document.createElement("div");
        row.className = "exp-dir-item";
        row.innerHTML =
          '<div class="exp-dir-label" title="' + escAttr(d) + '">' + esc(d) + "</div>" +
          '<button class="btn btn-ghost btn-sm" data-remove-dir="' + escAttr(d) + '">' + tr('移除') + '</button>';
        row.querySelector("[data-remove-dir]").addEventListener("click", (e) => {
          e.stopPropagation();
          state.expDirs = state.expDirs.filter((x) => x !== d);
          renderExportRows();
          updateExportHint();
        });
        sub.appendChild(row);
      }
      wrap.appendChild(sub);
    }
  }
  updateExportHint();
}

function updateExportHint() {
  const n = Object.values(state.expSelected).filter(Boolean).length;
  $("exp-hint").textContent = n > 0
    ? fmt("已选 {0} 项{1}", n, state.expDirs.length ? fmt("（含 {0} 个目录）", state.expDirs.length) : "")
    : tr("请至少勾选一项，或为「文件目录」添加目录");
}

/** 导出进度弹层（居中弹出；进度不随页面滚动隐藏）。 */
let lastExportPath = "";

function showExportModal(text, pct) {
  $("exp-modal").classList.remove("hidden");
  $("exp-modal-close").classList.add("hidden");
  if (typeof text === "string") $("exp-modal-text").textContent = text;
  if (typeof pct === "number") $("exp-modal-fill").style.width = Math.round(pct * 100) + "%";
}

function hideExportModal() {
  $("exp-modal").classList.add("hidden");
  $("exp-modal-close").classList.add("hidden");
}

function wireExport() {
  renderExportRows();
  $("btn-pick-dir").addEventListener("click", async () => {
    const btn = $("btn-pick-dir");
    btn.disabled = true;
    try {
      const p = await bindings().PickExportDir();
      if (p) {
        const key = normalizeDirKey(p);
        if (!state.expDirs.some((x) => normalizeDirKey(x) === key)) {
          state.expDirs.push(p);
          state.expSelected.files = true;
          renderExportRows();
        } else {
          $("exp-hint").textContent = "该目录已添加：" + p;
        }
      }
    } finally {
      btn.disabled = false;
    }
  });
  $("btn-export").addEventListener("click", async () => {
    const any = Object.values(state.expSelected).some(Boolean);
    if (!any) { $("exp-hint").textContent = "请至少勾选一项"; return; }
    $("btn-export").disabled = true;
    $("exp-open").classList.add("hidden");
    // 先让用户选择导出压缩包的保存位置（SaveFileDialog）
    const savePath = await bindings().PickSavePath();
    if (!savePath) {
      $("btn-export").disabled = false;
      return;
    }
    showExportModal(tr("正在准备导出…"), 0);
    await bindings().StartExport(
      state.expSelected.sessions,
      state.expSelected.plugins,
      state.expSelected.files,
      state.expDirs,
      savePath
    );
  });
  $("exp-open").addEventListener("click", () => {
    if (lastExportPath) bindings().OpenExportDir(lastExportPath);
  });
  $("exp-modal-close").addEventListener("click", hideExportModal);
}

// ==================== 导入页（逐项恢复：每行独立进度/状态/取消） ====================

/** 解析汇总提示（#imp-hint 仅用于压缩包解析提示；行内状态走各行的 data-ptext）。 */
function setImpHint(text, isErr) {
  $("imp-hint").textContent = text;
  $("imp-hint").classList.toggle("err", !!isErr);
}

/** 取某恢复项的运行态（不存在则初始化）。 */
function impSt(kind) {
  if (!state.imp[kind]) state.imp[kind] = { busy: false, text: "", pct: 0, pending: false, watch: 0 };
  return state.imp[kind];
}

/** 行内状态文字：tone = "" | ok | err | muted */
function impRowText(kind, text, tone) {
  text = tr(text);
  const st = impSt(kind);
  st.text = text || "";
  const el = document.querySelector('[data-ptext="' + kind + '"]');
  if (!el) return;
  el.textContent = st.text;
  el.className = "imp-ptext" + (tone === "ok" || tone === "err" ? " " + tone : "");
}

/** 行内进度：busy=true 显示进度条区域并更新填充，false 收起。 */
function impRowBusy(kind, busy, text, pct) {
  const st = impSt(kind);
  st.busy = !!busy;
  const row = document.querySelector('[data-ikind="' + kind + '"]');
  if (!row) return;
  const pr = row.querySelector("[data-prow]");
  const fill = row.querySelector("[data-pfill]");
  if (pr) pr.classList.toggle("hidden", !busy);
  if (fill && !busy) fill.style.width = "0%";
  if (busy) {
    if (typeof pct === "number") fill.style.width = Math.round(pct * 100) + "%";
    if (text) impRowText(kind, text, "");
  }
  syncImpRow(kind);
}

/** 按当前状态同步该行的「恢复/取消/已完成」按钮可见性与禁用态。 */
function syncImpRow(kind) {
  const row = document.querySelector('[data-ikind="' + kind + '"]');
  syncImportPickBtn(); // 任一行的 busy 变化都影响「添加压缩包…」的可用性
  if (!row) return;
  const done = !!state.impDone[kind];
  const st = impSt(kind);
  const restoreBtn = row.querySelector("[data-restore]");
  const cancelBtn = row.querySelector("[data-cancel]");
  const badge = row.querySelector("[data-okbadge]");
  if (restoreBtn) {
    restoreBtn.classList.toggle("hidden", done);
    restoreBtn.disabled = !!state.impHealAll || !!st.busy;
  }
  if (badge) badge.classList.toggle("hidden", !done);
  if (cancelBtn) {
    if (st.busy && !done) {
      cancelBtn.classList.remove("hidden");
      cancelBtn.disabled = !!state.impHealAll;
      cancelBtn.textContent = state.impHealAll ? tr("更新中不可取消") : tr("取消恢复");
    } else {
      cancelBtn.classList.add("hidden");
    }
  }
  row.classList.toggle("imp-item-done", done);
}

/** 共享自愈开始/结束：全局禁用各「恢复」按钮（自愈不可打断）。 */
function syncImpHealUI(on) {
  state.impHealAll = !!on;
  document.querySelectorAll("[data-ikind]").forEach((row) => {
    const k = row.getAttribute("data-ikind");
    syncImpRow(k);
  });
  syncImportPickBtn(); // 自愈阶段同样禁止重新添加压缩包
  if (on) {
    impRowText("plugins", "正在启动服务并校验插件兼容性…", "");
  }
}

function renderImportRows() {
  const wrap = $("imp-rows");
  wrap.innerHTML = "";
  if (!state.impItems.length) {
    setImpHint("", false);
    return;
  }
  setImpHint(fmt("共 {0} 个可恢复项", state.impItems.length), false);
  for (const it of state.impItems) {
    const div = document.createElement("div");
    div.className = "imp-item";
    div.dataset.ikind = it.kind;
    div.innerHTML =
      '<div class="imp-head">' +
      '<div class="imp-intro-main"><div class="exp-label">' + esc(it.label) + "</div>" +
      (it.size ? '<div class="exp-sub">' + fmtSize(it.size) + "</div>" : "") + "</div>" +
      '<div class="imp-actions">' +
      '<span data-okbadge class="imp-done hidden">' + tr("✓ 已完成") + '</span>' +
      '<button class="btn btn-primary btn-xs" data-restore="' + escAttr(it.kind) + '">' + tr('恢复') + '</button>' +
      '<button class="btn btn-outline btn-xs hidden" data-cancel="' + escAttr(it.kind) + '">' + tr('取消恢复') + '</button>' +
      "</div></div>" +
      '<div class="imp-progress-row hidden" data-prow>' +
      '<div class="imp-track"><div class="imp-fill" data-pfill></div></div>' +
      '<div class="imp-ptext muted" data-ptext="' + escAttr(it.kind) + '"></div>' +
      "</div>";
    div.querySelector("[data-restore]").addEventListener("click", () => impRestore(it));
    div.querySelector("[data-cancel]").addEventListener("click", () => impCancel(it.kind));
    // 插件项：小字点明恢复范围——包内每个环境各自落回自己的 profile（web → profiles/web、
    // desktop → profiles/desktop，见 exportimport.go 的 importPluginTargets）。
    if (it.kind === "plugins") {
      const note = document.createElement("div");
      note.className = "row-sub imp-scope-note";
      note.textContent = tr("插件按包内环境分别恢复到对应环境（Web UI / Desktop UI 各自独立）；包里没有的环境不受影响。");
      div.querySelector(".imp-head").insertAdjacentElement("afterend", note);
    }
    wrap.appendChild(div);
    // 恢复完成/仍忙碌的行重渲染后恢复状态
    if (state.impDone[it.kind]) syncImpRow(it.kind);
  }
}

/** 单行「恢复」：预览（含冲突弹窗）→ ApplyRestore（逐项状态由 import:* 事件驱动）。 */
async function impRestore(it) {
  const kind = it.kind;
  if (state.impHealAll) return; // 自愈中不可开始新恢复
  const st = impSt(kind);
  if (st.busy) return;
  try {
    impRowBusy(kind, true, tr("正在准备恢复…"), 0);
    // 1) 准备 + 冲突检测（files 类目此时由后端弹解压位置选择）
    const preview = await bindings().PreviewRestore(kind);
    if (!preview || preview.canceled) { impRowBusy(kind, false, "", 0); return; } // 用户取消选择
    if (preview.error) { const er = fmt("无法恢复：{0}", preview.error); impRowBusy(kind, false, er, 0); impRowText(kind, er, "err"); return; }
    // 2) 冲突处理：取消=不执行；跳过=保留现有只补缺失；覆盖=备份并替换
    let overwrite = true;
    if (preview.conflicts > 0) {
      const detail = (preview.tops || []).slice(0, 3).join("、");
      const choice = await confirmDialog3(
        "检测到数据冲突",
        "「" + it.label + "」与现有内容存在 " + preview.conflicts + " 项冲突" +
          (detail ? "（" + detail + (preview.conflicts > 3 ? " 等" : "") + "）" : "") +
          "。\n\n「跳过」将保留现有文件、只补缺失项；「覆盖并恢复」会备份并替换现有内容。",
        "覆盖并恢复",
        "跳过"
      );
      if (choice === "cancel") { impRowBusy(kind, false, "", 0); return; }
      overwrite = choice === "ok";
      if (!overwrite) {
        impRowText(kind, fmt("已选择跳过 {0} 项冲突，现有内容将保留。", preview.conflicts), "muted");
      }
    }
    // 3) 执行恢复：结果由 import:done 统一收尾
    impRowBusy(kind, true, tr("正在准备恢复…"), 0);
    armImpWatch(kind, 300000); // 兜底
    await bindings().ApplyRestore(kind, overwrite);
  } catch (e) {
    impRowBusy(kind, false, "", 0);
    impRowText(kind, fmt("恢复失败：{0}", e && e.message ? e.message : e), "err");
  }
}

/** 单行「取消恢复」：Go 端受理后该行立即解锁（后端回退在后台），结果由 import:done 收尾。 */
async function impCancel(kind) {
  if (state.impHealAll) return; // 自愈阶段不可取消
  let r = "";
  try {
    r = await bindings().CancelRestore(kind);
  } catch (e) { /* 忽略 */ }
  if (state.impHealAll || r === "healing") {
    syncImpHealUI(true);
    impRowText(kind, tr("服务正在启动校验，不可取消…"), "muted");
    return;
  }
  if (r !== "ok") {
    impRowBusy(kind, false, "", 0);
    impRowText(kind, tr("当前没有进行中的恢复任务"), "muted");
    return;
  }
  // 已受理：**保持该行占用**直到 import:done——后端还要回退文件并重启服务，
  // 提前解锁会让用户秒点「恢复」，而队列里旧任务尚未出队，被后端拒绝
  // （2026-09-13 现场问题：取消后立刻重新恢复，报「该恢复项已在处理中」）。
  const st = impSt(kind);
  st.busy = true;
  st.pending = true;
  impRowBusy(kind, true, "", 0);
  impRowText(kind, tr("已请求取消，正在回退到恢复前状态…"), "muted");
  armImpWatch(kind, 90000);
}

/** 每行兜底：长时间未收到 import:done 时复位该行。
 *  复位前先向后端确认该 kind 是否仍在处理（取消后的回退可能较慢），
 *  仍在处理就只续期、不解除占用，避免用户重试时被后端拒绝。 */
function armImpWatch(kind, ms) {
  const st = impSt(kind);
  clearTimeout(st.watch);
  st.watch = setTimeout(async () => {
    if (state.impHealAll) {
      impRowText(kind, "服务启动校验仍在进行（不可中断），请继续等待…", "muted");
      armImpWatch(kind, 60000);
      return;
    }
    let stillBusy = false;
    try {
      const kinds = await bindings().ImportInflight();
      stillBusy = !!kinds && kinds.indexOf(kind) >= 0;
    } catch (e) { /* 查询失败：按本地状态处理 */ }
    if (stillBusy) {
      impRowText(kind, st.pending ? tr("仍在回退到恢复前状态，请稍候…") : tr("服务端仍在处理，请稍候…"), "muted");
      armImpWatch(kind, 60000);
      return;
    }
    st.pending = false;
    if (!st.busy) return;
    impRowBusy(kind, false, "", 0);
    impRowText(kind, "恢复未在预期时间内收到服务端结果，界面已复位；若服务端仍在处理请稍候再试（日志页可查）。", "muted");
  }, ms);
}

function fmtSize(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}

function wireImport() {
  $("btn-import-pick").addEventListener("click", async () => {
    // 恢复进行中：不允许重新添加压缩包——重新解析会把导入项状态整体复位，
    // 与后台正在跑的恢复任务（按旧包内容执行）错位（2026-09-13 现场问题）。
    if (importBusy()) {
      setImpHint(tr("正在恢复导入项，恢复期间不能重新添加压缩包。"), true);
      return;
    }
    setImpHint("正在解析压缩包…", false);
    try {
      const res = await bindings().ImportPick();
      if (!res) { renderImportRows(); return; }
      state.impItems = res.items || [];
      state.impDone = {};
      state.imp = {};
      state.impHealAll = false;
      $("imp-path").textContent = res.path || "";
      $("imp-path").classList.remove("hidden");
      renderImportRows();
    } catch (e) {
      // 后端拒绝（恢复中）时保留当前导入项状态，只提示原因
      const msg = (e && e.message ? e.message : String(e));
      if (msg.indexOf("恢复") >= 0 || msg.indexOf("restore") >= 0 || msg.indexOf("Restore") >= 0) {
        setImpHint(msg, true);
        return;
      }
      setImpHint("解析失败：" + msg, false);
      $("imp-rows").innerHTML = "";
      state.impItems = [];
    }
  });
}

/** 是否有恢复任务在跑（含不可中断的共享自愈阶段）——决定「添加压缩包…」是否可用。 */
function importBusy() {
  if (state.impHealAll) return true;
  for (const k in state.imp) {
    if (state.imp[k] && state.imp[k].busy) return true;
  }
  return false;
}

/** 同步「添加压缩包…」按钮可用性与提示：恢复期间禁用（文案解释原因），并阻止状态复位。 */
function syncImportPickBtn() {
  const btn = $("btn-import-pick");
  if (!btn) return;
  const busy = importBusy();
  btn.disabled = busy;
  btn.title = busy ? tr("正在恢复导入项，恢复期间不能重新添加压缩包。") : "";
  return busy;
}

// ==================== 帮助页 ====================

// 帮助页操作绑定：「打开 Web UI」（带最新访问令牌）、「复制访问链接」（短暂成功/失败反馈，
// 与日志页路径复制同一模式）与「重启后台服务」（拿不到令牌链接时就地重新生成）。
// 按钮可用性/警告提示由 refreshHelpState 按服务状态刷新。
function wireHelp() {
  $("btn-help-open").addEventListener("click", async () => {
    const btn = $("btn-help-open");
    if (btn.disabled) return;
    try { await bindings().OpenWebUI(); } catch (e) { console.error("OpenWebUI", e); }
  });
  const copyBtn = $("btn-help-copy");
  let timer = null;
  copyBtn.addEventListener("click", async () => {
    if (copyBtn.disabled || copyBtn.dataset.busy) return;
    copyBtn.dataset.busy = "1"; // 复制异步进行中：忽略连点
    let ok = false;
    try {
      const url = await bindings().WebTokenURL();
      if (url) { await bindings().CopyToClipboard(url); ok = true; }
    } catch (e) { console.error("copy web access link", e); }
    delete copyBtn.dataset.busy;
    const orig = copyBtn.dataset.orig || copyBtn.textContent;
    const mark = tr(ok ? "已复制访问链接" : "复制失败");
    copyBtn.dataset.orig = orig;
    copyBtn.textContent = mark;
    copyBtn.classList.add(ok ? "copied" : "copied-fail");
    clearTimeout(timer);
    timer = setTimeout(() => {
      copyBtn.classList.remove("copied", "copied-fail");
      // 仅当仍显示反馈文案时还原（语言切换已重写文案则不覆盖）
      if (copyBtn.textContent === mark) copyBtn.textContent = copyBtn.dataset.orig || orig;
      delete copyBtn.dataset.orig;
    }, 1600);
  });
  // 帮助页（desktop 模式专有）：一键切换到 Web UI——与「常规」页下拉同一条路径（写入偏好 +
  // 启动后台服务 + 托盘「打开」改为指向网页端），先确认再执行；切换成功后本卡片随启动方式
  // 变为 web 而隐藏，页面自动显示 Web 侧帮助。
  const switchWebBtn = $("btn-help-switch-web");
  if (switchWebBtn) {
    switchWebBtn.addEventListener("click", async () => {
      if (switchWebBtn.disabled) return;
      const ok = await confirmDialog(tr("切换到 Web UI？"),
        tr("将启动托盘自带的 Web 服务（首次启动可能需要下载运行环境，耗时数分钟），并把托盘「打开」改为指向 Web UI。确认切换？"),
        tr("切换"));
      if (!ok) return;
      switchWebBtn.disabled = true;
      const busy = $("help-switch-busy");
      if (busy) {
        busy.textContent = tr("正在切换到 Web UI 并启动后台服务…");
        busy.classList.remove("hidden");
      }
      try { await bindings().SetLaunchTarget("web"); } catch (e) { console.error("SetLaunchTarget", e); }
      refreshConfig();
      refreshService();
      setTimeout(() => {
        switchWebBtn.disabled = false;
        if (busy) { busy.textContent = ""; busy.classList.add("hidden"); }
      }, 3000);
    });
  }
  // 重启后台服务：服务由本程序重新拉起后会被定点捕获新令牌（300ms 轮询），
  // 故完成后补几次状态刷新，让「复制访问链接」尽快恢复可用。
  const restartBtn = $("btn-help-restart");
  restartBtn.addEventListener("click", async () => {
    if (restartBtn.disabled || helpRestarting) return;
    helpRestarting = true;
    refreshHelpState(); // 三个操作立即锁住（重启期间状态会短暂变为非 running）
    setHelpBusy(tr("正在重启后台服务…"));
    let ok = false;
    try { ok = await bindings().RestartService(); } catch (e) { console.error("RestartService", e); }
    helpRestarting = false;
    if (!ok) {
      setHelpBusy(tr("重启失败，请查看日志"), true);
      refreshService();
      setTimeout(() => setHelpBusy(""), 4000);
      return;
    }
    setHelpBusy("");
    refreshService();
    setTimeout(refreshService, 1200);
    setTimeout(refreshService, 3000);
    setTimeout(refreshService, 6000);
  });
}

// ==================== 事件监听（Go → JS） ====================

function wireEvents() {
  // 语言切换：Go 已持久化并更新 curLang；此处就地双向切换（静态层快照还原 + 动态块重渲染 +
  // 页标题/下拉同步），不再整页 reload（reload 后 init 无条件显示 splash，设置视图仅由
  // splash:done / ui:show-settings 事件驱动出现，语言切换后会永久卡在 splash）。
  EventsOn("lang:changed", (d) => {
    if (!d) return;
    if (state.cfg) { state.cfg.language = d.pref; state.cfg.curLang = d.curLang; }
    const ls = $("sel-lang");
    if (ls && d.pref) ls.value = d.pref;
    applyStaticI18n();                 // 静态层 + 动态块按新语言重渲染
    showPage(state.page || "general"); // 页标题 / 日志轮询按新语言复位
  });

  // 账号/同步状态变化（登录、后台同步完成、令牌失效）→ 刷新「数据同步」页与左侧小字状态
  EventsOn("account:changed", () => { refreshSync(); refreshFiles(); });
  // 文件同步状态变化（扫描/上传/对账/应用）→ 刷新文件卡（容量条、列表、待应用）
  EventsOn("files:changed", () => { refreshFiles(); });

  // 同步改动应用结束（成功/取消/失败，Go 侧统一 emit sync:apply:done）：
  // 收起进度视图、复位按钮、刷新插件与服务状态，并把结果写进页内提示。
  EventsOn("sync:apply:done", (d) => {
    state.splashMode = "";
    showSettings();
    const btn = $("btn-sync-apply");
    if (btn) btn.disabled = false;
    loadPlugins();     // 应用可能新增/更新/移除插件
    refreshService();  // 应用期间服务被停过并重启
    const err = d && d.error;
    if (err) {
      syncHint("sync-account-hint", err, true);
    } else if (d && d.canceled) {
      syncHint("sync-account-hint", tr("已取消应用，剩余改动留待生效"));
    } else {
      syncHint("sync-account-hint", fmt("同步改动已生效：应用 {0} 项", (d && d.applied) || 0));
    }
    refreshSync();
  });

  EventsOn("splash:progress", (d) => {
    if (!d) return;
    // 相位复位事件（空文本 + 0 进度）只用于后端复位相位，不应把已收起的进度视图重新拉起：
    // 它若晚于 update:done 到达，窗口就停在进度视图（表现＝更新完成后不退出更新窗口）。
    const phaseReset = !d.text && d.pct === 0;
    if ((d.phase === "update" || d.phase === "sync") && !phaseReset && state.splashMode !== d.phase) {
      showSplash(d.phase, d.text || tr(d.phase === "sync" ? "正在应用同步改动…" : "正在更新…"));
    }
    if (d.phase === "startup" && !phaseReset) {
      $("splash").classList.remove("hidden");
      $("settings").classList.add("hidden");
      state.splashMode = "startup";
    }
    if (d.text) $("splash-status").textContent = d.text;
    if (typeof d.pct === "number") $("splash-fill").style.width = Math.round(d.pct * 100) + "%";
  });

  EventsOn("splash:done", () => {
    // 启动完成：切换设置视图（窗口随后由 Go 侧隐藏）；截图模式在此后重放滚动位置
    applyShotScroll();
    showSettings();
    refreshService();
  });

  EventsOn("ui:show-splash", () => showSplash("startup", tr("正在准备运行环境…")));
  // 托盘每次重开设置窗口：刷新版本与插件清单（更新/重置等操作可能在窗口隐藏期间完成，必须强一致重取）
  EventsOn("ui:show-settings", () => { showSettings(); refreshConfig(); refreshService(); refreshVersions(); loadPlugins(); });

  EventsOn("export:progress", (d) => {
    if (!d) return;
    showExportModal(d.text || "", d.pct);
  });

  EventsOn("export:done", (d) => {
    $("btn-export").disabled = false;
    if (d && d.error) {
      $("exp-hint").textContent = fmt("导出失败：{0}", d.error);
      showExportModal(fmt("导出失败：{0}", d.error), 0);
      $("exp-modal-close").classList.remove("hidden");
    } else if (d && d.path) {
      $("exp-hint").textContent = fmt("导出完成：{0}", d.path);
      lastExportPath = d.path;
      $("exp-open").classList.remove("hidden");
      showExportModal(tr("导出完成 ✓"), 1);
      setTimeout(hideExportModal, 1600);
    }
  });

  EventsOn("import:progress", (d) => {
    if (!d || !d.kind) return;
    if (d.healing) {
      // 进入批末启动校验：所有「恢复」按钮暂时禁用（不可打断），行内提示同步
      syncImpHealUI(true);
      impRowText(d.kind, d.text || "正在启动服务并校验插件兼容性…", "muted");
    } else {
      const st = impSt(d.kind);
      st.pct = d.pct || 0;
      st.busy = true;
      const row = document.querySelector('[data-ikind="' + d.kind + '"]');
      const fill = row ? row.querySelector("[data-pfill]") : null;
      if (row) row.querySelector("[data-prow]").classList.remove("hidden");
      if (fill) fill.style.width = Math.round((d.pct || 0) * 100) + "%";
      if (d.hint && d.text) impRowText(d.kind, d.text, "");
      else if (d.text && !state.impDone[d.kind]) impRowText(d.kind, d.text, "muted");
      syncImpRow(d.kind);
    }
  });

  EventsOn("import:done", (d) => {
    if (!d || !d.kind) return;
    const st = impSt(d.kind);
    // 入队被拒（同 kind 仍在上一次恢复里没收尾，例如刚取消、回退还在跑）：
    // 这不是「恢复失败」，而是该项仍被占用——保持占用并说明，等真正完成再解锁
    // （2026-09-13 现场问题：取消后立刻重试，报「该恢复项已在处理中」且按钮看似可用）。
    if (d.error && String(d.error).indexOf("已在处理中") >= 0) {
      st.busy = true;
      st.pending = false;
      impRowBusy(d.kind, true, "", 0);
      impRowText(d.kind, tr("上一项恢复仍在收尾，请稍候再试。"), "muted");
      armImpWatch(d.kind, 60000);
      return;
    }
    clearTimeout(st.watch);
    st.busy = false;
    st.pending = false;
    syncImpHealUI(false); // 自愈结束：恢复按钮重新可用（若仍有多项在跑由各自 busy 状态保持禁用）
    let msg;
    let tone = "muted";
    if (d.error) {
      tone = "err";
      msg = fmt("恢复失败：{0}", d.error) + (d.note ? "。" + d.note : "");
    } else if (d.canceled) {
      msg = (d.note ? fmt("已取消恢复{0}", "。" + d.note) : tr("已取消恢复") + tr("，已回退到恢复前状态"));
    } else {
      tone = "ok";
      msg = tr("恢复完成 ✓") + (d.note ? "。" + d.note : "");
      state.impDone[d.kind] = true; // 完成标记：该行显示 ✓ 已完成
    }
    impRowBusy(d.kind, false, "", 0);
    impRowText(d.kind, msg, tone);
    if (d.kind === "plugins") {
      // 导入会改变插件的安装/版本/激活状态（adopt 副本、待重指定、重新装回等），此前缓存的
      // 行内结论（如「已删除」「已更新 v1.7.17」「有新版本」）一律过期——整体清空后重载，
      // 避免重新导入的插件仍显示「已删除」这类与现状不符的状态。
      state.plugState = {};
      loadPlugins();
    }
  });

  EventsOn("service:restart", (d) => {
    if (d && d.stage) $("svc-sub").textContent = d.stage;
  });

  // 更新流程结束（成功/取消/失败，Go 侧统一 emit update:done）：回到设置视图并恢复
  // 「更新」按钮，用户可再次检查/发起更新。此前 Go 从未发出该事件，按钮取消后一直不可用。
  EventsOn("update:done", () => {
    // 完成即复位进度视图状态：此后任何迟到的相位复位事件都不再把窗口拉回进度视图
    // （更新完成后不退出更新窗口的另一处兜底）。
    state.splashMode = "";
    showSettings();
    const hub = $("btn-harness-update");
    if (hub) hub.disabled = false;
    const sysBtn = $("btn-systray-update");
    if (sysBtn) sysBtn.disabled = false;
    refreshVersions();
    refreshService(); // 更新/重启期间服务状态变过：同步圆点与文案，窗口重新打开即为最新
  });

  // 插件更新完成：刷新插件列表（版本/来源状态可能变化）
  EventsOn("plugins:changed", () => loadPlugins());

  // 待应用变更集合变化（登记/撤销/应用）：刷新提示条与行标记。
  EventsOn("plugin:pending:changed", () => loadPendingChanges());

  // 「立即应用」：把全部待应用变更交给 Go 端批处理（整批一次重启校验）。
  const applyPending = $("btn-apply-pending");
  if (applyPending) {
    applyPending.addEventListener("click", () => {
      applyPending.disabled = true;
      bindings().ApplyPendingPluginChanges();
      setTimeout(() => { applyPending.disabled = false; }, 1500);
    });
  }

  // 「全部撤销」：批量增删上百个插件时不必逐条点；带二次确认（撤销不改动已安装内容，
  // 仅清空待应用清单）。
  const discardAll = $("btn-discard-all-pending");
  if (discardAll) {
    discardAll.addEventListener("click", async () => {
      const n = (state.plugPending || []).length;
      if (!n) return;
      const ok = await confirmDialog("撤销全部待应用变更？",
        fmt("将撤销 {0} 项尚未生效的变更（更新/删除），已安装的插件不受影响。确认撤销吗？", n), "全部撤销");
      if (!ok) return;
      discardAll.disabled = true;
      bindings().DiscardAllPendingPluginChanges();
      setTimeout(() => { discardAll.disabled = false; }, 1500);
    });
  }

  // 单插件操作收尾（更新/删除/启用）：结果事件驱动行状态刷新——
  //  - 成功：清除 plugState 残留的「有新版本」提示（否则更新后行内仍假提示可更新），显示已更新版本；
  //  - 失败：行内显示失败原因（此前失败路径无任何事件，行永久停留「正在更新插件…」且无原因）。
  // loadPlugins 重渲染时由 applyPlugState 恢复本事件写入的行状态；同时记录 atVersion，
  // 使结论在该插件版本再次变化（外部更新 / 重新导入 / 回滚）时自动过期，不再残留「已删除」等旧状态。
  EventsOn("plugin:op:done", (d) => {
    if (!d || !d.name) return;
    const st = state.plugState[d.name] || (state.plugState[d.name] = {});
    const row = (state.plugRows || []).find((r) => r.name === d.name);
    // 删除类结论只有「该行不存在」时才成立 → atVersion 记 ""（行被重新装回/导入即过期）
    st.atVersion = d.op === "remove" ? "" : (d.version || (row && row.version) || "");
    if (d.ok) {
      let label = d.op === "remove" ? "已删除" : (d.op === "enable" ? "已启用" : "已更新");
      if (d.version) label += " " + vtag(d.version);
      if (d.reason) label += "（" + d.reason + "）";
      st.note = label;
      st.noteTone = "ok";
      st.upShow = false;
      st.upLatest = "";
    } else {
      const verb = d.op === "remove" ? "删除失败" : (d.op === "enable" ? "启用失败" : "更新失败");
      st.note = verb + "：" + (d.reason || "");
      if (st.note.length > 140) st.note = st.note.slice(0, 137) + "…";
      st.noteTone = "err";
      st.upShow = true; // 保留「更新」按钮便于修复后重试
    }
    loadPlugins();
  });
}

// 取消进度流程：更新 → CancelUpdate；同步改动应用 → CancelSyncApply（已应用的保留）。
function wireSplashCancel() {
  const btn = $("splash-cancel");
  btn.addEventListener("click", () => {
    btn.disabled = true; // 防重复点击（Go 端「替换重启」阶段也会忽略取消）
    $("splash-status").textContent = tr("正在取消…");
    const g = bindings();
    if (state.splashMode === "sync") {
      if (typeof g.CancelSyncApply === "function") g.CancelSyncApply();
      return;
    }
    g.CancelUpdate();
  });
}

// ==================== 确认弹层 ====================

/**
 * 三按钮确认弹层。返回 'ok' | 'skip' | 'cancel'：
 *  - okLabel 提供时为绿色危险/主按钮「覆盖并恢复」；
 *  - skipLabel 提供时显示中间按钮「跳过（保留现有）」；
 *  - 点「取消」或遮罩外不会执行任何动作，返回 'cancel'（调用方必须直接 return）。
 */
function confirmDialog3(title, msg, okLabel, skipLabel) {
  return new Promise((resolve) => {
    $("modal-title").textContent = tr(title) || tr("确认操作");
    $("modal-msg").textContent = tr(msg) || "";
    $("modal-ok").textContent = tr(okLabel) || tr("确定");
    const skipBtn = $("modal-skip");
    skipBtn.classList.toggle("hidden", !skipLabel);
    if (skipLabel) skipBtn.textContent = tr(skipLabel);
    $("modal").classList.remove("hidden");
    const done = (val) => {
      $("modal").classList.add("hidden");
      $("modal-cancel").removeEventListener("click", onCancel);
      $("modal-skip").removeEventListener("click", onSkip);
      $("modal-ok").removeEventListener("click", onOk);
      resolve(val);
    };
    const onCancel = () => done("cancel");
    const onSkip = () => done("skip");
    const onOk = () => done("ok");
    $("modal-cancel").addEventListener("click", onCancel);
    $("modal-skip").addEventListener("click", onSkip);
    $("modal-ok").addEventListener("click", onOk);
    $("modal-cancel").focus();
  });
}

/** 显示双按钮确认弹层（兼容既有调用），返回 Promise<boolean>（确定 true / 取消 false）。 */
function confirmDialog(title, msg, okLabel) {
  return confirmDialog3(title, msg, okLabel, null).then((v) => v === "ok");
}

// ==================== 启动 ====================

// ==================== 数据同步（账号） ====================
//
// 页面三态：未登录（邮箱验证码入口）/ 已登录（账号 + 同步状态）/ 重启生效提示（拉取到改动后常驻）。
// 状态来源是 Go 侧的 AccountStatus 快照（登录态、游标、待上报数、同步中/失败）。

let syncResendTimer = null;

/** syncFmtTime 把 Unix 秒格式化为本地「MM-DD HH:mm」。 */
function syncFmtTime(unixSec) {
  if (!unixSec) return "—";
  const d = new Date(unixSec * 1000);
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** syncHint 页内提示（isError 时用语义红）。 */
function syncHint(id, text, isError) {
  const el = $(id);
  if (!el) return;
  el.textContent = text || "";
  el.classList.toggle("is-error", !!isError);
}

/** syncStatusView 已登录时的状态行文案与语义色类。 */
function syncStatusView(st) {
  if (st.syncing) return { text: tr("同步中…"), cls: "sync-state-busy" };
  if (st.syncError) return { text: fmt("同步失败：{0}", st.syncError), cls: "sync-state-error" };
  if (st.pendingOps > 0) return { text: fmt("待同步 {0} 项", st.pendingOps), cls: "sync-state-pending" };
  // 有待生效改动时**不能**说「已同步」：改动还没落到本机，点「重启生效」才应用（问题⑥）
  if (st.pendingApply) return { text: applyPendingText(st), cls: "sync-state-pending" };
  if (st.applyError) return { text: fmt("上次应用失败：{0}", st.applyError), cls: "sync-state-error" };
  // 本会话还没检查过：account.json 里的 lastSyncedAt 是上一会话留下的，不能据此显示
  // 「已同步」——服务器上可能已有本机没拉到的记录（启动即检查，检查完成前显示检查中）
  if (!st.startupChecked) return { text: tr("正在检查同步…"), cls: "sync-state-busy" };
  if (st.lastSyncedAt > 0) return { text: fmt("已同步 · 最后同步 {0}", syncFmtTime(st.lastSyncedAt)), cls: "sync-state-ok" };
  return { text: tr("已登录，尚未同步"), cls: "" };
}

/** applyPendingText 待生效状态行文案（含上次应用失败原因）。 */
function applyPendingText(st) {
  const n = st.pendingApplyCount || 0;
  const base = n > 0 ? fmt("待生效 {0} 项", n) : tr("有待生效的同步改动");
  return st.applyError ? base + " · " + fmt("上次应用失败：{0}", st.applyError) : base + tr("，点「重启生效」应用");
}

/** renderSyncNavStatus 左侧导航小字状态：灰=未登录 / 蓝=同步中 / 黄=待同步·待生效 / 红=失败 / 绿=已同步。 */
function renderSyncNavStatus(st) {
  const el = $("sync-nav-status");
  if (!el) return;
  let cls = "nav-status";
  let text = "";
  if (st.loggedIn) {
    if (st.syncing) { cls += " is-syncing"; text = tr("同步中"); }
    else if (st.syncError) { cls += " is-error"; text = tr("同步失败"); }
    else if (st.pendingOps > 0) { cls += " is-pending"; text = fmt("待同步 {0} 项", st.pendingOps); }
    else if (st.pendingApply) { cls += " is-pending"; text = st.pendingApplyCount > 0 ? fmt("待生效 {0} 项", st.pendingApplyCount) : tr("待生效"); }
    else if (!st.startupChecked) { cls += " is-syncing"; text = tr("正在检查同步…"); }
    else if (st.lastSyncedAt > 0) { cls += " is-ok"; text = tr("已同步"); }
    else { text = tr("已登录，尚未同步"); }
  }
  el.className = cls;
  el.textContent = text;
}

/** refreshSync 读取 Go 侧状态并渲染（未打开该页时也刷新左侧小字）。 */
async function refreshSync() {
  const g = bindings();
  if (!g || typeof g.AccountStatus !== "function") return;
  let st;
  try {
    st = await g.AccountStatus();
  } catch (err) {
    console.error("AccountStatus", err);
    return;
  }
  renderSyncNavStatus(st || {});

  const loginCard = $("sync-login");
  const accCard = $("sync-account");
  if (!loginCard || !accCard) return;

  const loggedIn = !!(st && st.loggedIn);
  loginCard.classList.toggle("hidden", loggedIn);
  accCard.classList.toggle("hidden", !loggedIn);

  if (loggedIn) {
    // 登录态由状态快照决定：清掉上一次登出/失败留下的常驻文案，避免「已登录却写着已退出登录」
    syncHint("sync-account-hint", "");
    $("sync-account-email").textContent = st.email || "—";
    const view = syncStatusView(st);
    const line = $("sync-status-line");
    line.textContent = view.text;
    line.className = "row-sub " + view.cls;
    $("btn-sync-now").disabled = !!st.syncing || !!st.applying;
    // 重启生效提示：Go 侧给出 pendingApply 标记；应用进行中禁用按钮避免重复触发
    const restart = $("sync-restart");
    if (restart) restart.classList.toggle("hidden", !st.pendingApply);
    const applyBtn = $("btn-sync-apply");
    if (applyBtn) {
      applyBtn.disabled = !!st.applying;
      applyBtn.textContent = st.applying ? tr("正在应用…") : tr("重启生效");
    }
    if (st.pendingApply) {
      const sub = $("sync-restart-sub");
      if (sub) {
        sub.textContent = st.applyError
          ? fmt("上次应用失败：{0}。剩余改动已保留，可再次点击应用续做。", st.applyError)
          : fmt("共 {0} 项改动已保存但尚未生效；点击右侧按钮后合并并生效。", st.pendingApplyCount || 0);
      }
    }
  } else if (st && (st.expireReason === "session_expired" || st.expireReason === "token_expired")) {
    syncHint("sync-login-hint", tr("登录已过期，请重新登录"), true);
  }
}

/** startSyncCountdown 发送验证码后的重发倒计时。 */
function startSyncCountdown(sec) {
  const btn = $("btn-sync-send");
  if (!btn) return;
  if (syncResendTimer) clearInterval(syncResendTimer);
  let left = sec;
  const tick = () => {
    if (left <= 0) {
      clearInterval(syncResendTimer);
      syncResendTimer = null;
      btn.disabled = false;
      btn.textContent = curLangCode() === "en" ? I18N_EN.btnSyncSend : "发送验证码";
      return;
    }
    btn.disabled = true;
    btn.textContent = fmt("{0} 秒后可重发", left);
    left -= 1;
  };
  tick();
  syncResendTimer = setInterval(tick, 1000);
}

async function doSyncSendCode() {
  const g = bindings();
  if (!g) return;
  const email = ($("sync-email").value || "").trim();
  if (!email) { syncHint("sync-login-hint", tr("请先填写邮箱"), true); return; }
  syncHint("sync-login-hint", tr("正在发送…"));
  $("btn-sync-send").disabled = true;
  try {
    const res = await g.AccountRequestCode(email);
    const mins = Math.max(1, Math.round(((res && res.expiresInSec) || 600) / 60));
    syncHint("sync-login-hint", fmt("验证码已发送，{0} 分钟内有效", mins));
    startSyncCountdown((res && res.resendAfterSec) || 60);
    const code = $("sync-code");
    if (code) code.focus();
  } catch (err) {
    syncHint("sync-login-hint", String(err), true);
    $("btn-sync-send").disabled = false;
  }
}

async function doSyncLogin() {
  const g = bindings();
  if (!g) return;
  const email = ($("sync-email").value || "").trim();
  const code = ($("sync-code").value || "").trim();
  if (!email) { syncHint("sync-login-hint", tr("请先填写邮箱"), true); return; }
  if (!/^\d{6}$/.test(code)) { syncHint("sync-login-hint", tr("请输入 6 位验证码"), true); return; }

  $("btn-sync-login").disabled = true;
  syncHint("sync-login-hint", tr("登录中…"));
  try {
    await g.AccountVerify(email, code);
    $("sync-code").value = "";
    syncHint("sync-login-hint", "");
    syncHint("sync-account-hint", ""); // 清掉登出时留下的「已退出登录」
    // 登录后立即做一次同步检查（首次同步的「重启生效」提示由 Go 侧给出）
    if (typeof g.AccountSyncNow === "function") {
      try { await g.AccountSyncNow(); } catch (e) { console.error("AccountSyncNow", e); }
    }
  } catch (err) {
    syncHint("sync-login-hint", String(err), true);
  } finally {
    $("btn-sync-login").disabled = false;
    await refreshSync();
  }
}

async function doSyncLogout() {
  const g = bindings();
  if (!g) return;
  $("btn-sync-logout").disabled = true;
  try {
    await g.AccountLogout();
    syncHint("sync-account-hint", tr("已退出登录"));
    syncHint("sync-login-hint", "");
  } catch (err) {
    syncHint("sync-account-hint", String(err), true);
  } finally {
    $("btn-sync-logout").disabled = false;
    await refreshSync();
  }
}

async function doSyncNow() {
  const g = bindings();
  if (!g) return;
  if (typeof g.AccountSyncNow !== "function") { syncHint("sync-account-hint", tr("同步功能尚未就绪"), true); return; }
  syncHint("sync-account-hint", tr("正在同步…"));
  try {
    await g.AccountSyncNow();
    const st = (typeof g.AccountStatus === "function") ? await g.AccountStatus() : null;
    if (st && st.pendingApply) {
      // 「同步检查完成」不等于「已生效」：拉到的改动要用户点「重启生效」才落地（问题④⑥）
      syncHint("sync-account-hint", fmt("同步检查完成，{0} 项改动等待重启生效", st.pendingApplyCount || 0));
      showPage("sync");
    } else {
      syncHint("sync-account-hint", tr("同步检查完成，本机已是最新"));
    }
  } catch (err) {
    syncHint("sync-account-hint", String(err), true);
  } finally {
    await refreshSync();
  }
}

// 重启生效：长操作（停服 → 安装/重装 → 重启校验，分钟级），改为「进度视图 + 事件收尾」——
// 不再 await 整个调用（否则按钮自始至终无进度、失败也只在末尾给出）。
function doSyncApply() {
  const g = bindings();
  if (!g) return;
  if (typeof g.AccountApplyPending !== "function") { syncHint("sync-account-hint", tr("同步功能尚未就绪"), true); return; }
  $("btn-sync-apply").disabled = true;
  syncHint("sync-account-hint", tr("正在应用同步改动…"));
  showSplash("sync", tr("正在应用同步改动…"));
  try {
    const p = g.AccountApplyPending(); // 结果经 sync:apply:done 事件回报
    if (p && typeof p.catch === "function") p.catch(() => {});
  } catch (err) {
    showSettings();
    syncHint("sync-account-hint", String(err), true);
  }
}

function wireSync() {
  const send = $("btn-sync-send");
  if (!send) return;
  send.addEventListener("click", doSyncSendCode);
  $("btn-sync-login").addEventListener("click", doSyncLogin);
  $("btn-sync-logout").addEventListener("click", doSyncLogout);
  $("btn-sync-now").addEventListener("click", doSyncNow);
  $("btn-sync-apply").addEventListener("click", doSyncApply);
  const code = $("sync-code");
  if (code) code.addEventListener("keydown", (e) => { if (e.key === "Enter") doSyncLogin(); });
  const email = $("sync-email");
  if (email) email.addEventListener("keydown", (e) => { if (e.key === "Enter") doSyncSendCode(); });
}

// ==================== 数据同步：文件/文件夹 ====================
//
// 列表口径：行 = 用户添加的条目（文件夹/文件）。文件夹条目可展开显示内部文件树（子目录 → 文件）。
// 排序：文件夹优先，组内按 名称/大小/修改时间 升/降序（再次点同一键切换方向）。
// 删除一律二次确认；条目删除提供「同时删除本机文件 / 仅移出同步」两档。

let filesSort = { key: "name", dir: 1 };
const filesExpanded = {}; // 展开态：条目 id 或 `id/子路径` → true
let filesQuotaWarned = 0; // 已提示过的容量不足文件数（避免每次刷新重复弹窗）
// 列表重建节流/保护：上传期间状态变化要尽快反映，但绝不能在「按下~抬起」之间换掉 DOM，
// 否则 click 落空——表现为点三角形折叠不回去（2026-10-05 现场）。
let filesTreeRenderedAt = 0;
let filesPointerHeld = false;
// 初值必须是 -Infinity：用 0 的话，页面加载后的前 400ms 会被误判成「刚松开鼠标」，
// 首次列表渲染直接被跳过（列表空白，要等下一次事件才出现）。
let filesPointerReleasedAt = Number.NEGATIVE_INFINITY;
const FILES_TREE_MIN_INTERVAL = 400; // ms：上传期间最快 400ms 重建一次列表

/** filesTreeInteractive 是否处于指针交互中（含抬起后的短暂保护窗口）。 */
function filesTreeInteractive() {
  return filesPointerHeld || performance.now() - filesPointerReleasedAt < 400;
}
// 排序键的中文标签：渲染时再 tr()，语言切换后标签跟着走（不用缓存译文）。
const FILES_SORT_LABEL = { name: "名称", size: "大小", mtime: "修改时间" };

/** filesFmtSize 容量友好格式（换算后数值保持 1024 以内）。
 *
 * 用 1024 进制（Windows 资源管理器口径：同样标 KB/MB）：配额是产品约定的 10 MiB
 * （10485760 字节），按 1000 进制显示会变成「10.5 MB」，看起来像配额写错了
 * ——2026-10-06 用户反馈。改 1024 进制后同一份数据稳定显示「10.0 MB」。 */
function filesFmtSize(n) {
  const v = Math.max(0, Number(n) || 0);
  if (v < 1024) return v + " B";
  const units = ["KB", "MB", "GB", "TB"];
  let x = v / 1024;
  let i = 0;
  let shown = x >= 100 ? Math.round(x) : Math.round(x * 10) / 10;
  // 进位保护：1023.95 KB 四舍五入成 1024，要再进一级（否则显示「1024 KB」而不是「1 MB」）
  while (shown >= 1024 && i < units.length - 1) {
    x /= 1024;
    i += 1;
    shown = x >= 100 ? Math.round(x) : Math.round(x * 10) / 10;
  }
  return shown + " " + units[i];
}

/** filesStatusView 状态 → 徽标文案与语义色类。 */
function filesStatusView(status) {
  switch (status) {
    case "synced": return { text: tr("已同步"), cls: "is-ok" };
    case "uploading": return { text: tr("同步中"), cls: "is-active" };
    case "removed-local": return { text: tr("已在本机移除"), cls: "is-queued" };
    case "pending":
    case "pending-upload": return { text: tr("待同步"), cls: "is-queued" };
    case "pending-download": return { text: tr("待下载"), cls: "is-queued" };
    case "error": return { text: tr("同步失败"), cls: "is-error" };
    default: return { text: "", cls: "" };
  }
}

/** filesBuildTree 条目内扁平文件列表（relPath）→ 树。 */
function filesBuildTree(files) {
  const root = { name: "", path: "", dirs: [], dirMap: new Map(), files: [], size: 0, mtime: 0, count: 0, status: "synced" };
  for (const f of files || []) {
    const parts = String(f.relPath || "").split("/");
    let node = root;
    for (let i = 0; i < parts.length - 1; i += 1) {
      const key = parts[i];
      let child = node.dirMap.get(key);
      if (!child) {
        child = {
          name: key,
          path: parts.slice(0, i + 1).join("/"),
          dirs: [], dirMap: new Map(), files: [],
          size: 0, mtime: 0, count: 0, status: "synced",
        };
        node.dirMap.set(key, child);
        node.dirs.push(child);
      }
      node = child;
    }
    node.files.push(f);
  }
  filesTreeStats(root);
  return root;
}

/** filesStatusRank 状态的优先级：错误 > 同步中 > 待同步/待下载 > 已在本机移除 > 已同步。
 *  目录聚合取子树里优先级最高的状态——只要有后代在传，目录就该显示「同步中」。 */
function filesStatusRank(status) {
  switch (status) {
    case "error": return 4;
    case "uploading": return 3;
    case "pending":
    case "pending-upload":
    case "pending-download": return 2;
    case "removed-local": return 1;
    default: return 0;
  }
}

/** filesStatusPick 合并两个状态（取优先级更高者，同级保留前者）。 */
function filesStatusPick(a, b) {
  return filesStatusRank(b) > filesStatusRank(a) ? b : a;
}

/** filesTreeStats 目录聚合：大小 = 子树合计，时间 = 子树最新，状态取优先级最高者。 */
function filesTreeStats(node) {
  let size = 0;
  let mtime = 0;
  let count = 0;
  let status = "synced";
  for (const d of node.dirs) {
    const st = filesTreeStats(d);
    size += st.size;
    count += st.count;
    mtime = Math.max(mtime, st.mtime);
    status = filesStatusPick(status, st.status);
  }
  for (const f of node.files) {
    size += Number(f.size) || 0;
    count += 1;
    mtime = Math.max(mtime, Number(f.mtime) || 0);
    status = filesStatusPick(status, f.status);
  }
  node.size = size;
  node.mtime = mtime;
  node.count = count;
  node.status = status;
  return node;
}

/** filesSortTree 排序：目录优先，组内按当前排序键与方向。 */
function filesSortTree(node) {
  const cmp = (a, b) => {
    let r;
    if (filesSort.key === "size") r = (a.size || 0) - (b.size || 0);
    else if (filesSort.key === "mtime") r = (a.mtime || 0) - (b.mtime || 0);
    else r = String(a.name).localeCompare(String(b.name), "zh-Hans-CN");
    if (r === 0) r = String(a.name).localeCompare(String(b.name), "zh-Hans-CN");
    return r * filesSort.dir;
  };
  node.dirs.sort(cmp);
  node.files.sort(cmp);
  for (const d of node.dirs) filesSortTree(d);
  return node;
}

/** filesSortEntries 顶层条目列表排序：文件夹在前，再按当前键（名称/大小/修改时间）与方向。
 *  这是「排序按钮」对列表最直观的那一层——只排展开后的树内文件，用户点按钮会觉得没反应。 */
function filesSortEntries(list) {
  const cmp = (a, b) => {
    const ad = a.kind === "dir" ? 0 : 1;
    const bd = b.kind === "dir" ? 0 : 1;
    if (ad !== bd) return ad - bd;
    let r;
    if (filesSort.key === "size") r = (Number(a.size) || 0) - (Number(b.size) || 0);
    else if (filesSort.key === "mtime") r = (Number(a.mtime) || 0) - (Number(b.mtime) || 0);
    else r = String(a.name || "").localeCompare(String(b.name || ""), "zh-Hans-CN");
    if (r === 0) r = String(a.name || "").localeCompare(String(b.name || ""), "zh-Hans-CN");
    return r * filesSort.dir;
  };
  return (list || []).slice().sort(cmp);
}

/** filesRowHtml 一行（文件夹或文件）；depth 控制缩进。 */
function filesRowHtml(o) {
  const view = filesStatusView(o.status);
  const caret = o.isDir
    ? `<button type="button" class="files-caret" data-fact="toggle" data-entry="${esc(o.entryId)}" data-rel="${esc(o.rel)}" aria-expanded="${o.expanded ? "true" : "false"}" aria-label="${tr("展开或折叠")}">${o.expanded ? "▾" : "▸"}</button>`
    : '<span class="files-caret files-caret-empty"></span>';
  const meta = [];
  meta.push(filesFmtSize(o.size));
  if (o.isDir) meta.push(fmt("{0} 个文件", o.count || 0));
  if (o.speedBps > 0) meta.push(fmt("{0}/s", filesFmtSize(o.speedBps)));
  if (o.mtime) meta.push(syncFmtTime(o.mtime));
  const err = o.error ? `<div class="files-err">${esc(o.error)}</div>` : "";
  const actions = [];
  // 未同步完成的文件只允许移除（本机可能还没有内容，打开无意义）；同步完成的才可打开
  if (o.status === "removed-local") {
    // 本机文件已被删除（不传播到账号）：提供「重新同步」从账号拉回
    actions.push(`<button type="button" class="btn btn-ghost btn-xs" data-fact="restore" data-entry="${esc(o.entryId)}" data-rel="${esc(o.rel)}">${tr("重新同步")}</button>`);
  } else if (o.status === "synced") {
    actions.push(`<button type="button" class="btn btn-ghost btn-xs" data-fact="open" data-entry="${esc(o.entryId)}" data-rel="${esc(o.rel)}">${tr("打开")}</button>`);
  }
  actions.push(`<button type="button" class="btn btn-ghost btn-xs files-danger" data-fact="remove" data-entry="${esc(o.entryId)}" data-rel="${esc(o.rel)}" data-dir="${o.isDir ? "1" : "0"}">${tr("移除")}</button>`);
  return (
    `<div class="files-row" style="--depth:${o.depth}">` +
    caret +
    `<div class="files-main"><div class="files-name">${o.isDir ? "📁" : "📄"} ${esc(o.name)}</div>` +
    `<div class="files-meta">${esc(meta.join(" · "))}</div>${err}</div>` +
    (view.text ? `<span class="files-badge ${view.cls}">${view.text}</span>` : "") +
    `<div class="files-actions">${actions.join("")}</div></div>`
  );
}

/** filesNodeHtml 递归渲染树（展开的目录才渲染子行）。 */
function filesNodeHtml(node, entryId, depth) {
  let html = "";
  for (const d of node.dirs) {
    const key = entryId + "/" + d.path;
    const expanded = !!filesExpanded[key];
    html += filesRowHtml({
      entryId, rel: d.path, name: d.name, isDir: true, size: d.size, mtime: d.mtime,
      count: d.count, status: d.status, error: "", depth, expanded,
    });
    if (expanded) html += filesNodeHtml(d, entryId, depth + 1);
  }
  for (const f of node.files) {
    html += filesRowHtml({
      entryId, rel: f.relPath, name: f.name, isDir: false, size: f.size, mtime: f.mtime,
      count: 1, status: f.status, error: f.error || "", depth, speedBps: f.speedBps || 0,
    });
  }
  return html;
}

/** filesEntryHtml 一个条目行（+ 展开的文件树）。 */
function filesEntryHtml(e) {
  const isDir = e.kind === "dir";
  const expanded = !!filesExpanded[e.id];
  const view = filesStatusView(e.status);
  const meta = [filesFmtSize(e.size)];
  if (isDir) meta.push(fmt("{0} 个文件", (e.files || []).length));
  meta.push(e.isSource ? tr("本机原位置") : tr("接收目录"));
  if (e.path) meta.push(e.path);
  const caret = isDir
    ? `<button type="button" class="files-caret" data-fact="toggle" data-entry="${esc(e.id)}" data-rel="" aria-expanded="${expanded ? "true" : "false"}" aria-label="${tr("展开或折叠")}">${expanded ? "▾" : "▸"}</button>`
    : '<span class="files-caret files-caret-empty"></span>';
  // 错误文本：条目级错误优先，否则取第一个失败文件（行内可见，不必展开才知道为什么失败）
  const failed = (e.files || []).find((f) => f.error);
  const errText = e.error || (failed ? fmt("{0}：{1}", failed.name, failed.error) : "");
  const err = errText ? `<div class="files-err">${esc(errText)}</div>` : "";
  const tree = isDir && expanded
    ? filesNodeHtml(filesSortTree(filesBuildTree(e.files || [])), e.id, 1)
    : "";
  const actions = [];
  // 条目行：有文件「已在本机移除」时给一个整条目「重新同步」；打开照旧
  const removedCount = (e.files || []).filter((f) => f.status === "removed-local").length;
  actions.push(`<button type="button" class="btn btn-ghost btn-xs" data-fact="open" data-entry="${esc(e.id)}" data-rel="">${tr("打开")}</button>`);
  // 把该条目移动到本机的其它位置（系统式移动；文件条目用系统保存对话框，覆盖由系统询问）
  actions.push(`<button type="button" class="btn btn-ghost btn-xs" data-fact="relocate" data-entry="${esc(e.id)}" title="${esc(tr("移动到本机其它位置"))}">${tr("移动")}</button>`);
  if (removedCount > 0) {
    actions.push(`<button type="button" class="btn btn-ghost btn-xs" data-fact="restore" data-entry="${esc(e.id)}" data-rel="">${tr("重新同步")}</button>`);
  }
  const entryActions = actions.join("");
  // 本机曾是来源设备、但原路径已不存在（文件被删/移走）：说明为什么这里出现的是副本
  const adoptHint = e.sourcePathMissing
    ? `<div class="files-err">${esc(fmt("源设备原路径 {0} 在本机已不存在，内容已保存为接收目录副本", e.originPath || ""))}</div>`
    : "";
  return (
    `<div class="files-entry">` +
    `<div class="files-row files-row-entry" style="--depth:0">` + caret +
    `<div class="files-main"><div class="files-name">${isDir ? "📁" : "📄"} ${esc(e.name)}</div>` +
    `<div class="files-meta">${esc(meta.filter(Boolean).join(" · "))}</div>${err}${adoptHint}</div>` +
    (view.text ? `<span class="files-badge ${view.cls}">${view.text}</span>` : "") +
    `<div class="files-actions">${entryActions}` +
    `<button type="button" class="btn btn-ghost btn-xs files-danger" data-fact="remove" data-entry="${esc(e.id)}" data-rel="" data-dir="${isDir ? "1" : "0"}">${tr("移除")}</button>` +
    `</div></div>` + tree + `</div>`
  );
}

/** renderFilesCard 渲染文件卡（容量、待应用、列表、空态）。
 *
 * forceTree=true 时强制重建列表 DOM（用户操作后必须立即反映）。
 * 上传进行中**默认不重建列表**：上传期间每 300ms 推一次事件，若每次都换掉 DOM，
 * 点击（mousedown→mouseup 跨不过节点替换）会被吞掉——表现为「点三角形折叠不回去」。
 * 进度与容量仍实时更新（提示行 + 容量条），列表等上传结束或用户操作时再重建。 */
function renderFilesCard(st, opts) {
  const card = $("sync-files");
  if (!card) return;
  const forceTree = !!(opts && opts.forceTree);
  const loggedIn = !!(st && st.loggedIn);
  // 板块始终可见：未登录时清空内容，只留一行说明（用户要求）
  card.classList.remove("hidden");
  $("files-loggedout-row").classList.toggle("hidden", loggedIn);
  $("files-capacity-row").classList.toggle("hidden", !loggedIn);
  $("files-list-row").classList.toggle("hidden", !loggedIn);
  $("files-hint-row").classList.toggle("hidden", !loggedIn);
  if (!loggedIn) {
    filesQuotaWarned = 0;
    $("files-pending").classList.add("hidden");
    for (const id of ["btn-files-add-file", "btn-files-add-dir", "btn-files-sync"]) {
      const b = $(id);
      if (b) b.disabled = true;
    }
    return;
  }

  // 容量条
  const used = Number(st.quotaUsed) || 0;
  const limit = Number(st.quotaLimit) || 0;
  const pct = limit > 0 ? Math.min(1, used / limit) : 0;
  const fill = $("files-capacity-fill");
  if (fill) {
    fill.style.width = Math.round(pct * 100) + "%";
    fill.className = "files-fill" + (pct >= 1 ? " is-danger" : pct >= 0.8 ? " is-warn" : "");
  }
  $("files-capacity-text").textContent = limit > 0
    ? fmt("已用 {0} / 共 {1} · 剩余 {2}", filesFmtSize(used), filesFmtSize(limit), filesFmtSize(Math.max(0, limit - used)))
    : tr("正在读取容量…");

  // 待应用块
  const pending = $("files-pending");
  const n = Number(st.pendingCount) || 0;
  pending.classList.toggle("hidden", n === 0 && !st.applying);
  if (n > 0 || st.applying) {
    const list = (st.pendingFiles || []).slice(0, 3).join("、");
    const more = (st.pendingFiles || []).length > 3 ? "…" : "";
    $("files-pending-sub").textContent = st.applying
      ? tr("正在应用…")
      : fmt("共 {0} 项：{1}{2}", n, list, more);
  }

  // 列表 / 空态（上传中只更新提示与容量，避免频繁重建 DOM 吞掉点击）
  const entries = filesSortEntries(st.entries || []); // 顶层条目也按当前排序键排列
  state.filesEntries = entries; // 供行内操作按 id 取条目
  const toolbar = $("files-toolbar");
  toolbar.classList.toggle("hidden", entries.length === 0);
  document.querySelectorAll(".files-sort").forEach((b) => {
    const key = b.dataset.sort;
    const active = key === filesSort.key;
    b.classList.toggle("is-active", active);
    b.textContent = tr(FILES_SORT_LABEL[key] || key) + (active ? (filesSort.dir < 0 ? " ↓" : " ↑") : "");
  });
  $("files-empty").classList.toggle("hidden", entries.length > 0);
  // 状态一变就重建列表（上传期间最多 400ms 一次）；指针交互中先不换 DOM，避免吞掉点击。
  // 首次渲染（filesTreeRenderedAt 还是 0）必须无条件执行——performance.now() 从 0 起算，
  // 加载后 400ms 内到达的首个快照否则会被节流掉，列表一直空着。
  const now = performance.now();
  const treeDue = filesTreeRenderedAt === 0 || now - filesTreeRenderedAt >= FILES_TREE_MIN_INTERVAL;
  if (forceTree || (!filesTreeInteractive() && treeDue)) {
    $("files-tree").innerHTML = entries.map(filesEntryHtml).join("");
    filesTreeRenderedAt = now;
  }

  // 按钮可用性
  const busy = !!st.syncing || !!st.applying;
  $("btn-files-add-file").disabled = busy;
  $("btn-files-add-dir").disabled = busy;
  $("btn-files-sync").disabled = busy;
  $("btn-files-apply").disabled = busy;
  // 提示行：上传进度（带实时速度）优先，其次是应用/同步中、失败、最后同步时间
  if (st.uploading && st.uploadTotal > 0) {
    const speed = st.uploadSpeedBps > 0 ? fmt(" · {0}/s", filesFmtSize(st.uploadSpeedBps)) : "";
    filesHint(fmt("正在上传 {0}/{1}{2}", st.uploadDone, st.uploadTotal, speed));
  } else if (busy) {
    filesHint(st.applying ? tr("正在应用同步改动…") : tr("正在同步…"));
  } else if (st.lastError) {
    filesHint(fmt("同步失败：{0}", st.lastError), true);
  } else if (st.lastSyncedAt) {
    // 小字提示：最后同步时间 + 最近一次「远端改动自动落地」的时间与项数（用户要求保留痕迹）
    const applied = Number(st.remoteAppliedCount) || 0;
    const appliedAt = Number(st.remoteAppliedAt) || 0;
    filesHint(applied > 0 && appliedAt > 0
      ? fmt("已同步 · 最后同步 {0} · 远端更新 {1} 项于 {2}", syncFmtTime(st.lastSyncedAt), applied, syncFmtTime(appliedAt))
      : fmt("已同步 · 最后同步 {0}", syncFmtTime(st.lastSyncedAt)));
  } else {
    filesHint("");
  }

  // 容量不足：首次出现（或数量变化）时弹窗提示
  const blocked = Number(st.blockedCount) || 0;
  if (blocked > 0 && blocked !== filesQuotaWarned) {
    filesQuotaWarned = blocked;
    confirmDialog(
      "可用容量不足",
      fmt("有 {0} 个文件因容量不足未同步。请删除部分已同步文件或移除条目后重试；已同步的内容不受影响。", blocked),
      "知道了",
    );
  } else if (blocked === 0) {
    filesQuotaWarned = 0;
  }
}

/** refreshFiles 读取 Go 侧文件同步状态并渲染（forceTree 供用户操作后强制重建列表）。 */
async function refreshFiles(forceTree) {
  const g = bindings();
  if (!g || typeof g.FilesStatus !== "function") return;
  try {
    renderFilesCard(await g.FilesStatus(), { forceTree: !!forceTree });
  } catch (err) {
    console.error("FilesStatus", err);
  }
}

/** filesHint 卡内提示（isError 时用语义红）；action = { label, run } 时在文案后附一个按钮。 */
function filesHint(text, isError, action) {
  const el = $("files-hint");
  if (!el) return;
  const txt = $("files-hint-text");
  if (txt) txt.textContent = text || "";
  else el.textContent = text || "";
  el.classList.toggle("is-error", !!isError);
  const btn = $("files-hint-action");
  if (!btn) return;
  btn.onclick = null;
  if (action && action.label && typeof action.run === "function") {
    btn.textContent = tr(action.label);
    btn.classList.remove("hidden");
    btn.onclick = () => action.run();
  } else {
    btn.classList.add("hidden");
  }
}

async function filesDoAdd(kind) {
  const g = bindings();
  if (!g || typeof g.FilesAdd !== "function") return;
  filesHint(tr("正在读取所选内容…"));
  try {
    // 提示行交给 renderFilesCard（上传进度/失败原因由快照驱动，避免这里留下不更新的常驻文案）
    renderFilesCard(await g.FilesAdd(kind), { forceTree: true });
    await refreshFiles(true);
  } catch (err) {
    filesHint(String(err), true);
  }
}

async function filesDoSync() {
  const g = bindings();
  if (!g || typeof g.FilesSyncNow !== "function") return;
  filesHint(tr("正在同步…"));
  try {
    renderFilesCard(await g.FilesSyncNow(), { forceTree: true });
  } catch (err) {
    filesHint(String(err), true);
    await refreshFiles();
  }
}

async function filesDoApply() {
  const g = bindings();
  if (!g || typeof g.FilesApplyPending !== "function") return;
  filesHint(tr("正在应用同步改动…"));
  try {
    renderFilesCard(await g.FilesApplyPending(), { forceTree: true });
  } catch (err) {
    filesHint(String(err), true);
    await refreshFiles();
  }
}

async function filesDoOpen(entryId, rel) {
  const g = bindings();
  if (!g || typeof g.FilesOpenEntry !== "function") return;
  try {
    // 用户在选择打开方式的对话框里取消时，Go 侧静默返回 nil，不会走到这里
    await g.FilesOpenEntry(entryId, rel);
    filesHint("");
  } catch (err) {
    // 默认方式启动失败：显示原因，并允许重新选择打开方式
    filesHint(String(err), true, {
      label: "选择打开方式",
      run: () => filesDoOpenWith(entryId, rel),
    });
  }
}

/** filesDoOpenWith 让用户重新选择打开方式（Windows「打开方式」对话框）。 */
async function filesDoOpenWith(entryId, rel) {
  const g = bindings();
  if (!g || typeof g.FilesOpenEntryWith !== "function") return;
  try {
    await g.FilesOpenEntryWith(entryId, rel);
    filesHint("");
  } catch (err) {
    filesHint(String(err), true);
  }
}

/** filesDoRestore 把「已在本机移除」的文件重新纳入同步（rel 为空 = 整个条目）。 */
async function filesDoRestore(entryId, rel) {
  const g = bindings();
  if (!g || typeof g.FilesRestorePath !== "function") return;
  try {
    renderFilesCard(await g.FilesRestorePath(entryId, rel || ""), { forceTree: true });
    filesHint(tr("已重新纳入同步，稍后可从账号拉回"));
  } catch (err) {
    filesHint(String(err), true);
  }
}

/** filesDoRelocate 把条目移动到本机其它位置（系统式移动：搬文件、清理源；覆盖由系统对话框询问）。 */
async function filesDoRelocate(entryId) {
  const g = bindings();
  if (!g || typeof g.FilesSetLocalPath !== "function") return;
  try {
    const st = await g.FilesSetLocalPath(entryId);
    renderFilesCard(st, { forceTree: true });
    filesHint(tr("已移动同步位置；本机文件已搬到新位置"), false);
  } catch (err) {
    filesHint(String(err), true);
  }
}

async function filesDoRemove(entryId, rel, isDir) {
  const g = bindings();
  if (!g) return;
  const entry = (state.filesEntries || []).find((e) => e.id === entryId);
  if (!entry) return;
  // 移除只影响同步清单：本机文件一律保留（用户决策：不再提供「同时删除本机文件」）
  if (rel === "") {
    const ok = await confirmDialog(
      "移除同步条目？",
      fmt("「{0}」将从账号同步中移除（其它设备上的同一条目也会移除），本机文件保持不动。", entry.name),
      "移除",
    );
    if (!ok) return;
    try {
      renderFilesCard(await g.FilesRemoveEntry(entryId), { forceTree: true });
    } catch (err) {
      filesHint(String(err), true);
    }
    return;
  }
  const ok = await confirmDialog(
    isDir ? "移除该文件夹的同步内容？" : "移除该文件的同步内容？",
    fmt("「{0}」会从账号同步中删除（其它设备上的副本也会删除），本机文件保持不动。", rel),
    "移除",
  );
  if (!ok) return;
  try {
    renderFilesCard(await g.FilesRemovePath(entryId, rel), { forceTree: true });
  } catch (err) {
    filesHint(String(err), true);
  }
}

/** filesToggle 展开/折叠（条目或子目录）：本地状态立即翻转并强制重建列表。 */
function filesToggle(entryId, rel) {
  const key = rel ? entryId + "/" + rel : entryId;
  filesExpanded[key] = !filesExpanded[key];
  refreshFiles(true);
}

function wireFiles() {
  const addFile = $("btn-files-add-file");
  if (!addFile) return;
  addFile.addEventListener("click", () => filesDoAdd("file"));
  $("btn-files-add-dir").addEventListener("click", () => filesDoAdd("dir"));
  $("btn-files-sync").addEventListener("click", filesDoSync);
  $("btn-files-apply").addEventListener("click", filesDoApply);
  document.querySelectorAll(".files-sort").forEach((btn) => {
    btn.dataset.labelAsc = tr(btn.textContent.trim());
    btn.dataset.labelDesc = tr(btn.textContent.trim()) + " ↓";
    btn.addEventListener("click", () => {
      const key = btn.dataset.sort;
      if (filesSort.key === key) filesSort.dir = -filesSort.dir;
      else filesSort = { key, dir: 1 };
      refreshFiles(true); // 排序必须立即重建列表（不带 force 会被 400ms 节流吞掉 → 点击看起来没反应）
    });
  });
  $("files-tree").addEventListener("click", (ev) => {
    const btn = ev.target.closest("[data-fact]");
    if (!btn) return;
    const entryId = btn.dataset.entry || "";
    const rel = btn.dataset.rel || "";
    switch (btn.dataset.fact) {
      case "toggle": filesToggle(entryId, rel); break;
      case "open": filesDoOpen(entryId, rel); break;
      case "restore": filesDoRestore(entryId, rel); break;
      case "relocate": filesDoRelocate(entryId); break;
      case "remove": filesDoRemove(entryId, rel, btn.dataset.dir === "1"); break;
      default: break;
    }
  });
  // 指针按下期间不重建列表（避免点击因节点被替换而丢失）
  document.addEventListener("pointerdown", () => { filesPointerHeld = true; }, true);
  const releasePointer = () => { filesPointerHeld = false; filesPointerReleasedAt = performance.now(); };
  document.addEventListener("pointerup", releasePointer, true);
  document.addEventListener("pointercancel", releasePointer, true);
}

async function init() {
  snapshotStaticZh(); // 语言快照必须先于任何 en 覆盖（zh 还原基线）
  wireEvents();
  wireSplashCancel();
  wireGeneral();
  wireAbout();
  wireLogs();
  wireExport();
  wireImport();
  wireHelp();
  wireSync();
  wireFiles();

  document.querySelectorAll(".nav-item").forEach((b) => {
    b.addEventListener("click", () => showPage(b.dataset.page));
  });

  // 初始视图：等待 Go 侧 ui:show-splash 事件（非自启动时窗口显示 splash）
  showSplash("startup", tr("正在准备运行环境…"));

  // 左侧「数据同步」小字状态：启动即渲染一次（未打开该页也要可见）
  refreshSync();
  // 文件同步卡：未打开该页也先取一次（容量条与待应用提示要能立刻反映）
  refreshFiles();

  // 截图/预览：DSH_SYSTRAY_SHOT_PAGE 指定后直接显示对应页面；SHOT_SCROLL 指定滚动位置
  try {
    const shot = await bindings().GetShotPage();
    state.shotPage = shot || "";
    if (shot && PAGE_TITLES[shot]) showPage(shot);
    try {
      state.shotScroll = (await bindings().GetShotScroll()) || "";
    } catch (e) { /* ignore */ }
    if (state.shotScroll) {
      applyShotScroll();
      // splash 切换/内容渲染后再补几次，确保滚动位置稳定落定
      setTimeout(applyShotScroll, 900);
      setTimeout(applyShotScroll, 2500);
    }
    // 截图模式导入页：自动载入演示可恢复项（呈现「已加载、可逐项恢复」状态而非空态）
    if (shot === "import") {
      (async () => {
        try {
          const res = await bindings().ImportPick();
          if (res && res.items && res.items.length) {
            state.impItems = res.items;
            $("imp-path").textContent = res.path || "";
            $("imp-path").classList.remove("hidden");
            renderImportRows();
          }
        } catch (e) { /* ignore */ }
      })();
    }
  } catch (e) { /* ignore */ }

  // 恢复进行中进入导入页（窗口隐藏期间已发起恢复）：禁用「添加压缩包…」并说明原因
  (async () => {
    try {
      if (await bindings().RestoreBusy()) {
        syncImportPickBtn();
        setImpHint(tr("正在恢复导入项，恢复期间不能重新添加压缩包。"), true);
      }
    } catch (e) { /* ignore */ }
  })();

  // 拉取初始数据（即使 splash 阶段也可填充）
  refreshConfig();
  refreshService();
  refreshVersions();

  // 服务状态周期刷新（常规页与帮助页可见时：后者的操作按钮/警告提示按运行态渲染）
  setInterval(() => {
    if (state.page === "general" || state.page === "help") refreshService();
  }, 3000);
}

window.addEventListener("DOMContentLoaded", init);

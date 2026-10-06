(() => {
  // 浏览器直开（docs/mock/）：页面 / 语言 / 滚动位置从 URL 参数取；
  // 截图渲染器走 CDP addScriptToEvaluateOnNewDocument 注入全局，无 query，行为不变。
  const Q = (() => { try { return new URLSearchParams(location.search); } catch { return new URLSearchParams(""); } })();
  if (Q.get("page") !== null) window.__shotPage = Q.get("page") || "";
  if (Q.get("scroll") !== null) window.__shotScroll = Q.get("scroll") || "";
  const CFG = { demo: {"email":"demo@example.com","syncedAt":1767231000,"harnessDir":"C:\\Users\\demo\\.dsh","logPath":"C:\\Users\\demo\\AppData\\Roaming\\dsh-systray\\logs\\dsh-systray.log","appVersion":"1.1.0","harnessVersion":"0.1.1","port":3080,"webURL":"http://127.0.0.1:3080/","plugins":[{"id":"chat-billing","name":"chat-billing","version":"1.2.0","spec":"^1.2.0","source":"npm","profile":"","canUpdate":true,"reason":"","localDir":"","pendingLocal":false,"ghostDisabled":false,"disabled":false,"disabledReason":"","pendingOp":""},{"id":"session-indexer","name":"session-indexer","version":"0.4.1","spec":"^0.4.1","source":"npm","profile":"","canUpdate":true,"reason":"","localDir":"","pendingLocal":false,"ghostDisabled":false,"disabled":false,"disabledReason":"","pendingOp":""},{"id":"prompt-assistant","name":"prompt-assistant","version":"0.7.3","spec":"github:example/prompt-assistant","source":"github","profile":"","canUpdate":true,"reason":"","localDir":"","pendingLocal":false,"ghostDisabled":false,"disabled":false,"disabledReason":"","pendingOp":""},{"id":"my-dev-tool","name":"my-dev-tool","version":"0.2.0","spec":"file:…/my-dev-tool","source":"file","profile":"","canUpdate":false,"reason":"本地路径安装，无远程来源，无法更新","localDir":"","pendingLocal":false,"ghostDisabled":false,"disabled":false,"disabledReason":"","pendingOp":""},{"id":"legacy-bundle","name":"legacy-bundle","version":"1.8.0","spec":"https://example.com/packages/legacy-bundle-1.8.0.tgz","source":"tarball","profile":"","canUpdate":false,"reason":"以固定压缩包地址安装，无法判断更新","localDir":"","pendingLocal":false,"ghostDisabled":false,"disabled":false,"disabledReason":"","pendingOp":""}],"demoLog":["2026-08-11 09:12:01 [INFO] [app] dsh-systray v1.1.0 已启动（pid 12345）","2026-08-11 09:12:02 [INFO] [app] 运行环境就绪：node v24.9.0 / pnpm 10.34.5","2026-08-11 09:12:03 [INFO] [server] 正在启动 DeepSeek Harness Web 服务：127.0.0.1:3080","2026-08-11 09:12:04 [INFO] [server] 服务已就绪，可在托盘「打开 Web UI」进入","2026-08-11 09:12:05 [INFO] [app] 已检测到 12 个历史会话","2026-08-11 09:12:07 [WARN] [app] 日志文件较大，已截断显示最近 4000 行","2026-08-11 09:12:10 [INFO] [app] 导出完成：dsh-systray-export-20260811-091210-1a2b3c4d.zip","2026-08-11 09:12:12 [INFO] [app] 已是最新版本（当前 v1.1.0）"],"demoLogEn":["2026-08-11 09:12:01 [INFO] dsh-systray v1.1.0 starting (pid 12345)","2026-08-11 09:12:02 [INFO] runtime ready: node v24.9.0 / pnpm 10.34.5","2026-08-11 09:12:03 [server] starting DeepSeek Harness web service on 127.0.0.1:3080","2026-08-11 09:12:04 [server] service ready — open the Web UI","2026-08-11 09:12:05 [INFO] 12 session records detected","2026-08-11 09:12:07 [WARN] log file is large; showing the most recent 4000 lines","2026-08-11 09:12:10 [INFO] export finished: dsh-systray-export-20260811-091210-1a2b3c4d.zip","2026-08-11 09:12:12 [INFO] already up to date (current v1.1.0)"],"files":{"syncedAt":1767231000,"mtime":1786410600,"receiveDir":"C:\\Users\\demo\\Documents\\DeepSeekSync","quotaUsed":1234567,"quotaLimit":10485760,"entries":[{"id":"demo-entry-1","name":".dsh","kind":"dir","isSource":true,"path":"C:\\Users\\demo\\.dsh","size":1184280,"status":"synced","error":"","files":[{"relPath":"settings.yaml","name":"settings.yaml","size":1843,"mtime":0,"status":"synced","error":"","blocked":false},{"relPath":"profiles/web/package.json","name":"package.json","size":2458,"mtime":0,"status":"synced","error":"","blocked":false},{"relPath":"sessions/2026-08-11.jsonl","name":"2026-08-11.jsonl","size":1179979,"mtime":0,"status":"synced","error":"","blocked":false}]},{"id":"demo-entry-2","name":"MEMORY.md","kind":"file","isSource":true,"path":"C:\\Users\\demo\\dsh-workspace\\memory\\MEMORY.md","size":6144,"status":"synced","error":"","files":[{"relPath":"MEMORY.md","name":"MEMORY.md","size":6144,"mtime":0,"status":"synced","error":"","blocked":false}]},{"id":"demo-entry-3","name":"SKILL.md","kind":"file","isSource":true,"path":"C:\\Users\\demo\\dsh-workspace\\skills\\doc-export\\SKILL.md","size":2048,"status":"uploading","error":"","files":[{"relPath":"SKILL.md","name":"SKILL.md","size":2048,"mtime":0,"status":"uploading","error":"","blocked":false,"speedBps":1310720}]},{"id":"demo-entry-4","name":"notes.txt","kind":"file","isSource":false,"path":"C:\\Users\\demo\\Documents\\DeepSeekSync\\notes.txt","size":2048,"status":"synced","error":"","files":[{"relPath":"notes.txt","name":"notes.txt","size":2048,"mtime":0,"status":"synced","error":"","blocked":false}]}],"pendingCount":0,"pendingFiles":[],"remoteAppliedAt":1786410360,"remoteAppliedCount":2}}, lang: (Q.get("lang") || "zh") };
  const D = CFG.demo;
  const listeners = new Map();
  const noop = () => {};
  const emptyLog = () => ({ lines: [], nextOffset: 0, reset: false });

  const api = {
    // ---- 截图模式开关（真实程序由 DSH_SYSTRAY_SHOT_PAGE 提供）----
    GetShotPage: async () => window.__shotPage || "",
    GetShotScroll: async () => window.__shotScroll || "",

    // ---- 常规 ----
    GetConfig: async () => ({
      port: D.port, harnessDir: D.harnessDir, startupTimeoutSec: 300,
      updateMirror: "", harnessPrerelease: false, webURL: D.webURL,
      autostart: true, autostartLaunch: false,
      language: CFG.lang, curLang: CFG.lang === "auto" ? "zh" : CFG.lang,
      proxy: "",
      launchTarget: "auto", launchResolved: "web",
      desktopInstalled: false, desktopVersion: "", desktopPath: "",
      desktopRunning: false, desktopChannel: "nightly", desktopFeedURL: "",
    }),
    GetServiceState: async () => ({ state: "running", reason: "", webURL: D.webURL, runningPort: D.port, tokenFound: true }),
    GetVersions: async () => ({ app: D.appVersion, harness: D.harnessVersion, engine: "web" }),
    SetPort: noop, SetAutostart: noop, SetHarnessPrerelease: noop, SetUpdateMirror: noop,
    SetLanguage: noop, SetLaunchTarget: noop, SetHarnessDir: noop, SetStartupTimeoutSec: noop,

    // ---- 关于 ----
    GetInstalledPlugins: async () => D.plugins,
    GetPendingPluginChanges: async () => [],
    CheckPluginUpdate: async (id) => {
      const p = D.plugins.find((x) => x.id === id || x.name === id) || { name: id };
      const latest = p.name === "chat-billing" ? "1.4.0" : (p.version || "");
      return { name: p.name, current: p.version || "", latest, hasUpdate: latest !== p.version };
    },
    CheckSystrayUpdate: async () => ({ current: D.appVersion, latest: D.appVersion, hasUpdate: false, note: "", error: "" }),
    CheckHarnessUpdate: async () => ({ current: D.harnessVersion, latest: D.harnessVersion, hasUpdate: false, note: "", error: "" }),
    StartPluginUpdate: noop, RemovePlugin: noop, EnablePlugin: noop, ApplyLocalPluginUpdate: noop,
    DiscardPendingPluginChange: async () => true, DiscardAllPendingPluginChanges: async () => 0,
    ApplyPendingPluginChanges: async () => true, RepairPluginSessionEvents: async () => ({ ok: true, files: 0, events: 0, remainingSessions: 0, remainingEvents: 0, reason: "" }),
    PickLocalPluginPath: async () => ({ error: "", canceled: true, path: "", version: "", current: "", relation: "" }),
    StartUpdate: noop, StartHarnessUpdate: noop, CancelUpdate: noop,
    InstallDesktopApp: noop, LaunchDesktopApp: noop, OpenDefaultUI: noop,

    // ---- 日志 ----
    GetLogFiles: async () => [{ name: "dsh-systray.log", path: D.logPath, exists: true, size: 4096 }],
    GetLogPath: async () => D.logPath,
    // 日志页英文态：与前端一致地展示样例英文日志（真实运行日志是诊断内容，不翻译）
    ReadLogTail: async () => ({
      lines: CFG.lang === "en" ? D.demoLogEn : D.demoLog,
      nextOffset: 4096, reset: false,
    }),
    ReadLogArchives: async () => emptyLog(),
    ClearLog: noop,

    // ---- 导出 / 导入 ----
    GetExportOptions: async () => ([
      { kind: "sessions", label: "所有历史会话", sub: "sessions.zip · ~/.dsh/sessions", selected: true },
      { kind: "plugins", label: "已安装的插件", sub: "plugins.zip · 通过 dsh add 安装的插件", selected: false },
      { kind: "files", label: "需要打包的文件目录", sub: "files.zip · 恢复时选择解压位置", selected: false },
    ]),
    PickExportDir: async () => "", PickSavePath: async () => "", StartExport: noop, OpenExportDir: noop,
    ImportPick: async () => ({
      // 与 Go 侧截图模式的 ImportPick 同口径（演示包名 + i18n 后的条目标签）
      path: CFG.lang === "en"
        ? "C:\\Users\\demo\\Downloads\\dsh-systray-export-20260903-091210-1a2b3c4d.zip"
        : "dsh-systray-export-20260903-091210-1a2b3c4d.zip",
      items: CFG.lang === "en"
        ? [
            { kind: "sessions", label: "Session history", size: 2482124 },
            { kind: "plugins", label: "Installed plugins", size: 1892356 },
            { kind: "files", label: "Selected folders", size: 128512000 },
          ]
        : [
            { kind: "sessions", label: "历史会话记录", size: 2482124 },
            { kind: "plugins", label: "已安装插件", size: 1892356 },
            { kind: "files", label: "自选文件目录", size: 128512000 },
          ],
    }),
    GetImportItems: async () => [],
    PreviewRestore: async () => ({ canceled: true, conflict: false, conflicts: 0, tops: [], error: "" }),
    ApplyRestore: noop, CancelRestore: async () => "", RestoreBusy: async () => false, ImportInflight: async () => [],

    // ---- 账号同步（脱敏：演示邮箱与固定同步时间）----
    AccountStatus: async () => ({
      loggedIn: true, email: D.email, deviceId: "", expireReason: "",
      lastSyncedAt: D.syncedAt, baselineDone: true, syncing: false, syncError: "",
      pendingOps: 0, pendingApply: false, pendingApplyCount: 0,
      applying: false, applyError: "", startupChecked: true, apiBase: "https://connect.dsh.dev",
    }),
    AccountRequestCode: async () => ({ expiresInSec: 600, resendAfterSec: 60 }),
    AccountVerify: async () => api.AccountStatus(),
    AccountLogout: async () => ({ loggedIn: false, email: "", deviceId: "", expireReason: "", lastSyncedAt: 0, baselineDone: false, syncing: false, syncError: "", pendingOps: 0, pendingApply: false, pendingApplyCount: 0, applying: false, applyError: "", startupChecked: true, apiBase: "" }),
    AccountSyncNow: async () => api.AccountStatus(),
    AccountApplyPending: async () => api.AccountStatus(),
    CancelSyncApply: noop,

    // ---- 文件同步（数据同步页的文件卡；演示条目见 DEMO.files）----
    FilesStatus: async () => {
      const F = D.files;
      const stamp = (m) => (m || F.mtime);
      return {
        loggedIn: true, syncing: false, applying: false, lastError: "",
        lastSyncedAt: F.syncedAt, quotaUsed: F.quotaUsed, quotaLimit: F.quotaLimit, quotaTier: "free",
        receiveDir: F.receiveDir,
        entries: F.entries.map((e) => ({ ...e, files: e.files.map((f) => ({ ...f, mtime: stamp(f.mtime) })) })),
        pendingCount: F.pendingCount, pendingFiles: F.pendingFiles, blockedCount: 0,
        remoteAppliedAt: F.remoteAppliedAt, remoteAppliedCount: F.remoteAppliedCount,
      };
    },
    FilesAdd: async () => api.FilesStatus(),
    FilesSyncNow: async () => api.FilesStatus(),
    FilesApplyPending: async () => api.FilesStatus(),
    FilesRemoveEntry: async () => api.FilesStatus(),
    FilesRemovePath: async () => api.FilesStatus(),
    FilesRenameEntry: async () => api.FilesStatus(),
    FilesOpenEntry: noop,

    // ---- 帮助 / 重置 / 其它 ----
    WebTokenURL: async () => D.webURL,
    OpenWebUI: noop, RestartService: async () => true,
    GetResetStats: async () => ({ sessionCount: 12, pluginCount: 5 }),
    GetResetVersions: async () => ({ form: "list", current: D.harnessVersion, options: [], default: D.harnessVersion, note: "" }),
    ResetHarness: noop, PickHarnessDir: async () => "", CopyToClipboard: noop, HideWindow: noop,
  };

  // 未列出的绑定：返回 resolved undefined，避免前端 await 报错。
  const handler = {
    get: (t, k) => (k in t ? t[k] : async () => undefined),
  };
  window.go = { main: { App: new Proxy(api, handler) } };

  // Wails runtime 最小实现（EventsOn / EventsEmit / BrowserOpenURL …）
  window.runtime = new Proxy({
    EventsOn: (name, cb) => { if (!listeners.has(name)) listeners.set(name, []); listeners.get(name).push(cb); return () => {}; },
    EventsOnMultiple: (name, cb) => window.runtime.EventsOn(name, cb),
    EventsOnce: (name, cb) => window.runtime.EventsOn(name, cb),
    EventsOff: (name) => listeners.delete(name),
    EventsOffAll: () => listeners.clear(),
    EventsEmit: (name, ...args) => { (listeners.get(name) || []).forEach((cb) => { try { cb(...args); } catch (e) { console.error(e); } }); },
    WindowSetAlwaysOnTop: noop, WindowSetLightTheme: noop, WindowSetDarkTheme: noop,
    WindowSetSystemDefaultTheme: noop, WindowSetTitle: noop, WindowShow: noop, WindowHide: noop,
    WindowCenter: noop, WindowSetSize: noop, WindowSetMinSize: noop, WindowSetMaxSize: noop,
    ClipboardSetText: async () => true, ClipboardGetText: async () => "",
    BrowserOpenURL: noop, Quit: noop, LogPrint: noop, LogTrace: noop, LogDebug: noop,
    LogInfo: noop, LogWarning: noop, LogError: noop, LogFatal: noop,
    Environment: async () => ({ buildType: "production", platform: "windows", arch: "amd64" }),
  }, { get: (t, k) => (k in t ? t[k] : noop) });

  // 固定渲染时钟：截图物料里不出现「拍摄当天」的日期。
  const FROZEN = 1786410732000;
  const RealDate = Date;
  const FrozenDate = function (...a) { return a.length ? new RealDate(...a) : new RealDate(FROZEN); };
  FrozenDate.prototype = RealDate.prototype;
  FrozenDate.now = () => FROZEN;
  FrozenDate.parse = RealDate.parse;
  FrozenDate.UTC = RealDate.UTC;
  window.Date = FrozenDate;
})();
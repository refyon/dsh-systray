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
  logOffset: 0,
  logTimer: null,
  expDirs: [],          // 已选打包目录
  expSelected: { sessions: true, plugins: false, files: false },
  impItems: [],
  updateProgress: null, // {text, pct} 更新进度
  splashMode: "startup", // startup | update
};

// ==================== 页面路由 ====================

const PAGE_TITLES = { general: "常规", about: "关于", logs: "日志", export: "导出", import: "导入" };

function showPage(name) {
  state.page = name;
  document.querySelectorAll(".nav-item").forEach((b) => b.classList.toggle("active", b.dataset.page === name));
  $("page-title").textContent = PAGE_TITLES[name];
  document.querySelectorAll(".page").forEach((p) => p.classList.add("hidden"));
  $("page-" + name).classList.remove("hidden");
  if (name === "logs") startLogPolling();
  else stopLogPolling();
}

function showSplash(mode, statusText) {
  state.splashMode = mode || "startup";
  $("splash-cancel").classList.toggle("hidden", state.splashMode !== "update");
  $("splash-status").textContent = statusText || "正在准备运行环境…";
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
  } catch (e) { console.error("GetConfig", e); }
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
    $("svc-text").textContent = labels[state.svc.state] || state.svc.state;
    $("svc-sub").textContent = state.svc.state === "failed"
      ? (state.svc.reason || "请查看日志")
      : (state.svc.state === "running" ? "服务就绪，可打开 Web UI" : "服务就绪后可打开 Web UI");
  } catch (e) { console.error("GetServiceState", e); }
}

function wireGeneral() {
  $("sw-autostart").addEventListener("click", async () => {
    const on = $("sw-autostart").getAttribute("aria-checked") !== "true";
    await bindings().SetAutostart(on);
    $("sw-autostart").setAttribute("aria-checked", String(on));
  });
  $("inp-port").addEventListener("change", async (e) => {
    const v = parseInt(e.target.value, 10);
    if (v > 0 && v <= 65535) await bindings().SetPort(v);
    refreshConfig();
  });
  $("btn-pick-harness").addEventListener("click", async () => {
    const dir = await bindings().PickHarnessDir();
    if (dir) await bindings().SetHarnessDir(dir);
    refreshConfig();
  });
  $("btn-restart").addEventListener("click", async () => {
    $("btn-restart").disabled = true;
    $("svc-sub").textContent = "正在重启后台服务…";
    const ok = await bindings().RestartService();
    if (!ok) $("svc-sub").textContent = "重启失败，请查看日志";
    setTimeout(() => { $("btn-restart").disabled = false; refreshService(); }, 2000);
  });
}

// ==================== 关于页 ====================

async function refreshVersions() {
  const a = bindings();
  if (!a) return;
  try {
    const v = await a.GetVersions();
    $("ver-app").textContent = v.app || "dev";
    $("ver-harness").textContent = v.harness || "—";
  } catch (e) { console.error("GetVersions", e); }
}

function wireAbout() {
  $("sw-prerelease").addEventListener("click", async () => {
    const on = $("sw-prerelease").getAttribute("aria-checked") !== "true";
    await bindings().SetHarnessPrerelease(on);
    $("sw-prerelease").setAttribute("aria-checked", String(on));
  });
  $("btn-check-update").addEventListener("click", async () => {
    const btn = $("btn-check-update");
    btn.disabled = true;
    $("update-hint").textContent = "正在检查更新…";
    const info = await bindings().CheckUpdateManual();
    btn.disabled = false;
    if (info.error) {
      $("update-hint").textContent = "检查更新失败：" + info.error;
    } else if (info.hasUpdate) {
      $("update-hint").textContent = "发现新版本 " + info.latest + "（当前 " + (info.current || "dev") + "）";
      bindings().StartUpdate();
      showSplash("update", "正在准备更新…");
    } else {
      $("update-hint").textContent = "已是最新版本（" + (info.current || "dev") + "）";
    }
  });
}

// ==================== 日志页 ====================

const LEVEL_RE = /^(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})\s+(INFO|WARN|ERROR|DEBUG)\s+(.*)$/;

function renderLog(lines) {
  const view = $("log-view");
  const atBottom = view.scrollHeight - view.scrollTop - view.clientHeight < 40;
  for (const ln of lines) {
    const m = ln.match(LEVEL_RE);
    const div = document.createElement("div");
    div.className = "log-line";
    if (m) {
      const lvl = m[2].toLowerCase();
      div.innerHTML =
        '<span style="color:var(--muted)">' + esc(m[1]) + " </span>" +
        '<span class="lvl-' + lvl + '">' + esc(m[2]) + "</span> " +
        esc(m[3]);
    } else {
      div.textContent = ln;
    }
    view.appendChild(div);
  }
  // 限制 DOM 行数，避免长期运行后卡顿（保留最近 4000 行）
  while (view.childElementCount > 4000) view.removeChild(view.firstChild);
  if (atBottom) view.scrollTop = view.scrollHeight;
}

function esc(s) {
  const d = document.createElement("div");
  d.textContent = s;
  return d.innerHTML;
}

function escAttr(s) { return esc(s).replace(/"/g, "&quot;"); }

async function pollLog() {
  const a = bindings();
  if (!a || state.page !== "logs") return;
  try {
    const tail = await a.ReadLogTail(state.logOffset);
    if (tail.lines && tail.lines.length) renderLog(tail.lines);
    state.logOffset = tail.nextOffset;
  } catch (e) { console.error("ReadLogTail", e); }
}

function startLogPolling() {
  stopLogPolling();
  (async () => {
    const a = bindings();
    if (a) $("log-path").textContent = await a.GetLogPath();
  })();
  $("log-view").textContent = "";
  state.logOffset = 0;
  pollLog();
  state.logTimer = setInterval(pollLog, 2000);
}

function stopLogPolling() {
  if (state.logTimer) { clearInterval(state.logTimer); state.logTimer = null; }
}

function wireLogs() {
  $("btn-log-refresh").addEventListener("click", () => { $("log-view").textContent = ""; state.logOffset = 0; pollLog(); });
  $("btn-log-clear").addEventListener("click", async () => {
    await bindings().ClearLog();
    $("log-view").textContent = "";
    state.logOffset = 0;
  });
}

// ==================== 导出页 ====================

function renderExportRows() {
  const a = bindings();
  const wrap = $("exp-rows");
  wrap.innerHTML = "";
  const opts = [
    { kind: "sessions", label: "所有历史会话", sub: "sessions.zip · ~/.dsh/sessions" },
    { kind: "plugins", label: "已安装的插件", sub: "plugins.zip · 通过 dsh add 安装的插件" },
    { kind: "files", label: "需要打包的文件目录", sub: "files.zip · 恢复时选择解压位置" },
  ];
  for (const o of opts) {
    const div = document.createElement("div");
    div.className = "exp-item" + (state.expSelected[o.kind] ? " selected" : "");
    div.dataset.kind = o.kind;
    div.innerHTML =
      '<div class="check">✓</div>' +
      "<div><div class=\"exp-label\">" + o.label + "</div><div class=\"exp-sub\">" + o.sub + "</div></div>";
    div.addEventListener("click", () => {
      state.expSelected[o.kind] = !state.expSelected[o.kind];
      renderExportRows();
      updateExportHint();
    });
    wrap.appendChild(div);
  }
  // 文件目录子列表
  for (const d of state.expDirs) {
    const div = document.createElement("div");
    div.className = "exp-item selected";
    div.innerHTML =
      '<div class="check">✓</div><div style="flex:1;min-width:0"><div class="exp-label">' +
      esc(d) + "</div><div class=\"exp-sub\">用户选择目录</div></div>" +
      '<button class="btn btn-ghost" data-remove-dir="' + escAttr(d) + '">移除</button>';
    div.querySelector("[data-remove-dir]").addEventListener("click", (e) => {
      e.stopPropagation();
      state.expDirs = state.expDirs.filter((x) => x !== d);
      renderExportRows();
      updateExportHint();
    });
    wrap.appendChild(div);
  }
  updateExportHint();
}

function updateExportHint() {
  const n = Object.values(state.expSelected).filter(Boolean).length;
  $("exp-hint").textContent = n > 0
    ? "已选 " + n + " 项，点击「导出…」打包为 zip" + (state.expDirs.length ? "（含 " + state.expDirs.length + " 个目录）" : "")
    : "请至少勾选一项，或为「文件目录」添加目录";
}

function wireExport() {
  renderExportRows();
  $("btn-pick-dir").addEventListener("click", async () => {
    const p = await bindings().PickExportDir();
    if (p && !state.expDirs.includes(p)) {
      state.expDirs.push(p);
      state.expSelected.files = true;
      renderExportRows();
    }
  });
  $("btn-export").addEventListener("click", async () => {
    const any = Object.values(state.expSelected).some(Boolean);
    if (!any) { $("exp-hint").textContent = "请至少勾选一项"; return; }
    $("btn-export").disabled = true;
    $("exp-progress").classList.remove("hidden");
    $("exp-text").textContent = "正在准备导出…";
    await bindings().StartExport(
      state.expSelected.sessions,
      state.expSelected.plugins,
      state.expSelected.files,
      state.expDirs
    );
  });
}

// ==================== 导入页 ====================

function renderImportRows() {
  const wrap = $("imp-rows");
  wrap.innerHTML = "";
  if (!state.impItems.length) {
    $("imp-hint").textContent = "选择 dsh-systray 导出压缩包后可恢复会话、插件或文件目录。";
    return;
  }
  $("imp-hint").textContent = "解析成功：共 " + state.impItems.length + " 个可恢复项，点击右侧「恢复」逐项恢复。";
  for (const it of state.impItems) {
    const div = document.createElement("div");
    div.className = "imp-item";
    div.innerHTML =
      "<div><div class=\"exp-label\">" + esc(it.label) + "</div>" +
      (it.size ? '<div class="exp-sub">' + fmtSize(it.size) + "</div>" : "") + "</div>" +
      '<button class="btn btn-primary" data-restore="' + escAttr(it.kind) + '">恢复</button>';
    div.querySelector("[data-restore]").addEventListener("click", async () => {
      const btn = div.querySelector("button");
      btn.disabled = true;
      $("imp-progress").classList.remove("hidden");
      $("imp-text").textContent = "正在恢复…";
      // files 类目需要先选解压位置，由后端弹目录选择器
      await bindings().StartImport(it.kind, true);
      setTimeout(() => { btn.disabled = false; }, 1500);
    });
    wrap.appendChild(div);
  }
}

function fmtSize(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}

function wireImport() {
  $("btn-import-pick").addEventListener("click", async () => {
    try {
      const res = await bindings().ImportPick();
      if (!res) return;
      state.impItems = res.items || [];
      $("imp-path").textContent = res.path || "";
      $("imp-path").classList.remove("hidden");
      renderImportRows();
    } catch (e) {
      $("imp-hint").textContent = "解析失败：" + (e && e.message ? e.message : e);
      $("imp-rows").innerHTML = "";
      state.impItems = [];
    }
  });
}

// ==================== 事件监听（Go → JS） ====================

function wireEvents() {
  EventsOn("splash:progress", (d) => {
    if (!d) return;
    if (d.phase === "update" && state.splashMode !== "update") showSplash("update", d.text || "正在更新…");
    if (d.phase === "startup") {
      $("splash").classList.remove("hidden");
      $("settings").classList.add("hidden");
      state.splashMode = "startup";
    }
    if (d.text) $("splash-status").textContent = d.text;
    if (typeof d.pct === "number") $("splash-fill").style.width = Math.round(d.pct * 100) + "%";
  });

  EventsOn("splash:done", () => {
    // 启动完成：切换设置视图（窗口随后由 Go 侧隐藏）
    showSettings();
    refreshService();
  });

  EventsOn("ui:show-splash", () => showSplash("startup", "正在准备运行环境…"));
  EventsOn("ui:show-settings", () => { showSettings(); refreshConfig(); refreshService(); });

  EventsOn("update:progress", (d) => {
    if (!d) return;
    $("update-progress").classList.remove("hidden");
    $("update-text").textContent = d.text || "";
    $("update-fill").style.width = Math.round((d.pct || 0) * 100) + "%";
  });

  EventsOn("export:progress", (d) => {
    if (!d) return;
    $("exp-text").textContent = d.text || "";
    $("exp-fill").style.width = Math.round((d.pct || 0) * 100) + "%";
  });

  EventsOn("export:done", (d) => {
    $("btn-export").disabled = false;
    if (d && d.error) {
      $("exp-hint").textContent = "导出失败：" + d.error;
    } else if (d && d.path) {
      $("exp-hint").textContent = "导出完成：" + d.path;
      $("exp-fill").style.width = "100%";
    }
    setTimeout(() => { $("exp-progress").classList.add("hidden"); }, 6000);
  });

  EventsOn("import:progress", (d) => {
    if (!d) return;
    $("imp-text").textContent = d.text || "";
    $("imp-fill").style.width = Math.round((d.pct || 0) * 100) + "%";
  });

  EventsOn("import:done", (d) => {
    if (!d) return;
    if (d.error) {
      $("imp-hint").textContent = "恢复失败：" + d.error;
    } else {
      $("imp-hint").textContent = "恢复完成 ✓";
      $("imp-fill").style.width = "100%";
    }
    setTimeout(() => { $("imp-progress").classList.add("hidden"); }, 6000);
    renderImportRows();
  });

  EventsOn("service:restart", (d) => {
    if (d && d.stage) $("svc-sub").textContent = d.stage;
  });

  // 更新完成事件（更新进度窗口关闭时前端回到设置视图）
  EventsOn("update:done", () => showSettings());
}

// 取消更新
function wireSplashCancel() {
  $("splash-cancel").addEventListener("click", () => {
    bindings().CancelUpdate();
    $("splash-cancel").classList.add("hidden");
  });
}

// ==================== 启动 ====================

async function init() {
  wireEvents();
  wireSplashCancel();
  wireGeneral();
  wireAbout();
  wireLogs();
  wireExport();
  wireImport();

  document.querySelectorAll(".nav-item").forEach((b) => {
    b.addEventListener("click", () => showPage(b.dataset.page));
  });

  // 初始视图：等待 Go 侧 ui:show-splash 事件（非自启动时窗口显示 splash）
  showSplash("startup", "正在准备运行环境…");

  // 截图/预览：DSH_SYSTRAY_SHOT_PAGE 指定后直接显示对应页面
  try {
    const shot = await bindings().GetShotPage();
    if (shot && PAGE_TITLES[shot]) showPage(shot);
  } catch (e) { /* ignore */ }

  // 拉取初始数据（即使 splash 阶段也可填充）
  refreshConfig();
  refreshService();
  refreshVersions();

  // 服务状态周期刷新（设置视图可见时）
  setInterval(() => {
    if (state.page === "general") refreshService();
  }, 3000);
}

window.addEventListener("DOMContentLoaded", init);

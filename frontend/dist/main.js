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
  logName: "app.log",
  logOffset: 0,
  logTimer: null,
  expDirs: [],          // 已选打包目录
  expSelected: { sessions: true, plugins: false, files: false },
  impItems: [],
  impDone: {},          // kind → true：恢复完成标记（对应项显示 ✓ 已完成）
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
    if (!ok) $("svc-sub").textContent = "重启失败，请查看日志";
    setTimeout(() => { $("btn-restart").disabled = false; refreshService(); }, 2000);
  });
  $("btn-reset-harness").addEventListener("click", async () => {
    const btn = $("btn-reset-harness");
    const ok = await confirmDialog(
      "重置 DeepSeek Harness？",
      "重置将删除所有已安装的插件（无法恢复），并把 DeepSeek Harness 回退到官方最后发布的稳定版本，用于排除插件导致的服务启动失败。\n\n确定继续吗？",
      "确定重置"
    );
    if (!ok) return;
    btn.disabled = true;
    try {
      showSplash("startup", "正在重置 DeepSeek Harness…");
      await bindings().ResetHarness();
    } finally {
      setTimeout(() => { btn.disabled = false; }, 1500);
    }
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
    // 开启预发布通道前提示风险：可能导致服务启动失败
    if (on) {
      const ok = await confirmDialog(
        "开启预发布通道？",
        "开启后，harness 更新可能安装到不稳定的 alpha / beta / rc 预发布版，可能导致服务启动失败。确定开启吗？",
        "确定开启"
      );
      if (!ok) return;
    }
    await bindings().SetHarnessPrerelease(on);
    $("sw-prerelease").setAttribute("aria-checked", String(on));
  });
  $("btn-check-update").addEventListener("click", async () => {
    const btn = $("btn-check-update");
    const hub = $("btn-harness-update");
    btn.disabled = true;
    $("update-hint").textContent = "正在检查更新…";
    const info = await bindings().CheckUpdateManual();
    btn.disabled = false;
    if (info.error) {
      $("update-hint").textContent = "检查更新失败：" + info.error;
      hub.classList.add("hidden");
      return;
    }
    // 同时展示 harness（按预发布通道）与 dsh-systray 两个检查结果
    const parts = [];
    if (info.harnessHasUpdate) {
      parts.push("Harness 有新版本 " + info.harnessLatest + "（当前 " + (info.harnessCurrent || "—") + "）");
    }
    if (info.hasUpdate) {
      parts.push("dsh-systray 有新版本 " + info.latest + "（当前 " + (info.current || "dev") + "）");
    }
    if (!parts.length) {
      parts.push("已是最新版本（systray " + (info.current || "dev") + " · harness " + (info.harnessCurrent || "未检测到") + "）");
    }
    $("update-hint").textContent = parts.join("；");
    hub.classList.toggle("hidden", !info.harnessHasUpdate);
    // harness 有新版时优先交由用户处理；否则保持原有自动更新 dsh-systray 的行为
    if (info.hasUpdate && !info.harnessHasUpdate) {
      bindings().StartUpdate();
      showSplash("update", "正在准备更新…");
    }
  });
  $("btn-harness-update").addEventListener("click", async () => {
    const hub = $("btn-harness-update");
    hub.disabled = true;
    $("update-hint").textContent = "正在更新 DeepSeek Harness…";
    await bindings().StartHarnessUpdate();
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
    const tail = await a.ReadLogTail(state.logName, state.logOffset);
    if (tail.lines && tail.lines.length) renderLog(tail.lines);
    state.logOffset = tail.nextOffset;
  } catch (e) { console.error("ReadLogTail", e); }
}

function setLogFile(name) {
  state.logName = name;
  document.querySelectorAll(".log-tab").forEach((b) => {
    const on = b.dataset.log === name;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", String(on));
  });
  $("log-view").textContent = "";
  state.logOffset = 0;
  (async () => {
    const a = bindings();
    if (a) $("log-path").textContent = await a.GetLogPath(name);
  })();
  pollLog();
}

function startLogPolling() {
  stopLogPolling();
  const a = bindings();
  if (!a) return;
  // 初始化：更新文件选择器可用状态 + 当前路径
  (async () => {
    try {
      const files = await a.GetLogFiles();
      files.forEach((f) => {
        const tab = document.querySelector('.log-tab[data-log="' + f.name + '"]');
        if (tab) tab.disabled = !f.exists;
      });
    } catch (e) { /* ignore */ }
    setLogFile(state.logName);
  })();
  state.logTimer = setInterval(pollLog, 2000);
}

function stopLogPolling() {
  if (state.logTimer) { clearInterval(state.logTimer); state.logTimer = null; }
}

function wireLogs() {
  document.querySelectorAll(".log-tab").forEach((b) => {
    b.addEventListener("click", () => setLogFile(b.dataset.log));
  });
  $("btn-log-refresh").addEventListener("click", () => { $("log-view").textContent = ""; state.logOffset = 0; pollLog(); });
  $("btn-log-clear").addEventListener("click", async () => {
    await bindings().ClearLog(state.logName);
    $("log-view").textContent = "";
    state.logOffset = 0;
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
    // 文件目录二级列表：依附在「需要打包的文件目录」选项下方，无勾选框、保留移除按钮
    if (o.kind === "files") {
      const sub = document.createElement("div");
      sub.className = "exp-dirs" + (state.expSelected.files ? "" : " dim");
      for (const d of state.expDirs) {
        const row = document.createElement("div");
        row.className = "exp-dir-item";
        row.innerHTML =
          '<div class="exp-dir-label" title="' + escAttr(d) + '">' + esc(d) + "</div>" +
          '<button class="btn btn-ghost btn-sm" data-remove-dir="' + escAttr(d) + '">移除</button>';
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
    ? "已选 " + n + " 项，点击「导出…」打包为 zip" + (state.expDirs.length ? "（含 " + state.expDirs.length + " 个目录）" : "")
    : "请至少勾选一项，或为「文件目录」添加目录";
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
    showExportModal("正在准备导出…", 0);
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
    const done = !!state.impDone[it.kind];
    const div = document.createElement("div");
    div.className = "imp-item" + (done ? " imp-item-done" : "");
    div.innerHTML =
      "<div><div class=\"exp-label\">" + esc(it.label) + "</div>" +
      (it.size ? '<div class="exp-sub">' + fmtSize(it.size) + "</div>" : "") + "</div>" +
      (done ? '<span class="imp-done" title="本项已恢复完成">✓ 已完成</span>' : "") +
      '<button class="btn btn-primary" data-restore="' + escAttr(it.kind) + '">恢复</button>';
    div.querySelector("[data-restore]").addEventListener("click", async () => {
      const btn = div.querySelector("button");
      btn.disabled = true;
      try {
        // 1) 准备 + 冲突检测（files 类目此时由后端弹解压位置选择）
        const preview = await bindings().PreviewRestore(it.kind);
        if (!preview) return;
        if (preview.error) { $("imp-hint").textContent = "无法恢复：" + preview.error; return; }
        if (preview.canceled) return; // 用户取消了解压位置选择
        // 2) 有冲突 → 弹窗询问是否覆盖
        let overwrite = true;
        if (preview.conflicts > 0) {
          const detail = (preview.tops || []).slice(0, 3).join("、");
          overwrite = await confirmDialog(
            "检测到数据冲突",
            "「" + it.label + "」与现有内容存在 " + preview.conflicts + " 项冲突" +
              (detail ? "（" + detail + (preview.conflicts > 3 ? " 等" : "") + "）" : "") +
              "。\n\n选择「覆盖」将备份并替换现有内容；选择「跳过」则保留现有文件、只补缺失项。",
            "覆盖并恢复"
          );
          if (!overwrite) {
            $("imp-hint").textContent = "已选择跳过 " + preview.conflicts + " 项冲突，现有内容将保留。";
          }
        }
        // 3) 执行恢复
        $("imp-progress").classList.remove("hidden");
        $("imp-text").textContent = "正在恢复…";
        $("imp-fill").style.width = "0%";
        await bindings().ApplyRestore(it.kind, overwrite);
      } catch (e) {
        $("imp-hint").textContent = "恢复失败：" + (e && e.message ? e.message : e);
      } finally {
        setTimeout(() => { btn.disabled = false; }, 1200);
      }
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
    showExportModal(d.text || "", d.pct);
  });

  EventsOn("export:done", (d) => {
    $("btn-export").disabled = false;
    if (d && d.error) {
      $("exp-hint").textContent = "导出失败：" + d.error;
      showExportModal("导出失败：" + d.error, 0);
      $("exp-modal-close").classList.remove("hidden");
    } else if (d && d.path) {
      $("exp-hint").textContent = "导出完成：" + d.path;
      lastExportPath = d.path;
      $("exp-open").classList.remove("hidden");
      showExportModal("导出完成 ✓", 1);
      setTimeout(hideExportModal, 1600);
    }
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
      if (d.kind) state.impDone[d.kind] = true; // 完成标记：对应项显示 ✓ 已完成
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

// ==================== 确认弹层 ====================

/** 显示确认弹层，返回 Promise<boolean>（确定 true / 取消 false）。 */
function confirmDialog(title, msg, okLabel) {
  return new Promise((resolve) => {
    $("modal-title").textContent = title || "确认操作";
    $("modal-msg").textContent = msg || "";
    $("modal-ok").textContent = okLabel || "确定";
    $("modal").classList.remove("hidden");
    const done = (val) => {
      $("modal").classList.add("hidden");
      $("modal-cancel").removeEventListener("click", onCancel);
      $("modal-ok").removeEventListener("click", onOk);
      resolve(val);
    };
    const onCancel = () => done(false);
    const onOk = () => done(true);
    $("modal-cancel").addEventListener("click", onCancel);
    $("modal-ok").addEventListener("click", onOk);
    $("modal-cancel").focus();
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

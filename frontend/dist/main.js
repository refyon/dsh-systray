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
  logTimer: null,
  expDirs: [],          // 已选打包目录
  expSelected: { sessions: true, plugins: false, files: false },
  impItems: [],
  impDone: {},          // kind → true：恢复完成标记（对应项显示 ✓ 已完成）
  plugRows: [],         // 全量插件（Go PluginRow），供过滤渲染与事件委托按索引取用
  plugFilter: "",       // 插件过滤关键字（输入防抖后）
  plugState: {},        // name → {note,noteTone,upLatest,upShow}：滚动/过滤重渲染后恢复行内状态
  plugTimer: null,      // 过滤防抖计时器
  updateProgress: null, // {text, pct} 更新进度
  splashMode: "startup", // startup | update
  shotPage: "",         // 截图模式当前页（GetShotPage 返回；空=正常模式）
  shotScroll: "",       // 截图模式内容区滚动量（bottom/像素/空）
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
  // 关于页每次进入刷新版本号与插件清单（更新/导入等操作后保持最新）
  if (name === "about") {
    refreshVersions();
    loadPlugins();
  }
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
    updatePortHint();
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
    hint.textContent = "端口已修改为 " + p + "，当前服务仍运行于 " + rp + "——重启后台服务后生效。";
  } else {
    hint.classList.add("hidden");
  }
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
    updatePortHint();
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
  // 重置：打开勾选弹层（harness 必选；会话/插件按需勾选，展示将清除的数量）
  $("btn-reset-harness").addEventListener("click", async () => {
    const btn = $("btn-reset-harness");
    btn.disabled = true;
    try {
      const stats = await bindings().GetResetStats();
      const sc = (stats && stats.sessionCount) || 0;
      const pc = (stats && stats.pluginCount) || 0;
      $("reset-sessions-sub").textContent = "将清除 " + sc + " 条会话记录";
      $("reset-plugins-sub").textContent = "将清除 " + pc + " 个已安装插件";
      // 默认勾选插件（重置将物理删除已装插件，谨慎起见默认勾选）；会话默认不勾选（数据谨慎）
      $("reset-c-sessions").checked = false;
      $("reset-c-plugins").checked = pc > 0;
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
    showSplash("startup", "正在重置 DeepSeek Harness…");
    await bindings().ResetHarness(clearSessions, clearPlugins);
  });
  // 点遮罩等同取消
  $("reset-modal").querySelector(".modal-mask").addEventListener("click", () => $("reset-modal").classList.add("hidden"));
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
  const checkBtn = $("btn-check-" + which);
  const hintEl = $("hint-" + which);
  const upBtn = $(which === "systray" ? "btn-systray-update" : "btn-harness-update");
  checkBtn.disabled = true;
  hintEl.className = "update-note";
  hintEl.textContent = "正在检查更新…";
  upBtn.classList.add("hidden");
  try {
    const m = which === "systray" ? await a.CheckSystrayUpdate() : await a.CheckHarnessUpdate();
    if (m.error) {
      hintEl.classList.add("err");
      hintEl.textContent = "检查失败：" + m.error;
      return;
    }
    if (m.hasUpdate) {
      hintEl.classList.add("ok");
      hintEl.textContent = "发现新版本 " + vtag(m.latest) + "（当前 " + vtag(m.current) + "）";
      upBtn.classList.remove("hidden");
    } else if (m.note) {
      // 非网络失败的说明（如：仓库仅预发布而通道未开）——不再误报“无法获取”
      hintEl.textContent = m.current
        ? "已是最新（当前 " + vtag(m.current) + "）。" + m.note
        : m.note;
    } else {
      hintEl.textContent = "已是最新版本（" + vtag(m.current || m.latest) + "）";
    }
  } catch (e) {
    hintEl.classList.add("err");
    hintEl.textContent = "检查失败：" + (e && e.message ? e.message : e);
  } finally {
    checkBtn.disabled = false;
  }
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
  // dsh-systray / Harness：各自的检查按钮
  $("btn-check-systray").addEventListener("click", () => runModuleCheck("systray"));
  $("btn-check-harness").addEventListener("click", () => runModuleCheck("harness"));
  // dsh-systray 自身更新：确认后再下载（下载/安装/重启走 splash，窗口自动显示）
  $("btn-systray-update").addEventListener("click", async () => {
    const ok = await confirmDialog(
      "更新 dsh-systray？",
      "将下载并安装新版本并自动重启（当前为开发构建时无可用更新）。确认开始更新吗？",
      "开始更新"
    );
    if (!ok) return;
    $("btn-systray-update").disabled = true;
    bindings().StartUpdate();
    showSplash("update", "正在准备更新…");
  });
  // Harness 更新：确认后执行（进度走 splash，失败自动回退）
  $("btn-harness-update").addEventListener("click", async () => {
    const ok = await confirmDialog(
      "更新 DeepSeek Harness？",
      "将更新 DeepSeek Harness 到最新版本，更新期间服务会短暂重启，失败会自动回退。确认开始更新吗？",
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
      else if (btn.dataset.update !== undefined) doPluginUpdate(p, item, btn);
      else if (btn.dataset.del !== undefined) doPluginRemove(p, item, btn);
    });
  }
}

// ==================== 插件列表（每行单独检查 / 更新） ====================

const PLUG_SRC_LABEL = { npm: "npm", github: "GitHub", file: "本地", tarball: "压缩包", unknown: "未知来源" };

/** 行内小号状态文字：tone = ok | err | muted */
function setNote(item, text, tone) {
  const note = item.querySelector("[data-note]");
  if (!note) return;
  note.className = "plug-note" + (tone === "ok" || tone === "err" ? " " + tone : "");
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
  count.textContent = total ? (shown.length + " / " + total + " 个") : "";
  empty.textContent = total
    ? (shown.length ? "" : "没有匹配“" + state.plugFilter + "”的插件")
    : "未安装任何用户插件（在 Web UI 中通过 dsh add 安装）";
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
  badge.textContent = PLUG_SRC_LABEL[p.source] || p.source || "未知";
  name.appendChild(badge);

  const sub = document.createElement("div");
  sub.className = "plug-sub";
  sub.textContent = "当前版本 " + (p.version ? vtag(p.version) : "未安装") +
    (p.profile ? " · 环境 " + p.profile : "");

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
    checkBtn.textContent = "检查更新";
    checkBtn.dataset.check = "";
    actions.appendChild(checkBtn);
  }
  // 「更新」按钮：所有行同尺寸同文本，仅颜色区分——可用=主色（查出新版本后显示），不可用=灰
  const upBtn = document.createElement("button");
  upBtn.className = "btn btn-primary btn-xs";
  upBtn.dataset.update = "";
  upBtn.textContent = "更新";
  if (p.canUpdate) {
    upBtn.classList.add("hidden");
  } else {
    upBtn.classList.add("btn-muted"); // 与 btn-primary 相同版式，仅颜色表达不可用
  }
  upBtn.disabled = !p.canUpdate;
  actions.appendChild(upBtn);

  // 「删除」：确认后物理移除该插件（其全部环境），失败自动回退；完成后 Go 端发 plugins:changed 刷新。
  // 用“安静危险”样式（透明底 + 描边），避免整块红底在行内过于突兀。
  const delBtn = document.createElement("button");
  delBtn.className = "btn btn-danger-ghost btn-xs";
  delBtn.textContent = "删除";
  delBtn.dataset.del = "";
  actions.appendChild(delBtn);

  item.append(main, actions);
  if (!p.canUpdate && p.reason) setNote(item, p.reason, "muted");
  // 重渲染后恢复行内状态（检查结果 / 更新可用性），避免过滤/刷新丢失
  const st = state.plugState[p.name];
  if (st) applyPlugState(item, st);
  return item;
}

/** 恢复一行在检查/更新后留下的状态：note 文案语气 + 更新按钮可用性。 */
function applyPlugState(item, st) {
  if (st.note) setNote(item, st.note, st.noteTone || "muted");
  const upBtn = item.querySelector("button[data-update]");
  if (upBtn) {
    if (st.upShow) {
      upBtn.disabled = false;
      upBtn.classList.remove("hidden");
      upBtn.textContent = "更新" + (st.upLatest ? " v" + st.upLatest : "");
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
  const upBtn = item.querySelector("button[data-update]");
  const st = state.plugState[p.name] || (state.plugState[p.name] = {});
  try {
    const r = await a.CheckPluginUpdate(p.name);
    if (r.error) {
      st.note = "无法检查更新：" + r.error; st.noteTone = "err";
      setNote(item, st.note, st.noteTone);
      return;
    }
    if (r.hasUpdate) {
      st.note = "有新版本 " + vtag(r.latest) + "，可更新"; st.noteTone = "ok";
      st.upLatest = r.latest || ""; st.upShow = true;
      setNote(item, st.note, st.noteTone);
      if (upBtn) { upBtn.disabled = false; upBtn.classList.remove("hidden"); upBtn.textContent = "更新 v" + st.upLatest; }
    } else {
      st.note = "已是最新版本（" + vtag(r.latest) + "）"; st.noteTone = "muted";
      st.upShow = false;
      setNote(item, st.note, st.noteTone);
      if (upBtn) { upBtn.disabled = true; upBtn.classList.add("hidden"); }
    }
  } catch (e) {
    st.note = "检查失败：" + (e && e.message ? e.message : e); st.noteTone = "err";
    setNote(item, st.note, st.noteTone);
  } finally {
    btn.disabled = false;
  }
}

/** 单插件更新（确认后交给 Go 端执行，splash 进度，完成/失败弹窗）。 */
async function doPluginUpdate(p, item, upBtn) {
  const ver = (state.plugState[p.name] || {}).upLatest || "";
  const ok = await confirmDialog(
    "更新插件？",
    "将把插件 " + p.name + " 更新到" + (ver ? " " + vtag(ver) : "最新版本") +
      "。更新期间服务会短暂重启，失败会自动回退到更新前版本。确认开始更新吗？",
    "开始更新"
  );
  if (!ok) return;
  item.querySelectorAll("button").forEach((b) => { b.disabled = true; });
  setNote(item, "正在更新插件…", "muted");
  bindings().StartPluginUpdate(p.name); // 完成后 Go 端发 plugins:changed 刷新列表
}

/** 单插件删除（确认后交给 Go 端执行，splash 进度，失败自动回退）。 */
async function doPluginRemove(p, item, delBtn) {
  const ok = await confirmDialog(
    "删除插件？",
    "将物理删除插件 " + p.name +
      (p.profile ? "（环境 " + p.profile + "）" : "") +
      " 及其依赖，不可恢复。删除期间服务会短暂重启，失败会自动回退到删除前状态。确定删除吗？",
    "删除"
  );
  if (!ok) return;
  item.querySelectorAll("button").forEach((b) => { b.disabled = true; });
  setNote(item, "正在删除插件…", "muted");
  bindings().RemovePlugin(p.id); // 完成后 Go 端发 plugins:changed 刷新列表
}

// ==================== 日志页 ====================

// 日志行时间戳/级别识别：时间戳统一显示为行头（muted），兼容斜杠（Go log / 子进程前缀
// "2026/09/04 14:00:13"）与横杠+T 两种写法；级别词着色便于扫读。
const LOG_TS_RE = /^\s*(\d{4}[-\/]\d{2}[-\/]\d{2}[ T]\d{2}:\d{2}:\d{2})\s+(.*)$/;
const LOG_LVL_RE = /^\[?(INFO|WARN|ERROR|DEBUG)\]?\s+(.*)$/;

function renderLog(lines) {
  const view = $("log-view");
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
  if (!a || state.page !== "logs" || !state.logName) return;
  try {
    const tail = await a.ReadLogTail(state.logName, state.logOffset);
    if (tail.lines && tail.lines.length) renderLog(tail.lines);
    state.logOffset = tail.nextOffset;
  } catch (e) { console.error("ReadLogTail", e); }
}

/** 日志页固定查看统一日志文件（下拉切换已移除：所有日志合并为 dsh-systray.log）。 */
function setLogFile(name) {
  state.logName = name || "";
  $("log-view").textContent = "";
  state.logOffset = 0;
  (async () => {
    const a = bindings();
    if (a && state.logName) $("log-path").textContent = await a.GetLogPath(state.logName);
    else $("log-path").textContent = "";
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
        // 2) 有冲突 → 弹窗询问：取消=不执行任何恢复；跳过=保留现有只补缺失；覆盖=备份并替换
        let overwrite = true;
        if (preview.conflicts > 0) {
          const detail = (preview.tops || []).slice(0, 3).join("、");
          const choice = await confirmDialog3(
            "检测到数据冲突",
            "「" + it.label + "」与现有内容存在 " + preview.conflicts + " 项冲突" +
              (detail ? "（" + detail + (preview.conflicts > 3 ? " 等" : "") + "）" : "") +
              "。\n\n「跳过」将保留现有文件、只补缺失项；「覆盖并恢复」会备份并替换现有内容。",
            "覆盖并恢复",
            "跳过（保留现有）"
          );
          if (choice === "cancel") return; // 取消：不做任何恢复动作
          overwrite = choice === "ok";
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
    // 启动完成：切换设置视图（窗口随后由 Go 侧隐藏）；截图模式在此后重放滚动位置
    applyShotScroll();
    showSettings();
    refreshService();
  });

  EventsOn("ui:show-splash", () => showSplash("startup", "正在准备运行环境…"));
  // 托盘每次重开设置窗口：刷新版本与插件清单（更新/重置等操作可能在窗口隐藏期间完成，必须强一致重取）
  EventsOn("ui:show-settings", () => { showSettings(); refreshConfig(); refreshService(); refreshVersions(); loadPlugins(); });

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
  EventsOn("update:done", () => {
    showSettings();
    // 恢复更新按钮可用并刷新模块版本（systray 更新会重启整个应用，此处主要覆盖 harness）
    const hub = $("btn-harness-update");
    if (hub) hub.disabled = false;
    const sysBtn = $("btn-systray-update");
    if (sysBtn) sysBtn.disabled = false;
    refreshVersions();
  });

  // 插件更新完成：刷新插件列表（版本/来源状态可能变化）
  EventsOn("plugins:changed", () => loadPlugins());
}

// 取消更新
function wireSplashCancel() {
  $("splash-cancel").addEventListener("click", () => {
    bindings().CancelUpdate();
    $("splash-cancel").classList.add("hidden");
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
    $("modal-title").textContent = title || "确认操作";
    $("modal-msg").textContent = msg || "";
    $("modal-ok").textContent = okLabel || "确定";
    const skipBtn = $("modal-skip");
    skipBtn.classList.toggle("hidden", !skipLabel);
    if (skipLabel) skipBtn.textContent = skipLabel;
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

// check-frontend-files-card.mjs：文件同步卡的静态检查与纯函数回归（本仓库前端手写 JS + 手写 DOM，
// 与 check-frontend-i18n.mjs 同一思路：没有构建与单测框架，用脚本兜底）。
//
// 覆盖：
//   1) main.js 文件同步段引用的元素 id 是否都存在于 index.html；
//   2) 关键元素（文件卡/路径导航/树/空态/工具条/待应用块）是否存在；
//   3) 纯函数断言（Node + 最小 DOM 桩加载 main.js）：
//      容量换算 1000 以内与进位、目录树聚合、文件夹优先与三种排序、行渲染转义、
//      **导航式列表**（双击进入/路径导航/本机完整路径行/取消移动不误报）。
// 用法：node scripts/check-frontend-files-card.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const root = join(dirname(fileURLToPath(import.meta.url)), '..', 'src', 'frontend', 'dist');
const js = readFileSync(join(root, 'main.js'), 'utf8');
const html = readFileSync(join(root, 'index.html'), 'utf8');
const css = readFileSync(join(root, 'style.css'), 'utf8');

let failures = 0;
const fail = (msg) => { failures += 1; console.error('错误：' + msg); };
const pass = (msg) => console.log('PASS ' + msg);

// ---------- 1) & 2) 元素 id ----------
const SECTION_START = '// ==================== 数据同步：文件/文件夹';
const start = js.indexOf(SECTION_START);
const end = js.indexOf('async function init()', start);
if (start < 0 || end < 0) {
  fail('main.js 中找不到文件同步段（段标记被改动？）');
} else {
  const section = js.slice(start, end);
  const ids = new Set();
  for (const m of section.matchAll(/\$\("([^"]+)"\)/g)) ids.add(m[1]);
  for (const m of section.matchAll(/getElementById\("([^"]+)"\)/g)) ids.add(m[1]);
  const missing = [...ids].filter((id) => !html.includes(`id="${id}"`));
  if (missing.length) fail('main.js 引用但 index.html 缺失的 id：' + missing.join(', '));
  else pass(`文件同步段引用的 ${ids.size} 个元素 id 全部存在`);
}

for (const id of ['sync-files', 'files-capacity-fill', 'files-pending', 'files-toolbar', 'files-crumbs', 'files-empty', 'files-tree', 'files-hint-text', 'files-hint-action']) {
  if (!html.includes(`id="${id}"`)) fail(`index.html 缺少关键元素 #${id}`);
}

// ---------- 3) 纯函数断言 ----------
function makeElement() {
  let text = '';
  return {
    get textContent() { return text; },
    set textContent(v) { text = String(v); },
    get innerHTML() {
      return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
    },
    set innerHTML(v) { text = String(v); },
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    style: {},
    dataset: {},
    value: '',
    addEventListener() {},
    removeEventListener() {},
    focus() {},
    select() {},
    appendChild() {},
    closest() { return null; },
    querySelectorAll() { return []; },
  };
}

const elements = new Map();
const sandbox = {
  document: {
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, makeElement());
      return elements.get(id);
    },
    createElement() { return makeElement(); },
    querySelectorAll() { return []; },
    addEventListener() {},
    documentElement: {},
    title: '',
  },
  window: {
    addEventListener() {},
    removeEventListener() {},
    runtime: { EventsOn() {}, EventsOff() {}, EventsEmit() {}, EventsOnce() {}, BrowserOpenURL() {}, ClipboardSetText() {}, WindowSetTitle() {} },
  },
  console,
  setTimeout,
  clearTimeout,
  setInterval,
  clearInterval,
  GoApp: null,
  requestAnimationFrame() {},
};
sandbox.globalThis = sandbox;

// main.js 顶层的 let/const 不挂 vm 全局对象，追加探针把内部绑定暴露出来（见文件头说明）。
const probe = `
;globalThis.__files = {
  filesFmtSize, filesBuildTree, filesSortTree, filesEntryHtml, filesSortEntries, filesRowHtml,
  filesFindLevel, filesLevelRowsHtml, filesCrumbHtml, filesJoinPath, filesMetaHtml,
  filesRowActivate, filesEnter, filesNavRoot,
  get filesSort() { return filesSort; },
  set filesSort(v) { filesSort = v; },
  get filesNav() { return filesNav; },
  set filesNav(v) { filesNav = v; },
};
`;
vm.createContext(sandbox);
vm.runInContext(js + probe, sandbox, { filename: 'main.js' });

const T = sandbox.__files;
if (!T) {
  fail('探针未生效：main.js 结构变化导致纯函数无法访问');
  process.exit(1);
}

function check(name, actual, expected) {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a === e) pass(`${name}: ${a}`);
  else fail(`${name}: ${a} != ${e}`);
}
function checkTrue(name, cond) {
  if (cond) pass(name);
  else fail(name);
}

// 容量换算：1024 进制（Windows 资源管理器口径），换算后数值保持 1024 以内
check('0 字节', T.filesFmtSize(0), '0 B');
check('1023 字节', T.filesFmtSize(1023), '1023 B');
check('1024 字节', T.filesFmtSize(1024), '1 KB');
check('1536 字节', T.filesFmtSize(1536), '1.5 KB');
check('100 MB 级（≥100 显示整数）', T.filesFmtSize(100 * 1024 * 1024 + 500000), '100 MB');
check('1023.95 KB（进位到 MB）', T.filesFmtSize(1048524), '1 MB');
// 免费档配额就是 10 MiB：必须显示成 10.0 MB（按 1000 进制会显示 10.5 MB，看起来像配额写错）
check('10 MiB 配额', T.filesFmtSize(10485760), '10 MB');
check('已用 93144 字节', T.filesFmtSize(93144), '91 KB');
check('非法输入', T.filesFmtSize(undefined), '0 B');

// 目录树聚合与排序
const files = [
  { relPath: 'b.txt', name: 'b.txt', size: 10, mtime: 5, status: 'synced', error: '' },
  { relPath: 'a.txt', name: 'a.txt', size: 7, mtime: 9, status: 'pending-upload', error: '' },
  { relPath: 'sub/c.txt', name: 'c.txt', size: 5, mtime: 3, status: 'synced', error: '' },
  { relPath: 'sub/deep/d.txt', name: 'd.txt', size: 2, mtime: 8, status: 'error', error: '读取失败' },
];
const tree = T.filesBuildTree(files);
check('根聚合大小', tree.size, 24);
check('根聚合文件数', tree.count, 4);
check('根聚合时间取最新', tree.mtime, 9);
check('子目录聚合大小（含深层）', tree.dirs[0].size, 7);
check('状态取最差（子树有 error → error）', tree.status, 'error');

T.filesSort = { key: 'name', dir: 1 };
T.filesSortTree(tree);
check('名称升序：目录在前、文件在后', [tree.dirs[0].name, ...tree.files.map((f) => f.name)], ['sub', 'a.txt', 'b.txt']);
T.filesSort = { key: 'size', dir: -1 };
T.filesSortTree(tree);
check('大小降序', tree.files.map((f) => f.name), ['b.txt', 'a.txt']);
T.filesSort = { key: 'mtime', dir: 1 };
T.filesSortTree(tree);
check('修改时间升序', tree.files.map((f) => f.name), ['b.txt', 'a.txt']);
T.filesSort = { key: 'name', dir: -1 };
T.filesSortTree(tree);
check('名称降序（目录仍在前）', [tree.dirs[0].name, ...tree.files.map((f) => f.name)], ['sub', 'b.txt', 'a.txt']);
T.filesSort = { key: 'name', dir: 1 };

// 行渲染：转义、操作按钮、本机完整路径（导航式列表：条目行不再内联子行）
const entry = {
  id: 'e1', name: '<img src=x onerror=alert(1)>', kind: 'dir', isSource: true,
  path: 'C:/work/notes', size: 24, status: 'pending',
  files: files.map((f) => ({ ...f })),
};
const flat = T.filesEntryHtml(entry);
checkTrue('名称被转义（无原始标签）', !flat.includes('<img src=x'));
checkTrue('转义后保留可读文本', flat.includes('&lt;img'));
checkTrue('条目行含打开/移除，且不再提供重命名', flat.includes('data-fact="open"') && flat.includes('data-fact="remove"') && !flat.includes('data-fact="rename"'));
// 「更改本机位置」：把条目同步到本机其它文件/文件夹（旧位置文件保持不动）
checkTrue('条目行提供移动按钮', flat.includes('data-fact="relocate"') && flat.includes('>移动<'));
checkTrue('移动走 FilesSetLocalPath 绑定', /case "relocate": filesDoRelocate\(/.test(js) && /g\.FilesSetLocalPath\(entryId\)/.test(js));
checkTrue('移动后提示本机文件已搬走', js.includes('已移动同步位置；本机文件已搬到新位置'));
// 取消移动（系统对话框点取消）时 Go 返回 canceled=true：不得提示「已移动」
checkTrue('取消移动不误报「已移动」（前端判 canceled）', /if \(res && res\.canceled\) return;/.test(js));
checkTrue('Go 侧把取消标成 canceled', /Canceled: true/.test(readFileSync(join(root, '..', '..', 'account_files_bindings.go'), 'utf8')));
checkTrue('不再由 App 自建覆盖询问', !js.includes('更改位置') && !js.includes('正在把账号内容同步到新位置'));
checkTrue('条目行不再内联渲染子文件（双击进入才显示）', !flat.includes('c.txt') && !flat.includes('a.txt'));
// 每一行都带本机完整路径（小字行右侧、超长末尾省略，悬停由 title 气泡显示完整路径）
checkTrue('条目行显示本机完整路径', flat.includes('class="files-path"') && flat.includes('C:/work/notes'));
checkTrue('路径带 title（悬停气泡显示完整路径）', /class="files-path" title="C:\/work\/notes">C:\/work\/notes</.test(flat));
checkTrue('路径与小字同行（紧凑两行一行）', /<div class="files-meta"><span class="files-meta-text">[^<]*<\/span><span class="files-path"/.test(flat));
// 原「展开/折叠三角形」已按用户要求移除，改为双击进入
checkTrue('不再有折叠三角形（caret）', !js.includes('files-caret') && !flat.includes('data-fact="toggle"'));
checkTrue('行带双击所需属性', /data-row="dir"/.test(flat) && !flat.includes('data-istoggle'));
checkTrue('双击行进入文件夹/打开文件已接线', /files-tree"\)\.addEventListener\("dblclick"/.test(js) && /function filesRowActivate/.test(js));

// 根层条目行与目录内行**完全同款**（同标记、同字号字重、同小字口径）：层级由上方路径导航表达，
// 大量文件时不靠加粗/加间距区分，保持紧凑（2026-10-07 用户要求）。
const levelRows0 = T.filesLevelRowsHtml(entry, T.filesFindLevel(entry, ''), []);
checkTrue('两处都不再用加粗的条目行样式', !flat.includes('files-row-entry') && !levelRows0.includes('files-row-entry') && !css.includes('.files-row-entry'));
checkTrue('两处行标记一致（同 data-row 开头）', /^<div class="files-row" data-row="dir"/.test(flat) && /<div class="files-row" data-row="dir"/.test(levelRows0));
checkTrue('两处共用同一个 filesMetaHtml', /filesMetaHtml\(o\)/.test(js) && /filesMetaHtml\(\{/.test(js) && !js.includes('filesRowMeta('));
checkTrue('小字行左信息 + 右路径（路径占剩余宽度）', /\.files-meta \{[^}]*display: flex;/s.test(css) && /\.files-meta-text \{ flex: 0 0 auto;/.test(css) && /\.files-path \{[^}]*flex: 1 1 auto;/s.test(css));
checkTrue('行间紧凑（无条目间距、内边距收紧）', !css.includes('.files-entry + .files-entry') && /\.files-row \{[^}]*padding: 4px var\(--sp-2\);/s.test(css));
const metaHtml = T.filesMetaHtml({ size: 1234, isDir: true, count: 3, mtime: 1700000000, localPath: 'C:/x/a.txt' });
checkTrue('小字内容 = 大小 · 文件数 · 修改时间', /<span class="files-meta-text">1\.2 KB · 3 个文件 · \d{2}-\d{2} \d{2}:\d{2}<\/span>/.test(metaHtml));
check('无路径时不渲染路径节点', T.filesMetaHtml({ size: 0, isDir: false, mtime: 0 }), '<div class="files-meta"><span class="files-meta-text">0 B</span></div>');
checkTrue('条目行小字不再出现来源标注', !flat.includes('本机原位置') && !flat.includes('接收目录') && !js.includes('"本机原位置"'));

// 路径导航（服务器侧命名空间）：根目录 → 条目名 → 条目内相对路径；每一级可点跳级
const crumbsRoot = T.filesCrumbHtml(null, []);
checkTrue('根层显示「根目录」且不可点', crumbsRoot.includes('根目录') && crumbsRoot.includes('is-current') && !crumbsRoot.includes('data-crumb-entry'));
const crumbsDeep = T.filesCrumbHtml({ id: 'e1', name: 'notes' }, ['sub', 'deep']);
checkTrue('逐级显示条目名与相对路径', crumbsDeep.includes('>notes<') && crumbsDeep.includes('>sub<') && crumbsDeep.includes('>deep<'));
checkTrue('每一级都可点（含根目录）', (crumbsDeep.match(/data-crumb-entry=/g) || []).length === 3);
checkTrue('末级为当前位置（不可点）', /<span class="files-crumb is-current"[^>]*>deep<\/span>/.test(crumbsDeep));
checkTrue('面包屑点击接线（含回根目录）', /files-crumbs"\)\.addEventListener\("click"/.test(js) && /filesNavRoot\(\)/.test(js));

// 导航层级：只渲染当前层
const lvl = T.filesFindLevel(entry, '');
checkTrue('条目根层包含顶层文件与子目录', lvl.files.map((f) => f.name).sort().join(',') === 'a.txt,b.txt' && lvl.dirs[0].name === 'sub');
const deepLvl = T.filesFindLevel(entry, 'sub');
checkTrue('进入子目录后只拿该层节点', deepLvl.files.map((f) => f.name).join(',') === 'c.txt');
checkTrue('不存在的目录回退为 null（调用方退到条目根）', T.filesFindLevel(entry, 'nope/gone') === null);
T.filesNav = { entryId: '', rel: '' };
T.filesEnter('e1', 'sub');
checkTrue('filesEnter 记录当前位置', T.filesNav.entryId === 'e1' && T.filesNav.rel === 'sub');
T.filesNavRoot();
checkTrue('filesNavRoot 回到根层', T.filesNav.entryId === '' && T.filesNav.rel === '');
// 本机路径拼接：按 root 的分隔符风格（Windows 反斜杠 / 其它正斜杠）
check('Windows 路径拼接', T.filesJoinPath('H:\\DOC\\notes', 'sub/c.txt'), 'H:\\DOC\\notes\\sub\\c.txt');
check('POSIX 路径拼接', T.filesJoinPath('/home/demo/notes', 'sub/c.txt'), '/home/demo/notes/sub/c.txt');
check('路径已带尾部分隔符', T.filesJoinPath('H:\\DOC\\notes\\', 'a.txt'), 'H:\\DOC\\notes\\a.txt');
check('空相对路径返回根', T.filesJoinPath('H:\\DOC\\notes', ''), 'H:\\DOC\\notes');

// 行内规则：待同步/同步中的文件只能移除（不可打开）；同步完成的才可打开；同步中显示速度
const syncFile = { relPath: 'a.txt', name: 'a.txt', size: 1024, mtime: 1, status: 'synced', error: '', blocked: false };
const pendFile = { relPath: 'b.txt', name: 'b.txt', size: 1024, mtime: 1, status: 'pending-upload', error: '', blocked: false };
const busyFile = { relPath: 'c.txt', name: 'c.txt', size: 4096, mtime: 1, status: 'uploading', error: '', blocked: false, speedBps: 1048576 };
const e2 = { id: 'e2', name: 'dir2', kind: 'dir', isSource: true, path: 'C:/x', size: 6144, status: 'pending', files: [syncFile, pendFile, busyFile] };
const rows = T.filesLevelRowsHtml(e2, T.filesFindLevel(e2, ''), []);
const rowOf = (rel) => rows.split('<div class="files-row"').find((r) => r.includes(`data-rel="${rel}"`)) || '';
checkTrue('已同步文件行可打开', rowOf('a.txt').includes('data-fact="open"'));
checkTrue('待同步文件行不可打开（仅移除）', !rowOf('b.txt').includes('data-fact="open"') && rowOf('b.txt').includes('data-fact="remove"'));
checkTrue('同步中文件行不可打开', !rowOf('c.txt').includes('data-fact="open"'));
checkTrue('同步中文件行显示状态与速度', rowOf('c.txt').includes('同步中') && rowOf('c.txt').includes('1 MB/s'));
checkTrue('层级行也显示本机完整路径', rowOf('a.txt').includes('C:/x/a.txt'));
checkTrue('层级行不带缩进（导航式列表，一层一屏）', !rows.includes('--depth'));

// 顶层列表排序（点「名称/大小/修改时间」必须真的重排——只排当前层是不够的）
const sortList = [
  { id: 'a', name: 'zeta.txt', kind: 'file', size: 300, mtime: 30 },
  { id: 'b', name: 'alpha', kind: 'dir', size: 100, mtime: 10 },
  { id: 'c', name: 'beta.txt', kind: 'file', size: 200, mtime: 20 },
];
const names = (list) => list.map((e) => e.name).join(',');
T.filesSort = { key: 'name', dir: 1 };
checkTrue('按名称升序（文件夹在前）', names(T.filesSortEntries(sortList)) === 'alpha,beta.txt,zeta.txt');
T.filesSort = { key: 'name', dir: -1 };
checkTrue('按名称降序（文件夹仍在前）', names(T.filesSortEntries(sortList)) === 'alpha,zeta.txt,beta.txt');
T.filesSort = { key: 'size', dir: 1 };
checkTrue('按大小升序', names(T.filesSortEntries(sortList)) === 'alpha,beta.txt,zeta.txt');
T.filesSort = { key: 'size', dir: -1 };
checkTrue('按大小降序', names(T.filesSortEntries(sortList)) === 'alpha,zeta.txt,beta.txt');
T.filesSort = { key: 'mtime', dir: -1 };
checkTrue('按修改时间降序', names(T.filesSortEntries(sortList)) === 'alpha,zeta.txt,beta.txt');
T.filesSort = { key: 'mtime', dir: 1 };
checkTrue('不改动入参数组（返回副本）', sortList[0].name === 'zeta.txt' && names(T.filesSortEntries(sortList)) === 'alpha,beta.txt,zeta.txt');
T.filesSort = { key: 'name', dir: 1 };

// 「已在本机移除」：可重新同步（不显示打开）
const goneFile = { relPath: 'gone.txt', name: 'gone.txt', size: 2048, mtime: 1, status: 'removed-local', error: '', blocked: false };
const e3 = { id: 'e3', name: 'dir3', kind: 'dir', isSource: true, path: 'C:/y', size: 2048, status: 'removed-local', files: [goneFile] };
const goneRows = T.filesLevelRowsHtml(e3, T.filesFindLevel(e3, ''), []);
const goneRow = goneRows.split('<div class="files-row"').find((r) => r.includes('data-rel="gone.txt"')) || '';
checkTrue('已在本机移除的文件行显示徽标', goneRow.includes('已在本机移除'));
checkTrue('已在本机移除的文件行可重新同步', goneRow.includes('data-fact="restore"'));
checkTrue('已在本机移除的文件行不可打开', !goneRow.includes('data-fact="open"'));
const goneEntry = T.filesEntryHtml(e3);
checkTrue('条目行也提供整条目重新同步', goneEntry.includes('data-fact="restore"') && goneEntry.includes('data-rel=""'));

// 路径行：省略号截断 + 悬停 title 气泡（不再做悬停文字滚动——2026-10-07 用户反馈效果不好）
checkTrue('路径不换行 + 末尾省略', /\.files-path \{[^}]*white-space: nowrap;[^}]*text-overflow: ellipsis;/s.test(css));
checkTrue('没有悬停文字滚动的残留', !js.includes('filesMarquee') && !js.includes('is-marquee') && !css.includes('files-path-scroll'));
checkTrue('路径渲染带 title 气泡', T.filesMetaHtml({ size: 0, localPath: 'H:\\DOC\\a.txt' }).includes('title="H:\\DOC\\a.txt"'));
checkTrue('路径渲染不含滚动用的内层结构', !T.filesMetaHtml({ size: 0, localPath: 'H:\\DOC\\a.txt' }).includes('files-path-text'));

// 容量不足：弹窗**整个运行期只出现一次**（用户要求 2026-10-07：一次就够，别反复弹），
// 常驻信息改由提示行承载；Go 侧一旦容量不足就中止本轮上传（见 account_files.go）。
checkTrue('容量不足弹窗只弹一次（运行期标志）', /let filesQuotaPrompted = false;/.test(js) && /if \(blocked > 0 && !filesQuotaPrompted\)/.test(js) && /filesQuotaPrompted = true;/.test(js));
checkTrue('不再按「数量变化」重复弹窗', !js.includes('filesQuotaWarned') && !/blocked !== filesQuota/.test(js));
checkTrue('信息类弹层只有一个「知道了」（无取消）', /function infoDialog\(/.test(js) && /hideCancel: true/.test(js) && /classList\.toggle\("hidden", !!m\.hideCancel\)/.test(js));
checkTrue('容量不足时有常驻提示行（说明原因与出路）', /else if \(blocked > 0\) \{/.test(js) && js.includes('有 {0} 个文件因容量不足未同步；清理空间后点「立即同步」重试'));
checkTrue('blocked 在提示行之前声明（避免 TDZ）', js.indexOf('const blocked = Number(st.blockedCount)') < js.indexOf('else if (blocked > 0)'));
const goSrc = readFileSync(join(root, '..', '..', 'account_files.go'), 'utf8');
checkTrue('Go：账号已满即中止本轮（不发任何上传）', /if full \{\s*return nil, blocked/.test(goSrc));
checkTrue('Go：单个文件超总容量只拦它自己（不拖住其余文件）', /if m\.Size > q\.Limit \{[\s\S]{0,200}?blocked\+\+\s*continue/.test(goSrc));
checkTrue('Go：服务端判容量不足后中止剩余上传', /atomic\.StoreInt32\(&quotaStop, 1\)/.test(goSrc) && /atomic\.LoadInt32\(quotaStop\) == 1/.test(goSrc));;

// 目录状态聚合：只要有后代在传，目录（含更上一级）就显示「同步中」
const busyTree = T.filesBuildTree([
  { relPath: 'sub/deep/busy.txt', name: 'busy.txt', size: 10, mtime: 1, status: 'uploading' },
  { relPath: 'sub/idle.txt', name: 'idle.txt', size: 10, mtime: 1, status: 'pending-upload' },
  { relPath: 'done.txt', name: 'done.txt', size: 10, mtime: 1, status: 'synced' },
]);
checkTrue('子目录含在传文件 → 同步中', busyTree.dirMap.get('sub').dirMap.get('deep').status === 'uploading');
checkTrue('上级目录同样显示同步中', busyTree.dirMap.get('sub').status === 'uploading');
checkTrue('条目根聚合为同步中', busyTree.status === 'uploading');

const errTree = T.filesBuildTree([
  { relPath: 'sub/a.txt', name: 'a.txt', size: 1, mtime: 1, status: 'uploading' },
  { relPath: 'sub/b.txt', name: 'b.txt', size: 1, mtime: 1, status: 'error' },
]);
checkTrue('错误优先级高于同步中', errTree.dirMap.get('sub').status === 'error');

const pendTree = T.filesBuildTree([
  { relPath: 'sub/a.txt', name: 'a.txt', size: 1, mtime: 1, status: 'pending-upload' },
  { relPath: 'sub/b.txt', name: 'b.txt', size: 1, mtime: 1, status: 'synced' },
]);
checkTrue('待同步优先于已同步', pendTree.dirMap.get('sub').status === 'pending-upload');

const goneTree = T.filesBuildTree([
  { relPath: 'sub/a.txt', name: 'a.txt', size: 1, mtime: 1, status: 'removed-local' },
  { relPath: 'sub/b.txt', name: 'b.txt', size: 1, mtime: 1, status: 'synced' },
]);
checkTrue('已在本机移除优先于已同步', goneTree.dirMap.get('sub').status === 'removed-local');

console.log(failures === 0 ? '通过：文件卡静态检查与纯函数回归全部通过' : `${failures} 项失败`);
process.exit(failures === 0 ? 0 : 1);

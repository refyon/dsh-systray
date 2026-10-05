// check-frontend-files-card.mjs：文件同步卡的静态检查与纯函数回归（本仓库前端手写 JS + 手写 DOM，
// 与 check-frontend-i18n.mjs 同一思路：没有构建与单测框架，用脚本兜底）。
//
// 覆盖：
//   1) main.js 文件同步段引用的元素 id 是否都存在于 index.html；
//   2) 关键元素（文件卡/树/空态/工具条/待应用块/重命名弹层）是否存在；
//   3) 纯函数断言（Node + 最小 DOM 桩加载 main.js）：
//      容量换算 1000 以内与进位、目录树聚合、文件夹优先与三种排序、行渲染转义与层级展开。
//
// 用法：node scripts/check-frontend-files-card.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const root = join(dirname(fileURLToPath(import.meta.url)), '..', 'src', 'frontend', 'dist');
const js = readFileSync(join(root, 'main.js'), 'utf8');
const html = readFileSync(join(root, 'index.html'), 'utf8');

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

for (const id of ['sync-files', 'files-capacity-fill', 'files-pending', 'files-toolbar', 'files-empty', 'files-tree', 'files-rename-modal']) {
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
  filesFmtSize, filesBuildTree, filesSortTree, filesEntryHtml,
  get filesSort() { return filesSort; },
  set filesSort(v) { filesSort = v; },
  filesExpanded,
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

// 容量换算：换算后数值保持 1000 以内（含四舍五入进位到下一单位）
check('0 字节', T.filesFmtSize(0), '0 B');
check('999 字节', T.filesFmtSize(999), '999 B');
check('1000 字节', T.filesFmtSize(1000), '1 KB');
check('1234 字节', T.filesFmtSize(1234), '1.2 KB');
check('999499 字节（≥100 显示整数）', T.filesFmtSize(999499), '999 KB');
check('999950 字节（进位到 MB）', T.filesFmtSize(999950), '1 MB');
check('10 MiB', T.filesFmtSize(10 * 1024 * 1024), '10.5 MB');
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

// 行渲染：转义、操作按钮、层级展开
const entry = {
  id: 'e1', name: '<img src=x onerror=alert(1)>', kind: 'dir', isSource: true,
  path: 'C:/work/notes', size: 24, status: 'pending',
  files: files.map((f) => ({ ...f })),
};
const flat = T.filesEntryHtml(entry);
checkTrue('名称被转义（无原始标签）', !flat.includes('<img src=x'));
checkTrue('转义后保留可读文本', flat.includes('&lt;img'));
checkTrue('条目行含打开/重命名/移除', flat.includes('data-fact="open"') && flat.includes('data-fact="rename"') && flat.includes('data-fact="remove"'));
checkTrue('来源标注为本机原位置', flat.includes('本机原位置'));
checkTrue('未展开时不渲染子行', !flat.includes('c.txt'));

T.filesExpanded['e1'] = true;
const expanded = T.filesEntryHtml(entry);
checkTrue('展开条目后渲染顶层文件行', expanded.includes('a.txt') && expanded.includes('b.txt'));
checkTrue('子目录未展开时不渲染其内容', !expanded.includes('c.txt'));
checkTrue('子目录行有缩进层级', expanded.includes('--depth:1'));
T.filesExpanded['e1/sub'] = true;
const deeper = T.filesEntryHtml(entry);
checkTrue('展开子目录后渲染其中文件', deeper.includes('c.txt'));
checkTrue('深层目录缩进更大', deeper.includes('--depth:2'));
delete T.filesExpanded['e1'];
delete T.filesExpanded['e1/sub'];

console.log(failures === 0 ? '通过：文件卡静态检查与纯函数回归全部通过' : `${failures} 项失败`);
process.exit(failures === 0 ? 0 : 1);

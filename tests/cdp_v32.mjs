// v3.2 冒烟：真实后端 + 真实共享目录，验证三个视图、预览、待办、变动通知
// 用法: node tests/cdp_v32.mjs <baseUrl> <debugPort> <outDir> <shareDir>
import { writeFileSync, unlinkSync, statSync, existsSync } from 'node:fs';

const BASE = process.argv[2] || 'http://127.0.0.1:47961/';
const PORT = process.argv[3] || '9510';
const OUT = process.argv[4] || '.';
const SHARE = process.argv[5] || 'C:\\Temp\\lc32t\\share';

/* 每次从干净状态开始：上一轮留下的待办、公告、外来文件都清掉 */
for (const f of ['_lanchat\\todo.json', '_lanchat\\pin.md', '外人放进来的.txt', 'big.bin', 'big.bin.part']) {
  try { unlinkSync(SHARE + '\\' + f); } catch (e) { /* 不存也无所谓 */ }
}
writeFileSync(SHARE + '\\big.bin.part', Buffer.alloc(2 * 1024 * 1024, 0xAA));

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let fails = 0;
function check(name, ok, extra) {
  console.log((ok ? 'PASS ' : 'FAIL ') + name + (extra ? '  [' + extra + ']' : ''));
  if (!ok) fails++;
}

async function newTarget(url) {
  const r = await fetch(`http://127.0.0.1:${PORT}/json/new?${encodeURIComponent(url)}`, { method: 'PUT' });
  return r.json();
}
function connect(wsUrl) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(wsUrl);
    const pending = new Map();
    let id = 0;
    const errors = [];
    ws.onopen = () => resolve({
      errors,
      send(method, params) {
        const mid = ++id;
        ws.send(JSON.stringify({ id: mid, method, params }));
        return new Promise((res, rej) => pending.set(mid, { res, rej }));
      },
      close() { ws.close(); }
    });
    ws.onerror = () => reject(new Error('ws error'));
    ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.method === 'Runtime.exceptionThrown') {
        errors.push(JSON.stringify(msg.params.exceptionDetails).slice(0, 300));
      }
      if (msg.id && pending.has(msg.id)) {
        const p = pending.get(msg.id);
        pending.delete(msg.id);
        msg.error ? p.rej(new Error(JSON.stringify(msg.error))) : p.res(msg.result);
      }
    };
  });
}
async function evalJS(cdp, expression) {
  const r = await cdp.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 500));
  return r.result && r.result.value;
}
async function shot(cdp, name) {
  const r = await cdp.send('Page.captureScreenshot', { format: 'png' });
  writeFileSync(`${OUT}/${name}.png`, Buffer.from(r.data, 'base64'));
  console.log('SHOT ' + name);
}

const t = await newTarget(BASE);
const cdp = await connect(t.webSocketDebuggerUrl);
await cdp.send('Runtime.enable', {});
await cdp.send('Page.enable', {});
await cdp.send('Emulation.setDeviceMetricsOverride', { width: 1080, height: 760, deviceScaleFactor: 1, mobile: false });
await sleep(4000);

const boot = await evalJS(cdp, `JSON.stringify({
  native: !!window.__NATIVE, view: (window.__LANCHAT_API ? 1 : 0),
  rows: document.querySelectorAll('.msg').length,
  tabs: [].map.call(document.querySelectorAll('.vtab[data-v]'), b => b.textContent.trim()),
  addr: (document.getElementById('fAddr')||{}).textContent || ''
})`);
console.log('BOOT ' + boot);
const b = JSON.parse(boot);
check('页面加载出三个视图 tab', b.tabs.length === 3, b.tabs.join('|'));
check('启动时是原生模式', b.native === true);

await evalJS(cdp, `document.querySelector('.vtab[data-v="files"]').click(); 1`);
await sleep(900);
const fl = await evalJS(cdp, `JSON.stringify({
  rows: [].map.call(document.querySelectorAll('#filelist .frow'), r => r.querySelector('.fname').textContent),
  sum: (document.getElementById('fSum')||{}).textContent,
  addr: (document.getElementById('fAddr')||{}).textContent
})`);
console.log('FILES ' + fl);
const f = JSON.parse(fl);
check('文件页列出了共享文件夹里的文件', f.rows.indexOf('a.txt') >= 0 && f.rows.indexOf('docs') >= 0, f.rows.join(','));
check('文件页不显示 _lanchat', f.rows.indexOf('_lanchat') < 0);
check('文件页显示的是共享根路径', f.addr.indexOf('lc32t') >= 0, f.addr);
await shot(cdp, 'v32_files');

/* 文件页搜索：既验证「按当前关键词过滤」，也验证不会晚一个按键才刷新
   （以前渲染挂在 input 上、关键词在另一个监听里更新，第一下永远是旧词） */
await evalJS(cdp, `(function(){
  var si = document.getElementById('searchInp');
  si.value = 'a.t';
  si.dispatchEvent(new Event('input', { bubbles: true }));
  return 1;
})()`);
await sleep(320);
const fs1 = await evalJS(cdp, `JSON.stringify({
  rows: [].map.call(document.querySelectorAll('#filelist .frow'), r => r.querySelector('.fname').textContent),
  ph: document.getElementById('searchInp').placeholder
})`);
const f1 = JSON.parse(fs1);
check('文件页搜索框提示改成「搜索文件名」', f1.ph === '搜索文件名', f1.ph);
check('文件页搜索按当前关键词过滤', f1.rows.length === 1 && f1.rows[0] === 'a.txt', f1.rows.join(','));

await evalJS(cdp, `(function(){
  var si = document.getElementById('searchInp');
  si.value = '';
  si.dispatchEvent(new Event('input', { bubbles: true }));
  return 1;
})()`);
await sleep(320);
const fs2 = await evalJS(cdp, `JSON.stringify([].map.call(document.querySelectorAll('#filelist .frow'), r => r.querySelector('.fname').textContent))`);
const f2 = JSON.parse(fs2);
check('清空搜索后文件全部回来', f2.length >= 4 && f2.indexOf('b.md') >= 0, f2.join(','));

// 预览 a.txt（走预览按钮）
await evalJS(cdp, `(function(){
  var rows = [].slice.call(document.querySelectorAll('#filelist .frow'));
  var r = rows.filter(x => x.querySelector('.fname').textContent === 'a.txt')[0];
  var b = [].slice.call(r.querySelectorAll('.fop')).filter(x => x.textContent === '预览')[0];
  b.click(); return 1;
})()`);
await sleep(900);
const pv = await evalJS(cdp, `JSON.stringify({
  on: document.getElementById('pv').classList.contains('on'),
  name: (document.getElementById('pvName')||{}).textContent,
  body: ((document.getElementById('pvBody').querySelector('pre')||{}).textContent||'').slice(0,60)
})`);
console.log('PREVIEW ' + pv);
const p = JSON.parse(pv);
check('预览浮层打开了', p.on === true, p.name);
check('预览内容是文件正文', p.body.indexOf('hello') >= 0, p.body);
await shot(cdp, 'v32_preview');
await evalJS(cdp, `document.getElementById('pvClose').click(); 1`);
await sleep(300);

// 待办
await evalJS(cdp, `document.querySelector('.vtab[data-v="todo"]').click(); 1`);
await sleep(600);
await evalJS(cdp, `(function(){
  var i = document.getElementById('tdInput');
  i.value = '把 v3.2 的功能过一遍';
  document.getElementById('tdAddBtn').click();
  return 1;
})()`);
await sleep(900);
const td = await evalJS(cdp, `JSON.stringify({
  rows: document.querySelectorAll('#tdList .tdrow').length,
  text: ((document.querySelector('#tdList .tdtext')||{}).textContent||''),
  badge: (document.getElementById('todoCount')||{}).textContent
})`);
console.log('TODO ' + td);
const d = JSON.parse(td);
check('待办加进去了', d.rows === 1 && d.text.indexOf('过一遍') >= 0, d.text);
check('tab 上显示未完成条数', d.badge === '1', d.badge);

await evalJS(cdp, `document.querySelector('#tdList .tdchk').click(); 1`);
await sleep(800);
const td2 = await evalJS(cdp, `JSON.stringify({
  done: document.querySelector('#tdList .tdrow').classList.contains('done'),
  meta: ((document.querySelector('#tdList .tdmeta')||{}).textContent||''),
  badge: (document.getElementById('todoCount')||{}).textContent
})`);
console.log('TODO2 ' + td2);
const d2 = JSON.parse(td2);
check('勾选后变成已完成', d2.done === true, d2.meta);
check('已完成时 tab 徽标变空', d2.badge === '', d2.badge);
await shot(cdp, 'v32_todo');

// 置顶公告：走界面（点「置顶」→ 填 → 确定），确认按钮链路是通的
await evalJS(cdp, `(function(){ document.querySelector('.vtab[data-v="chat"]').click(); return 1; })()`);
await sleep(400);
await evalJS(cdp, `document.getElementById('pinEdit').click(); 1`);
await sleep(500);
await evalJS(cdp, `(function(){
  document.getElementById('modalInput').value = '本周五前把共享盘里的资料整理到 2026-10 目录。';
  document.getElementById('modalOk').click();
  return 1;
})()`);
await sleep(1000);
const pin = await evalJS(cdp, `JSON.stringify({
  on: !document.getElementById('pinbar').classList.contains('hidden'),
  text: (document.getElementById('pinText')||{}).textContent
})`);
console.log('PIN ' + pin);
const pinv = JSON.parse(pin);
check('置顶公告显示在聊天区顶部', pinv.on === true && pinv.text.indexOf('周五') >= 0, pinv.text);
await shot(cdp, 'v32_pin');

// 变动通知：从外面往共享里丢一个文件，等一轮扫描
writeFileSync(SHARE + '\\外人放进来的.txt', '这是从资源管理器拖进来的\n');
await sleep(4500);
const ev = await evalJS(cdp, `JSON.stringify({
  rows: [].map.call(document.querySelectorAll('.sysevt span'), s => s.textContent)
})`);
console.log('WATCH ' + ev);
const evv = JSON.parse(ev);
check('别人放进来的文件会在聊天里提示', evv.rows.some(s => s.indexOf('外人放进来的') >= 0), evv.rows.join('|'));

// 断点续传：共享里已经有一个 2MB 的 big.bin.part，再发一个 5MB 的 big.bin
// 正确的结果是最终 5MB（接着传）；如果从头写又会 append，就会变成 7MB
console.log('== 断点续传 ==');
await evalJS(cdp, `(async function(){
  var bytes = new Uint8Array(5 * 1024 * 1024);
  for (var i = 0; i < bytes.length; i += 4096) bytes[i] = 0xAA;
  await window.__LANCHAT_API.sendPlainFile(new File([bytes], 'big.bin'));
  return 1;
})()`);
await sleep(4000);
const bigSize = statSync(SHARE + '\\big.bin').size;
check('续传后文件是完整的 5 MB（不是 7 MB）', bigSize === 5 * 1024 * 1024, bigSize + ' 字节');

// 打包发送：40 个文件应该先问一句，确认后在页面里压成 zip 再落到共享
console.log('== 打包发送 ==');
const ask = await evalJS(cdp, `(async function(){
  var items = [];
  for (var i = 0; i < 40; i++) {
    items.push({ rel: 'a/b' + (i % 3) + '/文件' + i + '.txt', file: new File(['内容 ' + i + '\\n'], '文件' + i + '.txt') });
  }
  await window.__LANCHAT_API.sendFolder('打包测试', items);
  var bar = document.getElementById('askbar');
  return bar.classList.contains('hidden') ? 'no-ask' : document.getElementById('askYes').textContent;
})()`);
check('文件多时先问「要不要打包」', ask === '打包发送', ask);
await evalJS(cdp, `document.getElementById('askYes').click(); 1`);
await sleep(7000);
check('zip 落到了共享文件夹', existsSync(SHARE + '\\打包测试.zip'), '');
check('没有把 40 个散文件铺进共享', !existsSync(SHARE + '\\打包测试'), '');

// 归档：根目录堆了 20 个以上散文件时给建议，点了才真的搬
console.log('== 归档 ==');
for (let i = 0; i < 21; i++) writeFileSync(SHARE + '\\散文件' + i + '.txt', 'x');
const ar = await (await fetch(BASE + 'api/archive?root=' + encodeURIComponent(SHARE))).json();
check('归档建议给出分组', ar.total >= 21 && ar.groups.length >= 1, JSON.stringify(ar.groups));
const month = ar.groups[ar.groups.length - 1].month;
const ar2 = await (await fetch(BASE + 'api/archive', {
  method: 'POST', headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ root: SHARE, month })
})).json();
check('归档真的把文件搬进月份目录', ar2.moved > 0 && existsSync(SHARE + '\\' + month), JSON.stringify(ar2));

check('运行期间没有 JS 异常', cdp.errors.length === 0, cdp.errors.slice(0, 2).join(' ;; '));
console.log(fails === 0 ? 'ALL_PASS' : 'FAILED ' + fails);
cdp.close();
process.exit(fails === 0 ? 0 : 1);

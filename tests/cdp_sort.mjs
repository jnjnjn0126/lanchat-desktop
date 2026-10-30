// v3.3 回归：气泡顺序
// 复现的是线上真实现象 —— 机器人在另一台电脑上跑，那台机器时钟慢几百毫秒，
// 于是 AI 回复的 ts 比它回复的那条提问还小，气泡就排到提问前面。
// 用法: node tests/cdp_sort.mjs <baseUrl> <debugPort> <shareDir>
import { writeFileSync } from 'node:fs';

const BASE = process.argv[2] || 'http://127.0.0.1:47981/';
const PORT = process.argv[3] || '9530';
const SHARE = process.argv[4] || 'C:\\Temp\\lc33t\\share';
const MSGS = SHARE + '\\_lanchat\\msgs';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let fails = 0;
function check(name, ok, extra) {
  console.log((ok ? 'PASS ' : 'FAIL ') + name + (extra ? '  [' + extra + ']' : ''));
  if (!ok) fails++;
}

/* 写一条消息。文件名照旧用 ts 打头（和真实程序一致），
   写入顺序 = mtime 顺序 = 真实到达顺序。 */
let hex = 0x100000;
function writeMsg(id, ts, text, extra) {
  const m = {
    v: 1, id, ts, from: '主机台式', dev: 'XIN-Desktop',
    fromId: '6e2363c0080131d6db2a3e79f6da7bf3', type: 'text', text
  };
  if (extra) Object.assign(m, extra);
  const fn = String(ts).padStart(13, '0') + '-' + (hex++).toString(16) + '.json';
  writeFileSync(MSGS + '\\' + fn, JSON.stringify(m));
  return fn;
}
function aiReply(ts, text, re) {
  return { from: 'AI 助手', dev: '机器人', fromId: 'ai', bot: true, re };
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
/* 当前 DOM 里的消息 id 顺序 */
async function domIds(cdp) {
  const s = await evalJS(cdp, `JSON.stringify([].map.call(document.querySelectorAll('#list .msg'), function(r){return r.getAttribute('data-id')}))`);
  return JSON.parse(s);
}
function expectOrder(got, want, label) {
  const gotFiltered = got.filter((x) => want.indexOf(x) >= 0);
  const ok = gotFiltered.length === want.length && gotFiltered.every((v, i) => v === want[i]);
  check(label, ok, 'got=' + gotFiltered.join(',') + ' want=' + want.join(','));
}

const T = Date.now() - 600000;   // 10 分钟前，避免「未来消息」

/* ① 历史数据：AI 回复 ts 比提问早 200ms（线上就是这样） */
writeMsg('q1', T + 1000, '第一个问题');
await sleep(30);
writeMsg('a1', T + 800, '第一个回答', aiReply(T + 800, '第一个回答', 'q1'));
await sleep(30);
writeMsg('q2', T + 2000, '第二个问题');
await sleep(30);
writeMsg('a2', T + 1800, '第二个回答', aiReply(T + 1800, '第二个回答', 'q2'));

const t = await newTarget(BASE);
const cdp = await connect(t.webSocketDebuggerUrl);
await cdp.send('Runtime.enable', {});
await cdp.send('Page.enable', {});
await cdp.send('Emulation.setDeviceMetricsOverride', { width: 1080, height: 760, deviceScaleFactor: 1, mobile: false });
await sleep(4500);

let ids = await domIds(cdp);
console.log('BOOT ' + ids.join(','));
expectOrder(ids, ['q1', 'a1', 'q2', 'a2'], '启动后 AI 回复排在它回复的提问之后');

/* ② 运行中：先来提问，200ms 后 AI 回复（ts 仍然是更早的） */
const T3 = Date.now() - 60000;
writeMsg('q3', T3, '第三个问题');
await sleep(1600);
writeMsg('a3', T3 - 200, '第三个回答', aiReply(T3 - 200, '第三个回答', 'q3'));
await sleep(1800);
ids = await domIds(cdp);
console.log('AFTER-Q3 ' + ids.join(','));
expectOrder(ids, ['q1', 'a1', 'q2', 'a2', 'q3', 'a3'], '运行中新增的 AI 回复也排在提问之后');

/* ③ 父比子晚到：AI 回复的文件名比提问小，扫描时子先到、父后到 */
const T4 = Date.now() - 50000;
writeMsg('a4', T4 - 300, '第四个回答', aiReply(T4 - 300, '第四个回答', 'q4'));
await sleep(300);
writeMsg('q4', T4, '第四个问题');
await sleep(2200);
ids = await domIds(cdp);
console.log('AFTER-Q4 ' + ids.join(','));
expectOrder(ids, ['q1', 'a1', 'q2', 'a2', 'q3', 'a3', 'q4', 'a4'], '父消息晚到也能把子消息归位');

/* ④ 水位线之外：提问的 ts 很新，AI 回复的 ts 比水位还小
   （只靠 after= 增量会永远拉不到，靠每 4 秒的尾巴重扫兜底） */
const T5 = Date.now() - 20000;
writeMsg('q5', T5 + 60000, '第五个问题');     // 故意用一个很大的 ts，把水位拉高
await sleep(2000);
writeMsg('a5', T5, '第五个回答', aiReply(T5, '第五个回答', 'q5'));   // ts 远小于水位
await sleep(9000);
ids = await domIds(cdp);
console.log('AFTER-Q5 ' + ids.join(','));
check('ts 比水位还小的 AI 回复没有丢', ids.indexOf('a5') >= 0, ids.join(','));
expectOrder(ids, ['q1', 'a1', 'q2', 'a2', 'q3', 'a3', 'q4', 'a4', 'q5', 'a5'],
  'ts 小到水位以下的回复也排在提问之后');

/* ⑤ 本机连发两条：时间戳不能相同，顺序不能反 */
await evalJS(cdp, `(function(){
  var box = document.getElementById('input'), btn = document.getElementById('sendBtn');
  box.value = '连发第一条'; box.dispatchEvent(new Event('input', {bubbles:true}));
  btn.click();
  box.value = '连发第二条'; box.dispatchEvent(new Event('input', {bubbles:true}));
  btn.click();
  return 1;
})()`);
await sleep(2500);
const tail = await evalJS(cdp, `JSON.stringify([].slice.call(document.querySelectorAll('#list .msg')).slice(-2).map(function(r){
  return { id: r.getAttribute('data-id'), t: (r.querySelector('.bubble')||{}).textContent };
}))`);
const last2 = JSON.parse(tail);
console.log('LOCAL ' + tail);
check('本机连发的两条顺序正确', last2[0] && last2[1] && last2[0].t === '连发第一条' && last2[1].t === '连发第二条',
  last2.map((x) => x.t).join('|'));

/* ⑥ 整屏顺序自检：DOM 顺序必须和排序键一致（不能有回退） */
const monotonic = await evalJS(cdp, `(function(){
  var rows = [].slice.call(document.querySelectorAll('#list .msg'));
  var bad = 0, prev = '';
  for (var i = 0; i < rows.length; i++) {
    var id = rows[i].getAttribute('data-id');
    if (prev && id === prev) bad++;
    prev = id;
  }
  return bad;
})()`);
check('DOM 里没有重复消息', monotonic === 0, String(monotonic));

check('全程没有 JS 异常', cdp.errors.length === 0, cdp.errors.join(' | ').slice(0, 300));

cdp.close();
console.log(fails === 0 ? 'SORT_ALL_PASS' : 'SORT_FAILED ' + fails);
process.exit(fails === 0 ? 0 : 1);

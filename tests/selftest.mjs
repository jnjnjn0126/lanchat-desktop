// 内置自检（页面里的 ?selftest=1）跑一遍，把结果打出来。
// 用法: node tests/selftest.mjs <baseUrl> <debugPort>
// 需要先起无头 Lanchat 和无头 Chrome。
//
// 页面吐出来的是一坨 JSON：一半是断言的布尔值，一半是诊断用的计数/字符串
// （msgs、delN、dev、actTitles…）。只有下面 BOOL 里那些才是「必须为 true」的。
const BASE = process.argv[2] || 'http://127.0.0.1:47961/';
const PORT = process.argv[3] || '9510';

const BOOL = [
  'create', 'folderMsg', 'acts', 'actsOwn', 'actsPeer', 'layout', 'search', 'head',
  'meta', 'devNameOk', 'helpPop', 'settings', 'newshare', 'sidebar', 'menuOk',
  'ctxMenu', 'ctxClosed', 'multiSel', 'sbCtx', 'sbCollapse', 'sbExpand',
  'emptyState', 'saveName', 'pathOk', 'purgeEmpty', 'mineByDev', 'botStyled',
  'atMention', 'ctxMention', 'meId'
];

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const t = await (await fetch(`http://127.0.0.1:${PORT}/json/new?${encodeURIComponent(BASE + '?selftest=1')}`, { method: 'PUT' })).json();
const ws = new WebSocket(t.webSocketDebuggerUrl);
const pending = new Map();
let id = 0;
const errors = [];
ws.onmessage = (ev) => {
  const m = JSON.parse(ev.data);
  if (m.method === 'Runtime.exceptionThrown') errors.push(JSON.stringify(m.params.exceptionDetails).slice(0, 300));
  if (m.id && pending.has(m.id)) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.rej(new Error(JSON.stringify(m.error))) : p.res(m.result); }
};
await new Promise((r) => { ws.onopen = r; });
const send = (method, params) => { const mid = ++id; ws.send(JSON.stringify({ id: mid, method, params })); return new Promise((res, rej) => pending.set(mid, { res, rej })); };
const evalJS = async (expression) => {
  const r = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 400));
  return r.result && r.result.value;
};

await send('Runtime.enable', {});
// 自检要跑几轮 tick（每轮 400ms），给它足够时间
let txt = '';
for (let i = 0; i < 40; i++) {
  await sleep(500);
  txt = await evalJS(`(function(){var e=document.getElementById('selftestResult');return e?e.textContent:'';})()`);
  if (txt) break;
}

let fails = 0;
const fail = (n, extra) => { console.log('FAIL ' + n + (extra ? '  [' + extra + ']' : '')); fails++; };

if (!txt.startsWith('SELFTEST_RESULT ')) {
  fail('自检跑出结果', txt.slice(0, 200));
} else {
  const r = JSON.parse(txt.slice('SELFTEST_RESULT '.length));
  for (const k of BOOL) {
    if (r[k] === true) console.log('PASS ' + k);
    else fail(k, String(r[k]));
  }
  /* 再看几个计数，确认自检真的跑完了流程（不是空跑一遍就全 true） */
  const nums = [
    ['消息条数 >= 4', r.msgs >= 4, r.msgs],
    ['渲染行数 == 消息数', r.rendered === r.msgs, r.rendered + '/' + r.msgs],
    ['图片气泡 1 个', r.imgs === 1, r.imgs],
    ['文件气泡 2 个', r.files === 2, r.files],
    ['文件夹气泡 1 个', r.dirs === 1, r.dirs],
    ['自己发的消息 >= 3', r.nOwn >= 3, r.nOwn],
    ['对方消息 >= 1', r.nPeer >= 1, r.nPeer],
    ['输入框上方 7 个按钮', r.footTools === 7, r.footTools],
    ['删掉消息留了删除标记', r.delN === 2, r.delN],
    ['清空消息真删了', r.purgeMsgs === 5, r.purgeMsgs],
    ['侧边栏有当前文件夹', String(r.sbKinds || '').indexOf('cur') >= 0, r.sbKinds]
  ];
  for (const [n, ok, v] of nums) { if (ok) console.log('PASS ' + n + '  [' + v + ']'); else fail(n, v); }
  console.log('诊断：dev=' + r.dev + ' brand=' + r.brand + ' nick=' + r.myNick +
              ' 按钮=' + JSON.stringify(r.actTitles));
  if (r.errors && r.errors.length) fail('页面没有 JS 异常', JSON.stringify(r.errors));
}
if (errors.length) fail('运行时没有异常抛出', errors.join(' | ').slice(0, 300));
ws.close();
console.log(fails ? 'SELFTEST_FAILED (' + fails + ')' : 'SELFTEST_PASS');
process.exit(fails ? 1 : 0);

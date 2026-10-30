// v3.1 自检 + 界面验证
// 用法: node cdp_v31.mjs <baseUrl> <debugPort> <outDir>
import { writeFileSync } from 'node:fs';

const BASE = process.argv[2] || 'http://127.0.0.1:47951/';
const PORT = process.argv[3] || '9500';
const OUT = process.argv[4] || '.';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function newTarget(url) {
  const r = await fetch(`http://127.0.0.1:${PORT}/json/new?${encodeURIComponent(url)}`, { method: 'PUT' });
  if (!r.ok) throw new Error('new target failed: ' + r.status);
  return r.json();
}

function connect(wsUrl) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(wsUrl);
    const pending = new Map();
    let id = 0;
    ws.onopen = () => resolve({
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
  if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 400));
  return r.result && r.result.value;
}

async function shot(cdp, name) {
  const r = await cdp.send('Page.captureScreenshot', { format: 'png' });
  writeFileSync(`${OUT}/${name}.png`, Buffer.from(r.data, 'base64'));
  console.log('SHOT ' + name);
}

async function main() {
  /* 1) 自检 */
  const t1 = await newTarget(BASE + '?selftest=1');
  const c1 = await connect(t1.webSocketDebuggerUrl);
  await c1.send('Runtime.enable', {});
  await sleep(7000);
  const out = await evalJS(c1, `(function(){
    var d = document.getElementById('selftestResult');
    return d ? d.textContent : 'NO_RESULT';
  })()`);
  console.log('SELFTEST ' + out);
  c1.close();

  /* 2) 界面：造一条 AI 消息 + 打开 @ 面板，截图 */
  const t2 = await newTarget(BASE + '?demo=1');
  const c2 = await connect(t2.webSocketDebuggerUrl);
  await c2.send('Runtime.enable', {});
  await c2.send('Page.enable', {});
  await c2.send('Emulation.setDeviceMetricsOverride', { width: 1080, height: 760, deviceScaleFactor: 1, mobile: false });
  await sleep(2500);

  const info = await evalJS(c2, `(async function(){
    var sleep = ms => new Promise(r => setTimeout(r, ms));
    var api = window.__LANCHAT_API;
    var out = {};
    out.me = api.me();

    // 一条 AI 回复气泡
    api.setPeers([
      { id: 'peer-wang', name: '老王', host: 'PC-A', ts: Date.now() },
      { id: 'ai', name: 'AI 助手', host: '机器人', ts: Date.now() }
    ]);
    var bot = { v:1, id:'demo-bot-1', ts: Date.now(), from:'AI 助手', dev:'机器人',
                fromId:'ai', type:'text', text:'这两周的结论有三条：接口协议已定稿、联调通过、并发问题还挂着。',
                bot:true };
    api.ingest(bot);
    await sleep(300);

    // 打开的 @ 候选面板
    var box = document.getElementById('input');
    box.value = '@';
    box.focus();
    box.setSelectionRange(1, 1);
    box.dispatchEvent(new Event('input', { bubbles: true }));
    await sleep(400);
    out.atOpen = document.getElementById('atpop').classList.contains('on');
    out.atNames = [].map.call(document.querySelectorAll('#atpop .atitem .atn'), e => e.textContent);
    return JSON.stringify(out);
  })()`);
  console.log('UI ' + info);
  await shot(c2, 'v31_at_mention');

  const picked = await evalJS(c2, `(function(){
    var box = document.getElementById('input');
    var first = document.querySelector('#atpop .atitem');
    if (first) first.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
    var v = box.value;
    box.value = '';
    return v;
  })()`);
  console.log('AT_PICKED ' + JSON.stringify(picked));

  /* 3) 右键菜单里的「＠ 提到」 */
  const ctx = await evalJS(c2, `(async function(){
    var sleep = ms => new Promise(r => setTimeout(r, ms));
    var row = document.querySelector('.msg[data-id="demo-bot-1"]');
    if (!row) return 'NO_BOT_ROW';
    var r = row.getBoundingClientRect();
    row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true,
      clientX: Math.round(r.left + 60), clientY: Math.round(r.top + 10) }));
    await sleep(300);
    return JSON.stringify({
      labels: [].map.call(document.querySelectorAll('#ctxmenu .ctxitem'), b => b.textContent)
    });
  })()`);
  console.log('CTX ' + ctx);
  await shot(c2, 'v31_ctx_mention');
  c2.close();
}

main().catch((e) => { console.error('FAIL', e.message); process.exit(1); });

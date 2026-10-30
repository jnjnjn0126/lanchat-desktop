// 验证「继续上次」按钮：切回主页 → 点按钮 → 重新进入同一文件夹；顺带看窄窗口下的说明面板
import { writeFileSync } from 'node:fs';
const BASE = process.argv[2] || 'http://127.0.0.1:47901/';
const PORT = process.argv[3] || '9333';
const OUT = process.argv[4] || '.';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function newTarget(url) {
  const r = await fetch(`http://127.0.0.1:${PORT}/json/new?${encodeURIComponent(url)}`, { method: 'PUT' });
  return r.json();
}
function connect(wsUrl) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(wsUrl); const pending = new Map(); let id = 0;
    ws.onopen = () => resolve({
      send(m, p) { const mid = ++id; ws.send(JSON.stringify({ id: mid, method: m, params: p })); return new Promise((res, rej) => pending.set(mid, { res, rej })); },
      close() { ws.close(); }
    });
    ws.onerror = () => reject(new Error('ws'));
    ws.onmessage = (ev) => { const msg = JSON.parse(ev.data); if (msg.id && pending.has(msg.id)) { const p = pending.get(msg.id); pending.delete(msg.id); msg.error ? p.rej(new Error(JSON.stringify(msg.error))) : p.res(msg.result); } };
  });
}
async function evalJS(cdp, e) {
  const r = await cdp.send('Runtime.evaluate', { expression: e, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails));
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
await sleep(3000);

const first = await evalJS(cdp, `JSON.stringify({
  chat: !document.getElementById('chat').classList.contains('hidden'),
  dir: document.getElementById('dirName').textContent
})`);
console.log('AUTO_ENTER ' + first);

// 切回主页 → 点「继续上次」
await evalJS(cdp, `document.getElementById('switchBtn').click(); 1`);
await sleep(500);
const home = await evalJS(cdp, `JSON.stringify({
  resume: document.getElementById('recentBtn').textContent,
  visible: !document.getElementById('recentBtn').classList.contains('hidden'),
  chatHidden: document.getElementById('chat').classList.contains('hidden')
})`);
console.log('HOME ' + home);

await evalJS(cdp, `document.getElementById('recentBtn').click(); 1`);
await sleep(1600);
const back = await evalJS(cdp, `JSON.stringify({
  chat: !document.getElementById('chat').classList.contains('hidden'),
  dir: document.getElementById('dirName').textContent,
  msgs: document.querySelectorAll('.msg').length,
  err: document.getElementById('errbar').textContent
})`);
console.log('RESUME ' + back);

// 窄窗口（700px）：说明面板应改到卡片下方，且不出横向滚动条
await evalJS(cdp, `document.getElementById('switchBtn').click(); 1`);
await sleep(400);
await cdp.send('Emulation.setDeviceMetricsOverride', { width: 700, height: 700, deviceScaleFactor: 1, mobile: false });
await sleep(400);
const hp = JSON.parse(await evalJS(cdp, `(function(){ var r=document.getElementById('helpBtn').getBoundingClientRect(); return JSON.stringify({x:Math.round(r.left+r.width/2),y:Math.round(r.top+r.height/2)}); })()`));
await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: hp.x, y: hp.y });
await sleep(600);
const narrow = await evalJS(cdp, `(function(){
  var p = document.querySelector('.helppop').getBoundingClientRect();
  var c = document.querySelector('.card').getBoundingClientRect();
  var se = document.getElementById('setup');
  return JSON.stringify({
    pop: {l: Math.round(p.left), t: Math.round(p.top), r: Math.round(p.right), b: Math.round(p.bottom)},
    card: {l: Math.round(c.left), r: Math.round(c.right), b: Math.round(c.bottom)},
    below: p.top >= c.bottom - 1,
    hScroll: se.scrollWidth > se.clientWidth
  });
})()`);
console.log('NARROW ' + narrow);
await shot(cdp, 'home_narrow_help');
cdp.close();

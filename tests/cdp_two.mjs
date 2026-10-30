// 双窗口同步测试（隔离存储，模拟两台不同电脑）
// 用 CDP 的 browser context 隔离 localStorage，让两边拿到不同的 peerId。
// 用法: node cdp_two.mjs <pageUrl> <debugPort>
const PAGE = process.argv[2] || 'http://127.0.0.1:47830/';
const PORT = process.argv[3] || '9222';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

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
      async eval(expression) {
        const r = await this.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
        if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 300));
        return r.result && r.result.value;
      },
      close() { try { ws.close(); } catch (e) { } }
    });
    ws.onerror = () => reject(new Error('ws error: ' + wsUrl));
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

async function targetWs(targetId) {
  const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
  const t = list.find((x) => x.id === targetId);
  if (!t) throw new Error('target not found: ' + targetId);
  return t.webSocketDebuggerUrl;
}

async function main() {
  const ver = await (await fetch(`http://127.0.0.1:${PORT}/json/version`)).json();
  const browser = await connect(ver.webSocketDebuggerUrl);

  // 两个隔离 context = 两台"电脑"
  const c1 = (await browser.send('Target.createBrowserContext', {})).browserContextId;
  const c2 = (await browser.send('Target.createBrowserContext', {})).browserContextId;
  const t1 = (await browser.send('Target.createTarget', { url: PAGE, browserContextId: c1 })).targetId;
  const t2 = (await browser.send('Target.createTarget', { url: PAGE, browserContextId: c2 })).targetId;

  const A = await connect(await targetWs(t1));
  await A.send('Runtime.enable', {});
  const B = await connect(await targetWs(t2));
  await B.send('Runtime.enable', {});

  // 等两边真正导航到目标页（新 target 一开始是 about:blank，localStorage 会报 SecurityError）
  async function waitLoaded(c) {
    for (let i = 0; i < 40; i++) {
      try {
        const href = await c.eval('location.href');
        const ready = await c.eval('document.readyState');
        if (href && href.indexOf('127.0.0.1') >= 0 && ready === 'complete') return true;
      } catch (e) { /* 还没加载好 */ }
      await sleep(300);
    }
    return false;
  }
  const okA = await waitLoaded(A), okB = await waitLoaded(B);
  if (!okA || !okB) throw new Error('页面未加载完成 A=' + okA + ' B=' + okB);
  await sleep(3000); // 等两边 boot + 心跳

  const peerA = await A.eval('localStorage.getItem("lanchat.peerId")');
  const peerB = await B.eval('localStorage.getItem("lanchat.peerId")');

  await B.eval(`(() => {
    document.getElementById('input').value = '来自 B 电脑的消息';
    document.getElementById('sendBtn').click();
    return true;
  })()`);

  await sleep(2200); // A 每 0.4s 扫一次，心跳每 2.5s

  const seenA = await A.eval(`(() => {
    const rows = [...document.querySelectorAll('.msg')];
    const other = rows.filter(r => !r.classList.contains('mine'));
    const last = other[other.length - 1];
    return JSON.stringify({
      total: rows.length,
      fromOther: other.length,
      lastText: last ? ((last.querySelector('.bubble') || {}).textContent || '') : '',
      online: document.getElementById('onlineText').textContent,
      errbar: document.getElementById('errbar').textContent
    });
  })()`);

  const seenB = await B.eval(`(() => {
    const rows = [...document.querySelectorAll('.msg')];
    return JSON.stringify({
      total: rows.length,
      mine: rows.filter(r => r.classList.contains('mine')).length,
      errbar: document.getElementById('errbar').textContent
    });
  })()`);

  console.log('TWO ' + JSON.stringify({
    peerA, peerB, samePeer: peerA === peerB,
    seenA: JSON.parse(seenA), seenB: JSON.parse(seenB)
  }));

  await browser.send('Target.disposeBrowserContext', { browserContextId: c1 });
  await browser.send('Target.disposeBrowserContext', { browserContextId: c2 });
  browser.close(); A.close(); B.close();
}

main().catch((e) => { console.error('FAIL', e.message); process.exit(1); });

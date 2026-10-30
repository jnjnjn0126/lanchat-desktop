// 通过 CDP 驱动无头浏览器，验证原生模式下的真实收发链路（v3.0）。
// 用法: node cdp_test.mjs <pageUrl> <debugPort> <rootDir>
//   rootDir = 当前共享文件夹的本机路径，用来核对磁盘上真的多出了文件
import { readdirSync, statSync, existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

const PAGE = process.argv[2] || 'http://127.0.0.1:47830/';
const PORT = process.argv[3] || '9222';
const ROOT = process.argv[4] || '';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function newTarget() {
  const r = await fetch(`http://127.0.0.1:${PORT}/json/new?${encodeURIComponent(PAGE)}`, { method: 'PUT' });
  if (!r.ok) throw new Error('new target failed: ' + r.status + ' ' + (await r.text()));
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

function listDir(p) {
  try { return readdirSync(p); } catch (e) { return ['<缺失>']; }
}

async function main() {
  const target = await newTarget();
  const cdp = await connect(target.webSocketDebuggerUrl);
  await cdp.send('Runtime.enable', {});
  await sleep(2600); // 等页面完成 boot（自动进入上次文件夹）

  const expr = `(async () => {
    const sleep = ms => new Promise(r => setTimeout(r, ms));
    const out = {};
    out.native = !!window.__NATIVE;
    out.root = window.__NATIVE_INFO && window.__NATIVE_INFO.root;
    out.chatVisible = !document.getElementById('chat').classList.contains('hidden');
    out.dirName = (document.querySelector('#sbList .sbitem.on .sbt b') || {}).textContent || '';
    out.verChip = document.getElementById('verChat').textContent;

    // 1) 文字
    const before = document.querySelectorAll('.msg').length;
    document.getElementById('input').value = '原生模式 e2e 测试消息';
    document.getElementById('sendBtn').click();
    await sleep(800);
    out.msgsBefore = before;
    out.msgsAfter = document.querySelectorAll('.msg').length;

    // 2) 图片（还是走 blobs + 预览）
    const cv = document.createElement('canvas'); cv.width = 60; cv.height = 40;
    const cx = cv.getContext('2d'); cx.fillStyle = '#2f6fd0'; cx.fillRect(0, 0, 60, 40);
    const blob = await new Promise(r => cv.toBlob(r, 'image/png'));
    const dt = new DataTransfer();
    dt.items.add(new File([blob], 'e2e.png', { type: 'image/png' }));
    document.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true }));
    await sleep(1400);
    out.imgThumbs = document.querySelectorAll('img.thumb').length;

    // 3) 普通文件：应该直接落在共享文件夹根目录
    await window.__LANCHAT_API.sendPlainFile(new File([new Blob(['v3 file body'])], 'e2e文件.txt', { type: 'text/plain' }));
    await sleep(900);

    // 4) 文件夹：整棵树复制进共享文件夹
    await window.__LANCHAT_API.sendFolder('e2e文件夹', [
      { rel: 'a.txt', file: new File([new Blob(['aaa'])], 'a.txt') },
      { rel: '子目录/b.txt', file: new File([new Blob(['bbbb'])], 'b.txt') }
    ]);
    await sleep(1200);

    // 5) 三个气泡按钮 + 地址显示
    const acts = document.querySelector('.msg .acts');
    out.actTitles = acts ? [].map.call(acts.querySelectorAll('button'), b => b.title) : [];
    const cards = [].slice.call(document.querySelectorAll('.filecard'));
    out.cards = cards.map(c => ({
      dir: c.classList.contains('isdir'),
      name: (c.querySelector('.fname') || {}).textContent || '',
      sub: (c.querySelector('.fsub') || {}).textContent || '',
      path: (c.querySelector('.fpath') || {}).textContent || '',
      btn: (c.querySelector('.fbtn') || {}).textContent || ''
    }));

    // 6) 复制文件本体：点文件气泡的「复制」，看后端有没有把它放进剪贴板
    const fileRow = [].slice.call(document.querySelectorAll('.msg')).filter(r => {
      return !!r.querySelector('.filecard:not(.isdir)');
    })[0];
    let copied = null;
    if (fileRow) {
      fileRow.querySelector('.acts button').click();
      await sleep(700);
      copied = (document.getElementById('toast') || {}).textContent || '';
    }
    out.copyToast = copied;
    out.errbar = document.getElementById('errbar').textContent;
    return JSON.stringify(out);
  })()`;

  const res = await cdp.send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
  if (!res.result || typeof res.result.value !== 'string') {
    console.log('RAW ' + JSON.stringify(res));
    process.exit(1);
  }
  const out = JSON.parse(res.result.value);
  console.log('E2E ' + JSON.stringify(out, null, 1));

  if (ROOT) {
    const top = listDir(ROOT);
    console.log('DISK_ROOT ' + JSON.stringify(top.filter((n) => /e2e/.test(n))));
    const folder = join(ROOT, 'e2e文件夹');
    console.log('DISK_FOLDER ' + JSON.stringify({
      exists: existsSync(folder),
      top: existsSync(folder) ? listDir(folder) : [],
      sub: existsSync(join(folder, '子目录')) ? listDir(join(folder, '子目录')) : [],
      aBody: existsSync(join(folder, 'a.txt')) ? readFileSync(join(folder, 'a.txt'), 'utf8') : null,
      bBody: existsSync(join(folder, '子目录', 'b.txt')) ? readFileSync(join(folder, '子目录', 'b.txt'), 'utf8') : null,
      kind: existsSync(folder) ? (statSync(folder).isDirectory() ? 'dir' : 'file') : 'missing'
    }));
    const f = join(ROOT, 'e2e文件.txt');
    console.log('DISK_FILE ' + JSON.stringify({
      exists: existsSync(f),
      body: existsSync(f) ? readFileSync(f, 'utf8') : null,
      partLeft: existsSync(f + '.part')
    }));
    console.log('DISK_BLOBS ' + JSON.stringify(listDir(join(ROOT, '_lanchat', 'blobs'))));
  }
  cdp.close();
}

main().catch((e) => { console.error('FAIL', e.message); process.exit(1); });

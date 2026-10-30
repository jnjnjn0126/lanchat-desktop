// 用 CDP 打开页面，跑自检 + 截图（聊天页 / 主页 / 问号悬浮）
// 用法: node cdp_ui.mjs <baseUrl> <debugPort> <outDir>
import { writeFileSync } from 'node:fs';

const BASE = process.argv[2] || 'http://127.0.0.1:47901/';
const PORT = process.argv[3] || '9333';
const OUT = process.argv[4] || '.';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function newTarget(url) {
  const r = await fetch(`http://127.0.0.1:${PORT}/json/new?${encodeURIComponent(url)}`, { method: 'PUT' });
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

async function evalJS(cdp, expression) {
  const r = await cdp.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails));
  return r.result && r.result.value;
}

async function shot(cdp, name) {
  const r = await cdp.send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false });
  writeFileSync(`${OUT}/${name}.png`, Buffer.from(r.data, 'base64'));
  console.log('SHOT ' + name);
}

async function main() {
  /* ---------- 1) 自检 ---------- */
  const t1 = await newTarget(BASE + '?selftest=1');
  const c1 = await connect(t1.webSocketDebuggerUrl);
  await c1.send('Runtime.enable', {});
  await sleep(6000);
  const selfOut = await evalJS(c1, `(function(){
    var d = document.getElementById('selftestResult');
    return d ? d.textContent : 'NO_RESULT';
  })()`);
  console.log('SELFTEST ' + selfOut);
  c1.close();

  /* ---------- 2) 演示模式：聊天页截图 ---------- */
  const t2 = await newTarget(BASE + '?demo=1');
  const c2 = await connect(t2.webSocketDebuggerUrl);
  await c2.send('Runtime.enable', {});
  await c2.send('Page.enable', {});
  await c2.send('Emulation.setDeviceMetricsOverride', { width: 1080, height: 760, deviceScaleFactor: 1, mobile: false });
  await sleep(2200);

  // 自己发两条，凑出左右两侧；再发一个文件和一个文件夹，看 v3.0 的新卡片
  await evalJS(c2, `(async function(){
    var box = document.getElementById('input');
    box.value = '这是本机发出的消息，应该贴在右边';
    document.getElementById('sendBtn').click();
    await new Promise(r => setTimeout(r, 500));
    box.value = "第二条本机消息，悬停能看到 复制 / 文件夹内打开 / 另存为 三个按钮";
    document.getElementById('sendBtn').click();
    await new Promise(r => setTimeout(r, 900));
    window.__LANCHAT_API.setWhere({ root: 'D:\\\\局域网聊天共享', unc: '\\\\\\\\Xin-desktop\\\\局域网聊天共享', kind: 'map', host: 'Xin-desktop' });
    await window.__LANCHAT_API.sendPlainFile(new File([new Blob(['季度报表内容'])], '季度报表.xlsx'));
    await new Promise(r => setTimeout(r, 700));
    await window.__LANCHAT_API.sendFolder('项目资料', [
      { rel: '说明.txt', file: new File([new Blob(['说明'])], '说明.txt') },
      { rel: '图纸/A.dwg', file: new File([new Blob(['dwg'])], 'A.dwg') },
      { rel: '图纸/B.dwg', file: new File([new Blob(['dwg'])], 'B.dwg') }
    ]);
    await new Promise(r => setTimeout(r, 900));
    return 1;
  })()`);
  await sleep(1200);

  const metrics = await evalJS(c2, `(function(){
    var list = document.getElementById('list').getBoundingClientRect();
    var mid = list.left + list.width / 2;
    var out = [];
    document.querySelectorAll('.msg').forEach(function(row){
      var b = row.querySelector('.bubble');
      if (!b) return;
      var r = b.getBoundingClientRect();
      var acts = row.querySelector('.acts');
      var titles = acts ? [].map.call(acts.querySelectorAll('button'), function(x){ return x.title; }) : [];
      out.push({
        mine: row.classList.contains('mine'),
        left: Math.round(r.left), right: Math.round(r.right),
        cx: Math.round(r.left + r.width / 2 - mid),
        titles: titles
      });
    });
    return JSON.stringify({ mid: Math.round(mid), rows: out, ver: document.getElementById('verChat').textContent });
  })()`);
  console.log('METRICS ' + metrics);
  await shot(c2, 'chat');

  /* ---------- 2.1) 标题栏 / 输入区上方的工具条（v2.6） ---------- */
  const head = await evalJS(c2, `(function(){
    var bar = document.getElementById('chatbar').getBoundingClientRect();
    var box = document.getElementById('searchBox').getBoundingClientRect();
    var inp = document.getElementById('input').getBoundingClientRect();
    var tools = document.querySelectorAll('#chat footer .tool');
    var tr = tools.length ? tools[0].getBoundingClientRect() : null;
    return JSON.stringify({
      h: Math.round(bar.height),
      brand: document.querySelector('#chatbar .apptitle b').textContent,
      dev: document.getElementById('devName').textContent,
      barTools: document.querySelectorAll('#chatbar .tool').length,
      footTools: tools.length,
      searchRightGap: Math.round(inp.right - box.right),
      searchToolCenterDiff: tr ? Math.round((tr.top + tr.height / 2) - (box.top + box.height / 2)) : null,
      devFont: (function(){
        var d = document.querySelector('.msg .meta .dev');
        var w = document.querySelector('.msg .meta .who');
        return d && w ? Math.round(parseFloat(getComputedStyle(d).fontSize) * 10) / 10 + '/' + Math.round(parseFloat(getComputedStyle(w).fontSize) * 10) / 10 : '';
      })()
    });
  })()`);
  console.log('HEAD ' + head);

  /* ---------- 2.2) 侧边栏收起 / 展开 ---------- */
  const sbBefore = await evalJS(c2, `Math.round(document.getElementById('sidebar').getBoundingClientRect().width)`);
  await evalJS(c2, `document.getElementById('sbToggle').click(); 1`);
  await sleep(500);
  const sbAfter = JSON.parse(await evalJS(c2, `JSON.stringify({
    w: Math.round(document.getElementById('sidebar').getBoundingClientRect().width),
    collapsed: document.body.classList.contains('sbcollapsed'),
    mainW: Math.round(document.getElementById('chatmain').getBoundingClientRect().width)
  })`));
  console.log('SIDEBAR ' + JSON.stringify({ before: sbBefore, after: sbAfter }));
  await shot(c2, 'chat_collapsed');
  await evalJS(c2, `document.getElementById('sbToggle').click(); 1`);
  await sleep(500);
  const sbBack = await evalJS(c2, `Math.round(document.getElementById('sidebar').getBoundingClientRect().width)`);
  console.log('SIDEBAR_BACK ' + sbBack);

  /* ---------- 2.3) 搜索过滤 ---------- */
  await evalJS(c2, `(function(){
    var i = document.getElementById('searchInp');
    i.value = '本机';
    i.dispatchEvent(new Event('input'));
    return 1;
  })()`);
  await sleep(500);
  const searchInfo = await evalJS(c2, `JSON.stringify({
    all: document.querySelectorAll('.msg').length,
    vis: document.querySelectorAll('.msg:not(.qhide)').length,
    hasq: document.getElementById('searchBox').classList.contains('hasq'),
    allMatch: [].every.call(document.querySelectorAll('.msg:not(.qhide)'), function(r){
      return (r.getAttribute('data-q') || '').indexOf('本机') >= 0;
    })
  })`);
  console.log('SEARCH ' + searchInfo);
  await shot(c2, 'chat_search');
  await evalJS(c2, `document.getElementById('searchClear').click(); 1`);
  await sleep(300);

  /* ---------- 2.5) 气泡右键菜单（v2.7） ---------- */
  const ctxInfo = await evalJS(c2, `(function(){
    var row = document.querySelector('.msg.other') || document.querySelector('.msg');
    var r = row.getBoundingClientRect();
    row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true,
      clientX: Math.round(r.left + 90), clientY: Math.round(r.top + 20) }));
    var m = document.getElementById('ctxmenu');
    var box = m.getBoundingClientRect();
    return JSON.stringify({
      open: m.classList.contains('on'),
      items: [].map.call(m.querySelectorAll('.ctxitem'), function(b){ return b.textContent.trim(); }),
      x: Math.round(box.left), y: Math.round(box.top),
      w: Math.round(box.width), h: Math.round(box.height),
      inView: box.left >= 0 && box.top >= 0 && box.right <= innerWidth + 0.5 && box.bottom <= innerHeight + 0.5
    });
  })()`);
  console.log('CTX ' + ctxInfo);
  await sleep(300);   // 等菜单淡入动画结束再截图，不然拍到半透明
  await shot(c2, 'chat_ctx');

  /* ---------- 2.6) 多选 → 删除选中（演示目录在内存里，删了不心疼） ---------- */
  await evalJS(c2, `document.querySelectorAll('#ctxmenu .ctxitem')[1].click(); 1`);
  await sleep(300);
  const selState = await evalJS(c2, `(function(){
    var rows = document.querySelectorAll('.msg');
    rows[1].dispatchEvent(new MouseEvent('click', { bubbles: true }));
    var box = document.querySelector('.msg .selbox');
    return JSON.stringify({
      mode: document.body.classList.contains('selmode'),
      bar: document.getElementById('selbar').classList.contains('on'),
      n: document.querySelectorAll('.msg.sel').length,
      count: document.getElementById('selCount').textContent,
      boxes: document.querySelectorAll('.msg .selbox').length,
      boxVisible: box ? getComputedStyle(box).display !== 'none' : false,
      barY: Math.round(document.getElementById('selbar').getBoundingClientRect().top)
    });
  })()`);
  console.log('SEL ' + selState);
  await sleep(300);
  await shot(c2, 'chat_select');

  await evalJS(c2, `document.getElementById('selDel').click(); 1`);
  await sleep(400);
  const cfInfo = await evalJS(c2, `JSON.stringify({
    open: document.getElementById('confirm').classList.contains('on'),
    title: document.getElementById('cfTitle').textContent,
    text: (document.getElementById('cfText').textContent || '').slice(0, 42),
    buttons: [].map.call(document.querySelectorAll('#cfActions button'), function(b){ return b.textContent; })
  })`);
  console.log('DELETE_CONFIRM ' + cfInfo);
  await shot(c2, 'chat_delete_confirm');

  const beforeDel = await evalJS(c2, `document.querySelectorAll('.msg').length`);
  await evalJS(c2, `document.querySelector('#cfActions button.danger').click(); 1`);
  await sleep(900);
  const afterDel = await evalJS(c2, `JSON.stringify({
    rows: document.querySelectorAll('.msg').length,
    sel: document.querySelectorAll('.msg.sel').length,
    mode: document.body.classList.contains('selmode'),
    bar: document.getElementById('selbar').classList.contains('on')
  })`);
  console.log('DELETE ' + JSON.stringify({ before: beforeDel, after: JSON.parse(afterDel) }));
  await shot(c2, 'chat_after_delete');

  /* ---------- 2.6b) 单条删除：右键 → 删除这条消息 ---------- */
  const oneId = await evalJS(c2, `(function(){
    var rows = document.querySelectorAll('.msg');
    var row = rows[rows.length - 1];
    var r = row.getBoundingClientRect();
    row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true,
      clientX: Math.round(r.left + 80), clientY: Math.round(r.top + 16) }));
    return row.getAttribute('data-id');
  })()`);
  await sleep(250);
  await evalJS(c2, `document.querySelectorAll('#ctxmenu .ctxitem')[0].click(); 1`);
  await sleep(700);
  const oneDlg = await evalJS(c2, `JSON.stringify({
    on: document.getElementById('confirm').classList.contains('on'),
    text: (document.getElementById('cfText').textContent || '').slice(0, 26)
  })`);
  await evalJS(c2, `document.querySelector('#cfActions button.danger').click(); 1`);
  await sleep(1300);
  const oneRes = await evalJS(c2, `(function(){
    var id = ${JSON.stringify(oneId)};
    var still = document.querySelector('.msg[data-id="' + id + '"]');
    return JSON.stringify({ id: id, gone: !still, rows: document.querySelectorAll('.msg').length });
  })()`);
  console.log('DELETE_ONE ' + oneRes + ' dialog=' + oneDlg);
  await shot(c2, 'chat_after_delete_one');

  /* ---------- 2.7) 侧边栏右键菜单 ---------- */
  const sbCtx = await evalJS(c2, `(function(){
    var row = document.querySelector('#sbList .sbitem:not([data-miss="1"]):not([data-path=""])') ||
              document.querySelector('#sbList .sbitem');
    if (!row) return JSON.stringify({ noItems: true });
    var r = row.getBoundingClientRect();
    row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true,
      clientX: Math.round(r.left + 40), clientY: Math.round(r.top + 10) }));
    var m = document.getElementById('ctxmenu');
    var box = m.getBoundingClientRect();
    return JSON.stringify({
      open: m.classList.contains('on'),
      items: [].map.call(m.querySelectorAll('.ctxitem'), function(b){
        return b.textContent.trim() + (b.disabled ? '(禁用)' : '');
      }),
      inView: box.left >= 0 && box.top >= 0 && box.right <= innerWidth + 0.5 && box.bottom <= innerHeight + 0.5
    });
  })()`);
  console.log('SB_CTX ' + sbCtx);
  await sleep(450);
  await shot(c2, 'chat_sb_ctx');
  await evalJS(c2, `document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' })); 1`);
  await sleep(200);
  console.log('SB_CTX_CLOSED ' + await evalJS(c2, `!document.getElementById('ctxmenu').classList.contains('on')`));

  /* ---------- 2.7b) 侧边栏「删除」→ 主页文件夹列表同步（v2.7） ---------- */
  const GRP = `function grp(boxSel, headCls, itemCls, attr){
    var out = [], g = '';
    [].forEach.call(document.querySelector(boxSel).children, function(el){
      if (el.classList.contains(headCls)) g = el.textContent.trim();
      else if (el.classList.contains(itemCls)) out.push(g + '|' + (el.getAttribute(attr) || ''));
    });
    return out;
  }`;
  const pickTarget = await evalJS(c2, `(function(){
    ${GRP}
    var rows = [].slice.call(document.querySelectorAll('#sbList .sbitem'));
    // 挑一条属于「最近打开」、指向本次测试临时目录的记录；不动用户自己的文件夹
    var row = rows.filter(function(b){
      var p = b.getAttribute('data-path') || '';
      return /lc(?:2\d|30)/i.test(p) && !b.classList.contains('on') &&
        b.getAttribute('data-kind') === 'recent';
    })[0];
    if (!row) return JSON.stringify({ skip: true });
    window.__DEL = { name: row.getAttribute('data-name'), path: row.getAttribute('data-path') };
    var r = row.getBoundingClientRect();
    row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true,
      clientX: Math.round(r.left + 40), clientY: Math.round(r.top + 10) }));
    return JSON.stringify({ skip: false, name: window.__DEL.name, path: window.__DEL.path });
  })()`);
  console.log('SB_DELETE_TARGET ' + pickTarget);
  await sleep(300);
  if (pickTarget.indexOf('"skip":true') < 0) {
    await evalJS(c2, `document.querySelector('#ctxmenu .ctxitem.danger').click(); 1`);
    await sleep(500);
    console.log('SB_DELETE_DIALOG ' + await evalJS(c2, `JSON.stringify({
      on: document.getElementById('confirm').classList.contains('on'),
      title: document.getElementById('cfTitle').textContent,
      text: (document.getElementById('cfText').textContent || '').slice(0, 40)
    })`));
    await shot(c2, 'chat_sb_delete_confirm');
  }

  if (pickTarget.indexOf('"skip":true') < 0) {
    await evalJS(c2, `document.querySelector('#cfActions button.danger').click(); 1`);
    await sleep(1600);
    console.log('SB_DELETE ' + await evalJS(c2, `(function(){
      ${GRP}
      var items = grp('#sbList', 'sbgroup', 'sbitem', 'data-path');
      var p = window.__DEL.path.toLowerCase();
      var inRecent = items.filter(function(x){ return x.indexOf('最近打开|') === 0 &&
        x.slice(-p.length).toLowerCase() === p; });
      var stillAnywhere = items.filter(function(x){ return x.slice(-p.length).toLowerCase() === p; });
      return JSON.stringify({
        inRecent: inRecent.length, stillAnywhere: stillAnywhere.length,
        total: items.length,
        toast: (document.querySelector('[class*=toast]') || {}).textContent || ''
      });
    })()`));
    await shot(c2, 'chat_sb_deleted');
  }
  /* ---------- 2.7c) 「本机的网络共享」那一组也能删掉（v2.9 修） ----------
     这一组的数据来自 NetShareEnum，删「最近打开」的记录根本影响不到它，
     所以以前在这里右键删除等于没反应。 */
  await evalJS(c2, `document.getElementById('sbRefresh').click(); 1`);
  await sleep(1400);
  const shareTarget = await evalJS(c2, `(function(){
    var rows = [].slice.call(document.querySelectorAll('#sbList .sbitem[data-kind="share"]'));
    var row = rows.filter(function(b){ return /lc(?:2\d|30)/i.test(b.getAttribute('data-path') || ''); })[0];
    if (!row) return JSON.stringify({ skip: true, n: rows.length });
    window.__DELS = { name: row.getAttribute('data-name'), path: row.getAttribute('data-path') };
    var r = row.getBoundingClientRect();
    row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true,
      clientX: Math.round(r.left + 40), clientY: Math.round(r.top + 10) }));
    return JSON.stringify({ skip: false, name: window.__DELS.name, path: window.__DELS.path,
      menu: [].map.call(document.querySelectorAll('#ctxmenu .ctxitem'), function(x){ return x.textContent.trim(); }) });
  })()`);
  console.log('SB_SHARE_TARGET ' + shareTarget);
  await sleep(300);
  if (shareTarget.indexOf('"skip":true') < 0) {
    await evalJS(c2, `document.querySelector('#ctxmenu .ctxitem.danger').click(); 1`);
    await sleep(500);
    console.log('SB_SHARE_DIALOG ' + await evalJS(c2, `JSON.stringify({
      on: document.getElementById('confirm').classList.contains('on'),
      text: (document.getElementById('cfText').textContent || '').slice(0, 80),
      okLabel: document.querySelector('#cfActions button.danger').textContent.trim()
    })`));
    await shot(c2, 'chat_sb_share_confirm');
    await evalJS(c2, `document.querySelector('#cfActions button.danger').click(); 1`);
    await sleep(1500);
    /* 再刷一次：隐藏记录必须落在后端配置里，否则重启后又冒出来 */
    await evalJS(c2, `document.getElementById('sbRefresh').click(); 1`);
    await sleep(1400);
    console.log('SB_SHARE_DELETE ' + await evalJS(c2, `(function(){
      var items = [].slice.call(document.querySelectorAll('#sbList .sbitem'));
      var p = window.__DELS.path.toLowerCase();
      var hit = items.filter(function(b){ return (b.getAttribute('data-path') || '').toLowerCase() === p; });
      return JSON.stringify({ still: hit.length, total: items.length,
        groups: [].map.call(document.querySelectorAll('#sbList .sbgroup'), function(g){ return g.textContent.trim(); }),
        toast: (document.querySelector('[class*=toast]') || {}).textContent || '' });
    })()`));
    await shot(c2, 'chat_sb_share_deleted');
  }

  /* ---------- 2.4) 最小窗口 780x520：标题栏和工具条不能溢出 ---------- */
  await c2.send('Emulation.setDeviceMetricsOverride', { width: 780, height: 520, deviceScaleFactor: 1, mobile: false });
  await sleep(500);
  const minInfo = await evalJS(c2, `(function(){
    var bar = document.getElementById('chatbar').getBoundingClientRect();
    var btns = document.querySelectorAll('#chatbar .wbtn');
    var last = btns[btns.length - 1].getBoundingClientRect();
    var box = document.getElementById('searchBox').getBoundingClientRect();
    var tools = document.querySelectorAll('#chat footer .tool');
    var tl = tools.length ? tools[tools.length - 1].getBoundingClientRect() : null;
    var inp = document.getElementById('input').getBoundingClientRect();
    return JSON.stringify({
      barW: Math.round(bar.width), barH: Math.round(bar.height),
      lastBtnRight: Math.round(last.right), barOverflow: Math.round(last.right) > Math.round(bar.width),
      searchW: Math.round(box.width), searchGapToInput: Math.round(inp.right - box.right),
      toolsToSearchGap: tl ? Math.round(box.left - tl.right) : null,
      sideW: Math.round(document.getElementById('sidebar').getBoundingClientRect().width)
    });
  })()`);
  console.log('MIN ' + minInfo);
  await shot(c2, 'chat_min');
  await c2.send('Emulation.setDeviceMetricsOverride', { width: 1080, height: 760, deviceScaleFactor: 1, mobile: false });
  await sleep(300);

  // 悬停第一个本机气泡，让功能按钮浮出来
  const pos = JSON.parse(await evalJS(c2, `(function(){
    var row = document.querySelector('.msg.mine') || document.querySelector('.msg');
    var r = row.getBoundingClientRect();
    return JSON.stringify({ x: Math.round(r.right - 40), y: Math.round(r.top + r.height / 2) });
  })()`));
  await c2.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: pos.x, y: pos.y });
  await sleep(500);
  await shot(c2, 'chat_hover');

  /* ---------- 3) 空状态（v2.8：主页已删，没有文件夹时铺在聊天区） ---------- */
  await evalJS(c2, `window.__LANCHAT_API.showEmpty(true); 1`);
  await sleep(600);
  const empty = await evalJS(c2, `(function(){
    var e = document.getElementById('empty');
    var main = document.getElementById('chatmain');
    var er = e.getBoundingClientRect(), mr = main.getBoundingClientRect();
    var sb = document.getElementById('sidebar').getBoundingClientRect();
    return JSON.stringify({
      shown: !e.classList.contains('hidden'),
      covers: Math.round(er.width) === Math.round(mr.width) && Math.round(er.height) === Math.round(mr.height),
      inputDisabled: document.getElementById('input').disabled,
      toolsDisabled: document.getElementById('imgBtn').disabled,
      sbVisible: Math.round(sb.width) > 100 && !document.body.classList.contains('sbcollapsed'),
      sbNewBtn: !!document.getElementById('sbNew'),
      noHome: !document.getElementById('setup') && !document.querySelector('.card'),
      ebBtnsHidden: getComputedStyle(document.querySelector('#empty .ebtns')).display === 'none',
      mode: window.__WINMODE,
      text: (e.querySelector('p').textContent || '').replace(/\s+/g, ' ').trim()
    });
  })()`);
  console.log('EMPTY ' + empty);
  await shot(c2, 'chat_empty');

  /* 帮助气泡挪到标题栏了：悬浮问号能展开，且不超出窗口 */
  const hp = JSON.parse(await evalJS(c2, `(function(){
    var r = document.getElementById('helpBtn').getBoundingClientRect();
    return JSON.stringify({ x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) });
  })()`));
  await c2.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: hp.x, y: hp.y });
  await sleep(700);
  const popVis = await evalJS(c2, `(function(){
    var p = document.querySelector('.helppop'), r = p.getBoundingClientRect();
    return getComputedStyle(p).visibility + ' right=' + Math.round(r.right) + ' winW=' + window.innerWidth +
      ' inWin=' + (r.left >= 0 && r.right <= window.innerWidth + 0.5);
  })()`);
  console.log('HELP_POP ' + popVis);
  await shot(c2, 'chat_help');
  await c2.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: 6, y: 420 });   // 移开鼠标，免得后面的截图还挂着说明气泡

  /* 收起空状态，回到正常聊天 */
  await evalJS(c2, `window.__LANCHAT_API.showEmpty(false); 1`);
  await sleep(400);


  /* ---------- 3.6) 新建共享文件夹卡片（v2.8 起盖在聊天窗口里，不再换窗口） ---------- */
  await evalJS(c2, `window.__LANCHAT_API.openNewShare(); 1`);
  await sleep(1400);
  const ns = await evalJS(c2, `(function(){
    var box = document.querySelector('.nsbox').getBoundingClientRect();
    var ov = document.getElementById('newshare').getBoundingClientRect();
    var body = document.getElementById('nsForm');
    return JSON.stringify({
      open: document.getElementById('newshare').classList.contains('on'),
      mode: window.__WINMODE,
      overlayCovers: Math.abs(ov.width - window.innerWidth) < 1.5,
      boxW: Math.round(box.width), boxH: Math.round(box.height),
      boxInWin: box.left >= 0 && box.right <= window.innerWidth + 0.5 && box.top >= 0,
      barStillThere: getComputedStyle(document.getElementById('chatbar')).display !== 'none',
      parent: document.getElementById('nsParent').value,
      hint: (document.getElementById('nsHint').textContent || '').slice(0, 34),
      scroll: body.scrollHeight > body.clientHeight + 2
    });
  })()`);
  console.log('NEWSHARE ' + ns);
  await shot(c2, 'newshare');

  /* ---------- 3.7) 真的建一个（临时目录，跑完删掉） ---------- */
  await evalJS(c2, `(function(){
    var S = window.__LANCHAT_API.nsState();
    S.parent = 'C:/Temp/lc30/demo';
    document.getElementById('nsParent').value = S.parent;
    document.getElementById('nsName').value = '演示共享';
    document.getElementById('nsName').dispatchEvent(new Event('input'));
    document.getElementById('nsShareName').value = '';
    document.getElementById('nsCreate').click();
    return 1;
  })()`);
  await sleep(6000);
  const nres = await evalJS(c2, `(function(){
    var msg = document.getElementById('nsDoneMsg');
    return JSON.stringify({
      done: !document.getElementById('nsDone').classList.contains('hidden'),
      title: document.getElementById('nsDoneTitle').textContent,
      path: document.getElementById('nsResPath').textContent,
      unc: document.getElementById('nsResUNC').textContent,
      perm: document.getElementById('nsResPerm').textContent,
      notes: document.getElementById('nsResNotes').textContent,
      msg: (msg.textContent || '').slice(0, 80)
    });
  })()`);
  console.log('NEWSHARE_DONE ' + nres);
  await shot(c2, 'newshare_done');
  await evalJS(c2, `document.getElementById('nsX').click(); 1`);
  await sleep(600);
  const closed = await evalJS(c2, `JSON.stringify({open: document.getElementById('newshare').classList.contains('on'), mode: window.__WINMODE})`);
  console.log('NEWSHARE_CLOSED ' + closed);

  /* ---------- 3.8) 退出房间 → 回到空状态（leaveRoom 路径） ---------- */
  await evalJS(c2, `window.__LANCHAT_API.leaveRoom(); 1`);
  await sleep(600);
  const noRoom = await evalJS(c2, `JSON.stringify({
    emptyShown: !document.getElementById('empty').classList.contains('hidden'),
    rows: document.querySelectorAll('.msg').length,
    sbRows: document.querySelectorAll('#sbList .sbitem').length,
    inputDisabled: document.getElementById('input').disabled,
    mode: window.__WINMODE
  })`);
  console.log('NO_ROOM ' + noRoom);
  await shot(c2, 'chat_no_room');

  /* ---------- 4) 设置面板 ---------- */
  await c2.send('Emulation.setDeviceMetricsOverride', { width: 700, height: 640, deviceScaleFactor: 1, mobile: false });
  await sleep(400);
  await evalJS(c2, `document.getElementById('cfgBtn').click(); 1`);
  await sleep(800);
  const setInfo = await evalJS(c2, `(function(){
    var s = document.getElementById('settings');
    var seg = [].filter.call(document.querySelectorAll('#closeSeg button'), function(b){
      return b.classList.contains('on');
    }).map(function(b){ return b.textContent; });
    return JSON.stringify({
      open: s.classList.contains('on'),
      sw: document.getElementById('autoSwitch').getAttribute('aria-checked'),
      segOn: seg,
      root: document.getElementById('setRoot').textContent,
      chatbarH: Math.round(document.getElementById('chatbar').getBoundingClientRect().height),
      winmode: window.__WINMODE,
      bodyNative: document.body.classList.contains('native')
    });
  })()`);
  console.log('SETTINGS ' + setInfo);
  await shot(c2, 'settings');

  c2.close();
}

main().catch((e) => { console.error('FAIL', e.message); process.exit(1); });

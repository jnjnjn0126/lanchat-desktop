#!/bin/bash
# v3.2 一键验收：重建干净的共享目录 → 起无头 Lanchat → 起无头 Chrome → 跑 CDP 用例 → 收尾
# 跑法：bash tests/run_v32.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
PROJ="$(dirname "$HERE")"
PY="/c/Users/Administrator/.workbuddy/binaries/python/versions/3.13.12/python.exe"
NODE="/c/Users/Administrator/.workbuddy/binaries/node/versions/22.22.2-6/node.exe"
CHROME="/c/Program Files/Google/Chrome/Application/chrome.exe"
W=/c/Temp/lc32t
PORT=47961
DPORT=9510

unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy

echo "== 重建测试环境 =="
rm -rf "$W"
mkdir -p "$W/appdata/LanChat" "$W/share/_lanchat/msgs" "$W/share/_lanchat/peers" \
         "$W/share/_lanchat/blobs" "$W/share/docs" "$W/prof" "$PROJ/截图"
printf '{"root":"C:\\\\Temp\\\\lc32t\\\\share","recents":[],"noResume":false,"profile":{"name":"小新","peerId":"aabbccddeeff00112233445566778899"}}' \
  > "$W/appdata/LanChat/config.json"
printf 'hello 中文测试\nsecond line\n' > "$W/share/a.txt"
printf '# 标题\n\n正文内容\n' > "$W/share/b.md"
printf '子目录里的内容\n' > "$W/share/docs/d.txt"
"$PY" -c "
open(r'C:\Temp\lc32t\share\gbk.txt','wb').write('这是GBK编码的中文内容'.encode('gbk'))
open(r'C:\Temp\lc32t\share\big.bin.part','wb').write(bytes([0xAA])*2097152)
"

echo "== 启动 Lanchat（无头）=="
cd "$PROJ"
APPDATA="C:\\Temp\\lc32t\\appdata" LANCHAT_HEADLESS=1 LANCHAT_PORT=$PORT ./Lanchat.exe > "$W/lanchat.log" 2>&1 &
LC_PID=$!
for i in $(seq 1 40); do
  curl -s --noproxy '*' -o /dev/null "http://127.0.0.1:$PORT/api/ping" && break
  sleep 0.3
done

echo "== 启动 Chrome（无头）=="
"$CHROME" --headless=new --disable-gpu --no-first-run --no-proxy-server \
  --remote-debugging-port=$DPORT --user-data-dir="C:\\Temp\\lc32t\\prof" about:blank \
  > "$W/chrome.log" 2>&1 &
CH_PID=$!
for i in $(seq 1 50); do
  curl -s --noproxy '*' -o /dev/null "http://127.0.0.1:$DPORT/json/version" && break
  sleep 0.3
done

echo "== 跑用例 =="
NO_PROXY="127.0.0.1" "$NODE" "$HERE/cdp_v32.mjs" "http://127.0.0.1:$PORT/" "$DPORT" "$PROJ/截图" "C:\\Temp\\lc32t\\share"
RC=$?

echo "== 校验 zip 能不能被标准工具打开 =="
"$PY" - << 'EOF'
import os, zipfile
cands = [r'C:\Temp\lc32t\share\打包测试.zip', r'C:\Temp\lc32t\share\2026-10\打包测试.zip']
p = next((c for c in cands if os.path.exists(c)), None)
if not p:
    print('SKIP 没有生成 zip')
else:
    with zipfile.ZipFile(p) as z:
        bad = z.testzip()
        names = z.namelist()
        print('ZIP 条目 %d 个，损坏条目 %s' % (len(names), bad))
        print('样例：', names[:3])
        assert len(names) == 40, '条目数不对'
        assert names[0].startswith('打包测试/a/b'), names[0]
        assert bad is None
        print('ZIP_OK')
EOF
ZRC=$?

kill $CH_PID 2>/dev/null
kill $LC_PID 2>/dev/null
sleep 0.5

if [ $RC -eq 0 ] && [ $ZRC -eq 0 ]; then echo "ALL_PASS"; else echo "FAILED (cdp=$RC zip=$ZRC)"; fi
exit $(( RC + ZRC ))

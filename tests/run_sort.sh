#!/bin/bash
# v3.3 气泡顺序回归：重建干净目录 → 起无头 Lanchat → 起无头 Chrome → 跑 CDP 用例
# 跑法：bash tests/run_sort.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
PROJ="$(dirname "$HERE")"
NODE="/c/Users/Administrator/.workbuddy/binaries/node/versions/22.22.2-6/node.exe"
CHROME="/c/Program Files/Google/Chrome/Application/chrome.exe"
W=/c/Temp/lc33t
PORT=47981
DPORT=9530

unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy

echo "== 重建测试环境 =="
rm -rf "$W"
mkdir -p "$W/appdata/LanChat" "$W/share/_lanchat/msgs" "$W/share/_lanchat/peers" \
         "$W/share/_lanchat/blobs" "$W/prof"
printf '{"root":"C:\\\\Temp\\\\lc33t\\\\share","recents":[],"noResume":false,"profile":{"name":"小新","peerId":"aabbccddeeff00112233445566778899"}}' \
  > "$W/appdata/LanChat/config.json"
printf 'hello\n' > "$W/share/a.txt"

echo "== 启动 Lanchat（无头）=="
cd "$PROJ"
APPDATA="C:\\Temp\\lc33t\\appdata" LANCHAT_HEADLESS=1 LANCHAT_PORT=$PORT ./Lanchat.exe > "$W/lanchat.log" 2>&1 &
LC_PID=$!
for i in $(seq 1 40); do
  curl -s --noproxy '*' -o /dev/null "http://127.0.0.1:$PORT/api/ping" && break
  sleep 0.3
done

echo "== 启动 Chrome（无头）=="
"$CHROME" --headless=new --disable-gpu --no-first-run --no-proxy-server \
  --remote-debugging-port=$DPORT --user-data-dir="C:\\Temp\\lc33t\\prof" about:blank \
  > "$W/chrome.log" 2>&1 &
CH_PID=$!
for i in $(seq 1 50); do
  curl -s --noproxy '*' -o /dev/null "http://127.0.0.1:$DPORT/json/version" && break
  sleep 0.3
done

echo "== 跑用例 =="
NO_PROXY="127.0.0.1" "$NODE" "$HERE/cdp_sort.mjs" "http://127.0.0.1:$PORT/" "$DPORT" "C:\\Temp\\lc33t\\share"
RC=$?

kill $CH_PID 2>/dev/null
kill $LC_PID 2>/dev/null
sleep 0.5

if [ $RC -eq 0 ]; then echo "ALL_PASS"; else echo "FAILED ($RC)"; fi
exit $RC

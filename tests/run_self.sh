#!/bin/bash
# 跑页面内置自检（?selftest=1）：起一个干净的无头 Lanchat + 无头 Chrome，
# 让页面自己跑一遍断言，再把结果逐条打出来。
# 跑法：bash tests/run_self.sh
# 用的是独立 APPDATA（C:\Temp\lcself），不碰用户真实的 %APPDATA%\LanChat。
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
PROJ="$(dirname "$HERE")"
NODE="/c/Users/Administrator/.workbuddy/binaries/node/versions/22.22.2-6/node.exe"
CHROME="/c/Program Files/Google/Chrome/Application/chrome.exe"
W=/c/Temp/lcself
PORT=47971
DPORT=9511

unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy

mkdir -p "$W/appdata/LanChat" "$W/share/_lanchat" "$W/prof"
printf '{"root":"C:\\\\Temp\\\\lcself\\\\share","recents":[],"noResume":false,"profile":{"name":"小新","peerId":"aabbccddeeff00112233445566778899"}}' \
  > "$W/appdata/LanChat/config.json"
printf 'hello\n' > "$W/share/a.txt"

cd "$PROJ"
APPDATA="C:\\Temp\\lcself\\appdata" LANCHAT_HEADLESS=1 LANCHAT_PORT=$PORT ./Lanchat.exe > "$W/lanchat.log" 2>&1 &
LC_PID=$!
for i in $(seq 1 40); do
  curl -s --noproxy '*' -o /dev/null "http://127.0.0.1:$PORT/api/ping" && break
  sleep 0.3
done

"$CHROME" --headless=new --disable-gpu --no-first-run --no-proxy-server \
  --remote-debugging-port=$DPORT --user-data-dir="C:\\Temp\\lcself\\prof" about:blank \
  > "$W/chrome.log" 2>&1 &
CH_PID=$!
for i in $(seq 1 50); do
  curl -s --noproxy '*' -o /dev/null "http://127.0.0.1:$DPORT/json/version" && break
  sleep 0.3
done

NO_PROXY="127.0.0.1" "$NODE" "$HERE/selftest.mjs" "http://127.0.0.1:$PORT/" "$DPORT"
RC=$?

kill $CH_PID 2>/dev/null
kill $LC_PID 2>/dev/null
sleep 0.5
exit $RC

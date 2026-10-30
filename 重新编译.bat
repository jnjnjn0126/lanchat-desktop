@echo off
rem 重新编译 Lanchat.exe（需要 E:\tools\go，用法见 使用说明.txt）
rem 注意：rsrc_windows_amd64.syso 里装着 exe 的图标资源（蓝底白气泡 logo），
rem       删掉它就编不出带图标的 exe 了，构建时不要清理这个文件。
setlocal
cd /d "%~dp0"
set "GOROOT=E:\tools\go"
set "PATH=%GOROOT%\bin;%PATH%"
set "GOPATH=E:\tools\gopath"
set "GOPROXY=https://goproxy.cn,direct"

where go >nul 2>nul
if errorlevel 1 (
  echo [错误] 没找到 go 命令。请确认 E:\tools\go\bin 存在。
  pause
  exit /b 1
)

if not exist "rsrc_windows_amd64.syso" (
  echo [提醒] 缺少图标资源 rsrc_windows_amd64.syso，编出来的 exe 会用代码画的兜底图标。
)

echo 正在编译...
go build -trimpath -ldflags "-H windowsgui -s -w" -o "Lanchat.exe" .
if errorlevel 1 (
  echo [失败] 编译出错，请看上面的提示。
  pause
  exit /b 1
)
echo [完成] Lanchat.exe 已更新
pause

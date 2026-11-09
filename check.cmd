@echo off
rem FledgeFlow-oss: run every pre-PR check (cargo fmt/clippy/test, gofmt/go vet/go test, benchmarks once, leak check).
setlocal
set "FFOSS_PAUSE=0"
rem Windows' own find.exe (unquoted: a pipe drops the outer quotes); a Unix find from Git on PATH would scan the disk.
echo %cmdcmdline% | %SystemRoot%\System32\find.exe /i "/c" >nul && set "FFOSS_PAUSE=1"
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0start.ps1" -Check %*
set "FFOSS_RC=%errorlevel%"
if "%FFOSS_PAUSE%"=="1" pause
exit /b %FFOSS_RC%

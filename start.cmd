@echo off
rem FledgeFlow-oss launcher (double-click): checks Rust/Go, builds ffimg and tries it on a synthetic image.
rem Options: start.ps1 -? / README "Launcher" section.
setlocal
set "FFOSS_PAUSE=0"
rem Opened by double-click (cmd /c): keep the window open so the result can be read.
rem Windows' own find.exe (unquoted: a pipe drops the outer quotes); a Unix find from Git on PATH would scan the disk.
echo %cmdcmdline% | %SystemRoot%\System32\find.exe /i "/c" >nul && set "FFOSS_PAUSE=1"
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0start.ps1" %*
set "FFOSS_RC=%errorlevel%"
if "%FFOSS_PAUSE%"=="1" pause
exit /b %FFOSS_RC%

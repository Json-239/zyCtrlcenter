@echo off
REM 2026-09-22 detached start (NOT "start /b"): /b shares the parent console,
REM so the controller dies when the parent shell is cleaned up (2 silent deaths).
cd /d F:\ZyBin\zyCtrlcenter
set ROBOT_CTRL_HOST=127.0.0.1
set ROBOT_CTRL_PORT=27200
set CTRL_AUTO_RESTORE=1
start "zyctrlcenter" /min "F:\ZyBin\zyCtrlcenter\zyctrlcenter.exe" --deploy "F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy"

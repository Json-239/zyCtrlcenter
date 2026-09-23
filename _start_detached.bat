@echo off
REM ============================================================
REM  Production starter (Windows, detached): controller + robot.
REM
REM  - Detached via "start /min". Do NOT use "start /b": /b shares the parent
REM    console, so the controller dies when the parent shell is cleaned up
REM    (documented cause of 2 silent deaths on 2026-09-22).
REM  - --auto-robot: the controller starts robot_single_robot.exe itself
REM    (cwd = deploy dir; orphan robot processes in that dir are killed first
REM    so two processes never fight over the single control channel).
REM  - CTRL_AUTO_RESTORE=1: after a controller restart, re-dispatch tasks
REM    (start_chain / ghost_start) from stored intents.
REM
REM  NOTE: keep this file ASCII-only. cmd decodes .bat with the console code
REM  page; multi-byte (UTF-8) comment bytes can swallow quotes and break the
REM  parser mid-line (seen: a Chinese REM line ran as a bogus command).
REM  Panel after start: http://127.0.0.1:28082/
REM ============================================================
cd /d F:\ZyBin\zyCtrlcenter
set ROBOT_CTRL_HOST=127.0.0.1
set ROBOT_CTRL_PORT=27200
set CTRL_AUTO_RESTORE=1

REM Live server online count (livecount). Disabled per owner's decision 2026-09-22:
REM   GET http://<game-server>:8080/gm/online -> {"online_count":N,"success":true,"serverId":1000}
REM   No GM auth needed. To enable, allowlist our public egress IP 110.90.3.243
REM   on the game server, set ENABLED=1 here, or toggle at runtime via
REM   POST /api/livecount (no restart needed).
set CTRL_LIVECOUNT_ENABLED=0
set CTRL_LIVECOUNT_URL=http://47.96.8.240:8080
set CTRL_LIVECOUNT_SERVER_ID=1000

start "zyctrlcenter" /min "F:\ZyBin\zyCtrlcenter\zyctrlcenter.exe" --deploy "F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy" --auto-robot

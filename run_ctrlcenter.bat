@echo off
REM ============================================================
REM  zyCtrlcenter launcher (Go)
REM  - Ports: HTTP API 28082 / control channel 27200 (override by args)
REM  - Does NOT start the robot process by default (add --auto-robot)
REM  - Refuses to start twice on default ports: a second instance cannot bind
REM    the channel and the panel would show "channel not connected".
REM    Use --restart to replace the running instance.
REM  - Logs: logs\ctrlcenter_YYYYMMDD.log
REM  - Multi-instance: run_ctrlcenter.bat --web-port 28083 --ctrl-port 27201 --data-dir data_2
REM  - CTRL_AUTO_RESTORE=1 by default (restore engine re-dispatches lost tasks).
REM  NOTE: keep messages ASCII-only (cmd splits non-ASCII bytes in some code pages).
REM  2026-09-22 fix (double-click did nothing: "The syntax of the command is
REM    incorrect." and the script died before starting anything). Two cmd
REM    parsing bugs:
REM    1) the --deploy detection used  if "%ARGS%"=="%ARGS:--deploy=%"  on the
REM       same line as %ZYROBOT_DEPLOY% with nested quotes -> cmd's percent
REM       parser mangled it into  if ""=="--deploy=\ZyBin\..."  (unbalanced
REM       quotes -> parse error). Replaced with a token loop (no %VAR:sub%).
REM    2) the port probe spawned an external shell one-liner containing
REM       'if($c){...}'; the ')' broke block parsing. Replaced with netstat.
REM ============================================================
chcp 65001 >nul
setlocal enabledelayedexpansion
cd /d "%~dp0"

REM Robot connects to THIS controller: py config.py reads these env vars (default 17200 = py controller).
if not defined ROBOT_CTRL_HOST set "ROBOT_CTRL_HOST=127.0.0.1"
if not defined ROBOT_CTRL_PORT set "ROBOT_CTRL_PORT=27200"

REM Restore engine: re-dispatch tasks lost across a controller restart (production launcher uses this too).
if not defined CTRL_AUTO_RESTORE set "CTRL_AUTO_RESTORE=1"

REM Robot deploy copy dedicated to this controller (avoids fighting the py controller over one instance).
REM 2026-09-22: the hard-coded default path (F:\ZyBin\xm\...\single_robot_zy) is gone - that path does
REM   not exist on other machines, and passing a missing --deploy made the controller log errors.
REM   Now: ZYROBOT_DEPLOY=<dir> is passed as --deploy ONLY when the directory really exists;
REM   otherwise the flag is omitted (built-in default deploy dir is used) and a hint is printed.
REM   Per-run override still works: run_ctrlcenter.bat --deploy <dir>
REM   Recommended deploy copy (with robot: deploy\single_robot_zy - account range neutralized,
REM   ctrl_server_port=27200, accounts pushed by this controller via robot_manage add).
REM   Portable fallback (relative, keeps this machine's production copy in use without a
REM   hard-coded absolute path): the sibling reference project's dedicated copy single_robot_zy.
if not defined ZYROBOT_DEPLOY if exist "%~dp0..\xm\2d-xiyou-server\robot\deploy\single_robot_zy" set "ZYROBOT_DEPLOY=%~dp0..\xm\2d-xiyou-server\robot\deploy\single_robot_zy"
set "ARGS=%*"
set "DEPLOY_ARG="
set "HASDEPLOY="
if defined ARGS (
    for %%A in (%ARGS%) do (
        set "TOK=%%~A"
        if /i "!TOK!"=="--deploy" set "HASDEPLOY=1"
        if /i "!TOK:~0,9!"=="--deploy=" set "HASDEPLOY=1"
    )
)
if defined HASDEPLOY goto deploy_done
if not defined ZYROBOT_DEPLOY goto deploy_none
if "!ZYROBOT_DEPLOY:~-1!"=="\" set "ZYROBOT_DEPLOY=!ZYROBOT_DEPLOY:~0,-1!"
if not exist "!ZYROBOT_DEPLOY!" goto deploy_missing
set "DEPLOY_ARG=--deploy "%ZYROBOT_DEPLOY%""
echo [INFO] robot deploy dir: %ZYROBOT_DEPLOY%
goto deploy_done
:deploy_missing
echo [WARN] ZYROBOT_DEPLOY=%ZYROBOT_DEPLOY% not found - starting WITHOUT --deploy.
echo        panel still works; robot process is external (set ZYROBOT_DEPLOY to a real deploy dir).
goto deploy_done
:deploy_none
echo [INFO] ZYROBOT_DEPLOY not set - starting WITHOUT --deploy (built-in default deploy dir).
goto deploy_done
:deploy_done
if /i "%~1"=="--restart" (
    echo [RESTART] killing old zyctrlcenter.exe ...
    taskkill /IM zyctrlcenter.exe /F >nul 2>nul
    ping -n 2 127.0.0.1 >nul
    for /f "tokens=1,*" %%A in ("%*") do set "ARGS=%%B"
)

if "%ARGS%"=="" (
    echo [INFO] checking ports 27200 / 28082 ...
    for %%P in (27200 28082) do (
        set "OWNER="
        for /f "tokens=5" %%O in ('netstat -ano ^| findstr /c:"LISTENING" ^| findstr /c:":%%P "') do if not defined OWNER set "OWNER=%%O"
        if defined OWNER (
            echo [ERROR] port %%P is already in use by pid !OWNER! - a controller is probably already running.
            echo         Starting a second one cannot bind the control channel,
            echo         and the panel would show "channel not connected".
            echo         * just open the panel: http://127.0.0.1:28082/
            echo         * restart:            run_ctrlcenter.bat --restart
            echo         * run another one:    run_ctrlcenter.bat --web-port 28083 --ctrl-port 27201 --data-dir data_2
            echo.
            echo         If no zyctrlcenter.exe shows in Task Manager, the port is held by a
            echo         dying socket - wait ~30 seconds and run this file again.
            pause
            exit /b 1
        )
    )
)

if not defined GOEXE (
    if exist "C:\Program Files\Go\bin\go.exe" set "GOEXE=C:\Program Files\Go\bin\go.exe"
)
if not defined GOEXE (
    for /f "delims=" %%P in ('where go 2^>nul') do if not defined GOEXE set "GOEXE=%%P"
)
if not defined GOEXE (
    echo [ERROR] go.exe not found. Install Go or set GOEXE to the full path.
    pause
    exit /b 1
)

echo [BUILD] building zyctrlcenter.exe ...
"%GOEXE%" build -o zyctrlcenter.exe .
if errorlevel 1 (
    echo [ERROR] build failed
    pause
    exit /b 1
)

echo [RUN] starting controller ...
echo       panel:   http://127.0.0.1:28082/
echo       channel: 127.0.0.1:%ROBOT_CTRL_PORT%  (robot connects here; --auto-robot exports these two env vars)
echo       keep this window open while the controller runs; closing it stops the controller.
zyctrlcenter.exe %DEPLOY_ARG% %ARGS%
set "RC=%ERRORLEVEL%"
echo.
echo [EXIT] zyctrlcenter.exe returned code %RC% - the controller is no longer running.
echo        check logs\ctrlcenter_YYYYMMDD.log for the reason, then run this file again.
pause
endlocal

@echo off
REM ============================================================
REM  zyCtrlcenter one-click launcher (Windows)
REM  1) build the Vue panel  (web/dist, only when missing)
REM  2) build the controller (go build -o zyctrlcenter.exe .)
REM  3) run it (HTTP API :28082 / control channel :27200; panel served by the controller)
REM
REM  Usage:
REM    start.bat                    build panel if needed + build + run
REM    start.bat --skip-web         skip the panel build (use existing web/dist)
REM    start.bat --force-web        rebuild the panel even if web/dist exists
REM    start.bat --restart          kill a running zyctrlcenter.exe first
REM    start.bat --auto-robot ...   any other args are passed to zyctrlcenter.exe
REM
REM  Env:
REM    ZYROBOT_DEPLOY=<dir>       robot deploy dir; passed as --deploy ONLY when it exists
REM                               (do not add a trailing backslash)
REM    ZYCTRL_SKIP_WEB=1 / ZYCTRL_FORCE_WEB=1   same as the flags above
REM
REM  Panel after start: http://127.0.0.1:28082/
REM  Frontend dev mode: cd web ^&^& npm install ^&^& npm run dev  (http://localhost:5273)
REM
REM  NOTE: keep this file ASCII-only. cmd decodes the batch file with the console code
REM  page and multi-byte (UTF-8) bytes can swallow quotes/bytes, breaking the parser
REM  mid-line (same bug documented in run_ctrlcenter.bat).
REM ============================================================
chcp 65001 >nul
setlocal enabledelayedexpansion
cd /d "%~dp0"

REM Robot process (started with --auto-robot) reads these in its config.py; without them it
REM falls back to the py controller port 17200. Keep in sync with --ctrl-port (default 27200).
if not defined ROBOT_CTRL_HOST set "ROBOT_CTRL_HOST=127.0.0.1"
if not defined ROBOT_CTRL_PORT set "ROBOT_CTRL_PORT=27200"

set "SKIP_WEB="
set "FORCE_WEB="
set "RESTART="
set "SHOWHELP="
set "ARGS="

REM ---- parse args one by one (shift loop: avoids nested-quote parsing traps) ----
:parse_args
if "%~1"=="" goto args_done
if /i "%~1"=="-h" set "SHOWHELP=1"
if /i "%~1"=="-h" shift
if /i "%~1"=="-h" goto parse_args
if /i "%~1"=="--help" set "SHOWHELP=1"
if /i "%~1"=="--help" shift
if /i "%~1"=="--help" goto parse_args
if /i "%~1"=="--skip-web" set "SKIP_WEB=1"
if /i "%~1"=="--skip-web" shift
if /i "%~1"=="--skip-web" goto parse_args
if /i "%~1"=="--force-web" set "FORCE_WEB=1"
if /i "%~1"=="--force-web" shift
if /i "%~1"=="--force-web" goto parse_args
if /i "%~1"=="--restart" set "RESTART=1"
if /i "%~1"=="--restart" shift
if /i "%~1"=="--restart" goto parse_args
set "ARGS=%ARGS% %1"
shift
goto parse_args

:args_done
if defined ZYCTRL_SKIP_WEB set "SKIP_WEB=1"
if defined ZYCTRL_FORCE_WEB set "FORCE_WEB=1"

if defined SHOWHELP (
    echo Usage: start.bat [--skip-web] [--force-web] [--restart] [extra args for zyctrlcenter.exe]
    echo   --skip-web    skip the Vue panel build
    echo   --force-web   rebuild the Vue panel
    echo   --restart     kill a running zyctrlcenter.exe first
    echo   ZYROBOT_DEPLOY=dir   robot deploy dir, passed as --deploy only when it exists
    echo Panel: http://127.0.0.1:28082/
    exit /b 0
)

echo [INFO] zyCtrlcenter launcher, project dir: %CD%

if not exist "go.mod" (
    echo [ERROR] go.mod not found - run this script from the project root.
    pause
    exit /b 1
)

REM ---- tool check: go ----
if not defined GOEXE (
    if exist "C:\Program Files\Go\bin\go.exe" set "GOEXE=C:\Program Files\Go\bin\go.exe"
)
if not defined GOEXE (
    for /f "delims=" %%P in ('where go 2^>nul') do if not defined GOEXE set "GOEXE=%%P"
)
if not defined GOEXE (
    echo [ERROR] go.exe not found, cannot build the controller.
    echo         install Go 1.22+: https://go.dev/dl/
    echo         or set GOEXE to the full path of go.exe
    pause
    exit /b 1
)
echo [INFO] go: %GOEXE%
if not defined SKIP_WEB (
    if not defined NPMEXE (
        for /f "delims=" %%P in ('where npm.cmd 2^>nul') do if not defined NPMEXE set "NPMEXE=%%P"
    )
    if not defined NPMEXE (
        for /f "delims=" %%P in ('where npm 2^>nul') do if not defined NPMEXE set "NPMEXE=%%P"
    )
    if not defined NPMEXE (
        echo [ERROR] npm not found, cannot build the panel.
        echo         install Node.js LTS with npm: https://nodejs.org/
        echo         or skip the panel: start.bat --skip-web
        pause
        exit /b 1
    )
    echo [INFO] npm: !NPMEXE!
)

REM ---- 1) Vue panel ----
if defined SKIP_WEB goto web_done
if not exist "web\package.json" (
    echo [ERROR] web\package.json not found. Use start.bat --skip-web to run the controller only.
    pause
    exit /b 1
)
if defined FORCE_WEB goto web_build
if exist "web\dist\index.html" (
    echo [WEB] web\dist already exists, skipping panel build. Use --force-web to rebuild.
    goto web_done
)
:web_build
pushd web
if not exist "node_modules" (
    echo [WEB 1/2] npm install ... first run downloads dependencies, it may take a while.
    call "%NPMEXE%" install --no-audit --no-fund
    if errorlevel 1 (
        echo [ERROR] npm install failed: check network/proxy, or run it manually inside web\.
        popd
        pause
        exit /b 1
    )
)
echo [WEB 2/2] npm run build ...
call "%NPMEXE%" run build
if errorlevel 1 (
    echo [ERROR] npm run build failed. Fix the panel build, or run the controller with --skip-web.
    echo         hint: after pulling new frontend deps, run "npm install" inside web\ first.
    popd
    pause
    exit /b 1
)
popd
if not exist "web\dist\index.html" (
    echo [ERROR] build finished but web\dist\index.html is missing.
    pause
    exit /b 1
)
echo [WEB] panel built: web\dist
:web_done

REM ---- 2) build the controller ----
echo [GO] go build -o zyctrlcenter.exe .
call "%GOEXE%" build -o zyctrlcenter.exe .
if errorlevel 1 (
    echo [ERROR] go build failed.
    pause
    exit /b 1
)
echo [GO] built: zyctrlcenter.exe

REM ---- 3) --deploy only when the directory really exists (no hard-coded paths) ----
REM Portable fallback (relative): the sibling reference project's deploy copy dedicated to this
REM controller (single_robot_zy). On a fresh clone this does not exist, so nothing is passed.
if not defined ZYROBOT_DEPLOY if exist "%~dp0..\xm\2d-xiyou-server\robot\deploy\single_robot_zy" set "ZYROBOT_DEPLOY=%~dp0..\xm\2d-xiyou-server\robot\deploy\single_robot_zy"
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
echo [WARN] ZYROBOT_DEPLOY=%ZYROBOT_DEPLOY% does not exist - starting WITHOUT --deploy.
echo        panel works; start the robot yourself and point it to this control channel.
goto deploy_done
:deploy_none
echo [INFO] ZYROBOT_DEPLOY not set - starting WITHOUT --deploy, robot process is external.
goto deploy_done
:deploy_done

if defined RESTART (
    echo [RESTART] killing running zyctrlcenter.exe ...
    taskkill /IM zyctrlcenter.exe /F >nul 2>nul
    ping -n 2 127.0.0.1 >nul
)

REM port check only when no extra args are given (same rule as run_ctrlcenter.bat)
if not "%ARGS%"=="" goto run
echo [INFO] checking ports 27200 / 28082 ...
for %%P in (27200 28082) do (
    set "OWNER="
    for /f "tokens=5" %%O in ('netstat -ano ^| findstr /c:"LISTENING" ^| findstr /c:":%%P "') do if not defined OWNER set "OWNER=%%O"
    if defined OWNER (
        echo [ERROR] port %%P is already in use by pid !OWNER! - a controller is probably running.
        echo         open the panel: http://127.0.0.1:28082/
        echo         restart:        start.bat --restart
        echo         another one:    start.bat --web-port 28083 --ctrl-port 27201 --data-dir data_2
        pause
        exit /b 1
    )
)

:run
echo.
echo [RUN] starting controller ... keep this window open; closing it stops the controller.
echo       panel:   http://127.0.0.1:28082/     (change with --web-port)
echo       channel: 127.0.0.1:27200             (robot connects here; change with --ctrl-port)
echo       logs:    logs\ctrlcenter_YYYYMMDD.log
echo.
zyctrlcenter.exe %DEPLOY_ARG% %ARGS%
set "RC=%ERRORLEVEL%"
echo.
echo [EXIT] zyctrlcenter.exe returned %RC% - the controller stopped.
echo        usual cause: port already in use, or a bad flag. See logs\ctrlcenter_YYYYMMDD.log
pause
endlocal

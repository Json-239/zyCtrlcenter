@echo off
REM ============================================================
REM  Warm-restart runner (2026-10-27). Same as _run_ctrl_console.bat
REM  but WITHOUT --auto-robot: keep the running robot process untouched.
REM  stderr -> logs\stderr.log (silent-death forensics). ASCII+CRLF only.
REM ============================================================
cd /d F:\ZyBin\zyCtrlcenter
"F:\ZyBin\zyCtrlcenter\zyctrlcenter.exe" --deploy "F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy" 2>> "F:\ZyBin\zyCtrlcenter\logs\stderr.log"
echo [exit] %ERRORLEVEL% %date% %time% >> "F:\ZyBin\ZyCtrlcenter\logs\exit_codes.log"

@echo off
REM ============================================================
REM  Console runner for zyctrlcenter (2026-10-23 P0 forensics).
REM  Captures stderr (Go panic / runtime fatal / deadlock dump) to
REM  logs\stderr.log so silent deaths leave evidence. ASCII+CRLF only.
REM ============================================================
cd /d F:\ZyBin\zyCtrlcenter
"F:\ZyBin\zyCtrlcenter\zyctrlcenter.exe" --deploy "F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy" --auto-robot 2>> "F:\ZyBin\zyCtrlcenter\logs\stderr.log"
echo [exit] %ERRORLEVEL% %date% %time% >> "F:\ZyBin\ZyCtrlcenter\logs\exit_codes.log"

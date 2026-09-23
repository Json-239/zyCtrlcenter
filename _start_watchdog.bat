@echo off
REM ============================================================
REM  Standalone watchdog starter (detached via task scheduler).
REM  Why a .bat: task-scheduler runs with a minimal PATH; call the
REM  interpreter by absolute path so "python" is always found.
REM  Keep this file ASCII-only (cmd code-page rules, see _start_detached.bat).
REM ============================================================
cd /d F:\ZyBin\zyctrlcenter
"D:\Program Files\python.exe" F:\ZyBin\zyctrlcenter\tools\ctrlcenter_watchdog.py

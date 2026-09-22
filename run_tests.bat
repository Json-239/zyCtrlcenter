@echo off
REM ============================================================
REM  一键运行全部测试（Go 版测试框架，规范见 docs/04-测试/测试规范.md）
REM  - 步骤：静态检查（go vet）→ 全量测试 → 竞态检查（需 CGO/gcc，可选）
REM  - 退出码 0 = 全绿（vet / 测试 / 竞态 任一失败都返回非零）
REM  - 要留档/复现（含链数据闸门与用例数校验）：go run ./tools/test_report
REM ============================================================
chcp 65001 >nul
setlocal
cd /d "%~dp0"

if not defined GOEXE (
    if exist "C:\Program Files\Go\bin\go.exe" set "GOEXE=C:\Program Files\Go\bin\go.exe"
)
if not defined GOEXE (
    for /f "delims=" %%P in ('where go 2^>nul') do if not defined GOEXE set "GOEXE=%%P"
)
if not defined GOEXE (
    echo [ERROR] 未找到 go.exe，请安装 Go 或设置 GOEXE=go.exe完整路径
    pause
    exit /b 1
)

echo [TEST 1/3] 静态检查 go vet ./...
"%GOEXE%" vet ./...
set VET_RC=%errorlevel%

echo.
echo [TEST 2/3] 全量测试 go test ./test/... -count=1
"%GOEXE%" test ./test/... -count=1
set TEST_RC=%errorlevel%

echo.
set RACE_RC=0
where gcc >nul 2>nul
if errorlevel 1 goto skip_race
echo [TEST 3/3] 竞态检查 go test ./test/... -race ...
set CGO_ENABLED=1
"%GOEXE%" test ./test/... -race -count=1
set RACE_RC=%errorlevel%
goto race_done

:skip_race
echo [TEST 3/3] 跳过竞态检查：未找到 gcc，-race 需要 CGO 工具链；安装 mingw-w64 后自动启用

:race_done
echo.
set ALL_RC=0
if not "%VET_RC%"=="0" set ALL_RC=1
if not "%TEST_RC%"=="0" set ALL_RC=1
if not "%RACE_RC%"=="0" set ALL_RC=1
if %ALL_RC%==0 (
    echo [RESULT] 全部通过
    echo [TIP] 需要可复现的测试报告（含链数据闸门/用例数校验）：go run ./tools/test_report
) else (
    echo [RESULT] 存在失败 vet=%VET_RC% test=%TEST_RC% race=%RACE_RC%
)
endlocal & exit /b %ALL_RC%

@echo off
setlocal
set PATH=C:\Program Files\Go\bin;%PATH%
cd /d "%~dp0"

echo === Building GitHub Accelerator (GUI mode, no console) ===
go build -ldflags="-H windowsgui" -o github-accelerator.exe .
if %ERRORLEVEL% equ 0 (
    echo ✅ Build success: github-accelerator.exe
) else (
    echo ❌ Build failed
    exit /b 1
)

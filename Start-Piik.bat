@echo off
rem Starts the Piik App with its native capture helper and public-link tunnel.
rem Usage: Start-Piik.bat          normal start (opens the launcher in your browser)
rem        Start-Piik.bat debug    also writes diagnostics to logs\
setlocal
set "ROOT=%~dp0"
set "APP=%ROOT%build\dist\piik-app.exe"
set "CAPTURE=%ROOT%runtime\native\piik-capture.exe"
set "TUNNEL=%ROOT%runtime\tunnel\cloudflared.exe"

for %%F in ("%APP%" "%CAPTURE%" "%ROOT%runtime\native\piik-capture-bin.exe" "%TUNNEL%") do (
  if not exist "%%~F" (
    echo Missing %%~F
    pause
    exit /b 1
  )
)

rem Close a previous Piik App and its helpers so the new one starts cleanly.
taskkill /F /T /IM piik-app.exe >nul 2>&1
taskkill /F /IM piik-capture.exe >nul 2>&1
taskkill /F /IM piik-capture-bin.exe >nul 2>&1
rem A killed App can leave its tunnel running; stop only this folder's copy.
powershell -NoProfile -Command "Get-Process cloudflared -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq '%TUNNEL%' } | Stop-Process -Force" >nul 2>&1

set "EXTRA="
if /I "%~1"=="debug" set EXTRA=--debug --log-dir "%ROOT%logs"

echo Starting Piik App... (close this window to stop it)
"%APP%" --capture-process "%CAPTURE%" --tunnel-process "%TUNNEL%" %EXTRA%
echo Piik App exited with code %ERRORLEVEL%.
pause

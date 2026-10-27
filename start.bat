@echo off
cd /d %~dp0
start "" poolmate.exe
timeout /t 2 >nul
start "" http://127.0.0.1:7864/

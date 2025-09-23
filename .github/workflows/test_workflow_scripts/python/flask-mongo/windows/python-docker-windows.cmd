@echo off
REM Windows variant using Git Bash via bash.exe. Requires Docker Desktop on Windows with Linux containers.
REM This script is intended to be executed with a Bash shell step in GitHub Actions (shell: bash),
REM but kept here for parity; it calls into bash-compatible commands using Git for Windows' bash
REM when run locally.

REM Delegate to a bash-compatible inline script to avoid duplicating logic.
"%SystemRoot%\System32\bash.exe" -lc "cd $(pwd)/samples-python/flask-mongo && bash ./../../.github/workflows/test_workflow_scripts/python-docker-windows.sh"

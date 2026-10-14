# Tests for go-retry.ps1's Invoke-GoWithRetry and for go-dedup's
# Invoke-DockerBuildWithRetry (golang/go-dedup/golang-docker-windows.ps1): the
# windows lanes' half of the shared fetch rule (fetch.sh, go-retry.sh). Plain
# PowerShell, no Pester, like .github/scripts/windows' tests:
#
#   pwsh -NoProfile -File .github/workflows/test_workflow_scripts/go-retry.tests.ps1
#
# `go` and `docker` are fakes on PATH that record each call and fail as a case
# says; Start-Sleep is shadowed by a function that records the wait instead of
# sleeping. Nothing touches the network or a real daemon.
$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$script:failures = 0
function Check([bool]$cond, [string]$name, [string]$detail = '') {
  if ($cond) { Write-Host "  PASS  $name" } else { Write-Host "  FAIL  $name $detail"; $script:failures++ }
}

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("go-retry-tests-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
$savedPath = $env:PATH
$savedProxy = $env:GOPROXY
$env:PATH = "$tmp$([System.IO.Path]::PathSeparator)$env:PATH"

# fake NAME FAILURES MESSAGE: an executable NAME on PATH that appends
# "<args>|GOPROXY=<value>" to NAME.calls and fails its first FAILURES calls,
# printing MESSAGE.
function New-Fake([string]$name, [int]$failures, [string]$message) {
  $calls = Join-Path $tmp "$name.calls"
  Remove-Item -Force -ErrorAction SilentlyContinue $calls
  $body = @"
#!/usr/bin/env bash
n=`$(( `$(wc -l 2>/dev/null <"$calls" || echo 0) + 1 ))
echo "`$*|GOPROXY=`${GOPROXY-<unset>}" >>"$calls"
if [ "`$n" -le $failures ]; then echo '$message' >&2; exit 1; fi
exit 0
"@
  $path = Join-Path $tmp $name
  Set-Content -Path $path -Value $body -NoNewline
  & chmod +x $path
  return $calls
}
function Lines([string]$path) { if (Test-Path $path) { @(Get-Content $path) } else { @() } }

$script:sleeps = @()
function Start-Sleep { param([int]$Seconds) $script:sleeps += $Seconds }

$reset = 'go: example.com/m@v1.0.0: read "https://proxy.golang.org/x.zip": stream error: stream ID 7; INTERNAL_ERROR; received from peer'
$masked = 'go: example.com/m@v1.0.0: invalid version: git ls-remote -q --end-of-options https://example.com/m: exit status 128'
$compile = './main.go:3:1: syntax error: non-declaration statement outside function body'

. (Join-Path $here 'go-retry.ps1')

try {
  Write-Host "Invoke-GoWithRetry:"
  $env:GOPROXY = $null
  $calls = New-Fake 'go' 2 $reset
  $script:sleeps = @()
  $okRun = Invoke-GoWithRetry -GoArgs @('mod', 'download') -What 'go mod download' -Quiet
  $c = @(Lines $calls)
  Check ($okRun -and $c.Count -eq 3) "a reset stream is retried until it clears" "($($c.Count) calls)"
  Check (($c | Where-Object { $_ -like '*|GOPROXY=https://proxy.golang.org|direct' }).Count -eq 3) "every attempt goes through GOPROXY=https://proxy.golang.org|direct" ($c -join '; ')
  Check (-not $env:GOPROXY) "the caller's (unset) GOPROXY is restored after the call" "GOPROXY=$env:GOPROXY"
  Check (($script:sleeps -join ' ') -eq '10 20') "waits attempt x 10 s between attempts" "sleeps=$($script:sleeps -join ' ')"

  $calls = New-Fake 'go' 2 $masked
  $okRun = Invoke-GoWithRetry -GoArgs @('mod', 'tidy') -Quiet
  Check ($okRun -and @(Lines $calls).Count -eq 3) "an origin error the pipe reports in place of the proxy's is retried"

  $calls = New-Fake 'go' 9 $compile
  $okRun = Invoke-GoWithRetry -GoArgs @('build', './...') -Quiet
  Check ((-not $okRun) -and @(Lines $calls).Count -eq 1) "a compile error is not retried"

  $calls = New-Fake 'go' 99 $reset
  $script:sleeps = @()
  $okRun = Invoke-GoWithRetry -GoArgs @('mod', 'download') -Quiet
  Check ((-not $okRun) -and @(Lines $calls).Count -eq 5) "a persistent transient failure stops after exactly 5 attempts" "($(@(Lines $calls).Count) calls)"
  Check (($script:sleeps -join ' ') -eq '10 20 30 40') "waits 10 20 30 40, none after the last" "sleeps=$($script:sleeps -join ' ')"

  $env:GOPROXY = 'off'
  $calls = New-Fake 'go' 0 ''
  $null = Invoke-GoWithRetry -GoArgs @('mod', 'download') -Quiet
  Check (@(Lines $calls)[0] -like '*|GOPROXY=off') "a GOPROXY the caller set is kept" ("calls: " + (@(Lines $calls) -join '; '))
  Check ($env:GOPROXY -eq 'off') "and is still set after the call"
  $env:GOPROXY = $null

  Write-Host "Invoke-DockerBuildWithRetry (golang/go-dedup/golang-docker-windows.ps1):"
  # The function as the script defines it, taken from its syntax tree so the
  # script's other top-level code does not run.
  $dedup = Join-Path $here 'golang/go-dedup/golang-docker-windows.ps1'
  $ast = [System.Management.Automation.Language.Parser]::ParseFile($dedup, [ref]$null, [ref]$null)
  $fn = $ast.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Invoke-DockerBuildWithRetry' }, $true)
  Check ($null -ne $fn) "the script defines Invoke-DockerBuildWithRetry"
  . ([scriptblock]::Create($fn.Extent.Text))

  $calls = New-Fake 'docker' 2 'go: github.com/bytedance/sonic@v1.11.6: read "https://proxy.golang.org/x.zip": stream error: stream ID 175; INTERNAL_ERROR; received from peer'
  $script:sleeps = @()
  Invoke-DockerBuildWithRetry
  Check (@(Lines $calls).Count -eq 3) "a module fetch reset inside the build is retried until it clears (keploy#4791)"
  Check (($script:sleeps -join ' ') -eq '10 20') "waits attempt x 10 s" "sleeps=$($script:sleeps -join ' ')"

  $calls = New-Fake 'docker' 1 "ERROR: ReadTimeoutError(""HTTPSConnectionPool(host='files.pythonhosted.org', port=443): Read timed out."")"
  Invoke-DockerBuildWithRetry
  Check (@(Lines $calls).Count -eq 2) "a pip download timing out inside the build is retried"

  $calls = New-Fake 'docker' 9 $compile
  $threw = $false
  try { Invoke-DockerBuildWithRetry } catch { $threw = $true }
  Check ($threw -and @(Lines $calls).Count -eq 1) "a compile error throws on the first attempt"

  $calls = New-Fake 'docker' 1 'toomanyrequests: You have reached your pull rate limit.'
  Invoke-DockerBuildWithRetry
  Check (@(Lines $calls).Count -eq 2) "a Docker Hub rate limit is still retried"

  $calls = New-Fake 'docker' 99 'i/o timeout'
  $threw = $false
  try { Invoke-DockerBuildWithRetry } catch { $threw = $true }
  Check ($threw -and @(Lines $calls).Count -eq 5) "a fetch failure that never clears throws after exactly 5 attempts" "($(@(Lines $calls).Count) calls)"
}
finally {
  $env:PATH = $savedPath
  $env:GOPROXY = $savedProxy
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

if ($script:failures -gt 0) { Write-Host "go-retry.tests.ps1: $script:failures failure(s)"; exit 1 }
Write-Host "go-retry.tests.ps1: all passed"

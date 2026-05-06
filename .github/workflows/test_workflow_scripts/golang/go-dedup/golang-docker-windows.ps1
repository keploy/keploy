<#
  PowerShell test runner for Keploy (Windows) - go-dedup sample

  - Synchronous (PID-controlled) record phase; no background jobs
  - Cleans keploy dirs/files up-front
  - Generates keploy.yml and adds noise filter for current_time
  - Sends a fixed set of HTTP calls to generate tests
  - Kills entire process tree (keploy + docker compose) to avoid hangs
  - Runs replay and validates the report
#>

$ErrorActionPreference = 'Stop'

# --- Resolve Keploy binaries (defaults for local dev) ---
$defaultKeploy = 'C:\Users\offic\Downloads\keploy_win\keploy.exe'
if (-not $env:RECORD_BIN) { $env:RECORD_BIN = $defaultKeploy }
if (-not $env:REPLAY_BIN) { $env:REPLAY_BIN = $defaultKeploy }

# Ensure USERPROFILE (needed for docker volume mounts inside keploy)
if (-not $env:USERPROFILE -or $env:USERPROFILE -eq '') {
  $candidate = "$env:HOMEDRIVE$env:HOMEPATH"
  if ($candidate -and $candidate -ne '') { $env:USERPROFILE = $candidate }
}

# Create Keploy config/home so docker doesn’t fall back to NetworkService
try {
  if ($env:USERPROFILE -and $env:USERPROFILE -ne '') {
    $keployCfg = Join-Path $env:USERPROFILE ".keploy-config"
    $keployHome = Join-Path $env:USERPROFILE ".keploy"
    New-Item -ItemType Directory -Path $keployCfg -Force -ErrorAction SilentlyContinue | Out-Null
    New-Item -ItemType Directory -Path $keployHome -Force -ErrorAction SilentlyContinue | Out-Null
  }
} catch {}

# Parameterize the application's base URL
$env:APP_BASE_URL = if ($env:APP_BASE_URL) { $env:APP_BASE_URL } else { 'http://localhost:8080' }

Write-Host "Using RECORD_BIN = $env:RECORD_BIN"
Write-Host "Using REPLAY_BIN = $env:REPLAY_BIN"
Write-Host "Using APP_BASE_URL = $env:APP_BASE_URL"

# --- Helper: runner work path ---
function Get-RunnerWorkPath {
  if ($env:GITHUB_WORKSPACE) { return $env:GITHUB_WORKSPACE }
  return (Get-Location).Path
}

# --- Helper: remove keploy dirs robustly ---
function Remove-KeployDirs {
  param([string[]]$Candidates)

  # Stop any leftover keploy processes so files aren't locked
  try {
    Get-Process -ErrorAction SilentlyContinue |
      Where-Object {
        $_.ProcessName -in @('keploy','keploy-record','keploy-replay') -or
        $_.Path -like '*\keploy*.exe' -or
        $_.CommandLine -like '*keploy*'
      } |
      Sort-Object StartTime -Descending |
      ForEach-Object {
        taskkill /PID $_.Id /T /F | Out-Null 2>$null
      }
  } catch {}

  foreach ($p in $Candidates) {
    if (-not $p -or -not (Test-Path -LiteralPath $p)) { continue }
    Write-Host "Cleaning keploy directory: $p"
    try {
      cmd /c "attrib -R -S -H `"$p\*`" /S /D" 2>$null | Out-Null
      Remove-Item -LiteralPath $p -Recurse -Force -ErrorAction Stop
    } catch {
      Write-Warning "Remove-Item failed for $p, using rmdir fallback: $_"
      cmd /c "rmdir /S /Q `"$p`"" 2>$null | Out-Null
    }
  }
}

# --- Find a free port and generate a random container name, then patch docker-compose ---
# NOTE: We deliberately do NOT start the search at 8080. On Windows
# Docker Desktop runners, port 8080 is very commonly held by
# vpnkit / wsl's port-forwarder or by a prior test's container whose
# TIME_WAIT hasn't cleared, so Find-FreePort's TcpListener probe
# reports 8080 as free (briefly) but the subsequent `docker compose
# up` fails to publish on it with "address already in use". Start
# from a random high port in the IANA dynamic range (49152-65000
# with a small margin below the ephemeral cap) and iterate upwards
# from there. Keeps the host port well clear of anything a
# developer tool or Docker Desktop typically reserves.
function Find-FreePort {
  param([int]$start)
  if (-not $start) { $start = Get-Random -Minimum 49152 -Maximum 60000 }
  for ($p = $start; $p -le 65535; $p++) {
    try {
      $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, $p)
      $listener.Start()
      $listener.Stop()
      return $p
    } catch {
      continue
    }
  }
  throw 'No free TCP port found'
}

$appPort = Find-FreePort
$id = ([guid]::NewGuid()).ToString().Split('-')[0]
$containerName = "dedup-go-$id"

$dcFile = Join-Path (Get-Location) 'docker-compose.yml'
if (Test-Path $dcFile) {
  Write-Host "Patching docker-compose.yml: host port 8080 -> $appPort (container-side stays 8080) and container_name 'dedup-go' -> '$containerName'"
  $dc = Get-Content -Path $dcFile -Raw -ErrorAction Stop

  # Patch ONLY the host side of the port mapping; the container
  # side must remain 8080 because the Go sample hardcodes
  # `router.Run(":8080")`.
  #
  # Previous implementation used a backreference regex
  # `(?m)("?)8080:8080\1` to handle both quoted and unquoted forms
  # in one go. On Windows PowerShell 5.1 that pattern
  # intermittently produced `- <appPort>:8080"` (leading quote
  # stripped, trailing quote kept), which docker-compose parsed as
  # `containerPort: 8080"` and rejected with `invalid
  # containerPort: 8080"` — the canonical symptom on
  # keploy/keploy#4076 run 24629876059. The sample committed at
  # github.com/keploy/samples-go/go-dedup uses the double-quoted
  # short form exclusively, so a literal substitution is both
  # simpler and provably correct. If the sample ever introduces a
  # different port form we'll fail loudly below rather than
  # silently writing malformed YAML.
  $dcPatched = $dc.Replace('"8080:8080"', ('"' + $appPort + ':8080"'))
  $dcPatched = $dcPatched.Replace('dedup-go', $containerName)

  $expectedPortFragment = ('"' + $appPort + ':8080"')
  if ($dcPatched -notlike "*$expectedPortFragment*") {
    Write-Host "---- docker-compose.yml (unpatched) ----"
    Write-Host $dc
    Write-Host "----------------------------------------"
    throw "Port substitution did not take effect. The base docker-compose.yml does not contain the expected ""8080:8080"" literal — bailing out rather than running the record phase against an unpatched file."
  }

  # Dump the patched YAML so any future containerPort/hostPort
  # diagnostic lands with the exact text docker-compose parsed.
  Write-Host "---- patched docker-compose.yml ----"
  Write-Host $dcPatched
  Write-Host "------------------------------------"

  Set-Content -Path $dcFile -Value $dcPatched -Encoding UTF8
} else {
  Write-Warning "docker-compose.yml not found at $dcFile; continuing without patching."
}

# Update APP_BASE_URL to use the chosen port
$env:APP_BASE_URL = "http://localhost:$appPort"
Write-Host "Chosen app port: $appPort"
Write-Host "Chosen container name: $containerName"

Write-Host "Building Docker image(s) with docker compose..."
docker compose build

# --- Clean previous keploy outputs ---
$candidates = @(".\keploy")
if ($env:GITHUB_WORKSPACE) { $candidates += (Join-Path $env:GITHUB_WORKSPACE 'keploy') }
Remove-KeployDirs -Candidates $candidates
Remove-Item -LiteralPath ".\keploy.yml" -Force -ErrorAction SilentlyContinue
Write-Host "Pre-clean complete."

# --- Generate keploy.yml and add noise for timestamp endpoint ---
Write-Host "Generating keploy config..."
& $env:RECORD_BIN config --generate

$configFile = ".\keploy.yml"
if (-not (Test-Path $configFile)) { throw "Config file '$configFile' not found after generation." }

# Add noise to ignore current_time in body (go-dedup /timestamp endpoint)
(Get-Content $configFile -Raw) -replace 'global:\s*\{\s*\}', 'global: {"body": {"current_time":[]}}' |
  Set-Content -Path $configFile -Encoding UTF8
Write-Host "Updated global noise in keploy.yml to ignore 'current_time'."

# --- Helpers for record flow ---
function Test-RecordingComplete {
  param(
    [string]$root,
    [int]$idx,
    [int]$minFiles = 7,
    [int]$minBytes = 100
  )
  $p1 = Join-Path $root "keploy\test-set-$idx\tests"
  $p2 = ".\keploy\test-set-$idx\tests"
  foreach ($p in @($p1,$p2)) {
    if (-not (Test-Path $p)) { continue }
    $files = Get-ChildItem -Path $p -Filter "*.yaml" -ErrorAction SilentlyContinue
    if (-not $files) { continue }
    $valid = ($files | Where-Object { $_.Length -ge $minBytes }).Count
    if ($valid -ge $minFiles) { return $true }
  }
  return $false
}

function Kill-Tree {
  param([int]$ProcessId)
  try {
    Write-Host "Stopping Keploy process tree (root PID $ProcessId)…"
    cmd /c "taskkill /PID $ProcessId /T /F" | Out-Null
  } catch {
    Write-Warning "taskkill failed for $ProcessId : $_"
  }
}

# =========================
# ========== RECORD =======
# =========================
$logPath = "$containerName.record.txt"
$errLogPath = "$containerName.record.err.txt"
$expectedTestSetIndex = 0
$workDir = Get-RunnerWorkPath
$base = $env:APP_BASE_URL

# Configure image for recording (optional override via DOCKER_IMAGE_RECORD)
# $env:KEPLOY_DOCKER_IMAGE = if ($env:DOCKER_IMAGE_RECORD) { $env:DOCKER_IMAGE_RECORD } else { 'keploy:record' }

# 1. Correctly quote the docker command for Keploy
$dockerCmd = "docker compose up"
$recArgs = @(
  'record',
  '-c', '"docker compose up"',
  '--container-name', $containerName,
  '--generate-github-actions=false'
)

Write-Host "Starting keploy record (expecting test-set-$expectedTestIndex)…"
Write-Host "Executing: $env:RECORD_BIN $($recArgs -join ' ')"

# 2. Start Keploy using Start-Process and tail the log in a background job.
# This gives us a reliable PID and still allows log streaming via a job.
$argList = $recArgs -join ' '
# Start a background job that tails the log file so Sync-Logs can Receive-Job from it
Write-Host "Starting keploy record via Start-Process..."
$proc = Start-Process -FilePath $env:RECORD_BIN -ArgumentList $argList -NoNewWindow -RedirectStandardOutput $logPath -RedirectStandardError $errLogPath -PassThru
Write-Host "Keploy record started, PID: $($proc.Id)"
$REC_PID = $proc.Id

# Start a background job that tails both stdout and stderr log files so Sync-Logs can Receive-Job from it
$recJob = Start-Job -ScriptBlock { param($out,$err) Get-Content -Path @($out,$err) -Wait -ErrorAction SilentlyContinue } -ArgumentList $logPath,$errLogPath

Write-Host "`n=========================================================="
Write-Host "Dumping full Keploy Record Logs from files: '$logPath' and '$errLogPath'"
Write-Host "=========================================================="
Get-Content -Path @($logPath,$errLogPath) -ErrorAction SilentlyContinue
Write-Host "=========================================================="

# This function will print any new logs from the background job
function Sync-Logs {
    param($job)
    try {
        Receive-Job -Job $job -ErrorAction SilentlyContinue
    } catch {}
}

# Wait for app readiness.
#
# Previous implementation broke on the first 200 response and sent
# traffic immediately — and on Windows Docker Desktop that first
# 200 was unreliable: the port publish can briefly route to a
# vpnkit/wsl stub that returns 200 before the in-container Go
# binary is actually listening, so 4 seconds later the real
# traffic got TCP RST ("Unable to connect to the remote server")
# and 0 tests were recorded. See keploy/keploy#4076 run
# 24629542035/job/72014589521 for the canonical symptom.
#
# The fix has three parts:
#   1. Require $stabilityCount consecutive 200s before declaring
#      ready (instead of a single 200), so a flap can't race.
#   2. Sleep $settleSec after the probe succeeds, giving keploy's
#      recording proxy/interception layer time to fully wire up.
#   3. Wrap each traffic request in Invoke-WithRetry so a transient
#      connection-refused on the first request no longer aborts
#      the remaining five — the prior `try { req1; req2; ... } catch`
#      swallowed the first failure and sent $sent=0 requests.
Write-Host "Waiting for app to respond on $base/hello/keploy …"
$deadline       = (Get-Date).AddMinutes(5)
$stabilityCount = 3
$settleSec      = 5
$okStreak       = 0
# Loop termination is gated on (a) the time deadline and (b) the
# keploy record process still being alive — NOT on $recJob.State,
# because the tail Start-Job can transiently flip out of 'Running'
# on Windows when Get-Content -Wait briefly races a log rotation
# and exits, which made the probe bail after ~5 seconds on run
# 24630134970. Using the keploy PID we already know about is the
# authoritative signal: if keploy exited, there's no point waiting.
do {
  Sync-Logs -job $recJob
  $keployAlive = $true
  if ($REC_PID) {
    $keployAlive = [bool](Get-Process -Id $REC_PID -ErrorAction SilentlyContinue)
    if (-not $keployAlive) { break }
  }
  try {
    $r = Invoke-WebRequest -Method GET -Uri "$base/hello/keploy" -TimeoutSec 5 -UseBasicParsing -ErrorAction Stop
    if ($r.StatusCode -eq 200) {
      $okStreak++
      if ($okStreak -ge $stabilityCount) { break }
      Start-Sleep -Seconds 1
    } else {
      $okStreak = 0
    }
  } catch {
    $okStreak = 0
    Start-Sleep 3
  }
} while ((Get-Date) -lt $deadline)

if ($okStreak -lt $stabilityCount) {
  Write-Warning "App readiness probe did not reach ${stabilityCount} consecutive 200s before deadline; continuing with traffic anyway — downstream record validation will surface the failure."
} else {
  Write-Host "App readiness confirmed ($stabilityCount consecutive 200s). Settling ${settleSec}s before sending traffic…"
  Start-Sleep -Seconds $settleSec
}

# Per-request retry wrapper. Each API call is independent —
# retries isolate a transient connection-refused blip from
# aborting the whole record phase, which is what the old
# single-try/catch flow did.
function Invoke-WithRetry {
  param(
    [scriptblock]$Action,
    [string]$Name,
    [int]$MaxAttempts = 5,
    [int]$InitialSleepSec = 2
  )
  for ($i = 1; $i -le $MaxAttempts; $i++) {
    try {
      & $Action | Out-Null
      return $true
    } catch {
      if ($i -ge $MaxAttempts) {
        Write-Warning "Request '$Name' failed after $MaxAttempts attempts: $_"
        return $false
      }
      $sleep = [int]($InitialSleepSec * [Math]::Pow(1.5, $i - 1))
      Write-Host "Request '$Name' attempt $i failed ($_); retrying in ${sleep}s…"
      Start-Sleep -Seconds $sleep
    }
  }
  return $false
}

# Send traffic to generate tests.
Write-Host "Sending HTTP requests to generate tests…"
$sent = 0
if (Invoke-WithRetry -Name "GET hello/Keploy"   -Action { Invoke-RestMethod -Method GET    -Uri "$base/hello/Keploy" -TimeoutSec 30 })                                                                      { $sent++ }
if (Invoke-WithRetry -Name "POST user"          -Action { Invoke-RestMethod -Method POST   -Uri "$base/user"         -Body (@{name="John Doe";email="john@keploy.io"}        | ConvertTo-Json) -ContentType "application/json" -TimeoutSec 30 }) { $sent++ }
if (Invoke-WithRetry -Name "PUT item/item123"   -Action { Invoke-RestMethod -Method PUT    -Uri "$base/item/item123" -Body (@{id="item123";name="Updated Item";price=99.99} | ConvertTo-Json) -ContentType "application/json" -TimeoutSec 30 }) { $sent++ }
if (Invoke-WithRetry -Name "GET products"       -Action { Invoke-RestMethod -Method GET    -Uri "$base/products"     -TimeoutSec 30 })                                                                                                           { $sent++ }
if (Invoke-WithRetry -Name "DELETE products/…"  -Action { Invoke-RestMethod -Method DELETE -Uri "$base/products/prod001" -TimeoutSec 30 })                                                                                                       { $sent++ }
if (Invoke-WithRetry -Name "GET api/v2/users"   -Action { Invoke-RestMethod -Method GET    -Uri "$base/api/v2/users" -TimeoutSec 30 })                                                                                                           { $sent++ }

Write-Host "Sent $sent request(s). Waiting for tests to flush to disk…"

$pollUntil = (Get-Date).AddSeconds(60)
do {
  Sync-Logs -job $recJob # <-- Print logs while waiting
  if (Test-RecordingComplete -root $workDir -idx $expectedTestSetIndex -minFiles 7) { break }
  Start-Sleep 3
} while ((Get-Date) -lt $pollUntil -and $recJob.State -eq 'Running')



# If we don't have a PID (some environments), try a short polling fallback to find the process
if (-not $REC_PID -or $REC_PID -eq 0) {
  $exeName = [System.IO.Path]::GetFileNameWithoutExtension($env:RECORD_BIN)
  for ($i = 0; $i -lt 10; $i++) {
    Start-Sleep -Seconds 1
    $p = Get-Process -Name $exeName -ErrorAction SilentlyContinue | Sort-Object StartTime -Descending | Select-Object -First 1
    if ($p) { $REC_PID = $p.Id; break }
  }
}

if ($REC_PID -and $REC_PID -ne 0) {
    Write-Host "Found Keploy PID: $REC_PID"
    Write-Host "Killing keploy process tree..."
    # /T: Kill the process and any child processes started by it (tree kill)
    # /F: Forcefully terminate
    cmd /c "taskkill /PID $REC_PID /T /F" 2>$null | Out-Null
} else {
    Write-Host "Keploy record process not found."
}


# Stop Keploy (and docker compose) deterministically
# We get the process ID of the actual keploy.exe process started by the job
# $keployProcessId = (Get-Job -Id $recJob.Id).ChildJobs[0].ProcessId
# Kill-Tree -ProcessId $keployProcessId
# Stop-Job $recJob
# Remove-Job $recJob

# Verify recording
$testSetPath = ".\keploy\test-set-$expectedTestSetIndex\tests"
if (-not (Test-Path $testSetPath)) { Write-Error "Test directory not found at $testSetPath"; exit 1 }
$testCount = (Get-ChildItem -Path $testSetPath -Filter "*.yaml").Count
if ($testCount -eq 0) { Write-Error "No test files were created. Review the full logs in the file '$logPath'"; exit 1 }

Write-Host "Successfully recorded $testCount test file(s) in test-set-$expectedTestSetIndex"

# =========================
# ========== REPLAY =======
# =========================

# Bring down services before test mode (preserve volumes)
Write-Host "Shutting down docker compose services before test mode (preserving volumes)…"
docker compose down
Start-Sleep -Seconds 5

$testContainer = $containerName
$testLog = "$testContainer.test.txt"

# Configure image for replay (optional override via DOCKER_IMAGE_REPLAY)
# $env:KEPLOY_DOCKER_IMAGE = if ($env:DOCKER_IMAGE_REPLAY) { $env:DOCKER_IMAGE_REPLAY } else { 'keploy:replay' }

$testArgs = @(
  'test',
  '-c', 'docker compose up',
  '--container-name', $testContainer,
  '--api-timeout', '60',
  '--delay', '30',
  # Record captured the in-container URL (`localhost:8080`) because
  # that's the port the Gin app actually binds. We patched
  # docker-compose to publish the container on `$appPort`:8080 to
  # avoid colliding with vpnkit/wsl on Windows runners. keploy's
  # --port flag rewrites the testcase destination at replay time,
  # so the recorded localhost:8080 lands on localhost:$appPort
  # where docker is actually publishing — which is the only
  # reachable listener on the host. Without this flag replay
  # dials :8080 and gets TCP RST (run 24630753752, tests 1-9).
  '--port', "$appPort",
  '--generate-github-actions=false'
)

Write-Host "Starting keploy replay…"
Write-Host "Executing: $env:REPLAY_BIN $($testArgs -join ' ')"
# keploy-replay.exe invokes `docker compose up` under the hood and
# docker writes lifecycle events ("Container … Recreate", "Network
# … Removing", …) to stderr even on a successful run. With
# $ErrorActionPreference = 'Stop' set at the top of this script,
# piping those stderr records through Tee-Object promotes them to
# a terminating NativeCommandError and the script dies at line
# 421 before we ever get to read the report file. Seen on run
# 24630134970/job/72016179815 where replay actually completed but
# the script exited 1 because docker printed a benign "Recreate"
# line. Flip to 'Continue' just for the replay invocation; the
# real pass/fail signal is the report YAML we validate below.
$prevEap = $ErrorActionPreference
$ErrorActionPreference = 'Continue'
try {
  & $env:REPLAY_BIN @testArgs 2>&1 | Tee-Object -FilePath $testLog
  $replayExit = $LASTEXITCODE
} finally {
  $ErrorActionPreference = $prevEap
}
Write-Host "Replay exited with code $replayExit"

# Validate replay report
$report = ".\keploy\reports\test-run-0\test-set-0-report.yaml"
if (-not (Test-Path $report)) {
  Write-Error "Missing report file: $report (replay exit $replayExit)"
  Get-Content $testLog -ErrorAction SilentlyContinue | Select-Object -Last 200
  exit 1
}

$line = Select-String -Path $report -Pattern 'status:' | Select-Object -First 1
$status = ($line.ToString() -replace '.*status:\s*', '').Trim()
Write-Host "Test status for test-set-0: $status"

if ($status -ne 'PASSED') {
  Write-Error "Replay failed (status: $status). See logs below:"
  Get-Content $testLog -ErrorAction SilentlyContinue | Select-Object -Last 200
  exit 1
}

Write-Host "All tests passed successfully!"
exit 0

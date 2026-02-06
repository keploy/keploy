
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
$defaultKeploy = if ($env:GITHUB_WORKSPACE) { Join-Path $env:GITHUB_WORKSPACE 'bin\keploy.exe' } else { '.\keploy.exe' }
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
    $files = Get-ChildItem -Path $p -Filter "test-*.yaml" -ErrorAction SilentlyContinue
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
$containerName = "dedup-go"
$logPath = "$containerName.record.txt"
$expectedTestSetIndex = 0
$workDir = Get-RunnerWorkPath
$base = $env:APP_BASE_URL

# Configure image for recording (optional override via DOCKER_IMAGE_RECORD)
$env:KEPLOY_DOCKER_IMAGE = if ($env:DOCKER_IMAGE_RECORD) { $env:DOCKER_IMAGE_RECORD } else { 'keploy:record' }

# 1. Correctly quote the docker command for Keploy
$dockerCmd = "go run main.go"
$recArgs = @(
  'record',
  '-c', $dockerCmd,
  '--generate-github-actions=false'
)

Write-Host "Starting keploy record (expecting test-set-$expectedTestSetIndex)…"
Write-Host "Executing: $env:RECORD_BIN $($recArgs -join ' ')"

# 2. Start Keploy in a background job.
# This allows the script to continue while Keploy runs.
# We use Tee-Object to send output to BOTH the log file and the job's output stream.
$recJob = Start-Job -ScriptBlock {
    # Use the $using: scope to access variables from the parent script
    & $using:env:RECORD_BIN @($using:recArgs) 2>&1 | Tee-Object -FilePath $using:logPath
}

Write-Host "`n=========================================================="
Write-Host "Dumping full Keploy Record Logs from file: '$logPath'"
Write-Host "=========================================================="
Get-Content -Path $logPath -ErrorAction SilentlyContinue
Write-Host "=========================================================="

# This function will print any new logs from the background job
function Sync-Logs {
    param($job)
    try {
        Receive-Job -Job $job -ErrorAction SilentlyContinue
    } catch {}
}

# Wait for app readiness
Write-Host "Waiting for app to respond on $base/hello/keploy …"
$deadline = (Get-Date).AddMinutes(5)
$ready = $false
do {
  Sync-Logs -job $recJob # <-- Print Keploy logs here
  try {
    $r = Invoke-WebRequest -Method GET -Uri "$base/hello/keploy" -TimeoutSec 5 -UseBasicParsing -ErrorAction Stop
    if ($r.StatusCode -eq 200) { break }
  } catch { Start-Sleep 3 }
} while ((Get-Date) -lt $deadline -and $recJob.State -eq 'Running')

# Send traffic to generate tests
Write-Host "Sending HTTP requests to generate tests…"
$sent = 0
try {
  Invoke-RestMethod -Method GET    -Uri "$base/hello/Keploy";                                                           $sent++
  Invoke-RestMethod -Method POST   -Uri "$base/user"           -Body (@{name="John Doe";email="john@keploy.io"} | ConvertTo-Json) -ContentType "application/json"; $sent++
  Invoke-RestMethod -Method PUT    -Uri "$base/item/item123"   -Body (@{id="item123";name="Updated Item";price=99.99} | ConvertTo-Json) -ContentType "application/json"; $sent++
  Invoke-RestMethod -Method GET    -Uri "$base/products";                                                                     $sent++
  Invoke-RestMethod -Method DELETE -Uri "$base/products/prod001";                                                            $sent++
  Invoke-RestMethod -Method GET    -Uri "$base/api/v2/users";                                                                $sent++
} catch { Write-Warning "A request failed: $_" }

Write-Host "Sent $sent request(s). Waiting for tests to flush to disk…"

$pollUntil = (Get-Date).AddSeconds(60)
do {
  Sync-Logs -job $recJob # <-- Print logs while waiting
  if (Test-RecordingComplete -root $workDir -idx $expectedTestSetIndex -minFiles 7) { break }
  Start-Sleep 3
} while ((Get-Date) -lt $pollUntil -and $recJob.State -eq 'Running')


$REC_PROC = Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
  Where-Object {
    ($_.CommandLine -and ($_.CommandLine -match 'keploy.*record' -or $_.CommandLine -match 'keploy-record' -or $_.CommandLine -match 'keploy(\.exe)?')) -or
    ($_.Name -and $_.Name -match 'keploy')
  } |
  Select-Object -First 1

Write-Host "Value of REC_PROC: $REC_PROC"

$REC_PID = if ($REC_PROC) { $REC_PROC.ProcessId } else { $null }

if ($REC_PID -and $REC_PID -ne 0) {
    Write-Host "Found Keploy PID: $REC_PID"
    Write-Host "Killing keploy process tree..."
    cmd /c "taskkill /PID $REC_PID /T /F" 2>$null | Out-Null
    cmd /c "taskkill /PID $REC_PROC /T /F" 2>$null | Out-Null
} else {
    Write-Host "Keploy record process not found. Dumping candidate processes containing 'keploy' in CommandLine:"
    Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
      Where-Object { $_.CommandLine -and ($_.CommandLine -match 'keploy') } |
      Select-Object ProcessId, Name, @{Name='Cmd';Expression={$_.CommandLine}} |
      ForEach-Object { Write-Host "$($_.ProcessId)  $($_.Name)  $($_.Cmd)" }
}


# Stop Keploy (and docker compose) deterministically
# We get the process ID of the actual keploy.exe process started by the job
# $keployProcessId = (Get-Job -Id $recJob.Id).ChildJobs[0].ProcessId
# Kill-Tree -ProcessId $keployProcessId
# Stop-Job $recJob
# Remove-Job $recJob

# Verify recording
$testSetPath = ".\keploy\test-set-$expectedTestSetIndex\tests"
if (-not (Test-Path $testSetPath)) { Write-Error "Test directory not found at $testSetPath"; Get-Content .\keploy_agent.log; exit 1 }
$testCount = (Get-ChildItem -Path $testSetPath -Filter "test-*.yaml").Count
if ($testCount -eq 0) { Write-Error "No test files were created. Review the full logs in the file '$logPath'"; exit 1 }

Write-Host "Successfully recorded $testCount test file(s) in test-set-$expectedTestSetIndex"

# =========================
# ========== REPLAY =======
# =========================

# Bring down services before test mode (preserve volumes)
Write-Host "Shutting down docker compose services before test mode (preserving volumes)…"
docker compose down
Start-Sleep -Seconds 5

$testContainer = "dedup-go"
$testLog = "$testContainer.test.txt"

# Configure image for replay (optional override via DOCKER_IMAGE_REPLAY)
$env:KEPLOY_DOCKER_IMAGE = if ($env:DOCKER_IMAGE_REPLAY) { $env:DOCKER_IMAGE_REPLAY } else { 'keploy:replay' }

$testArgs = @(
  'test',
  '-c', 'go run main.go',
  '--api-timeout', '60',
  '--delay', '30',
  # '--port', '8080',
  '--generate-github-actions=false'
)

Write-Host "Starting keploy replay…"
Write-Host "Executing: $env:REPLAY_BIN $($testArgs -join ' ')"
& $env:REPLAY_BIN @testArgs 2>&1 | Tee-Object -FilePath $testLog

# Validate replay report
$report = ".\keploy\reports\test-run-0\test-set-0-report.yaml"
if (-not (Test-Path $report)) {
  Write-Error "Missing report file: $report"
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
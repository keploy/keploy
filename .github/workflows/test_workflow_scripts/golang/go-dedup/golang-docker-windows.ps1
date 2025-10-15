
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

# --- Helper: cleanup log files and handles ---
function Remove-LogFiles {
  param([string[]]$LogFiles)
  
  foreach ($logFile in $LogFiles) {
    if (-not $logFile -or -not (Test-Path -LiteralPath $logFile)) { continue }
    Write-Host "Cleaning log file: $logFile"
    
    # Wait a bit for file handles to be released
    Start-Sleep -Seconds 2
    
    # Try multiple approaches to remove the file
    $attempts = 0
    $maxAttempts = 5
    while ($attempts -lt $maxAttempts) {
      try {
        Remove-Item -LiteralPath $logFile -Force -ErrorAction Stop
        Write-Host "Successfully removed $logFile"
        break
      } catch {
        $attempts++
        if ($attempts -eq $maxAttempts) {
          Write-Warning "Could not remove $logFile after $maxAttempts attempts: $_"
        } else {
          Write-Host "Attempt $attempts to remove $logFile failed, retrying in 2 seconds..."
          Start-Sleep -Seconds 2
        }
      }
    }
  }
}

# --- Build docker images from compose ---
Write-Host "Building Docker image(s) with docker compose..."
docker compose build

# --- Clean previous keploy outputs ---
$candidates = @(".\keploy")
if ($env:GITHUB_WORKSPACE) { $candidates += (Join-Path $env:GITHUB_WORKSPACE 'keploy') }
Remove-KeployDirs -Candidates $candidates
Remove-Item -LiteralPath ".\keploy.yml" -Force -ErrorAction SilentlyContinue

# Clean up any existing log files
$logFiles = @(".\keploy-logs.txt", ".\dedup-go.record.txt", ".\dedup-go.test.txt")
Remove-LogFiles -LogFiles $logFiles

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

# --- Helper: comprehensive process cleanup ---
function Stop-AllKeployProcesses {
  Write-Host "Performing comprehensive Keploy process cleanup..."
  
  # First, try to find and kill all Keploy-related processes
  try {
    $keployProcesses = Get-Process -ErrorAction SilentlyContinue |
      Where-Object {
        $_.ProcessName -in @('keploy','keploy-record','keploy-replay') -or
        $_.Path -like '*\keploy*.exe' -or
        ($_.CommandLine -and $_.CommandLine -like '*keploy*')
      }
    
    if ($keployProcesses) {
      Write-Host "Found $($keployProcesses.Count) Keploy process(es) to terminate"
      $keployProcesses | Sort-Object StartTime -Descending | ForEach-Object {
        Write-Host "Terminating process: $($_.ProcessName) (PID: $($_.Id))"
        try {
          taskkill /PID $_.Id /T /F | Out-Null 2>$null
        } catch {
          Write-Warning "Failed to kill process $($_.Id): $_"
        }
      }
    } else {
      Write-Host "No Keploy processes found to terminate"
    }
  } catch {
    Write-Warning "Error during process enumeration: $_"
  }
  
  # Wait a moment for processes to fully terminate
  Start-Sleep -Seconds 3
  
  # Double-check and force kill any remaining processes
  try {
    $remainingProcesses = Get-Process -ErrorAction SilentlyContinue |
      Where-Object {
        $_.ProcessName -in @('keploy','keploy-record','keploy-replay') -or
        $_.Path -like '*\keploy*.exe'
      }
    
    if ($remainingProcesses) {
      Write-Host "Found $($remainingProcesses.Count) remaining Keploy process(es), force killing..."
      $remainingProcesses | ForEach-Object {
        try {
          Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue
        } catch {}
      }
    }
  } catch {}
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
$dockerCmd = "docker compose up"
$recArgs = @(
  'record',
  '-c', $dockerCmd,
  '--container-name', $containerName,
  '--generate-github-actions=false',
  '--debug'
)

Write-Host "Starting keploy record (expecting test-set-$expectedTestIndex)…"
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


# Stop all Keploy processes comprehensively
Stop-AllKeployProcesses

# Clean up the background job
try {
  if ($recJob -and $recJob.State -eq 'Running') {
    Stop-Job $recJob -ErrorAction SilentlyContinue
  }
  if ($recJob) {
    Remove-Job $recJob -Force -ErrorAction SilentlyContinue
  }
} catch {
  Write-Warning "Error cleaning up background job: $_"
}

# Verify recording
$testSetPath = ".\keploy\test-set-$expectedTestSetIndex\tests"
if (-not (Test-Path $testSetPath)) { 
  Write-Error "Test directory not found at $testSetPath"
  Stop-AllKeployProcesses
  exit 1 
}
$testCount = (Get-ChildItem -Path $testSetPath -Filter "test-*.yaml").Count
if ($testCount -eq 0) { 
  Write-Error "No test files were created. Review the full logs in the file '$logPath'"
  Stop-AllKeployProcesses
  exit 1 
}

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
  '-c', 'docker compose up',
  '--container-name', $testContainer,
  '--api-timeout', '60',
  '--delay', '20',
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
  Stop-AllKeployProcesses
  exit 1
}

$line = Select-String -Path $report -Pattern 'status:' | Select-Object -First 1
$status = ($line.ToString() -replace '.*status:\s*', '').Trim()
Write-Host "Test status for test-set-0: $status"

if ($status -ne 'PASSED') {
  Write-Error "Replay failed (status: $status). See logs below:"
  Get-Content $testLog -ErrorAction SilentlyContinue | Select-Object -Last 200
  Stop-AllKeployProcesses
  exit 1
}

Write-Host "All tests passed successfully!"

# Final cleanup to prevent file handle issues
Write-Host "Performing final cleanup..."
Stop-AllKeployProcesses

# Clean up log files that might still have open handles
$finalLogFiles = @(".\keploy-logs.txt", $logPath, $testLog)
Remove-LogFiles -LogFiles $finalLogFiles

# Ensure docker compose is fully stopped
try {
  docker compose down --remove-orphans 2>$null | Out-Null
} catch {}

Write-Host "Cleanup complete."
exit 0

<#
  PowerShell test runner for Keploy (Windows) - http-pokeapi sample

  - Synchronous (PID-controlled) record phase using Start-Process
  - Cleans keploy dirs/files up-front
  - Generates keploy.yml
  - Sends a fixed set of HTTP calls to generate tests
  - Kills entire process tree (keploy + app) to avoid hangs
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

# Create Keploy config/home so docker doesn't fall back to NetworkService
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

# --- Generate keploy.yml ---
Write-Host "Generating keploy config..."
& $env:RECORD_BIN config --generate

$configFile = ".\keploy.yml"
if (-not (Test-Path $configFile)) { throw "Config file '$configFile' not found after generation." }

Write-Host "Keploy config generated."

# --- Helpers for record flow ---
function Test-RecordingComplete {
  param(
    [string]$root,
    [int]$idx,
    [int]$minFiles = 4,
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
    Write-Host "Stopping Keploy process tree (root PID $ProcessId)..."
    cmd /c "taskkill /PID $ProcessId /T /F" | Out-Null
  } catch {
    Write-Warning "taskkill failed for ${ProcessId}: $_"
  }
}

# =========================
# ========== RECORD =======
# =========================
$containerName = "http-pokeapi"
$workDir = Get-RunnerWorkPath

# Use absolute paths to avoid working directory issues
$logPath = Join-Path $workDir "$containerName.record.txt"
$expectedTestSetIndex = 0
$base = $env:APP_BASE_URL

# Configure image for recording (optional override via DOCKER_IMAGE_RECORD)
$env:KEPLOY_DOCKER_IMAGE = if ($env:DOCKER_IMAGE_RECORD) { $env:DOCKER_IMAGE_RECORD } else { 'keploy:record' }

# Build the command arguments
$appCmd = "go run ."
$recArgsString = "record -c `"$appCmd`" --generate-github-actions=false"

Write-Host "Starting keploy record (expecting test-set-$expectedTestSetIndex)..."
Write-Host "Executing: $env:RECORD_BIN $recArgsString"

# Start Keploy using Start-Process for reliable PID tracking
# This gives us the actual PID of the keploy process, not a job wrapper
$recProcess = Start-Process -FilePath $env:RECORD_BIN `
    -ArgumentList $recArgsString `
    -PassThru `
    -RedirectStandardOutput $logPath `
    -RedirectStandardError "$logPath.err" `
    -NoNewWindow

$REC_PID = $recProcess.Id
Write-Host "Started Keploy record process with PID: $REC_PID"

# Give Keploy a moment to start up
Start-Sleep -Seconds 5

# Function to print logs from the log file
function Show-Logs {
    param([string]$path)
    if (Test-Path $path) {
        Get-Content -Path $path -ErrorAction SilentlyContinue | Write-Host
    }
}

# Wait for app readiness - http-pokeapi has a 2-second sleep at startup
Write-Host "Waiting for app to respond on $base/api/greet..."
$deadline = (Get-Date).AddMinutes(5)
$ready = $false
do {
  # Check if the process is still running
  if ($recProcess.HasExited) {
    Write-Host "Keploy record process exited unexpectedly with exit code: $($recProcess.ExitCode)"
    Show-Logs -path $logPath
    Show-Logs -path "$logPath.err"
    throw "Keploy record process exited prematurely"
  }
  
  try {
    $r = Invoke-WebRequest -Method GET -Uri "$base/api/greet" -TimeoutSec 5 -UseBasicParsing -ErrorAction Stop
    if ($r.StatusCode -eq 200) { 
      Write-Host "Application is ready!"
      $ready = $true
      break 
    }
  } catch { Start-Sleep 3 }
} while ((Get-Date) -lt $deadline)

if (-not $ready -and -not $recProcess.HasExited) {
  Write-Warning "Application did not respond within deadline, but process still running. Continuing..."
}

# Send traffic to generate tests
Write-Host "Sending HTTP requests to generate tests..."
$sent = 0
try {
  # Test greet endpoint with different formats
  Invoke-RestMethod -Method GET -Uri "$base/api/greet"; $sent++
  Invoke-RestMethod -Method GET -Uri "$base/api/greet?format=xml"; $sent++
  Invoke-RestMethod -Method GET -Uri "$base/api/greet?format=html"; $sent++
  
  # Test locations endpoint
  Invoke-RestMethod -Method GET -Uri "$base/api/locations"; $sent++
  
  # Test pokemon endpoint (using a well-known pokemon name)
  Invoke-RestMethod -Method GET -Uri "$base/api/pokemon/pikachu"; $sent++
  Invoke-RestMethod -Method GET -Uri "$base/api/pokemon/charizard"; $sent++
  
} catch { Write-Warning "A request failed: $_" }

Write-Host "Sent $sent request(s). Waiting for tests to flush to disk..."

$pollUntil = (Get-Date).AddSeconds(60)
do {
  if (Test-RecordingComplete -root $workDir -idx $expectedTestSetIndex -minFiles 4) { break }
  Start-Sleep 3
} while ((Get-Date) -lt $pollUntil -and -not $recProcess.HasExited)

# Now kill the Keploy process tree using the reliable PID we captured
Write-Host "`n=========================================================="
Write-Host "Dumping Keploy Record Logs from file: '$logPath'"
Write-Host "=========================================================="
Show-Logs -path $logPath
if (Test-Path "$logPath.err") {
    Write-Host "`n--- STDERR ---"
    Show-Logs -path "$logPath.err"
}
Write-Host "=========================================================="

Write-Host "Killing Keploy process tree (PID: $REC_PID)..."
if (-not $recProcess.HasExited) {
    Kill-Tree -ProcessId $REC_PID
    # Wait for process to exit
    $recProcess.WaitForExit(10000)
}

# Also kill any remaining keploy-related processes
Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
  Where-Object {
    ($_.CommandLine -and ($_.CommandLine -match 'keploy.*record' -or $_.CommandLine -match 'keploy-record' -or $_.CommandLine -match 'keploy(\.exe)?')) -or
    ($_.Name -and $_.Name -match 'keploy')
  } |
  ForEach-Object {
    Write-Host "Also killing remaining process: $($_.ProcessId) - $($_.Name)"
    cmd /c "taskkill /PID $($_.ProcessId) /T /F" 2>$null | Out-Null
  }

# Verify recording
$testSetPath = ".\keploy\test-set-$expectedTestSetIndex\tests"
if (-not (Test-Path $testSetPath)) { 
    Write-Error "Test directory not found at $testSetPath"
    Get-Content .\keploy_agent.log -ErrorAction SilentlyContinue
    exit 1 
}
$testCount = (Get-ChildItem -Path $testSetPath -Filter "test-*.yaml").Count
if ($testCount -eq 0) { 
    Write-Error "No test files were created. Review the full logs in the file '$logPath'"
    exit 1 
}

Write-Host "Successfully recorded $testCount test file(s) in test-set-$expectedTestSetIndex"

# =========================
# ========== REPLAY =======
# =========================

# Give some time for processes to clean up
Start-Sleep -Seconds 5

$testContainer = "http-pokeapi"
$testLog = Join-Path $workDir "$testContainer.test.txt"

# Configure image for replay (optional override via DOCKER_IMAGE_REPLAY)
$env:KEPLOY_DOCKER_IMAGE = if ($env:DOCKER_IMAGE_REPLAY) { $env:DOCKER_IMAGE_REPLAY } else { 'keploy:replay' }

$testArgs = @(
  'test',
  '-c', 'go run .',
  '--api-timeout', '60',
  '--delay', '30',
  '--debug',
  # '--port', '8080',
  '--generate-github-actions=false'
)

Write-Host "Starting keploy replay..."
Write-Host "Executing: $env:REPLAY_BIN $($testArgs -join ' ')"
& $env:REPLAY_BIN @testArgs 2>&1 | Tee-Object -FilePath $testLog

# Validate replay report
$report = ".\keploy\reports\test-run-0\test-set-0-report.yaml"
if (-not (Test-Path $report)) {
  Write-Error "Missing report file: $report"
  Get-Content $testLog -ErrorAction SilentlyContinue | Select-Object -Last 200
  exit 1
}

$line = Select-String -Path $report -Pattern 'status: ' | Select-Object -First 1
$status = ($line.ToString() -replace '.*status:\s*', '').Trim()
Write-Host "Test status for test-set-0: $status"

if ($status -ne 'PASSED') {
  Write-Error "Replay failed (status: $status). See logs below:"
  Get-Content $testLog -ErrorAction SilentlyContinue | Select-Object -Last 200
  exit 1
}

Write-Host "All tests passed successfully!"
exit 0
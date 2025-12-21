<#
  PowerShell test runner for Keploy (Windows) - gin-mongo sample
  
  - Starts MongoDB container as a dependency
  - Runs Go app natively with Keploy record
  - Sends HTTP calls to generate tests
  - Runs replay and validates the report
#>

$ErrorActionPreference = 'Continue'

# --- Resolve Keploy binaries (defaults for local dev) ---
$defaultKeploy = if ($env:GITHUB_WORKSPACE) { Join-Path $env:GITHUB_WORKSPACE 'bin\keploy.exe' } else { '.\keploy.exe' }
if (-not $env:RECORD_BIN) { $env:RECORD_BIN = $defaultKeploy }
if (-not $env:REPLAY_BIN) { $env:REPLAY_BIN = $defaultKeploy }

# Ensure USERPROFILE (needed for docker volume mounts inside keploy)
if (-not $env:USERPROFILE -or $env:USERPROFILE -eq '') {
  $candidate = "$env:HOMEDRIVE$env:HOMEPATH"
  if ($candidate -and $candidate -ne '') { $env:USERPROFILE = $candidate }
}

# Create Keploy config/home directories
try {
  if ($env:USERPROFILE -and $env:USERPROFILE -ne '') {
    $keployCfg = Join-Path $env:USERPROFILE ".keploy-config"
    $keployHome = Join-Path $env:USERPROFILE ".keploy"
    New-Item -ItemType Directory -Path $keployCfg -Force -ErrorAction SilentlyContinue | Out-Null
    New-Item -ItemType Directory -Path $keployHome -Force -ErrorAction SilentlyContinue | Out-Null
  }
} catch {}

# Parameterize the application's base URL
$env:APP_BASE_URL = if ($env:APP_BASE_URL) { $env:APP_BASE_URL } else { 'http://127.0.0.1:8080' }
$MONGO_HOST = "127.0.0.1"
$MONGO_PORT = 27017

Write-Host "Using RECORD_BIN = $env:RECORD_BIN"
Write-Host "Using REPLAY_BIN = $env:REPLAY_BIN"
Write-Host "Using APP_BASE_URL = $env:APP_BASE_URL"

# --- Helper:  runner work path ---
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
      Write-Warning "Remove-Item failed for $p, using rmdir fallback:  $_"
      cmd /c "rmdir /S /Q `"$p`"" 2>$null | Out-Null
    }
  }
}

# --- Helper: wait for MongoDB to be ready ---
function Wait-ForMongo {
  param(
    [string]$MongoHost = "localhost",
    [int]$Port = 27017,
    [int]$TimeoutSeconds = 120
  )
  
  Write-Host "Waiting for MongoDB at ${MongoHost}:${Port}..."
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  
  while ((Get-Date) -lt $deadline) {
    try {
      $tcpClient = New-Object System.Net.Sockets.TcpClient
      $tcpClient.Connect($MongoHost, $Port)
      $tcpClient.Close()
      Write-Host "MongoDB is ready!"
      return $true
    } catch {
      Start-Sleep -Seconds 2
    }
  }
  
  Write-Error "MongoDB did not become ready within $TimeoutSeconds seconds"
  return $false
}

# --- Clean previous keploy outputs ---
$candidates = @(".\keploy")
if ($env:GITHUB_WORKSPACE) { $candidates += (Join-Path $env:GITHUB_WORKSPACE 'keploy') }
Remove-KeployDirs -Candidates $candidates
Remove-Item -LiteralPath ".\keploy.yml" -Force -ErrorAction SilentlyContinue
Write-Host "Pre-clean complete."

Write-Host "Starting MongoDB container..."
docker rm -f mongoDb 2>$null | Out-Null
docker run -d --name mongoDb -p 27017:27017 mongo:5.0
if ($LASTEXITCODE -ne 0) {
  Write-Error "Failed to start MongoDB container"
  exit 1
}

# Wait for MongoDB to be ready
if (-not (Wait-ForMongo -MongoHost $MONGO_HOST -Port $MONGO_PORT)) {
  Write-Error "MongoDB failed to start"
  docker logs mongoDb
  exit 1
}

# --- Generate keploy.yml and add noise for timestamp ---
Write-Host "Generating keploy config..."
$currentDir = (Get-Location).Path
# Convert backslashes to forward slashes for YAML compatibility
$currentDirYaml = $currentDir -replace '\\', '/'
Write-Host "Current directory: $currentDir"

# Generate config with explicit path to current directory
& $env:RECORD_BIN config --generate --path "."

$configFile = ".\keploy.yml"
if (-not (Test-Path $configFile)) { throw "Config file '$configFile' not found after generation." }

# Update the path in the config to use current directory explicitly (with forward slashes)
$configContent = Get-Content $configFile -Raw
$configContent = $configContent -replace 'path:\s*""', "path: `"$currentDirYaml/keploy`""
$configContent = $configContent -replace 'path:\s*"."', "path: `"$currentDirYaml/keploy`""

# Add noise to ignore 'ts' field in response body (timestamp from URL shortener)
$configContent = $configContent -replace 'global:\s*\{\s*\}', 'global:  {"body": {"ts": [], "error": []}}'

Set-Content -Path $configFile -Value $configContent -Encoding UTF8
Write-Host "Updated keploy.yml - path set to: $currentDirYaml/keploy"
Write-Host "Updated global noise in keploy.yml to ignore 'ts' and 'error'."

# --- Update main.go to use 127.0.0.1 instead of mongoDb hostname ---
Write-Host "Updating main.go to use 127.0.0.1 for MongoDB connection..."
$mainGoPath = ".\main.go"
if (Test-Path $mainGoPath) {
  $content = Get-Content $mainGoPath -Raw
  
  # REPLACE 'localhost' with '127.0.0.1' to avoid IPv6 issues on Windows
  $newContent = $content -replace 'mongo[Dd]b:\s*27017', '127.0.0.1:27017'
  
  Set-Content -Path $mainGoPath -Value $newContent -Encoding UTF8
  Write-Host "Updated MongoDB host in main.go to 127.0.0.1:27017"
}

# =========================
# ===== PRE-BUILD APP =====
# =========================
# Pre-build the Go application to avoid timeouts during Keploy recording
Write-Host "Pre-building Go application to avoid timeouts..."

# Download all dependencies first
go mod download
if ($LASTEXITCODE -ne 0) {
  Write-Error "Failed to download Go dependencies"
  exit 1
}

# Build the application binary
go build -o app.exe .
if ($LASTEXITCODE -ne 0) {
  Write-Error "Failed to build Go application"
  exit 1
}

# Validate and get Absolute Path
if (-not (Test-Path ".\app.exe")) {
  Write-Error "FATAL: app.exe was not found after build!"
  exit 1
}
$appAbsPath = (Resolve-Path ".\app.exe").Path
Write-Host "Build complete. App executable located at: $appAbsPath"
Write-Host ""

# --- Helpers for record flow ---
function Test-RecordingComplete {
  param(
    [string]$root,
    [int]$idx,
    [int]$minFiles = 2,
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
    Write-Warning "taskkill failed for $ProcessId :  $_"
  }
}


# =========================
# ========== RECORD =======
# =========================
$containerName = "gin-mongo"
$logPath = "$containerName.record.txt"
$expectedTestSetIndex = 0
$workDir = Get-RunnerWorkPath
$base = $env:APP_BASE_URL

# 1. Configure the command for Keploy (using absolute path to pre-built binary)
$goCmd = $appAbsPath
$recArgs = @(
  'record',
  '-c', $goCmd,
  '--generate-github-actions=false',
  '--debug'
)

Write-Host "Starting keploy record (expecting test-set-$expectedTestSetIndex)…"
Write-Host "Executing: $env:RECORD_BIN $($recArgs -join ' ')"

# 2. Start Keploy in a background job
$recJob = Start-Job -ScriptBlock {
    Set-Location $using:workDir
    & $using:env:RECORD_BIN @($using:recArgs) 2>&1 | Tee-Object -FilePath $using:logPath
}

# This function will print any new logs from the background job
function Sync-Logs {
    param($job)
    try {
        Receive-Job -Job $job -ErrorAction SilentlyContinue
    } catch {}
}

# Wait for app readiness without generating extra HTTP test cases
Write-Host "Waiting for app port to be reachable at $base …"
$deadline = (Get-Date).AddMinutes(5)
$baseUri = [Uri]$base
$probePort = if ($baseUri.Port -gt 0) { $baseUri.Port } else { 80 }
do {
  Sync-Logs -job $recJob
  try {
    $client = New-Object System.Net.Sockets.TcpClient
    $client.ReceiveTimeout = 2000
    $client.SendTimeout = 2000
    $client.Connect($baseUri.Host, $probePort)
    $client.Close()
    Write-Host "Application port is reachable."
    Start-Sleep -Seconds 2
    break
  } catch {
    Start-Sleep 3
  }
} while ((Get-Date) -lt $deadline -and $recJob.State -eq 'Running')

# Send traffic to generate tests
Write-Host "Sending HTTP requests to generate tests (using curl.exe)…"
$sent = 0

try {
  # 1. Create a shortened URL
  Write-Host "Sending POST to $base/url..."
  # We use curl.exe explicitly because Invoke-RestMethod was hanging
  $responseJson = cmd /c "curl.exe -s -X POST $base/url -d ""{\""url\"":\""https://google.com\""}"" -H ""Content-Type: application/json"""
  
  Write-Host "POST /url response: $responseJson"
  $sent++
  
  # Parse the JSON to get the short URL
  $jsonObj = $responseJson | ConvertFrom-Json
  $shortenedUrl = $jsonObj.url
  
  if ($shortenedUrl) {
    $shortPath = ($shortenedUrl -split '/')[-1]
    Write-Host "Shortened path: $shortPath"
    
    # 2. Try to redirect
    Write-Host "Sending GET to $base/$shortPath..."
    # curl -I checks headers (like redirects) without downloading body
    cmd /c "curl.exe -v $base/$shortPath 2>&1" | Out-Null
    $sent++
  }
  
  # 3. Create another shortened URL
  $responseJson2 = cmd /c "curl.exe -s -X POST $base/url -d ""{\""url\"":\""https://github.com/keploy\""}"" -H ""Content-Type: application/json"""
  Write-Host "POST /url response: $responseJson2"
  $sent++
  
  # 4. Test verify-email endpoint
  Write-Host "Sending GET to verify-email..."
  cmd /c "curl.exe -s $base/verify-email?email=test@example.com" | Out-Null
  $sent++

} catch { 
  Write-Warning "A request failed: $_" 
}

Write-Host "Sent $sent request(s). Waiting for tests to flush to disk…"

$pollUntil = (Get-Date).AddSeconds(60)
do {
  Sync-Logs -job $recJob
  if (Test-RecordingComplete -root $workDir -idx $expectedTestSetIndex -minFiles 2) { break }
  Start-Sleep 3
} while ((Get-Date) -lt $pollUntil -and $recJob.State -eq 'Running')

Write-Host "`n=========================================================="
Write-Host "Dumping full Keploy Record Logs from file:  '$logPath'"
Write-Host "=========================================================="
Get-Content -Path $logPath -ErrorAction SilentlyContinue
Write-Host "=========================================================="

# ==========================================================
# ROBUST CLEANUP: Kill whatever is holding Port 8080
# ==========================================================
Write-Host "Ensuring Port 8080 is free for Replay..."

# 1. Kill the specific process holding the port
$tcp = Get-NetTCPConnection -LocalPort 8080 -ErrorAction SilentlyContinue
if ($tcp) {
    $pid8080 = $tcp.OwningProcess
    Write-Host "Found process ID $pid8080 holding Port 8080. Killing it..."
    cmd /c "taskkill /PID $pid8080 /F /T" 2>$null | Out-Null
}

# 2. Cleanup any lingering Keploy agents
Get-Process keploy -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 2

# Verify recording - check both possible locations
$testSetPath1 = ".\keploy\test-set-$expectedTestSetIndex\tests"
$testSetPath2 = Join-Path $workDir "keploy\test-set-$expectedTestSetIndex\tests"

$testSetPath = $null
if (Test-Path $testSetPath1) {
  $testSetPath = $testSetPath1
  Write-Host "Found tests at: $testSetPath1"
} elseif (Test-Path $testSetPath2) {
  $testSetPath = $testSetPath2
  Write-Host "Found tests at: $testSetPath2"
} else {
  Write-Error "Test directory not found at either location:"
  Write-Error "  Location 1: $testSetPath1"  
  Write-Error "  Location 2: $testSetPath2"
  Get-Content .\keploy_agent.log -ErrorAction SilentlyContinue
  Get-Content $logPath -ErrorAction SilentlyContinue
  exit 1 
}

$testCount = (Get-ChildItem -Path $testSetPath -Filter "test-*.yaml").Count
if ($testCount -eq 0) { 
  Write-Error "No test files were created.  Review the full logs in the file '$logPath'"
  exit 1 
}

Write-Host "Successfully recorded $testCount test file(s) in test-set-$expectedTestSetIndex at $testSetPath"


# =========================
# ========== REPLAY =======
# =========================

# Note: MongoDB container stays running for replay
Write-Host "MongoDB container will remain running for replay phase..."
Start-Sleep -Seconds 5

$testLog = "$containerName.test.txt"

$testArgs = @(
  'test',
  '-c', $appAbsPath,
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

# Cleanup
Write-Host "Cleaning up MongoDB container..."
docker rm -f mongoDb 2>$null | Out-Null

exit 0
<#
  PowerShell test runner for Keploy (Windows) - gin-mongo sample
  
  - Starts MongoDB container as a dependency
  - Runs Go app natively with Keploy record
  - Sends HTTP calls to generate tests
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
$env:APP_BASE_URL = if ($env:APP_BASE_URL) { $env:APP_BASE_URL } else { 'http://localhost:8080' }
$MONGO_HOST = "localhost"
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

# --- Start MongoDB container ---
Write-Host "Starting MongoDB container..."
docker rm -f mongoDb 2>$null | Out-Null
docker run -d --name mongoDb -p 27017:27017 mongo:latest
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
& $env:RECORD_BIN config --generate

$configFile = ".\keploy.yml"
if (-not (Test-Path $configFile)) { throw "Config file '$configFile' not found after generation." }

# Add noise to ignore 'ts' field in response body (timestamp from URL shortener)
(Get-Content $configFile -Raw) -replace 'global:\s*\{\s*\}', 'global:  {"body": {"ts": []}}' |
  Set-Content -Path $configFile -Encoding UTF8
Write-Host "Updated global noise in keploy.yml to ignore 'ts'."

# --- Update main.go to use localhost instead of mongoDb hostname ---
Write-Host "Updating main.go to use localhost for MongoDB connection..."
$mainGoPath = ".\main.go"
if (Test-Path $mainGoPath) {
  (Get-Content $mainGoPath -Raw) -replace 'mongoDb: 27017', 'localhost:27017' |
    Set-Content -Path $mainGoPath -Encoding UTF8
  Write-Host "Updated MongoDB host in main.go"
}

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

# 1. Configure the go run command for Keploy
$goCmd = "go run ."
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
Write-Host "Sending HTTP requests to generate tests…"
$sent = 0

try {
  # 1. Create a shortened URL
  $body1 = @{url="https://google.com"} | ConvertTo-Json
  $response1 = Invoke-RestMethod -Method POST -Uri "$base/url" -Body $body1 -ContentType "application/json"
  Write-Host "POST /url response: $($response1 | ConvertTo-Json -Compress)"
  $sent++
  
  # Extract the shortened URL path from response
  $shortenedUrl = $response1.url
  if ($shortenedUrl) {
    $shortPath = ($shortenedUrl -split '/')[-1]
    Write-Host "Shortened path: $shortPath"
    
    # 2. Try to redirect (this will return 303, which is expected)
    try {
      $response2 = Invoke-WebRequest -Method GET -Uri "$base/$shortPath" -MaximumRedirection 0 -UseBasicParsing -ErrorAction SilentlyContinue
    } catch {
      # 303 redirect throws an exception in PowerShell, but that's the expected behavior
      Write-Host "GET /$shortPath returned redirect (expected)"
    }
    $sent++
  }
  
  # 3. Create another shortened URL
  $body3 = @{url="https://github.com/keploy"} | ConvertTo-Json
  $response3 = Invoke-RestMethod -Method POST -Uri "$base/url" -Body $body3 -ContentType "application/json"
  Write-Host "POST /url response: $($response3 | ConvertTo-Json -Compress)"
  $sent++
  
  # 4. Test verify-email endpoint (optional, may fail if DNS lookup fails)
  try {
    $response4 = Invoke-WebRequest -Method GET -Uri "$base/verify-email?email=test@example.com" -UseBasicParsing -ErrorAction SilentlyContinue
    Write-Host "GET /verify-email response received"
    $sent++
  } catch {
    Write-Warning "verify-email endpoint failed (expected in some environments): $_"
  }

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

# Find and kill Keploy process
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
} else {
    Write-Host "Keploy record process not found.  Dumping candidate processes containing 'keploy' in CommandLine:"
    Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
      Where-Object { $_.CommandLine -and ($_.CommandLine -match 'keploy') } |
      Select-Object ProcessId, Name, @{Name='Cmd';Expression={$_.CommandLine}} |
      ForEach-Object { Write-Host "$($_.ProcessId)  $($_.Name)  $($_.Cmd)" }
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
  Write-Error "No test files were created.  Review the full logs in the file '$logPath'"
  exit 1 
}

Write-Host "Successfully recorded $testCount test file(s) in test-set-$expectedTestSetIndex"

# =========================
# ========== REPLAY =======
# =========================

# Note: MongoDB container stays running for replay
Write-Host "MongoDB container will remain running for replay phase..."
Start-Sleep -Seconds 5

$testLog = "$containerName.test.txt"

$testArgs = @(
  'test',
  '-c', 'go run .',
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
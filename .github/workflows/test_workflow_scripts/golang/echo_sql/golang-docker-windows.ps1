<# 
  PowerShell test runner for Keploy (Windows).
  - Uses a simplified, more reliable recording loop.
  - Controls the keploy process directly instead of using a complex background job.
  - Corrected syntax for calling .NET static methods.
#>

$ErrorActionPreference = 'Stop'

# --- Resolve Keploy binaries (defaults for local dev) ---
$defaultKeploy = 'C:\Users\offic\Downloads\keploy_win\keploy.exe'
if (-not $env:RECORD_BIN) { $env:RECORD_BIN = $defaultKeploy }
if (-not $env:REPLAY_BIN) { $env:REPLAY_BIN = $defaultKeploy }

# Ensure USERPROFILE is set (needed for docker volume mounts inside keploy)
if (-not $env:USERPROFILE -or $env:USERPROFILE -eq '') {
  $candidate = "$env:HOMEDRIVE$env:HOMEPATH"
  if ($candidate -and $candidate -ne '') { $env:USERPROFILE = $candidate }
}

# Create keploy config/data dirs so docker doesn't fall back to NetworkService profile
try {
  if ($env:USERPROFILE -and $env:USERPROFILE -ne '') {
    $keployCfg = Join-Path $env:USERPROFILE ".keploy-config"
    $keployHome = Join-Path $env:USERPROFILE ".keploy"
    New-Item -ItemType Directory -Path $keployCfg -Force -ErrorAction SilentlyContinue | Out-Null
    New-Item -ItemType Directory -Path $keployHome -Force -ErrorAction SilentlyContinue | Out-Null
  }
} catch {}

# Optionally parameterize app URLs
# FIX: Changed health URL to a path that will respond (even with a 404) instead of a non-existent endpoint.
$env:APP_HEALTH_URL    = if ($env:APP_HEALTH_URL) { $env:APP_HEALTH_URL } else { 'http://localhost:8082/' }
$env:APP_POST_URL      = if ($env:APP_POST_URL) { $env:APP_POST_URL } else { 'http://localhost:8082/url' }

Write-Host "Using RECORD_BIN = $env:RECORD_BIN"
Write-Host "Using REPLAY_BIN = $env:REPLAY_BIN"
Write-Host "Using APP_HEALTH_URL = $env:APP_HEALTH_URL"
Write-Host "Using APP_POST_URL = $env:APP_POST_URL"

# --- Build Docker image(s) defined by compose ---
Write-Host "Building Docker image(s) with docker compose..."
docker compose build

# --- Clean previous keploy outputs ---
Write-Host "Cleaning .\keploy\ directory (if exists)..."
Remove-Item -LiteralPath ".\keploy" -Recurse -Force -ErrorAction SilentlyContinue

# --- Generate keploy.yml ---
Write-Host "Generating keploy config..."
& $env:RECORD_BIN config --generate

# --- Update global noise in keploy.yml ---
$configFile = ".\keploy.yml"
if (-not (Test-Path $configFile)) {
  throw "Config file '$configFile' not found after generation."
}
(Get-Content $configFile -Raw) -replace 'global:\s*\{\s*\}', 'global: {"body": {"ts":[]}}' |
  Set-Content -Path $configFile -Encoding UTF8
Write-Host "Updated global noise in keploy.yml"

# --- Record twice ---
for ($i = 1; $i -le 2; $i++) {
  $containerName = "echoApp"
  $logPath = "$containerName.record.$i.txt"
  $expectedTestSetIndex = $i - 1

  Write-Host "--- Starting Recording Iteration $i (for test-set-$expectedTestSetIndex) ---"

  # Configure Docker image for recording
  if ($env:DOCKER_IMAGE_RECORD) { $env:KEPLOY_DOCKER_IMAGE = $env:DOCKER_IMAGE_RECORD } 
  else { $env:KEPLOY_DOCKER_IMAGE = 'keploy:record' }
  
  $recArgs = @(
    'record',
    '-c', '"docker compose up"',
    '--container-name', $containerName,
    '--generate-github-actions=false'
  )

  $keployProcess = $null
  try {
    # FIX #2: Use Start-Process for robust background execution and logging.
    # This avoids the ".NET Task Runspace" error entirely.
    Write-Host "Starting Keploy record process... Logs will be written to $logPath"
    $keployProcess = Start-Process -FilePath $env:RECORD_BIN -ArgumentList $recArgs -NoNewWindow -PassThru -RedirectStandardOutput $logPath -RedirectStandardError $logPath
    Write-Host "Keploy started with PID $($keployProcess.Id)."

    # 2. Wait for the application to become healthy
    Write-Host "Waiting for application to become healthy at $env:APP_HEALTH_URL..."
    $maxWaitSeconds = 90
    $stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
    $appIsHealthy = $false
    while ($stopwatch.Elapsed.TotalSeconds -lt $maxWaitSeconds) {
        try {
            # FIX #1: Check for any response, even an error code like 404.
            # A response proves the server is up. Invoke-WebRequest throws an error on non-200 codes.
            # We catch it and treat it as a success signal for the health check.
            Invoke-WebRequest -Uri $env:APP_HEALTH_URL -UseBasicParsing -TimeoutSec 5
            # If we get here, it means a 200 OK was received (unlikely but possible)
            Write-Host "Application is healthy (received 200 OK)!"
            $appIsHealthy = $true
            break
        } catch [Microsoft.PowerShell.Commands.HttpResponseException] {
            # This is the expected case: the server responded with an error (e.g., 404).
            # This confirms the server is running.
            Write-Host "Application is healthy (received HTTP response)!"
            $appIsHealthy = $true
            break
        } catch {
            # This catch block will handle connection errors (server not up yet).
            Write-Host "App not ready, waiting..."
            Start-Sleep -Seconds 5
        }
    }

    if (-not $appIsHealthy) {
        throw "Application did not become healthy within $maxWaitSeconds seconds."
    }

    # 3. Send API requests to be recorded
    Write-Host "Sending API requests..."
    foreach ($u in @('https://google.com', 'https://facebook.com')) {
        try {
            $body = @{ url = $u } | ConvertTo-Json -Compress
            Write-Host "Sending POST request with URL: $u"
            Invoke-RestMethod -Method POST -Uri $env:APP_POST_URL -ContentType "application/json" -Body $body -TimeoutSec 10
            Write-Host "Successfully sent request for $u"
            Start-Sleep -Milliseconds 500
        } catch {
            Write-Warning "Failed to send request for $u : $_"
        }
    }
    
    # 4. Wait a few seconds for Keploy to capture traffic and write test files
    Write-Host "Waiting for Keploy to write files..."
    Start-Sleep -Seconds 10

  } finally {
    # 5. Stop the Keploy process
    if ($null -ne $keployProcess -and -not $keployProcess.HasExited) {
        Write-Host "Stopping Keploy process (PID: $($keployProcess.Id))..."
        Stop-Process -Id $keployProcess.Id -Force # Use idiomatic Stop-Process
        Write-Host "Keploy process stopped."
    } else {
        Write-Host "Keploy process already exited or was not started."
    }
    # The complex Task.WaitAll is no longer needed.
  }

  # --- Verification ---
  Write-Host "Verifying recording for iteration $i..."
  $testSetPath = ".\keploy\test-set-$expectedTestSetIndex\tests"
  if (-not (Test-Path $testSetPath) -or -not (Get-ChildItem -Path $testSetPath -Filter "test-*.yaml")) {
      Write-Error "No test files were created for iteration $i. Check logs in $logPath."
      Get-Content $logPath
      exit 1
  }

  $testCount = (Get-ChildItem -Path $testSetPath -Filter "test-*.yaml").Count
  Write-Host "Found $testCount test file(s) for test-set-$expectedTestSetIndex."

  if (Select-String -Path $logPath -Pattern 'WARNING:\s*DATA\s*RACE|FATAL|PANIC|Failed to record' -SimpleMatch) {
    Write-Error "Critical error found in recording log. See details below."
    Get-Content $logPath
    exit 1
  }

  Write-Host "Successfully recorded test-set-$expectedTestSetIndex."
  Start-Sleep -Seconds 5 # Small delay before next iteration
}

# --- Stop services before test mode ---
Write-Host "Shutting down docker compose services before test mode..."
docker compose down

# --- Test (replay) ---
$testContainer = "echoApp"
$testLog = "$testContainer.test.txt"

# Configure Docker image for replay
if ($env:DOCKER_IMAGE_REPLAY) { $env:KEPLOY_DOCKER_IMAGE = $env:DOCKER_IMAGE_REPLAY }
else { $env:KEPLOY_DOCKER_IMAGE = 'keploy:replay' }

Write-Host "Starting keploy test..."
Write-Host "Replay phase image: $env:KEPLOY_DOCKER_IMAGE"

$testArgs = @(
  'test',
  '-c', 'docker compose up',
  '--container-name', $testContainer,
  '--api-timeout', '60',
  '--delay', '20',
  '--generate-github-actions=false'
)

Write-Host "Executing: $env:REPLAY_BIN $($testArgs -join ' ')"
& $env:REPLAY_BIN @testArgs 2>&1 | Tee-Object -FilePath $testLog

# Check test log for critical errors
if (Select-String -Path $testLog -Pattern 'FATAL|PANIC|WARNING:\s*DATA\s*RACE' -SimpleMatch) {
  Write-Error "Critical error or race condition found during test."
  Get-Content $testLog
  exit 1
}

# --- Parse reports and ensure both test sets passed ---
$allPassed = $true
for ($idx = 0; $idx -le 1; $idx++) {
  $report = ".\keploy\reports\test-run-0\test-set-$idx-report.yaml"
  if (-not (Test-Path $report)) {
    Write-Error "Missing report file: $report"
    $allPassed = $false
    break
  }
  $line = Select-String -Path $report -Pattern 'status:' | Select-Object -First 1
  $status = ($line.ToString() -replace '.*status:\s*', '').Trim()
  Write-Host "Test status for test-set-${idx}: $status"
  if ($status -ne 'PASSED') {
    $allPassed = $false
    Write-Host "Test-set-$idx did not pass."
  }
}

if ($allPassed) { 
  Write-Host "All tests passed successfully!" 
  exit 0 
} else { 
  Write-Error "Some tests failed. See log for details:"
  Get-Content $testLog
  exit 1 
}

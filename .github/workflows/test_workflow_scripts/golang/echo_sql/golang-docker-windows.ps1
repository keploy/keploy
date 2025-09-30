<# 
  PowerShell test runner for Keploy (Windows).
  This script is the functional equivalent of the Linux bash test runner.
  - It uses a background job (Start-Job) to send API traffic.
  - The background job is responsible for stopping the blocking 'keploy record' process.
  - All logic for the background job is self-contained in a ScriptBlock.
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

# Set App URLs from environment variables or use defaults
$env:APP_HEALTH_URL = $env:APP_HEALTH_URL ?? 'http://localhost:8082/health'
$env:APP_POST_URL   = $env:APP_POST_URL   ?? 'http://localhost:8082/url'

Write-Host "Using RECORD_BIN = $env:RECORD_BIN"
Write-Host "Using REPLAY_BIN = $env:REPLAY_BIN"

# --- Build Docker image(s) ---
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

  # --- SCRIPT BLOCK FOR BACKGROUND JOB ---
  # This entire block runs in a separate process. All necessary functions and logic
  # must be defined inside it. This is the PowerShell equivalent of `send_request &`.
  $scriptBlock = {
    
    # PowerShell equivalent of the 'container_kill' function
    function Stop-Keploy {
      try {
        $procs = Get-Process -ErrorAction SilentlyContinue | Where-Object {
          $_.Name -match '^keploy(-record)?$' -or $_.Path -match 'keploy(-record)?\.exe$'
        } | Sort-Object StartTime -Descending
        $p = $procs | Select-Object -First 1
        if ($null -ne $p) {
          Write-Host "BACKGROUND JOB: Stopping Keploy PID $($p.Id) ($($p.Name))"
          Stop-Process -Id $p.Id -Force
        } else {
          Write-Host "BACKGROUND JOB: No Keploy process found to kill."
        }
      } catch {
        Write-Warning "BACKGROUND JOB: Failed to stop keploy: $_"
      }
    }

    # PowerShell equivalent of the 'send_request' function logic
    function Send-Request-And-Stop {
      Start-Sleep -Seconds 10
      $appStarted = $false
      $maxRetries = 20 # Wait for up to ~1 minute for the app to be healthy
      $retryCount = 0

      Write-Host "BACKGROUND JOB: Waiting for app at $env:APP_HEALTH_URL..."
      while (-not $appStarted -and $retryCount -lt $maxRetries) {
        try {
          Invoke-WebRequest -Method GET -Uri $env:APP_HEALTH_URL -TimeoutSec 5 -UseBasicParsing | Out-Null
          $appStarted = $true
        } catch {
          Start-Sleep -Seconds 3
          $retryCount++
        }
      }

      if (-not $appStarted) {
        Write-Error "BACKGROUND JOB: App never became healthy. The recording will be stopped."
        return # The 'finally' block below will still run to stop Keploy
      }
      
      Write-Host "BACKGROUND JOB: App started. Sending API requests..."

      # Make API calls to record test cases
      $urls = 'https://google.com', 'https://facebook.com'
      foreach ($u in $urls) {
        $body = @{ url = $u } | ConvertTo-Json -Compress
        Invoke-RestMethod -Method POST -Uri $env:APP_POST_URL -ContentType "application/json" -Body $body | Out-Null
      }

      # Final health check
      Invoke-WebRequest -Method GET -Uri $env:APP_HEALTH_URL -TimeoutSec 5 -UseBasicParsing | Out-Null
      
      Write-Host "BACKGROUND JOB: Requests sent. Waiting 5s before stopping Keploy."
      Start-Sleep -Seconds 5
    }

    # --- JOB EXECUTION ---
    try {
        Send-Request-And-Stop
    }
    finally {
        # This ensures Keploy is *always* stopped, unblocking the main script.
        Stop-Keploy
    }
  }

  # Launch the traffic generator in the background
  $jobName = "SendRequest_$i"
  $job = Start-Job -Name $jobName -ScriptBlock $scriptBlock

  # Set agent image if provided by workflow
  $env:KEPLOY_DOCKER_IMAGE = if ($env:DOCKER_IMAGE_RECORD) { $env:DOCKER_IMAGE_RECORD } else { 'keploy:record' }
  Write-Host "Record phase image: $env:KEPLOY_DOCKER_IMAGE"
  Write-Host "Starting keploy record (iteration $i)... This will block until the background job stops it."
  
  # This command blocks until the background job calls Stop-Keploy
  & $env:RECORD_BIN record -c "docker compose up" --container-name $containerName --generate-github-actions=false --debug 2>&1 | Tee-Object -FilePath $logPath

  # --- Job Cleanup and Log Retrieval ---
  Write-Host "Keploy record has been stopped. Finalizing job."
  try {
    Wait-Job -Name $jobName -Timeout 120
    Receive-Job -Name $jobName # Print output from background job for debugging
  } catch {
      Write-Warning "The background job timed out or failed."
      Receive-Job -Name $jobName # Try to get any error messages
  }
  finally {
    if (Get-Job -Name $jobName -ErrorAction SilentlyContinue) { Remove-Job -Name $jobName -Force -ErrorAction SilentlyContinue }
  }

  # --- Guard-rails ---
  if (Select-String -Path $logPath -Pattern 'WARNING:\s*DATA\s*RACE' -Quiet) {
    Write-Host "Race condition detected in recording."
    Get-Content $logPath
    exit 1
  }
  if (Select-String -Path $logPath -Pattern 'ERROR' -Quiet) {
    Write-Host "Error found in recording."
    Get-Content $logPath
    exit 1
  }

  Start-Sleep -Seconds 5
  Write-Host "Recorded test case and mocks for iteration $i"
}

# --- Stop services before test mode ---
Write-Host "Shutting down docker compose services before test mode..."
docker compose down

# --- Test (replay) ---
$testContainer = "echoApp"
$testLog = "$testContainer.test.txt"

$env:KEPLOY_DOCKER_IMAGE = if ($env:DOCKER_IMAGE_REPLAY) { $env:DOCKER_IMAGE_REPLAY } else { 'keploy:replay' }
Write-Host "Replay phase image: $env:KEPLOY_DOCKER_IMAGE"
Write-Host "Starting keploy test..."

& $env:REPLAY_BIN test -c 'docker compose up' --container-name $testContainer --api-timeout 60 --delay 20 --generate-github-actions=false 2>&1 | Tee-Object -FilePath $testLog

# Check test log
if (Select-String -Path $testLog -Pattern 'ERROR' -Quiet) {
  Write-Host "Error found during test."
  Get-Content $testLog
  exit 1
}
if (Select-String -Path $testLog -Pattern 'WARNING:\s*DATA\s*RACE' -Quiet) {
  Write-Host "Race condition detected during test."
  Get-Content $testLog
  exit 1
}

# --- Parse reports and ensure both test sets passed ---
$allPassed = $true
for ($idx = 0; $idx -le 1; $idx++) {
  $report = ".\keploy\reports\test-run-0\test-set-$idx-report.yaml"
  if (-not (Test-Path $report)) {
    Write-Host "Missing report file: $report"
    $allPassed = $false
    break
  }
  $line = Select-String -Path $report -Pattern 'status:' | Select-Object -First 1
  if ($line) {
    $status = ($line.ToString() -replace '.*status:\s*', '').Trim()
    Write-Host "Test status for test-set-${idx}: $status"
    if ($status -ne 'PASSED') {
      $allPassed = $false
      Write-Host "Test-set-$idx did not pass."
      break
    }
  } else {
    Write-Host "Could not find status in report file: $report"
    $allPassed = $false
    break
  }
}

if ($allPassed) { Write-Host "All tests passed"; exit 0 } else { Get-Content $testLog; exit 1 }
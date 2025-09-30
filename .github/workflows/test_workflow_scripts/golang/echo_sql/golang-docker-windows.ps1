<# 
  PowerShell test runner for Keploy (Windows).
  - Honors RECORD_BIN / REPLAY_BIN (resolved via PATH if only a file name)
  - Honors DOCKER_IMAGE_RECORD / DOCKER_IMAGE_REPLAY via KEPLOY_DOCKER_IMAGE
  - Fixes Stop-Keploy to catch keploy-record.exe as well
  - Standardizes flags to kebab-case
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

# Optionally parameterize app URLs (kept your current defaults)
$env:APP_HEALTH_URL    = if ($env:APP_HEALTH_URL) { $env:APP_HEALTH_URL } else { 'http://localhost:8082/health' }
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
  $containerName = "echoApp"   # adjust per sample if needed
  $logPath = "$containerName.record.$i.txt"

  # --- SCRIPT BLOCK FOR BACKGROUND JOB ---
  # Pass necessary variables as parameters to the job
  $scriptBlock = {
    param(
      [string]$healthUrl,
      [string]$postUrl,
      [int]$iteration
    )
    
    # This function stops the Keploy process. It runs within the background job.
    function Stop-Keploy {
      try {
        # Match both keploy.exe and keploy-record.exe
        $procs = Get-Process -ErrorAction SilentlyContinue | Where-Object {
          $_.Name -match '^keploy(-record)?$' -or $_.Path -match 'keploy(-record)?\.exe$'
        } | Sort-Object StartTime -Descending
        $p = $procs | Select-Object -First 1
        if ($null -ne $p) {
          Write-Host "BACKGROUND JOB: Stopping Keploy PID $($p.Id) ($($p.ProcessName))"
          Stop-Process -Id $p.Id -Force
          Write-Host "BACKGROUND JOB: Keploy process stopped successfully"
        } else {
          Write-Host "BACKGROUND JOB: No Keploy process found to kill."
        }
      } catch {
        Write-Warning "BACKGROUND JOB: Failed to stop keploy: $_"
      }
    }

    # This function continuously polls the app and sends traffic
    function Send-Request {
      param(
        [string]$healthUrl,
        [string]$postUrl
      )
      
      # Initial wait for Docker and Keploy to start up
      Write-Host "BACKGROUND JOB: Initial wait for services to start..."
      Start-Sleep -Seconds 10
      
      $appStarted = $false
      $maxRetries = 40  # Wait for up to 2 minutes (40 * 3 seconds)
      $retryCount = 0
      $successfulRequests = 0
      $targetRequests = 2  # Number of successful requests needed

      Write-Host "BACKGROUND JOB: Waiting for app at $healthUrl..."
      
      # Keep polling until we get successful requests
      while ($retryCount -lt $maxRetries) {
        try {
          # Try health check first
          $response = Invoke-WebRequest -Method GET -Uri $healthUrl -TimeoutSec 5 -UseBasicParsing -ErrorAction Stop
          if ($response.StatusCode -eq 200) {
            if (-not $appStarted) {
              $appStarted = $true
              Write-Host "BACKGROUND JOB: App is now responding! Starting to send test requests..."
            }
            
            # App is ready, send test requests
            foreach ($u in @('https://google.com', 'https://facebook.com')) {
              try {
                $body = @{ url = $u } | ConvertTo-Json -Compress
                Write-Host "BACKGROUND JOB: Sending POST request with URL: $u"
                $postResponse = Invoke-RestMethod -Method POST -Uri $postUrl -ContentType "application/json" -Body $body -TimeoutSec 10 -ErrorAction Stop
                $successfulRequests++
                Write-Host "BACKGROUND JOB: Successfully sent request $successfulRequests (URL: $u)"
                
                # Small delay between requests
                Start-Sleep -Milliseconds 500
              } catch {
                Write-Warning "BACKGROUND JOB: Failed to send POST request for $u : $_"
              }
            }
            
            # If we've sent enough successful requests, we can stop
            if ($successfulRequests -ge $targetRequests) {
              Write-Host "BACKGROUND JOB: Successfully sent $successfulRequests requests. Mission accomplished!"
              
              # Final health check
              try {
                Invoke-WebRequest -Method GET -Uri $healthUrl -TimeoutSec 5 -UseBasicParsing -ErrorAction Stop | Out-Null
                Write-Host "BACKGROUND JOB: Final health check successful"
              } catch {
                Write-Warning "BACKGROUND JOB: Final health check failed: $_"
              }
              
              # Wait a bit to ensure Keploy captures everything
              Write-Host "BACKGROUND JOB: Waiting 5 seconds before stopping Keploy..."
              Start-Sleep -Seconds 5
              break
            }
          }
        } catch {
          if ($appStarted) {
            Write-Warning "BACKGROUND JOB: App stopped responding: $_"
            $appStarted = $false
          } else {
            Write-Host "BACKGROUND JOB: App not ready yet (attempt $($retryCount + 1)/$maxRetries). Retrying in 3 seconds..."
          }
        }
        
        Start-Sleep -Seconds 3
        $retryCount++
      }

      if ($successfulRequests -eq 0) {
        Write-Error "BACKGROUND JOB: Failed to send any requests within the timeout period."
      } else {
        Write-Host "BACKGROUND JOB: Total successful requests sent: $successfulRequests"
      }
    }

    # --- EXECUTION LOGIC FOR THE JOB ---
    try {
        Write-Host "BACKGROUND JOB: Starting for iteration $iteration"
        Write-Host "BACKGROUND JOB: Health URL = $healthUrl"
        Write-Host "BACKGROUND JOB: POST URL = $postUrl"
        Send-Request -healthUrl $healthUrl -postUrl $postUrl
    }
    catch {
        Write-Error "BACKGROUND JOB: Exception occurred: $_"
    }
    finally {
        # Ensure Keploy is always stopped, even if Send-Request fails.
        Write-Host "BACKGROUND JOB: Initiating Keploy shutdown..."
        Stop-Keploy
    }
  }
  # --- END OF SCRIPT BLOCK ---

  # Launch traffic generator in background with parameters
  $jobName = "SendRequest_$i"
  Write-Host "Starting background job: $jobName"
  $job = Start-Job -Name $jobName -ScriptBlock $scriptBlock -ArgumentList $env:APP_HEALTH_URL, $env:APP_POST_URL, $i

  # If the workflow provided an agent image for recording, honor it
  if ($env:DOCKER_IMAGE_RECORD) {
    $env:KEPLOY_DOCKER_IMAGE = $env:DOCKER_IMAGE_RECORD
    Write-Host "Record phase will use agent image: $env:KEPLOY_DOCKER_IMAGE"
  } else {
    $env:KEPLOY_DOCKER_IMAGE = 'keploy:record'
  }

  Write-Host "Starting keploy record (iteration $i)..."
  Write-Host "Record phase image: $env:KEPLOY_DOCKER_IMAGE"

  $recArgs = @(
    'record',
    '-c', 'docker compose up',
    '--container-name', $containerName,
    '--generate-github-actions=false',
    '--debug'
  )
  
  # This command blocks until the background job kills it with Stop-Keploy
  Write-Host "Executing: $env:RECORD_BIN $($recArgs -join ' ')"
  & $env:RECORD_BIN @recArgs 2>&1 | Tee-Object -FilePath $logPath

  # Wait for traffic job to finish and get its logs for debugging
  Write-Host "Keploy record command finished. Waiting for background job to complete..."
  
  try {
    $jobResult = Wait-Job -Name $jobName -Timeout 30
    if ($jobResult) {
      Write-Host "Background job completed. Status: $($jobResult.State)"
    } else {
      Write-Warning "Background job did not complete within timeout"
    }
    
    # Print all output from the background job for visibility
    Write-Host "=== Background Job Output ==="
    Receive-Job -Name $jobName
    Write-Host "=== End Background Job Output ==="
  } catch {
    Write-Warning "The background job failed or timed out. Retrieving logs..."
    Get-Job -Name $jobName | Select-Object *
    Receive-Job -Name $jobName # Attempt to get any error output
  }
  finally {
    if (Get-Job -Name $jobName -ErrorAction SilentlyContinue) { 
      Stop-Job -Name $jobName -ErrorAction SilentlyContinue
      Remove-Job -Name $jobName -Force -ErrorAction SilentlyContinue | Out-Null 
    }
  }

  # Guard-rails
  if (Select-String -Path $logPath -Pattern 'WARNING:\s*DATA\s*RACE' -SimpleMatch) {
    Write-Host "Race condition detected in recording."
    Get-Content $logPath
    exit 1
  }
  if (Select-String -Path $logPath -Pattern 'ERROR' -SimpleMatch) {
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

# If the workflow provided an agent image for replay, honor it
if ($env:DOCKER_IMAGE_REPLAY) {
  $env:KEPLOY_DOCKER_IMAGE = $env:DOCKER_IMAGE_REPLAY
  Write-Host "Replay phase will use agent image: $env:KEPLOY_DOCKER_IMAGE"
} else {
  $env:KEPLOY_DOCKER_IMAGE = 'keploy:replay'
}

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

# Check test log
if (Select-String -Path $testLog -Pattern 'ERROR' -SimpleMatch) {
  Write-Host "Error found during test."
  Get-Content $testLog
  exit 1
}
if (Select-String -Path $testLog -Pattern 'WARNING:\s*DATA\s*RACE' -SimpleMatch) {
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
  $status = ($line.ToString() -replace '.*status:\s*', '').Trim()
  Write-Host "Test status for test-set-${idx}: $status"
  if ($status -ne 'PASSED') {
    $allPassed = $false
    Write-Host "Test-set-$idx did not pass."
    break
  }
}

if ($allPassed) { 
  Write-Host "All tests passed" 
  exit 0 
} else { 
  Get-Content $testLog
  exit 1 
}
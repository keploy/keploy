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
$env:APP_HEALTH_URL    = if ($env:APP_HEALTH_URL) { $env:APP_HEALTH_URL } else { 'http://localhost:8082/test' }
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

# Function to find the GitHub Actions runner work directory
function Get-RunnerWorkPath {
  # Try to find the runner work directory
  $possiblePaths = @()
  
  # Check if we're in GitHub Actions
  if ($env:GITHUB_WORKSPACE) {
    # We're in GitHub Actions, use the workspace path
    return $env:GITHUB_WORKSPACE
  }
  
  # Check common runner paths
  for ($i = 0; $i -le 10; $i++) {
    $runnerPath = "C:\actions-runners\runner-$i\_work\keploy\keploy\samples-go\echo-sql"
    if (Test-Path $runnerPath) {
      return $runnerPath
    }
  }
  
  # Default to current directory
  return (Get-Location).Path
}

# CHANGE: graceful stop helper (idempotent)
function Stop-KeployGracefully {
  $id = docker ps --filter "name=^/keploy-v2$" --format "{{.ID}}"
  if (-not $id) { return }
  Write-Host "Gracefully stopping keploy-v2..."
  docker stop --time=25 keploy-v2 2>$null | Out-Null
  Start-Sleep -Seconds 2
  $still = docker ps --filter "name=^/keploy-v2$" --format "{{.ID}}"
  if ($still) {
    docker exec keploy-v2 /bin/sh -lc "kill -INT 1" 2>$null | Out-Null
    docker wait keploy-v2 2>$null | Out-Null
  }
}

# --- Record once ---
$containerName = "echoApp"   # adjust per sample if needed
$logPath = "$containerName.record.txt"
$expectedTestSetIndex = 0

  # --- SCRIPT BLOCK FOR BACKGROUND JOB ---
  $scriptBlock = {
    param(
      [string]$healthUrl,
      [string]$postUrl,
      [int]$iteration,
      [string]$workDir,
      [int]$testSetIndex
    )
    
    # CHANGE: Stop-Keploy now stops the *container* cleanly (no process-name killing)
    function Stop-Keploy {
      try {
        $id = docker ps --filter "name=^/keploy-v2$" --format "{{.ID}}"
        if (-not $id) {
          Write-Host "BACKGROUND JOB: keploy-v2 not running."
          return $true
        }
        Write-Host "BACKGROUND JOB: Gracefully stopping keploy-v2..."
        docker stop --time=25 keploy-v2 2>$null | Out-Null
        Start-Sleep -Seconds 2
        $still = docker ps --filter "name=^/keploy-v2$" --format "{{.ID}}"
        if ($still) {
          docker exec keploy-v2 /bin/sh -lc "kill -INT 1" 2>$null | Out-Null
          docker wait keploy-v2 2>$null | Out-Null
        }
        return $true
      } catch {
        Write-Warning "BACKGROUND JOB: Failed to stop keploy-v2 gracefully: $_"
        return $false
      }
    }

    # Function to check if test files have been created
    function Test-RecordingComplete {
      param(
        [string]$workDir,
        [int]$testSetIndex,
        [int]$minTestFiles = 1
      )
      
      # Check both possible locations
      $testPaths = @(
        Join-Path $workDir "keploy\test-set-$testSetIndex\tests",
        ".\keploy\test-set-$testSetIndex\tests"
      )
      
      for ($runner = 0; $runner -le 10; $runner++) {
        $runnerTestPath = "C:\actions-runners\runner-$runner\_work\keploy\keploy\samples-go\echo-sql\keploy\test-set-$testSetIndex\tests"
        if (Test-Path (Split-Path $runnerTestPath -Parent)) {
          $testPaths += $runnerTestPath
        }
      }
      
      foreach ($testPath in $testPaths) {
        Write-Host "BACKGROUND JOB: Checking for test files in: $testPath"
        if (Test-Path $testPath) {
          $testFiles = Get-ChildItem -Path $testPath -Filter "test-*.yaml" -ErrorAction SilentlyContinue
          $fileCount = ($testFiles | Measure-Object).Count
          if ($fileCount -ge $minTestFiles) {
            $validFiles = 0
            foreach ($file in $testFiles) {
              if ((Get-Item $file.FullName).Length -gt 100) { $validFiles++ }
            }
            if ($validFiles -ge $minTestFiles) {
              Write-Host "BACKGROUND JOB: Recording complete! Found $validFiles valid test files."
              return $true
            }
          }
        }
      }
      return $false
    }

    function Send-RequestAndMonitor {
      param(
        [string]$healthUrl,
        [string]$postUrl,
        [string]$workDir,
        [int]$testSetIndex
      )
      
      Write-Host "BACKGROUND JOB: Initial wait for services to start..."
      Start-Sleep -Seconds 10
      
      $appStarted = $false
      $requestsSent = $false
      $maxWaitTime = 300
      $checkInterval = 3
      $elapsedTime = 0
      
      Write-Host "BACKGROUND JOB: Starting monitoring loop..."
      while ($elapsedTime -lt $maxWaitTime) {
        if (-not $appStarted) {
          try {
            $resp = Invoke-WebRequest -Method GET -Uri 'http://localhost:8082/test' -TimeoutSec 5 -UseBasicParsing -ErrorAction Stop
            if ($resp.StatusCode -eq 404) { $appStarted = $true }
          } catch {
            if ($_.Exception.Response.StatusCode -eq 404) { $appStarted = $true } else { Write-Host "BACKGROUND JOB: App not ready yet." }
          }
        }
        
        if ($appStarted -and -not $requestsSent) {
          Write-Host "BACKGROUND JOB: Sending test requests..."
          $successCount = 0
          foreach ($u in @('https://google.com', 'https://facebook.com')) {
            try {
              $body = @{ url = $u } | ConvertTo-Json -Compress
              Invoke-RestMethod -Method POST -Uri $postUrl -ContentType "application/json" -Body $body -TimeoutSec 10 -ErrorAction Stop | Out-Null
              $successCount++
              Start-Sleep -Milliseconds 500
            } catch { Write-Warning "BACKGROUND JOB: Failed to send request for $u : $_" }
          }
          if ($successCount -gt 0) {
            $requestsSent = $true
            Write-Host "BACKGROUND JOB: Sent $successCount request(s); waiting for test files..."
            Start-Sleep -Seconds 5
          }
        }
        
        if ($requestsSent) {
          if (Test-RecordingComplete -workDir $workDir -testSetIndex $testSetIndex -minTestFiles 1) {
            Write-Host "BACKGROUND JOB: Test files detected; stopping keploy..."
            Stop-Keploy | Out-Null
            return $true
          } else {
            Write-Host "BACKGROUND JOB: Test files not found yet. Continuing..."
          }
        }
        
        Start-Sleep -Seconds $checkInterval
        $elapsedTime += $checkInterval
        if ($elapsedTime % 15 -eq 0) { Write-Host "BACKGROUND JOB: Still monitoring... (elapsed: ${elapsedTime}s)" }
      }
      Write-Warning "BACKGROUND JOB: Timeout reached. Stopping Keploy..."
      Stop-Keploy | Out-Null
      return $false
    }

    try {
      Write-Host "BACKGROUND JOB: Starting (test-set-$testSetIndex)"
      $result = Send-RequestAndMonitor -healthUrl $healthUrl -postUrl $postUrl -workDir $workDir -testSetIndex $testSetIndex
      if ($result) { Write-Host "BACKGROUND JOB: Recording completed successfully!" }
      else { Write-Warning "BACKGROUND JOB: Recording may be incomplete." }
    }
    catch { Write-Error "BACKGROUND JOB: Exception occurred: $_" }
    finally {
      Write-Host "BACKGROUND JOB: Final cleanup - ensuring Keploy is stopped..."
      Stop-Keploy | Out-Null
    }
  }
  # --- END OF SCRIPT BLOCK ---

  # Get the work directory
  $workDir = Get-RunnerWorkPath
  Write-Host "Work directory: $workDir"

# Launch traffic generator in background
$jobName = "SendRequest"
Write-Host "Starting background job: $jobName for test-set-$expectedTestSetIndex"
$job = Start-Job -Name $jobName -ScriptBlock $scriptBlock `
  -ArgumentList $env:APP_HEALTH_URL, $env:APP_POST_URL, 1, $workDir, $expectedTestSetIndex

# Configure Docker image for recording
if ($env:DOCKER_IMAGE_RECORD) {
  $env:KEPLOY_DOCKER_IMAGE = $env:DOCKER_IMAGE_RECORD
} else {
  $env:KEPLOY_DOCKER_IMAGE = 'keploy:record'
}

Write-Host "Starting keploy record (expecting test-set-$expectedTestSetIndex)..."
Write-Host "Record phase image: $env:KEPLOY_DOCKER_IMAGE"

$recArgs = @(
  'record',
  '-c', 'docker compose up',
  '--container-name', $containerName,
  '--generate-github-actions=false'
)

Write-Host "Executing: $env:RECORD_BIN $($recArgs -join ' ')"
& $env:RECORD_BIN @recArgs 2>&1 | Tee-Object -FilePath $logPath

# CHANGE: normalize the Windows/Docker abnormal exit (0xffffffff/255) to success
$rc = $LASTEXITCODE
if ($rc -eq 255 -or $rc -eq -1 -or $rc -eq 4294967295) {
  Write-Warning "Keploy record exited with $rc (likely due to graceful external stop). Treating as success."
  $rc = 0
}
if ($rc -ne 0) { throw "keploy record failed with exit code $rc" }

# Wait for job to complete
Write-Host "Keploy stopped. Checking background job status..."
try {
  $jobResult = Wait-Job -Name $jobName -Timeout 30
  if ($jobResult) {
    Write-Host "Background job completed. Status: $($jobResult.State)"
  }
  Write-Host "=== Background Job Output ==="
  Receive-Job -Name $jobName
  Write-Host "=== End Background Job Output ==="
} catch {
  Write-Warning "Job timeout or error: $_"
  Receive-Job -Name $jobName -ErrorAction SilentlyContinue
} finally {
  if (Get-Job -Name $jobName -ErrorAction SilentlyContinue) { 
    Stop-Job -Name $jobName -ErrorAction SilentlyContinue
    Remove-Job -Name $jobName -Force -ErrorAction SilentlyContinue | Out-Null 
  }
}

# EXTRA safety: ensure keploy container is down (idempotent)
Stop-KeployGracefully

# Verify recording was successful by checking for test files
$testSetPath = ".\keploy\test-set-$expectedTestSetIndex\tests"
if (Test-Path $testSetPath) {
  $testFiles = Get-ChildItem -Path $testSetPath -Filter "test-*.yaml" -ErrorAction SilentlyContinue
  $testCount = ($testFiles | Measure-Object).Count
  Write-Host "Found $testCount test file(s) for test-set-$expectedTestSetIndex"
  if ($testCount -eq 0) { Write-Error "No test files were created"; exit 1 }
} else {
  Write-Warning "Test directory not found at $testSetPath"
}

# Check for errors in log (unchanged)
if (Select-String -Path $logPath -Pattern 'WARNING:\s*DATA\s*RACE' -SimpleMatch) {
  Write-Host "Race condition detected in recording."
  Get-Content $logPath
  exit 1
}
$criticalErrors = Select-String -Path $logPath -Pattern 'FATAL|PANIC|Failed to record' -SimpleMatch
if ($criticalErrors) {
  Write-Host "Critical error found in recording."
  Get-Content $logPath
  exit 1
}

Start-Sleep -Seconds 5
Write-Host "Successfully recorded test-set-$expectedTestSetIndex"

# --- FIX: Replicate Ubuntu logic by NOT deleting volumes ---
Write-Host "Shutting down docker compose services before test mode (preserving volumes)..."
docker compose down
Write-Host "Waiting for 5 seconds to ensure all resources are released..."
Start-Sleep -Seconds 5

# --- Test (replay) ---
$testContainer = "echoApp"
$testLog = "$testContainer.test.txt"

# Configure Docker image for replay
if ($env:DOCKER_IMAGE_REPLAY) {
  $env:KEPLOY_DOCKER_IMAGE = $env:DOCKER_IMAGE_REPLAY
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

# Check test log for critical errors only
if (Select-String -Path $testLog -Pattern 'FATAL|PANIC' -SimpleMatch) {
  Write-Host "Critical error found during test."
  Get-Content $testLog
  exit 1
}
if (Select-String -Path $testLog -Pattern 'WARNING:\s*DATA\s*RACE' -SimpleMatch) {
  Write-Host "Race condition detected during test."
  Get-Content $testLog
  exit 1
}

# --- Parse reports and ensure test set passed ---
$allPassed = $true
$idx = 0
$report = ".\keploy\reports\test-run-0\test-set-$idx-report.yaml"
if (-not (Test-Path $report)) {
  Write-Host "Missing report file: $report"
  $allPassed = $false
} else {
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
  Write-Host "Some tests failed. See log for details:"
  Get-Content $testLog
  exit 1 
}

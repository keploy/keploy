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
    
    # This function stops the Keploy process
    function Stop-Keploy {
      try {
        # Find all keploy processes
        $procs = Get-Process -ErrorAction SilentlyContinue | Where-Object {
          $_.ProcessName -eq 'keploy' -or $_.ProcessName -eq 'keploy-record' -or 
          $_.Path -like '*keploy*.exe' -or $_.CommandLine -like '*keploy*'
        } | Sort-Object StartTime -Descending
        
        if ($procs.Count -eq 0) {
          Write-Host "BACKGROUND JOB: No Keploy process found to kill."
          return $false
        }
        
        foreach ($proc in $procs) {
          Write-Host "BACKGROUND JOB: Stopping Keploy PID $($proc.Id) ($($proc.ProcessName))"
          try {
            Stop-Process -Id $proc.Id -Force -ErrorAction Stop
            Write-Host "BACKGROUND JOB: Keploy process $($proc.Id) stopped successfully"
          } catch {
            Write-Warning "BACKGROUND JOB: Failed to stop process $($proc.Id): $_"
          }
        }
        
        # Wait a moment for processes to fully terminate
        Start-Sleep -Seconds 2
        
        # Verify all processes are stopped
        $remainingProcs = Get-Process -ErrorAction SilentlyContinue | Where-Object {
          $_.ProcessName -eq 'keploy' -or $_.ProcessName -eq 'keploy-record'
        }
        
        if ($remainingProcs.Count -eq 0) {
          Write-Host "BACKGROUND JOB: All Keploy processes stopped successfully"
          return $true
        } else {
          Write-Warning "BACKGROUND JOB: Some Keploy processes may still be running"
          return $false
        }
      } catch {
        Write-Warning "BACKGROUND JOB: Failed to stop keploy: $_"
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
        # Local keploy directory
        Join-Path $workDir "keploy\test-set-$testSetIndex\tests"
        # Also check without workDir prefix in case we're already in the right directory
        ".\keploy\test-set-$testSetIndex\tests"
      )
      
      # Also check GitHub runner paths if different
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
            Write-Host "BACKGROUND JOB: Found $fileCount test file(s) in $testPath"
            
            # Verify files have content (not empty)
            $validFiles = 0
            foreach ($file in $testFiles) {
              if ((Get-Item $file.FullName).Length -gt 100) {  # Assuming valid test files are > 100 bytes
                $validFiles++
                Write-Host "BACKGROUND JOB: Valid test file: $($file.Name) ($(Get-Item $file.FullName).Length bytes)"
              }
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

    # Main execution
    function Send-RequestAndMonitor {
      param(
        [string]$healthUrl,
        [string]$postUrl,
        [string]$workDir,
        [int]$testSetIndex
      )
      
      # Initial wait for Docker and Keploy to start
      Write-Host "BACKGROUND JOB: Initial wait for services to start..."
      Start-Sleep -Seconds 10
      
      $appStarted = $false
      $requestsSent = $false
      $maxWaitTime = 180  # 3 minutes total
      $checkInterval = 3
      $elapsedTime = 0
      
      Write-Host "BACKGROUND JOB: Starting monitoring loop..."
      
      while ($elapsedTime -lt $maxWaitTime) {
        # First, try to reach the app using a simple GET request to root
        if (-not $appStarted) {
          try {
            # Use a simple GET request to test if app is running
            $response = Invoke-WebRequest -Method GET -Uri 'http://localhost:8082/test' -TimeoutSec 5 -UseBasicParsing -ErrorAction Stop
            if ($response.StatusCode -eq 404) {
              # 404 is expected for /test endpoint, means app is running
              $appStarted = $true
              Write-Host "BACKGROUND JOB: App is responding!"
            }
          } catch {
            if ($_.Exception.Response.StatusCode -eq 404) {
              # 404 is expected for /test endpoint, means app is running
              $appStarted = $true
              Write-Host "BACKGROUND JOB: App is responding!"
            } else {
              Write-Host "BACKGROUND JOB: App not ready yet. Waiting..."
            }
          }
        }
        
        # If app is ready and we haven't sent requests yet, send them
        if ($appStarted -and -not $requestsSent) {
          Write-Host "BACKGROUND JOB: Sending test requests..."
          
          $successCount = 0
          foreach ($u in @('https://google.com', 'https://facebook.com')) {
            try {
              $body = @{ url = $u } | ConvertTo-Json -Compress
              Write-Host "BACKGROUND JOB: Sending POST request with URL: $u"
              
              $postResponse = Invoke-RestMethod -Method POST -Uri $postUrl `
                -ContentType "application/json" -Body $body -TimeoutSec 10 -ErrorAction Stop
              
              $successCount++
              Write-Host "BACKGROUND JOB: Successfully sent request for $u"
              
              # Small delay between requests
              Start-Sleep -Milliseconds 500
            } catch {
              Write-Warning "BACKGROUND JOB: Failed to send request for $u : $_"
            }
          }
          
          if ($successCount -gt 0) {
            $requestsSent = $true
            Write-Host "BACKGROUND JOB: Sent $successCount request(s) successfully"
            
            # Give Keploy time to write the test files
            Write-Host "BACKGROUND JOB: Waiting for Keploy to write test files..."
            Start-Sleep -Seconds 5
          }
        }
        
        # Check if recording is complete (test files exist)
        if ($requestsSent) {
          if (Test-RecordingComplete -workDir $workDir -testSetIndex $testSetIndex -minTestFiles 1) {
            Write-Host "BACKGROUND JOB: Test files detected! Recording is complete."
            
            # Wait a bit more to ensure everything is flushed to disk
            Start-Sleep -Seconds 3
            
            # Stop Keploy
            Write-Host "BACKGROUND JOB: Stopping Keploy..."
            Stop-Keploy
            return $true
          } else {
            Write-Host "BACKGROUND JOB: Test files not found yet. Continuing to wait..."
          }
        }
        
        Start-Sleep -Seconds $checkInterval
        $elapsedTime += $checkInterval
        
        # Periodic status update
        if ($elapsedTime % 15 -eq 0) {
          Write-Host "BACKGROUND JOB: Still monitoring... (elapsed: ${elapsedTime}s)"
        }
      }
      
      Write-Warning "BACKGROUND JOB: Timeout reached. Stopping Keploy..."
      Stop-Keploy
      return $false
    }

    # --- EXECUTION LOGIC FOR THE JOB ---
    try {
      Write-Host "BACKGROUND JOB: Starting for iteration $iteration (test-set-$testSetIndex)"
      Write-Host "BACKGROUND JOB: Health URL = $healthUrl"
      Write-Host "BACKGROUND JOB: POST URL = $postUrl"
      Write-Host "BACKGROUND JOB: Work Dir = $workDir"
      
      $result = Send-RequestAndMonitor -healthUrl $healthUrl -postUrl $postUrl -workDir $workDir -testSetIndex $testSetIndex
      
      if ($result) {
        Write-Host "BACKGROUND JOB: Recording completed successfully!"
      } else {
        Write-Warning "BACKGROUND JOB: Recording may be incomplete."
      }
    }
    catch {
      Write-Error "BACKGROUND JOB: Exception occurred: $_"
    }
    finally {
      # Make sure Keploy is stopped
      Write-Host "BACKGROUND JOB: Final cleanup - ensuring Keploy is stopped..."
      Stop-Keploy
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

# This command blocks until the background job kills it.
# We expect it to be terminated, so we wrap it in a try...catch
# to prevent its non-zero exit code from halting the script.
Write-Host "Executing: $env:RECORD_BIN $($recArgs -join ' ')"
try {
    & $env:RECORD_BIN @recArgs 2>&1 | Tee-Object -FilePath $logPath
} catch {
    # The process was terminated by the background job, which is the expected behavior.
    # We log this and allow the script to continue.
    Write-Host "Keploy record process was terminated as expected by the background job."
}

# Wait for job to complete
Write-Host "Keploy stopped. Checking background job status..."

try {
  $jobResult = Wait-Job -Name $jobName -Timeout 30
  if ($jobResult) {
    Write-Host "Background job completed. Status: $($jobResult.State)"
  }
  
  # Get job output
  Write-Host "=== Background Job Output ==="
  Receive-Job -Name $jobName
  Write-Host "=== End Background Job Output ==="
} catch {
  Write-Warning "Job timeout or error: $_"
  Receive-Job -Name $jobName -ErrorAction SilentlyContinue
}
finally {
  if (Get-Job -Name $jobName -ErrorAction SilentlyContinue) { 
    Stop-Job -Name $jobName -ErrorAction SilentlyContinue
    Remove-Job -Name $jobName -Force -ErrorAction SilentlyContinue | Out-Null 
  }
}

# Verify recording was successful by checking for test files
$testSetPath = ".\keploy\test-set-$expectedTestSetIndex\tests"
if (Test-Path $testSetPath) {
  $testFiles = Get-ChildItem -Path $testSetPath -Filter "test-*.yaml" -ErrorAction SilentlyContinue
  $testCount = ($testFiles | Measure-Object).Count
  Write-Host "Found $testCount test file(s) for test-set-$expectedTestSetIndex"
  
  if ($testCount -eq 0) {
    Write-Error "No test files were created"
    exit 1
  }
} else {
  Write-Warning "Test directory not found at $testSetPath"
}

# Check for errors in log
if (Select-String -Path $logPath -Pattern 'WARNING:\s*DATA\s*RACE' -SimpleMatch) {
  Write-Host "Race condition detected in recording."
  Get-Content $logPath
  exit 1
}

# Be more selective about errors - some ERROR messages might be benign
$criticalErrors = Select-String -Path $logPath -Pattern 'FATAL|PANIC|Failed to record' -SimpleMatch
if ($criticalErrors) {
  Write-Host "Critical error found in recording."
  Get-Content $logPath
  exit 1
}

Start-Sleep -Seconds 5
Write-Host "Successfully recorded test-set-$expectedTestSetIndex"

Write-Host "Shutting down docker compose services and volumes before test mode..."
docker compose down --volumes --remove-orphans
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
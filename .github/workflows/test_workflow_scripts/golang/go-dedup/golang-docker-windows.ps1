<# 
  PowerShell test runner for Keploy (Windows), adapted for the go-dedup sample.
  - Honors RECORD_BIN / REPLAY_BIN (resolved via PATH if only a file name)
  - Honors DOCKER_IMAGE_RECORD / DOCKER_IMAGE_REPLAY via KEPLOY_DOCKER_IMAGE
  - Fixes Stop-Keploy to terminate the entire process tree, preventing hangs.
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

# Parameterize the application's base URL
$env:APP_BASE_URL = if ($env:APP_BASE_URL) { $env:APP_BASE_URL } else { 'http://localhost:8080' }

Write-Host "Using RECORD_BIN = $env:RECORD_BIN"
Write-Host "Using REPLAY_BIN = $env:REPLAY_BIN"
Write-Host "Using APP_BASE_URL = $env:APP_BASE_URL"

# --- Build Docker image(s) defined by compose ---
Write-Host "Building Docker image(s) with docker compose..."
docker compose build

# --- Clean previous keploy outputs (robust; multi-location) ---
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
        # Use taskkill here as well for robustness
        taskkill /PID $_.Id /T /F | Out-Null -ErrorAction SilentlyContinue
      }
  } catch {}

  foreach ($p in $Candidates) {
    if (-not $p -or -not (Test-Path -LiteralPath $p)) { continue }
    Write-Host "Cleaning keploy directory: $p"
    try {
      # Clear pesky read-only/hidden attributes first
      cmd /c "attrib -R -S -H `"$p\*`" /S /D" 2>$null | Out-Null
      Remove-Item -LiteralPath $p -Recurse -Force -ErrorAction Stop
    } catch {
      Write-Warning "Remove-Item failed for $p, using rmdir fallback: $_"
      cmd /c "rmdir /S /Q `"$p`"" 2>$null | Out-Null
    }
  }
}

# Build candidate paths to clean
$candidates = @(".\keploy")
if ($env:GITHUB_WORKSPACE) { $candidates += (Join-Path $env:GITHUB_WORKSPACE 'keploy') }

# Also remove any old keploy.yml alongside the directory
Remove-KeployDirs -Candidates $candidates
Remove-Item -LiteralPath ".\keploy.yml" -Force -ErrorAction SilentlyContinue
Write-Host "Pre-clean complete."

# --- Generate keploy.yml ---
Write-Host "Generating keploy config..."
& $env:RECORD_BIN config --generate

# --- Update global noise in keploy.yml for go-dedup's timestamp endpoint ---
$configFile = ".\keploy.yml"
if (-not (Test-Path $configFile)) {
  throw "Config file '$configFile' not found after generation."
}
(Get-Content $configFile -Raw) -replace 'global:\s*\{\s*\}', 'global: {"body": {"current_time":[]}}' |
  Set-Content -Path $configFile -Encoding UTF8
Write-Host "Updated global noise in keploy.yml to ignore 'current_time' field."

# Function to find the GitHub Actions runner work directory
function Get-RunnerWorkPath {
  if ($env:GITHUB_WORKSPACE) {
    return $env:GITHUB_WORKSPACE
  }
  return (Get-Location).Path
}


# --- Record once ---
$containerName = "dedup-go"   # Updated container name from docker-compose.yml
$logPath = "$containerName.record.txt"
$expectedTestSetIndex = 0

  # --- SCRIPT BLOCK FOR BACKGROUND JOB ---
  $scriptBlock = {
    param(
      [string]$baseUrl,
      [int]$iteration,
      [string]$workDir,
      [int]$testSetIndex
    )
    
    # This function stops the Keploy process
    function Stop-Keploy {
      try {
        $procs = Get-Process -ErrorAction SilentlyContinue | Where-Object {
          $_.ProcessName -in @('keploy', 'keploy-record') -or 
          $_.Path -like '*keploy*.exe' -or $_.CommandLine -like '*keploy*'
        } | Sort-Object StartTime -Descending
        
        if ($procs.Count -eq 0) {
          Write-Host "BACKGROUND JOB: No Keploy process found to kill."
          return $false
        }
        
        foreach ($proc in $procs) {
          $pid = $proc.Id
          Write-Host "BACKGROUND JOB: Stopping Keploy process tree (root PID $pid)..."
          try {
            # <# --- FIX --- #>
            # Use taskkill with /T to terminate the process and its entire tree (i.e., docker compose).
            # Simply killing the keploy process would orphan `docker compose up`, causing a hang later.
            taskkill /PID $pid /T /F | Out-Null -ErrorAction SilentlyContinue -WarningAction SilentlyContinue
            Write-Host "BACKGROUND JOB: Keploy process tree for PID $pid stopped."
          } catch {
            Write-Warning "BACKGROUND JOB: Failed to stop process tree for PID $pid: $_"
          }
        }
        
        Start-Sleep -Seconds 2
        
        $remainingProcs = Get-Process -ErrorAction SilentlyContinue | Where-Object {
          $_.ProcessName -in @('keploy', 'keploy-record')
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
        [int]$minTestFiles = 5 # Expecting more tests from the varied traffic
      )
      
      $testPaths = @(
        Join-Path $workDir "keploy\test-set-$testSetIndex\tests",
        ".\keploy\test-set-$testSetIndex\tests"
      )
      
      foreach ($testPath in $testPaths) {
        Write-Host "BACKGROUND JOB: Checking for test files in: $testPath"
        
        if (Test-Path $testPath) {
          $testFiles = Get-ChildItem -Path $testPath -Filter "test-*.yaml" -ErrorAction SilentlyContinue
          $fileCount = ($testFiles | Measure-Object).Count
          
          if ($fileCount -ge $minTestFiles) {
            Write-Host "BACKGROUND JOB: Found $fileCount test file(s) in $testPath"
            
            $validFiles = 0
            foreach ($file in $testFiles) {
              if ((Get-Item $file.FullName).Length -gt 100) {
                $validFiles++
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

    # Main execution to send traffic
    function Send-RequestAndMonitor {
      param(
        [string]$baseUrl,
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
            # NOTE: Updated to check a known valid endpoint for this app
            $response = Invoke-WebRequest -Method GET -Uri "$baseUrl/timestamp" -TimeoutSec 5 -UseBasicParsing -ErrorAction Stop
            if ($response.StatusCode -eq 200) {
              $appStarted = $true
              Write-Host "BACKGROUND JOB: App is responding!"
            }
          } catch {
            Write-Host "BACKGROUND JOB: App not ready yet. Waiting..."
          }
        }
        
        if ($appStarted -and -not $requestsSent) {
          Write-Host "BACKGROUND JOB: Sending test requests to go-dedup app..."
          
          $successCount = 0
          try {
            # --- Send a variety of requests to generate diverse test cases ---
            Write-Host "BACKGROUND JOB: GET /hello/Keploy"
            Invoke-RestMethod -Method GET -Uri "$baseUrl/hello/Keploy"
            $successCount++
            
            Write-Host "BACKGROUND JOB: POST /user"
            $userBody = @{ name = "John Doe"; email = "john@keploy.io" } | ConvertTo-Json
            Invoke-RestMethod -Method POST -Uri "$baseUrl/user" -Body $userBody -ContentType "application/json"
            $successCount++

            Write-Host "BACKGROUND JOB: PUT /item/item123"
            $itemBody = @{ id = "item123"; name = "Updated Item"; price = 99.99 } | ConvertTo-Json
            Invoke-RestMethod -Method PUT -Uri "$baseUrl/item/item123" -Body $itemBody -ContentType "application/json"
            $successCount++
            
            Write-Host "BACKGROUND JOB: GET /products"
            Invoke-RestMethod -Method GET -Uri "$baseUrl/products"
            $successCount++

            Write-Host "BACKGROUND JOB: DELETE /products/prod001"
            Invoke-RestMethod -Method DELETE -Uri "$baseUrl/products/prod001"
            $successCount++

            Write-Host "BACKGROUND JOB: GET /timestamp (to test noise filtering)"
            Invoke-RestMethod -Method GET -Uri "$baseUrl/timestamp"
            $successCount++

            Write-Host "BACKGROUND JOB: GET /api/v2/users"
            Invoke-RestMethod -Method GET -Uri "$baseUrl/api/v2/users"
            $successCount++
          } catch {
             Write-Warning "BACKGROUND JOB: A request failed: $_"
          }
          
          if ($successCount -gt 0) {
            $requestsSent = $true
            Write-Host "BACKGROUND JOB: Sent $successCount request(s) successfully"
            Write-Host "BACKGROUND JOB: Waiting for Keploy to write test files..."
            Start-Sleep -Seconds 5
          }
        }
        
        if ($requestsSent) {
          if (Test-RecordingComplete -workDir $workDir -testSetIndex $testSetIndex) {
            Write-Host "BACKGROUND JOB: Test files detected! Recording is complete."
            Start-Sleep -Seconds 3
            Write-Host "BACKGROUND JOB: Stopping Keploy..."
            Stop-Keploy
            return $true
          } else {
            Write-Host "BACKGROUND JOB: Test files not found yet. Continuing to wait..."
          }
        }
        
        Start-Sleep -Seconds $checkInterval
        $elapsedTime += $checkInterval
      }
      
      Write-Warning "BACKGROUND JOB: Timeout reached. Stopping Keploy..."
      Stop-Keploy
      return $false
    }

    # --- EXECUTION LOGIC FOR THE JOB ---
    try {
      Write-Host "BACKGROUND JOB: Starting for iteration $iteration (test-set-$testSetIndex)"
      Write-Host "BACKGROUND JOB: Base URL = $baseUrl"
      Write-Host "BACKGROUND JOB: Work Dir = $workDir"
      
      $result = Send-RequestAndMonitor -baseUrl $baseUrl -workDir $workDir -testSetIndex $testSetIndex
      
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
      Write-Host "BACKGROUND JOB: Final cleanup - ensuring Keploy is stopped..."
      Stop-Keploy
    }
  }
  # --- END OF SCRIPT BLOCK ---

  $workDir = Get-RunnerWorkPath
  Write-Host "Work directory: $workDir"

# Launch traffic generator in background
$jobName = "SendRequest"
Write-Host "Starting background job: $jobName for test-set-$expectedTestIndex"
$job = Start-Job -Name $jobName -ScriptBlock $scriptBlock `
  -ArgumentList $env:APP_BASE_URL, 1, $workDir, $expectedTestSetIndex

# Configure Docker image for recording
$env:KEPLOY_DOCKER_IMAGE = if ($env:DOCKER_IMAGE_RECORD) { $env:DOCKER_IMAGE_RECORD } else { 'keploy:record' }

Write-Host "Starting keploy record (expecting test-set-$expectedTestSetIndex)..."
Write-Host "Record phase image: $env:KEPLOY_DOCKER_IMAGE"

$recArgs = @(
  'record',
  '-c', 'docker compose up',
  '--container-name', $containerName,
  '--generate-github-actions=false'
)

Write-Host "Executing: $env:RECORD_BIN $($recArgs -join ' ')"
try {
    & $env:RECORD_BIN @recArgs 2>&1 | Tee-Object -FilePath $logPath
} catch {
    Write-Host "Keploy record process was terminated as expected by the background job."
}

# Wait for job to complete
Write-Host "Keploy stopped. Checking background job status..."
try {
  Wait-Job -Name $jobName -Timeout 30 | Out-Null
  Write-Host "=== Background Job Output ==="
  Receive-Job -Name $jobName
  Write-Host "=== End Background Job Output ==="
} catch {
  Write-Warning "Job timeout or error: $_"
  Receive-Job -Name $jobName -ErrorAction SilentlyContinue
}
finally {
  Get-Job -Name $jobName -ErrorAction SilentlyContinue | Remove-Job -Force -ErrorAction SilentlyContinue
}

# Verify recording was successful
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
  Write-Error "Test directory not found at $testSetPath"
  exit 1
}

if ((Select-String -Path $logPath -Pattern 'FATAL|PANIC|Failed to record' -SimpleMatch)) {
  Write-Host "Critical error found in recording."
  Get-Content $logPath
  exit 1
}

Start-Sleep -Seconds 5
Write-Host "Successfully recorded test-set-$expectedTestSetIndex"

# --- Shut down docker services before testing ---
Write-Host "Shutting down docker compose services before test mode (preserving volumes)..."
docker compose down
Write-Host "Waiting for 5 seconds to ensure all resources are released..."
Start-Sleep -Seconds 5


# --- Test (replay) ---
$testContainer = "dedup-go" # Updated container name
$testLog = "$testContainer.test.txt"

# Configure Docker image for replay
$env:KEPLOY_DOCKER_IMAGE = if ($env:DOCKER_IMAGE_REPLAY) { $env:DOCKER_IMAGE_REPLAY } else { 'keploy:replay' }

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
if (Select-String -Path $testLog -Pattern 'FATAL|PANIC' -SimpleMatch) {
  Write-Host "Critical error found during test."
  Get-Content $testLog
  exit 1
}

# --- Parse reports and ensure test set passed ---
$allPassed = $true
$idx = 0
$report = ".\keploy\reports\test-run-0\test-set-$idx-report.yaml"
if (-not (Test-Path $report)) {
  Write-Error "Missing report file: $report"
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
  Write-Error "Some tests failed. See log for details:"
  Get-Content $testLog
  exit 1 
}
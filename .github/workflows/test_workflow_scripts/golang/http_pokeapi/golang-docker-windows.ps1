<# 
  PowerShell test runner for Keploy (Windows) - http-pokeapi Docker example.
  This script uses the robust patterns established in the working echo-sql sample.
#>

$ErrorActionPreference = 'Stop'

# --- Resolve Keploy binaries ---
if (-not $env:RECORD_BIN) { throw "RECORD_BIN environment variable not set." }
if (-not $env:REPLAY_BIN) { throw "REPLAY_BIN environment variable not set." }

Write-Host "Using RECORD_BIN = $env:RECORD_BIN"
Write-Host "Using REPLAY_BIN = $env:REPLAY_BIN"

# --- Clean previous artifacts ---
Write-Host "Cleaning keploy/ directory and config file (if they exist)..."
Remove-Item -LiteralPath ".\keploy" -Recurse -Force -ErrorAction SilentlyContinue
Remove-Item -LiteralPath ".\keploy.yml" -Force -ErrorAction SilentlyContinue

# --- Build Docker Image ---
Write-Host "Building Docker image with docker compose..."
docker compose build

# --- Generate Keploy config ---
Write-Host "Generating Keploy config..."
& $env:RECORD_BIN config --generate
# No special noise configuration is needed for this application.

# --- SCRIPT BLOCK FOR BACKGROUND TRAFFIC GENERATION ---
$scriptBlock = {
    param(
      [int]$iterationIndex
    )
    
    # --- This is the proven, robust function from echo-sql to stop the Keploy process ---
    function Stop-Keploy {
      try {
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
        Start-Sleep -Seconds 2
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

    # Main execution for the background job
    try {
        Write-Host "BACKGROUND JOB: Starting traffic generation for iteration $($iterationIndex)..."
        Start-Sleep -Seconds 6 # Initial wait for app container to start

        # Health check loop
        $appStarted = $false
        $maxWait = 60
        $elapsed = 0
        while (-not $appStarted -and $elapsed -lt $maxWait) {
            try {
                # --- FIX: Use Invoke-WebRequest for more robust health checking ---
                Invoke-WebRequest -Method GET -Uri 'http://localhost:8080/api/locations' -TimeoutSec 2 -UseBasicParsing
                $appStarted = $true
                Write-Host "BACKGROUND JOB: App is responding!"
            } catch {
                Write-Host "BACKGROUND JOB: App not ready yet. Waiting..."
                Start-Sleep -Seconds 3
                $elapsed += 3
            }
        }

        if (-not $appStarted) {
            throw "Application did not start within the timeout period."
        }
        
        # --- FIX: Use Invoke-WebRequest and manually parse JSON to avoid "response ended prematurely" error ---
        $locationsResponseJson = (Invoke-WebRequest -Method GET -Uri 'http://localhost:8080/api/locations' -UseBasicParsing -ErrorAction SilentlyContinue).Content
        $locationsResponse = $locationsResponseJson | ConvertFrom-Json
        
        if ($null -ne $locationsResponse -and $locationsResponse.location.Count -gt $iterationIndex) {
            $location = $locationsResponse.location[$iterationIndex]
            Write-Host "BACKGROUND JOB: Selected location: $location"

            $pokemonsResponseJson = (Invoke-WebRequest -Method GET -Uri "http://localhost:8080/api/locations/$location" -UseBasicParsing -ErrorAction SilentlyContinue).Content
            $pokemonsResponse = $pokemonsResponseJson | ConvertFrom-Json
            
            if ($null -ne $pokemonsResponse -and $pokemonsResponse.Count -gt $iterationIndex) {
                $pokemon = $pokemonsResponse[$iterationIndex]
                Write-Host "BACKGROUND JOB: Selected pokemon: $pokemon"
                Invoke-WebRequest -Method GET -Uri "http://localhost:8080/api/pokemon/$pokemon" -UseBasicParsing -ErrorAction SilentlyContinue
            } else {
                Write-Warning "BACKGROUND JOB: Could not get a valid pokemon from location: $location"
            }
        } else {
            Write-Warning "BACKGROUND JOB: Could not get a valid location from the locations API."
        }

        # This endpoint returns text/plain, so Invoke-WebRequest is required
        Invoke-WebRequest -Method GET -Uri 'http://localhost:8080/api/greet' -UseBasicParsing -ErrorAction SilentlyContinue
        
        Write-Host "BACKGROUND JOB: All requests sent."

        # Wait for Keploy to capture everything
        Start-Sleep -Seconds 7
    }
    catch {
      Write-Error "BACKGROUND JOB: Exception occurred: $_"
    }
    finally {
      # Make sure Keploy is stopped to unblock the main script
      Write-Host "BACKGROUND JOB: Final cleanup - stopping Keploy..."
      Stop-Keploy
    }
}
# --- END OF SCRIPT BLOCK ---


# --- Record two test sets ---
$containerName = "http-pokeapi" # This is the service name from docker-compose.yml
foreach ($i in 0..1) {
    $iteration = $i + 1
    $logPath = "${containerName}_${iteration}.txt"
    Write-Host "--- Starting recording for iteration ${iteration} ---"

    $job = Start-Job -ScriptBlock $scriptBlock -ArgumentList $i

    $recArgs = @(
      'record',
      '-c', 'docker compose up',
      '--container-name', $containerName,
      '--generate-github-actions=false'
    )
    
    try {
        & $env:RECORD_BIN @recArgs 2>&1 | Tee-Object -FilePath $logPath
    } catch {
        Write-Host "Keploy record process was terminated as expected by the background job."
    }

    Wait-Job $job
    Write-Host "--- Background Job Output (Iteration ${iteration}) ---"
    Receive-Job $job
    Write-Host "------------------------------------------------"
    Remove-Job $job

    Write-Host "Shutting down docker compose services after iteration ${iteration}..."
    docker compose down --volumes

    if (Select-String -Path $logPath -Pattern 'ERROR|FATAL|DATA RACE' -SimpleMatch) {
        Write-Error "Critical error or race condition detected in recording log for iteration ${iteration}."
        Get-Content $logPath
        exit 1
    }
    Write-Host "Successfully recorded test-set-${i}"
    Start-Sleep -Seconds 5
}


# --- Test (replay) ---
$testLog = "test_logs.txt"
Write-Host "--- Starting Keploy test run ---"

$testArgs = @(
  'test',
  '-c', 'docker compose up',
  '--container-name', $containerName,
  '--delay', '10',
  '--generate-github-actions=false'
)

& $env:REPLAY_BIN @testArgs 2>&1 | Tee-Object -FilePath $testLog

if (Select-String -Path $testLog -Pattern 'FATAL|DATA RACE' -SimpleMatch) {
  Write-Error "Critical error or race condition detected during test."
  Get-Content $testLog
  exit 1
}

# --- Parse reports ---
$allPassed = $true
foreach ($i in 0..1) {
    $report = ".\keploy\reports\test-run-0\test-set-$i-report.yaml"
    if (-not (Test-Path $report)) {
        Write-Error "Missing report file: $report"
        $allPassed = $false
        break
    }
    
    $line = Select-String -Path $report -Pattern 'status:' | Select-Object -First 1
    $status = ($line.ToString() -replace '.*status:\s*', '').Trim()
    Write-Host "Test status for test-set-${i}: $status"

    if ($status -ne 'PASSED') {
        $allPassed = $false
        Write-Error "Test-set-$i did not pass."
    }
}

if ($allPassed) { 
  Write-Host "All tests passed successfully!" 
  exit 0 
} else { 
  Write-Error "Some tests failed. See full log for details."
  exit 1 
}

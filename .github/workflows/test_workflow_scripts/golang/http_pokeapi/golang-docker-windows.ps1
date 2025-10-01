<# 
  PowerShell test runner for Keploy (Windows) - http-pokeapi Docker example.
  This script uses the robust, simplified patterns from the working echo-sql sample.
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
    # --- This is the proven, robust function from echo-sql to stop the Keploy process ---
    function Stop-Keploy {
      try {
        $procs = Get-Process -ErrorAction SilentlyContinue | Where-Object {
          $_.ProcessName -eq 'keploy' -or $_.ProcessName -eq 'keploy-record' -or 
          $_.Path -like '*keploy*.exe' -or $_.CommandLine -like '*keploy*'
        } | Sort-Object StartTime -Descending
        
        if ($procs.Count -eq 0) {
          Write-Host "BACKGROUND JOB: No Keploy process found to kill."
        } else {
          foreach ($proc in $procs) {
            Write-Host "BACKGROUND JOB: Stopping Keploy PID $($proc.Id) ($($proc.ProcessName))"
            try { Stop-Process -Id $proc.Id -Force } catch { Write-Warning "BACKGROUND JOB: Failed to stop process $($proc.Id): $_" }
          }
        }
      } catch {
        Write-Warning "BACKGROUND JOB: Failed to stop keploy: $_"
      }
    }

    # Main execution for the background job
    try {
        Write-Host "BACKGROUND JOB: Starting traffic generation..."
        Start-Sleep -Seconds 6 # Initial wait for app container to start

        # Health check loop
        $appStarted = $false
        $maxWait = 60
        $elapsed = 0
        while (-not $appStarted -and $elapsed -lt $maxWait) {
            try {
                $response = Invoke-WebRequest -Method GET -Uri 'http://localhost:8080/api/locations' -TimeoutSec 2 -UseBasicParsing
                if ($response.StatusCode -eq 200) {
                    $appStarted = $true
                    Write-Host "BACKGROUND JOB: App is responding!"
                }
            } catch {
                Write-Host "BACKGROUND JOB: App not ready yet. Waiting..."
                Start-Sleep -Seconds 3
                $elapsed += 3
            }
        }

        if (-not $appStarted) { throw "Application did not start within the timeout period." }
        
        # --- Use try/catch for each API call to ensure the job NEVER crashes ---
        try {
            $locationsResponse = Invoke-WebRequest -Method GET -Uri 'http://localhost:8080/api/locations' -UseBasicParsing
            if ($locationsResponse.StatusCode -eq 200) {
                $locations = $locationsResponse.Content | ConvertFrom-Json
                if ($null -ne $locations -and $locations.location.Count -gt 0) {
                    $location = $locations.location[0] # Just use the first one
                    Write-Host "BACKGROUND JOB: Selected location: $location"

                    try {
                        $pokemonsResponse = Invoke-WebRequest -Method GET -Uri "http://localhost:8080/api/locations/$location" -UseBasicParsing
                        if ($pokemonsResponse.StatusCode -eq 200) {
                            $pokemons = $pokemonsResponse.Content | ConvertFrom-Json
                            if ($null -ne $pokemons -and $pokemons.Count -gt 0) {
                                $pokemon = $pokemons[0] # Just use the first one
                                Write-Host "BACKGROUND JOB: Selected pokemon: $pokemon"
                                try {
                                    Invoke-WebRequest -Method GET -Uri "http://localhost:8080/api/pokemon/$pokemon" -UseBasicParsing
                                } catch { Write-Warning "BACKGROUND JOB: Request to /api/pokemon/$pokemon failed." }
                            }
                        }
                    } catch { Write-Warning "BACKGROUND JOB: Request to /api/locations/$location failed." }
                }
            }
        } catch { Write-Warning "BACKGROUND JOB: Request to /api/locations failed." }

        try {
            Invoke-WebRequest -Method GET -Uri 'http://localhost:8080/api/greet' -UseBasicParsing
        } catch { Write-Warning "BACKGROUND JOB: Request to /api/greet failed." }
        
        Write-Host "BACKGROUND JOB: All requests sent."
        Start-Sleep -Seconds 7
    }
    catch {
      Write-Error "BACKGROUND JOB: A critical, unexpected exception occurred: $_"
    }
    finally {
      # This block will always run, ensuring the main process is unblocked.
      Write-Host "BACKGROUND JOB: Final cleanup - stopping Keploy..."
      Stop-Keploy
    }
}
# --- END OF SCRIPT BLOCK ---


# --- Record one test set ---
$containerName = "http-pokeapi"
$logPath = "${containerName}_record.txt"
Write-Host "--- Starting recording for one test set ---"

$job = Start-Job -ScriptBlock $scriptBlock

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
Write-Host "--- Background Job Output ---"
Receive-Job $job
Write-Host "-----------------------------"
Remove-Job $job

Write-Host "Shutting down docker compose services..."
docker compose down --volumes

if (Select-String -Path $logPath -Pattern 'ERROR|FATAL|DATA RACE' -SimpleMatch) {
    Write-Error "Critical error detected in recording log."
    Get-Content $logPath
    exit 1
}
Write-Host "Successfully recorded test-set-0"
Start-Sleep -Seconds 5


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
  Write-Error "Critical error detected during test."
  Get-Content $testLog
  exit 1
}

# --- Parse report ---
$allPassed = $true
$report = ".\keploy\reports\test-run-0\test-set-0-report.yaml"
if (-not (Test-Path $report)) {
    Write-Error "Missing report file: $report"
    $allPassed = $false
} else {
    $line = Select-String -Path $report -Pattern 'status:' | Select-Object -First 1
    $status = ($line.ToString() -replace '.*status:\s*', '').Trim()
    Write-Host "Test status for test-set-0: $status"
    if ($status -ne 'PASSED') {
        $allPassed = $false
        Write-Error "Test-set-0 did not pass."
    }
}

if ($allPassed) { 
  Write-Host "All tests passed successfully!" 
  exit 0 
} else { 
  Write-Error "Some tests failed. See full log for details."
  exit 1 
}

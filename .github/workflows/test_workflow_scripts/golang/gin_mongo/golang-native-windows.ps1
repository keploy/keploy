<#
  PowerShell test runner for Keploy (Windows) - gin-mongo sample
#>

$ErrorActionPreference = 'Stop'

# --- Resolve Keploy binaries ---
if (-not $env:RECORD_BIN) { $env:RECORD_BIN = '.\keploy.exe' }
if (-not $env:REPLAY_BIN) { $env:REPLAY_BIN = '.\keploy.exe' }

# --- Helper: Remove Keploy Dirs Robustly ---
function Remove-KeployDirs {
    param([string[]]$Candidates)
    foreach ($p in $Candidates) {
        if (Test-Path -LiteralPath $p) {
            Write-Host "Cleaning directory: $p"
            try {
                cmd /c "attrib -R -S -H `"$p\*`" /S /D" 2>$null | Out-Null
                Remove-Item -LiteralPath $p -Recurse -Force -ErrorAction SilentlyContinue
            } catch {
                cmd /c "rmdir /S /Q `"$p`"" 2>$null | Out-Null
            }
        }
    }
}

# --- Helper: Kill Process Tree ---
function Kill-Tree {
    param([int]$ProcessId)
    if ($ProcessId -gt 0) {
        Write-Host "Killing process tree (PID $ProcessId)..."
        cmd /c "taskkill /PID $ProcessId /T /F" 2>$null | Out-Null
    }
}

# --- Helper: Send Traffic with Timeout and Log Streaming ---
function Send-Request {
    param($Job) # Accept the background job to check its status
    
    $baseUrl = "http://localhost:8080"
    $appStarted = $false
    $retries = 0
    $maxRetries = 20 # 20 * 3 seconds = 60 seconds timeout
    
    Write-Host "Waiting for App to start at $baseUrl..."
    
    # Health check loop
    while (-not $appStarted) {
        # 1. Check if the background job has crashed or failed
        if ($Job.State -ne 'Running') {
            Write-Error "Background job stopped unexpectedly (State: $($Job.State))! Dumping logs..."
            
            # Retrieve any available logs, including errors
            $logs = Receive-Job -Job $Job -Keep
            if ($logs) {
                $logs | Write-Host
            } else {
                Write-Warning "No logs captured from the background job. Possible path or startup error."
            }
            
            # Check for specific job failure reason
            if ($Job.ChildJobs[0].Error) {
                Write-Error $Job.ChildJobs[0].Error
            }
            
            throw "Application failed to start."
        }

        # 2. Print any new logs from the app while waiting
        if ($Job.HasMoreData) {
            Receive-Job -Job $Job -Keep | Write-Host
        }

        # 3. Check Timeout
        if ($retries -ge $maxRetries) {
            throw "Timeout waiting for app to start."
        }

        try {
            $response = Invoke-WebRequest -Method Post `
                -Uri "$baseUrl/url" `
                -ContentType 'application/json' `
                -Body (@{ url = "https://facebook.com" } | ConvertTo-Json) `
                -ErrorAction SilentlyContinue
            
            if ($response.StatusCode -eq 200) {
                $appStarted = $true
            }
        } catch {
            $retries++
            Start-Sleep -Seconds 3
        }
    }
    Write-Host "✅ App started."

    # Record Test Cases
    try {
        Write-Host "Sending traffic..."
        Invoke-RestMethod -Method Post -Uri "$baseUrl/url" -ContentType 'application/json' -Body (@{ url = "https://google.com" } | ConvertTo-Json) | Out-Null
        Invoke-RestMethod -Method Post -Uri "$baseUrl/url" -ContentType 'application/json' -Body (@{ url = "https://facebook.com" } | ConvertTo-Json) | Out-Null
        Invoke-WebRequest -Method Get -Uri "$baseUrl/CJBKJd92" -ErrorAction SilentlyContinue | Out-Null
        Invoke-RestMethod -Method Get -Uri "$baseUrl/verify-email?email=test@gmail.com" -Headers @{ Accept = "application/json" } | Out-Null
        Invoke-RestMethod -Method Get -Uri "$baseUrl/verify-email?email=admin@yahoo.com" -Headers @{ Accept = "application/json" } | Out-Null
        Write-Host "Traffic generation complete."
    } catch {
        Write-Warning "Error sending traffic: $_"
    }
}

# =============================================================================
# 1. Git & Environment Setup
# =============================================================================

Write-Host "Checking out branch 'native-linux'..."
git fetch origin
git checkout native-linux

# Start Mongo Service (Replaces Docker)
Write-Host "Starting local MongoDB Service..."
try {
    # Service is disabled by default on runner, enable it first
    Set-Service -Name "MongoDB" -StartupType Manual
    Start-Service -Name "MongoDB"
    Write-Host "✅ MongoDB Service started."
} catch {
    Write-Error "Failed to start MongoDB Service. Ensure it is installed (default on windows-latest)."
    Write-Error $_
    exit 1
}

# Wait a moment for Mongo to actually be ready
Write-Host "Waiting 5 seconds for MongoDB to initialize..."
Start-Sleep -Seconds 5

# Cleanup existing config
if (Test-Path "./keploy.yml") {
    Remove-Item "./keploy.yml" -Force
}

# Generate Config
Write-Host "Generating Keploy config..."
Write-Host "⏳ Executing: $env:RECORD_BIN config --generate"
& $env:RECORD_BIN config --generate

# Update Config
$configFile = ".\keploy.yml"
$configContent = Get-Content $configFile -Raw
$configContent = $configContent -replace 'global: \{\}', 'global: {"body": {"ts":[]}}'
$configContent = $configContent -replace 'ports: 0', 'ports: 27017'
Set-Content -Path $configFile -Value $configContent

# Cleanup existing tests
Remove-KeployDirs -Candidates @(".\keploy")

# Build the binary
Write-Host "Building Go binary..."
$buildCmd = 'go build -cover "-coverpkg=./..." -o ginApp.exe .'
Write-Host "⏳ Executing: $buildCmd"
Invoke-Expression $buildCmd

if (-not (Test-Path ".\ginApp.exe")) {
    Write-Error "Binary build failed. ginApp.exe not found."
    exit 1
}

# =============================================================================
# 2. Recording Phase (Loop 1..2)
# =============================================================================

for ($i = 1; $i -le 2; $i++) {
    $appName = "javaApp_${i}" 
    $logFile = "${appName}.txt"
    
    Write-Host "`n=== Iteration ${i}: Recording ==="
    
    # --- FIX: Resolve Absolute Paths for Background Job ---
    $currentDir = (Get-Location).Path
    $keployPath = (Resolve-Path $env:RECORD_BIN).Path
    $appPath    = (Resolve-Path ".\ginApp.exe").Path
    
    Write-Host "Background Job Info:"
    Write-Host "  WorkDir: $currentDir"
    Write-Host "  Keploy:  $keployPath"
    Write-Host "  App:     $appPath"

    # Start Keploy Record in Background Job (With explicit paths)
    $recJob = Start-Job -ScriptBlock {
        param($workDir, $keployBin, $appBin)
        
        # Force the job to run in the correct directory
        Set-Location -Path $workDir
        $env:Path = $using:env:Path
        
        Write-Host "Job started. Executing: $keployBin record -c $appBin"
        
        try {
            & $keployBin record -c $appBin 2>&1
        } catch {
            Write-Error "CRITICAL: Failed to launch process: $_"
        }
    } -ArgumentList $currentDir, $keployPath, $appPath

    # Drive Traffic (passing the job to check for crashes)
    try {
        Send-Request -Job $recJob
    } catch {
        Write-Error $_
        Receive-Job $recJob -Keep | Tee-Object -FilePath $logFile
        exit 1
    }

    # Wait for Keploy to process
    Write-Host "Waiting 10 seconds for recording..."
    Start-Sleep -Seconds 10

    # Find and Kill Keploy Process
    $REC_PROC = Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
      Where-Object { $_.CommandLine -match 'keploy.*record' -or $_.CommandLine -match 'ginApp.exe' } |
      Select-Object -First 1

    if ($REC_PROC) {
        Kill-Tree -ProcessId $REC_PROC.ProcessId
    } else {
        Write-Warning "Keploy process not found to kill."
    }

    # Retrieve logs from job AND PRINT THEM
    Write-Host "`n⬇️⬇️⬇️ Keploy Record Logs ($appName) ⬇️⬇️⬇️"
    Receive-Job $recJob -Keep | Tee-Object -FilePath $logFile
    Write-Host "⬆️⬆️⬆️ End Keploy Record Logs ⬆️⬆️⬆️`n"
    
    Remove-Job $recJob -Force

    # Check for Errors in logs
    if (Select-String -Path $logFile -Pattern "ERROR") {
        Write-Error "Error found in pipeline (Iteration $i)..."
        exit 1
    }
    if (Select-String -Path $logFile -Pattern "WARNING: DATA RACE") {
        Write-Error "Race condition detected in recording (Iteration $i)..."
        exit 1
    }
    
    Write-Host "Recorded test case and mocks for iteration ${i}"
}

# =============================================================================
# 3. Test Phase
# =============================================================================

Write-Host "Shutting down mongo before test mode..."
Write-Host "⏳ Executing: Stop-Service -Name MongoDB"
try {
    Stop-Service -Name "MongoDB" -Force -ErrorAction SilentlyContinue
    Write-Host "MongoDB Service stopped."
} catch {
    Write-Warning "Could not stop MongoDB service: $_"
}

Write-Host "Starting Replay..."
$testLogFile = "test_logs.txt"

# Replay also benefits from absolute paths
$keployPath = (Resolve-Path $env:REPLAY_BIN).Path
Write-Host "⏳ Executing: $keployPath test -c `"./ginApp.exe`" --delay 7"
& $keployPath test -c "./ginApp.exe" --delay 7 2>&1 | Tee-Object -FilePath $testLogFile

# =============================================================================
# 4. Validation & Coverage
# =============================================================================

$covMatch = Select-String -Path $testLogFile -Pattern "Total Coverage Percentage:\s+([0-9]+(\.[0-9]+)?)" | Select-Object -Last 1

if (-not $covMatch) {
    Write-Error "::error::No coverage percentage found in $testLogFile"
    exit 1
}

$coveragePercent = [double]$covMatch.Matches.Groups[1].Value
Write-Host "📊 Extracted coverage: ${coveragePercent}%"

if ($coveragePercent -lt 50.0) {
    Write-Error "::error::Coverage below threshold (50%). Found: ${coveragePercent}%"
    exit 1
} else {
    Write-Host "✅ Coverage meets threshold (>= 50%)"
}

if (Select-String -Path $testLogFile -Pattern "ERROR") {
    Write-Error "Error found in pipeline..."
    Get-Content $testLogFile
    exit 1
}

if (Select-String -Path $testLogFile -Pattern "WARNING: DATA RACE") {
    Write-Error "Race condition detected in test..."
    Get-Content $testLogFile
    exit 1
}

$allPassed = $true

0..1 | ForEach-Object {
    $idx = $_
    $reportFile = ".\keploy\reports\test-run-0\test-set-$idx-report.yaml"
    
    if (Test-Path $reportFile) {
        $statusLine = Select-String -Path $reportFile -Pattern "status:" | Select-Object -First 1
        $status = ($statusLine.ToString() -split ":")[1].Trim()
        
        Write-Host "Test status for test-set-${idx}: $status"
        
        if ($status -ne "PASSED") {
            $allPassed = $false
            Write-Host "Test-set-${idx} did not pass."
        }
    } else {
        Write-Warning "Report file not found for test-set-$idx"
        $allPassed = $false
    }
}

if ($allPassed) {
    Write-Host "All tests passed"
    exit 0
} else {
    Write-Error "Some tests failed. Dumping logs:"
    Get-Content $testLogFile
    exit 1
}

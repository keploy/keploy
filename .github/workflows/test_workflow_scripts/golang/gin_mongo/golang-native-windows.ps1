<#
  PowerShell test runner for Keploy (Windows) - gin-mongo sample
  UPDATED: With Debugging Steps
#>

$ErrorActionPreference = 'Stop'

# --- Resolve Keploy binaries ---
if (-not $env:RECORD_BIN) { $env:RECORD_BIN = 'keploy.exe' }
if (-not $env:REPLAY_BIN) { $env:REPLAY_BIN = 'keploy.exe' }

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

function Drain-JobOutput {
    param(
        [Parameter(Mandatory)] [System.Management.Automation.Job] $Job,
        [Parameter(Mandatory)] [string] $LogFile
    )

    # Use ChildJobs[0] to ensure the buffer is actually cleared after reading
    $data = $Job.ChildJobs[0] | Receive-Job -ErrorAction SilentlyContinue
    
    if ($null -ne $data) {
        # Pipe directly to Tee-Object to avoid extra newlines from Out-String
        $data | Tee-Object -FilePath $LogFile -Append
    }
}

# --- Helper: Send Traffic with Timeout and Log Streaming ---
function Send-Request {
    param(
        [Parameter(Mandatory)] $Job,
        [Parameter(Mandatory)] [string] $LogFile
    )

    $port = 8080
    $baseUrl = "http://localhost:$port"
    $retries = 0
    $maxRetries = 20  # 20 * 3s = 60s

    Write-Host "Waiting for Port $port to open..."

    while ($true) {
        # Print any NEW logs from job (won't repeat)
        Drain-JobOutput -Job $Job -LogFile $LogFile

        # If job died, fail fast
        if ($Job.State -ne 'Running') {
            Write-Error "Background job stopped unexpectedly (State: $($Job.State))."
            Drain-JobOutput -Job $Job -LogFile $LogFile

            if ($Job.ChildJobs -and $Job.ChildJobs[0].Error -and $Job.ChildJobs[0].Error.Count -gt 0) {
                Write-Error ($Job.ChildJobs[0].Error | Out-String)
            }
            throw "Application failed to start."
        }

        # Timeout
        if ($retries -ge $maxRetries) {
            throw "Timeout waiting for app to start."
        }

        # TCP check
        try {
            $tcpClient = New-Object System.Net.Sockets.TcpClient
            $tcpClient.Connect("localhost", $port)
            if ($tcpClient.Connected) {
                $tcpClient.Close()
                break
            }
        } catch {
            $retries++
            Start-Sleep -Seconds 3
        }
    }

    Write-Host "✅ Port $port is open. App started."
    Drain-JobOutput -Job $Job -LogFile $LogFile  # flush any last startup logs

    # Traffic
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

    Drain-JobOutput -Job $Job -LogFile $LogFile  # flush logs after traffic
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

# --- DEBUG: Check Network Listener ---
Write-Host "🔍 DEBUG: Checking Port 27017 Listener..."
$netstat = netstat -an | findstr "27017"
if (-not $netstat) {
    Write-Error "CRITICAL: Nothing is listening on port 27017. MongoDB failed to bind."
} else {
    Write-Host $netstat
    # Check for IPv4 (127.0.0.1) vs IPv6 ([::1])
    if ($netstat -match "127.0.0.1:27017") {
        Write-Host "✅ MongoDB is listening on IPv4 Loopback."
    } elseif ($netstat -match "\[::1\]:27017") {
        Write-Warning "⚠️ MongoDB is listening on IPv6 Only. 127.0.0.1 connection might fail."
    }
}
# -------------------------------------

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

# --- FIX: Patch main.go for Localhost MongoDB ---
$mainFile = ".\main.go"
if (Test-Path $mainFile) {
    Write-Host "Patching main.go to use localhost for MongoDB..."
    $txt = Get-Content $mainFile -Raw
    $txt = $txt -replace 'mongodb://mongoDb:27017', 'mongodb://127.0.0.1:27017'
    Set-Content -Path $mainFile -Value $txt
    
    # --- DEBUG: Verify Patch ---
    Write-Host "🔍 DEBUG: Verifying main.go connection string..."
    $checkPatch = Get-Content $mainFile | Select-String "mongodb://"
    Write-Host "Found line: $checkPatch"
    if ($checkPatch -notmatch "127.0.0.1") {
        Write-Warning "⚠️ Patch might have failed. Expected 127.0.0.1"
    }
    # ---------------------------
}

# Build the binary
Write-Host "Building Go binary..."
$buildCmd = 'go build -cover "-coverpkg=./..." -o ginApp.exe .'
Write-Host "⏳ Executing: $buildCmd"
Invoke-Expression $buildCmd

if (-not (Test-Path ".\ginApp.exe")) {
    Write-Error "Binary build failed. ginApp.exe not found."
    exit 1
}

# --- DEBUG: Pre-flight Connectivity Check ---
Write-Host "🔍 DEBUG: Testing direct TCP connection to MongoDB (127.0.0.1:27017)..."
try {
    $tcp = New-Object System.Net.Sockets.TcpClient
    $tcp.Connect("127.0.0.1", 27017)
    if ($tcp.Connected) {
        Write-Host "✅ Direct connection successful."
        $tcp.Close()
    }
} catch {
    Write-Error "❌ Direct connection to MongoDB failed: $_"
    Write-Warning "If this fails, the issue is Windows/Mongo, not Keploy."
}
# --------------------------------------------

# =============================================================================
# 2. Recording Phase (Loop 1..2)
# =============================================================================

for ($i = 1; $i -le 2; $i++) {
    $appName = "javaApp_${i}" 
    $logFile = "${appName}.txt"
    
    Write-Host "`n=== Iteration ${i}: Recording ==="
    
    $currentDir = (Get-Location).Path
    $keployPath = (Get-Command $env:RECORD_BIN).Source
    $appPath    = (Resolve-Path ".\ginApp.exe").Path
    
    Write-Host "Background Job Info:"
    Write-Host "  WorkDir: $currentDir"
    Write-Host "  Keploy:  $keployPath"
    Write-Host "  App:     $appPath"

    # Start Keploy Record in Background Job
    $recJob = Start-Job -ScriptBlock {
        param($workDir, $keployBin, $appBin)
        Set-Location -Path $workDir
        $env:Path = $using:env:Path
        # UPDATED: Added --debug flag
        Write-Host "Job started. Executing: $keployBin record -c $appBin --debug"
        try {
            & $keployBin record -c $appBin --debug 2>&1
        } catch {
            Write-Error "CRITICAL: Failed to launch process: $_"
        }
    } -ArgumentList $currentDir, $keployPath, $appPath

    # Drive Traffic (passing the job to check for crashes)
    try {
        Send-Request -Job $recJob -LogFile $logFile
    } catch {
        Write-Error $_
        Drain-JobOutput -Job $recJob -LogFile $logFile
        exit 1
    }

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

    Write-Host "`n⬇️⬇️⬇️ Keploy Record Logs ($appName) ⬇️⬇️⬇️"
    Drain-JobOutput -Job $recJob -LogFile $logFile
    Write-Host "⬆️⬆️⬆️ End Keploy Record Logs ⬆️⬆️⬆️`n"
    
    Remove-Job $recJob -Force

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
try {
    Stop-Service -Name "MongoDB" -Force -ErrorAction SilentlyContinue
    Write-Host "MongoDB Service stopped."
} catch {
    Write-Warning "Could not stop MongoDB service: $_"
}

Write-Host "Starting Replay..."
$testLogFile = "test_logs.txt"

$keployPath = (Get-Command $env:REPLAY_BIN).Source
Write-Host "⏳ Executing: $keployPath test -c `"./ginApp.exe`" --delay 7"
& $keployPath test -c ".\ginApp.exe" --delay 7 2>&1 | Tee-Object -FilePath $testLogFile

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

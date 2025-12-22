<#
  PowerShell test runner for Keploy (Windows) - gin-mongo sample
  FIXED: Recursive patching for IPv4 (127.0.0.1)
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
    $data = $Job.ChildJobs[0] | Receive-Job -ErrorAction SilentlyContinue
    if ($null -ne $data) {
        $data | Tee-Object -FilePath $LogFile -Append
    }
}

# --- Helper: Send Traffic ---
function Send-Request {
    param(
        [Parameter(Mandatory)] $Job,
        [Parameter(Mandatory)] [string] $LogFile
    )

    $port = 8080
    $baseUrl = "http://localhost:$port"
    $retries = 0
    $maxRetries = 20

    Write-Host "Waiting for Port $port to open..."

    while ($true) {
        Drain-JobOutput -Job $Job -LogFile $LogFile

        if ($Job.State -ne 'Running') {
            Write-Error "Background job stopped unexpectedly."
            Drain-JobOutput -Job $Job -LogFile $LogFile
            throw "Application failed to start."
        }

        if ($retries -ge $maxRetries) {
            throw "Timeout waiting for app to start."
        }

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
    Drain-JobOutput -Job $Job -LogFile $LogFile

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
    Drain-JobOutput -Job $Job -LogFile $LogFile
}

# =============================================================================
# 1. Git & Environment Setup
# =============================================================================

Write-Host "Checking out branch 'native-linux'..."
git fetch origin
git checkout native-linux

# Start Mongo Service
Write-Host "Starting local MongoDB Service..."
try {
    Set-Service -Name "MongoDB" -StartupType Manual
    Start-Service -Name "MongoDB"
    Write-Host "✅ MongoDB Service started."
} catch {
    Write-Error "Failed to start MongoDB Service."
    exit 1
}

Start-Sleep -Seconds 5

# Verify Listener (IPv4 Check)
$netstat = netstat -an | findstr "27017"
if ($netstat -match "127.0.0.1:27017") {
    Write-Host "✅ MongoDB listening on IPv4."
} else {
    Write-Warning "⚠️ MongoDB might not be on IPv4. Netstat output: $netstat"
}

# Cleanup & Config
if (Test-Path "./keploy.yml") { Remove-Item "./keploy.yml" -Force }
Write-Host "Generating Keploy config..."
& $env:RECORD_BIN config --generate

$configFile = ".\keploy.yml"
$configContent = Get-Content $configFile -Raw
$configContent = $configContent -replace 'global: \{\}', 'global: {"body": {"ts":[]}}'
$configContent = $configContent -replace 'ports: 0', 'ports: 27017'
Set-Content -Path $configFile -Value $configContent

Remove-KeployDirs -Candidates @(".\keploy")

# --- FIX: Recursively Patch ALL Go files for IPv4 ---
Write-Host "🔍 Patching all .go files to force 127.0.0.1:27017..."
Get-ChildItem -Filter "*.go" -Recurse | ForEach-Object {
    $fileContent = Get-Content $_.FullName -Raw
    $newContent = $fileContent
    
    # Replace 'mongoDb:27017' (Docker alias)
    if ($newContent -match 'mongoDb:27017') {
        Write-Host "  -> Patching mongoDb alias in $($_.Name)"
        $newContent = $newContent -replace 'mongoDb:27017', '127.0.0.1:27017'
    }
    
    # Replace 'localhost:27017' (IPv6 risk)
    if ($newContent -match 'localhost:27017') {
        Write-Host "  -> Patching localhost alias in $($_.Name)"
        $newContent = $newContent -replace 'localhost:27017', '127.0.0.1:27017'
    }

    if ($fileContent -ne $newContent) {
        Set-Content -Path $_.FullName -Value $newContent
    }
}
# ----------------------------------------------------

# Build
Write-Host "Building Go binary..."
$buildCmd = 'go build -o ginApp.exe .'
Invoke-Expression $buildCmd

if (-not (Test-Path ".\ginApp.exe")) {
    Write-Error "Binary build failed."
    exit 1
}

# =============================================================================
# 2. Recording Phase
# =============================================================================

for ($i = 1; $i -le 2; $i++) {
    $appName = "javaApp_${i}" 
    $logFile = "${appName}.txt"
    
    Write-Host "`n=== Iteration ${i}: Recording ==="
    $currentDir = (Get-Location).Path
    $keployPath = (Get-Command $env:RECORD_BIN).Source
    $appPath    = (Resolve-Path ".\ginApp.exe").Path

    # Start Keploy (Removed --debug to reduce noise, re-add if needed)
    $recJob = Start-Job -ScriptBlock {
        param($workDir, $keployBin, $appBin)
        Set-Location -Path $workDir
        $env:Path = $using:env:Path
        # Ensure we force IPv4 env vars just in case app uses them
        $env:MONGO_URI = "mongodb://127.0.0.1:27017"
        $env:URI = "mongodb://127.0.0.1:27017"
        
        & $keployBin record -c $appBin 2>&1
    } -ArgumentList $currentDir, $keployPath, $appPath

    try {
        Send-Request -Job $recJob -LogFile $logFile
    } catch {
        Write-Error $_
        Drain-JobOutput -Job $recJob -LogFile $logFile
        exit 1
    }

    Write-Host "Waiting 10 seconds for recording..."
    Start-Sleep -Seconds 10

    $REC_PROC = Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
      Where-Object { $_.CommandLine -match 'keploy.*record' -or $_.CommandLine -match 'ginApp.exe' } |
      Select-Object -First 1

    if ($REC_PROC) { Kill-Tree -ProcessId $REC_PROC.ProcessId }

    Write-Host "`n⬇️⬇️⬇️ Keploy Record Logs ($appName) ⬇️⬇️⬇️"
    Drain-JobOutput -Job $recJob -LogFile $logFile
    Write-Host "⬆️⬆️⬆️ End Keploy Record Logs ⬆️⬆️⬆️`n"
    
    Remove-Job $recJob -Force

    if (Select-String -Path $logFile -Pattern "ERROR") {
        Write-Error "Error found in pipeline..."
        exit 1
    }
    if (Select-String -Path $logFile -Pattern "WARNING: DATA RACE") {
        Write-Error "Race condition detected..."
        exit 1
    }
}

# =============================================================================
# 3. Test Phase
# =============================================================================

Write-Host "Shutting down mongo..."
Stop-Service -Name "MongoDB" -Force -ErrorAction SilentlyContinue

Write-Host "Starting Replay..."
$testLogFile = "test_logs.txt"
$keployPath = (Get-Command $env:REPLAY_BIN).Source

& $keployPath test -c ".\ginApp.exe" --delay 7 2>&1 | Tee-Object -FilePath $testLogFile

# =============================================================================
# 4. Validation
# =============================================================================

# Filter out known harmless errors (like taskkill "process not found")
$logErrors = Select-String -Path $testLogFile -Pattern "ERROR"
$realErrors = $logErrors | Where-Object { $_.Line -notmatch "The process .* not found" }

if ($realErrors) {
    Write-Error "Error found in pipeline..."
    # Optional: print the actual errors found for debugging
    $realErrors | ForEach-Object { Write-Host $_ }
    exit 1
}

$allPassed = $true
0..1 | ForEach-Object {
    $idx = $_
    $reportFile = ".\keploy\reports\test-run-0\test-set-$idx-report.yaml"
    if (Test-Path $reportFile) {
        $status = ((Select-String -Path $reportFile -Pattern "status:" | Select-Object -First 1).ToString() -split ":")[1].Trim()
        if ($status -ne "PASSED") { $allPassed = $false }
    } else { $allPassed = $false }
}

if ($allPassed) {
    Write-Host "All tests passed"
    exit 0
} else {
    Write-Error "Some tests failed."
    exit 1
}

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

# Recursively patch all Go files for IPv4
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

function Invoke-RecordIteration {
    param(
        [Parameter(Mandatory)] [int] $Iter,
        [string] $ExtraFlags = ''
    )
    $label = if ($ExtraFlags) { '_json' } else { '' }
    $appName = "ginApp_${Iter}${label}"
    $logFile = "${appName}.txt"

    Write-Host "`n=== Iteration ${Iter}${label}: Recording ==="
    $currentDir = (Get-Location).Path
    $keployPath = (Get-Command $env:RECORD_BIN).Source
    $appPath    = (Resolve-Path ".\ginApp.exe").Path

    $recJob = Start-Job -ScriptBlock {
        param($workDir, $keployBin, $appBin, $extra)
        Set-Location -Path $workDir
        $env:Path = $using:env:Path
        $env:MONGO_URI = "mongodb://127.0.0.1:27017"
        $env:URI = "mongodb://127.0.0.1:27017"
        if ($extra) {
            & $keployBin record $extra.Split(' ') -c $appBin 2>&1
        } else {
            & $keployBin record -c $appBin 2>&1
        }
    } -ArgumentList $currentDir, $keployPath, $appPath, $ExtraFlags

    try {
        Send-Request -Job $recJob -LogFile $logFile
    } catch {
        Write-Error $_
        Drain-JobOutput -Job $recJob -LogFile $logFile
        exit 1
    }

    Write-Host "Waiting 5 seconds for recording..."
    Start-Sleep -Seconds 5

    $REC_PROC = Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
      Where-Object { $_.CommandLine -match 'keploy.*record' -or $_.CommandLine -match 'ginApp.exe' } |
      Select-Object -First 1

    if ($REC_PROC) { Kill-Tree -ProcessId $REC_PROC.ProcessId }
    # Wait for keploy to flush mocks to disk
    Start-Sleep -Seconds 10

    Write-Host "`n⬇️⬇️⬇️ Keploy Record Logs ($appName) ⬇️⬇️⬇️"
    Drain-JobOutput -Job $recJob -LogFile $logFile
    Write-Host "⬆️⬆️⬆️ End Keploy Record Logs ⬆️⬆️⬆️`n"
    
    Remove-Job $recJob -Force

    # Scan for errors, but apply filter (force-kill on Windows causes expected app termination errors)
    # NativeCommandError / NativeCommandErrorMessage: Windows PowerShell 5.1
    # promotes any stderr text from a native process to a PowerShell
    # ErrorRecord when the child process is run under a Start-Job. The
    # ErrorCategory and FullyQualifiedErrorId fields literally contain the
    # substring "ERROR", so Tee-Object'd pipeline output ends up matching
    # the "ERROR" scan here even when keploy itself logged nothing of
    # the sort. pwsh (7+) formats these differently and does not emit
    # those lines into the tee'd stream, which is why the old pwsh
    # runner pool never tripped on this. Exclude both the NativeCommand
    # boilerplate and any lines prefixed with "+ " (PS's error-record
    # backtrace arrows).
    $recErrors = Select-String -Path $logFile -Pattern "ERROR"
    $filteredRecErrors = $recErrors | Where-Object {
        $_.Line -notmatch "Failed to read upstream response.*wsarecv" -and
        $_.Line -notmatch "user application terminated unexpectedly" -and
        $_.Line -notmatch "unknown error received from application" -and
        $_.Line -notmatch "NativeCommandError" -and
        $_.Line -notmatch "RemoteException" -and
        $_.Line -notmatch "^\s*\+\s" -and
        $_.Line -notmatch "ParentContainsErrorRecordException"
    }

    if ($filteredRecErrors) {
        Write-Error "Error found in pipeline..."
        $filteredRecErrors | ForEach-Object { Write-Host $_ }
        exit 1
    }
    if (Select-String -Path $logFile -Pattern "WARNING: DATA RACE") {
        Write-Error "Race condition detected..."
        exit 1
    }
}

for ($i = 1; $i -le 2; $i++) {
    Invoke-RecordIteration -Iter $i
}

# =============================================================================
# 3. Test Phase
# =============================================================================

# Keep MongoDB running during test replay. Keploy will serve mocks for
# matched requests; unmatched requests fall through to the real database
# which returns the same data recorded earlier, preventing flaky failures
# caused by non-deterministic mock matching across test sets.

Write-Host "Starting Replay..."
$testLogFile = "test_logs.txt"
$keployPath = (Get-Command $env:REPLAY_BIN).Source

& $keployPath test -c ".\ginApp.exe" --delay 20 2>&1 | Tee-Object -FilePath $testLogFile

# =============================================================================
# 4. Validation
# =============================================================================

Write-Host "Verifying test reports..."

# 1. Check for "ERROR" in logs (excluding harmless taskkill, shutdown, and wsarecv errors)
$logErrors = Select-String -Path $testLogFile -Pattern "ERROR"
$realErrors = $logErrors | Where-Object {
    $_.Line -notmatch "The process .* not found" -and
    $_.Line -notmatch "Error removing file.*keploy-logs\.txt" -and
    $_.Line -notmatch "remove keploy-logs\.txt: The process cannot access the file because it is being used by another process" -and
    $_.Line -notmatch "Failed to read upstream response.*wsarecv" -and
    $_.Line -notmatch "user application terminated unexpectedly" -and
    $_.Line -notmatch "unknown error received from application" -and
    $_.Line -notmatch "connection\(\) error occured during connection handshake" -and
    $_.Line -notmatch "dial tcp.*27017.*connectex"
}

if ($realErrors) {
    Write-Error "Real errors found in application logs..."
    $realErrors | ForEach-Object { Write-Host $_ }
    exit 1
}

# 2. Dynamic Report Validation
# Find ALL report YAML files, regardless of "test-run-X" folder name
$reportFiles = Get-ChildItem -Path ".\keploy\reports" -Filter "*report.yaml" -Recurse -ErrorAction SilentlyContinue

if (-not $reportFiles) {
    Write-Error "❌ Validation Failed: No report files found in .\keploy\reports."
    # List directory structure to help debug if it fails again
    Get-ChildItem -Path ".\keploy" -Recurse | Select-Object FullName
    exit 1
}

$anyFailed = $false

foreach ($file in $reportFiles) {
    $content = Get-Content $file.FullName
    
    # Check if the file reports a failure
    if ($content -match "status: FAILED") {
        Write-Error "❌ Test Failed in: $($file.Name)"
        $anyFailed = $true
    } 
    elseif ($content -match "status: PASSED") {
        Write-Host "✅ Verified: $($file.Name)"
    }
}

if ($anyFailed) {
    Write-Error "Some tests failed according to reports."
    exit 1
}

Write-Host "🎉 All tests passed successfully."
exit 0
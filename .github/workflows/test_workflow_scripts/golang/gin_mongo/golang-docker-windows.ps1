# PowerShell equivalent of the original bash script for Windows runners.

# CRITICAL: The original script sourced 'test-iid.sh'. You MUST convert
# 'test-iid.sh' to a PowerShell script ('test-iid.ps1') that sets any necessary
# environment variables using the syntax: $env:VAR_NAME = "value"
# Then, you would "dot source" it here like this:
# . "$env:GITHUB_WORKSPACE\.github\workflows\test_workflow_scripts\test-iid.ps1"
# For now, this line is commented out as the contents are unknown.

# --- Setup ---
Write-Host "Starting setup..."
# Ensure network exists
docker network inspect keploy-network *> $null
if ($LASTEXITCODE -ne 0) {
    docker network create keploy-network | Out-Null
}
# Ensure mongoDb container name is free, then start via compose (detached)
docker rm -f mongoDb 2>$null | Out-Null
Start-Sleep -Seconds 2
docker compose up -d mongo

# Generate the keploy-config file.
# In PowerShell, we use '&' (the call operator) to execute commands from a variable path.
# No 'sudo' is needed as the runner usually has sufficient permissions.
& $env:RECORD_BIN config --generate

# Update the global noise to ts.
# This is the PowerShell equivalent of `sed -i`. It reads the file, replaces the string, and writes it back.
$configFile = ".\keploy.yml"
(Get-Content $configFile) -replace 'global: {}', 'global: {"body": {"ts":[]}}' | Set-Content $configFile

# Remove any preexisting keploy tests and mocks.
# `Remove-Item` is the equivalent of `rm -rf`. -ErrorAction SilentlyContinue prevents errors if the folder doesn't exist.
Remove-Item -Path "keploy" -Recurse -Force -ErrorAction SilentlyContinue

docker logs mongoDb 2>$null | Out-Null

# --- Recording Phase ---
Write-Host "Starting recording phase..."
# Avoid buildx error: image already exists
docker rmi -f gin-mongo:latest 2>$null | Out-Null
docker build -t gin-mongo .
Start-Sleep -Seconds 5
# Ensure previous app container is removed
docker rm -f ginApp 2>$null | Out-Null

# PowerShell function definitions
function Stop-KeployProcess {
    Write-Host "Attempting to stop Keploy process..."
    $keployProcess = Get-Process keploy -ErrorAction SilentlyContinue
    if ($keployProcess) {
        Write-Host "Found Keploy PID: $($keployProcess.Id). Killing process."
        Stop-Process -Id $keployProcess.Id -Force
    } else {
        Write-Host "Keploy process not found."
    }
}

function Send-Requests {
    Write-Host "Request sender started. Waiting for application to be ready..."
    Start-Sleep -Seconds 10
    $app_started = $false
    while (-not $app_started) {
        try {
            # Invoke-WebRequest is the PowerShell equivalent of curl.
            Invoke-WebRequest -Uri http://localhost:8080/CJBKJd92 -UseBasicParsing
            $app_started = $true
        } catch {
            Write-Host "App not ready yet. Retrying in 3 seconds..."
            Start-Sleep -Seconds 3
        }
    }
    Write-Host "App started. Sending API requests to record."

    # Use Invoke-RestMethod for API calls as it's cleaner for JSON.
    $headers = @{ "content-type" = "application/json" }

    $body1 = '{"url": "https://google.com"}'
    Invoke-RestMethod -Method Post -Uri http://localhost:8080/url -Headers $headers -Body $body1

    $body2 = '{"url": "https://facebook.com"}'
    Invoke-RestMethod -Method Post -Uri http://localhost:8080/url -Headers $headers -Body $body2

    Invoke-WebRequest -Uri http://localhost:8080/CJBKJd92 -UseBasicParsing

    # Wait for Keploy to record the tcs and mocks.
    Write-Host "Requests sent. Waiting 5 seconds for recording to complete."
    Start-Sleep -Seconds 5
    Stop-KeployProcess
}

# Loop for recording
for ($i = 1; $i -le 2; $i++) {
    $containerName = "ginApp_${i}"
    $logFile = "${containerName}.txt"

    # Define the command to run the application
    $appCommand = "docker run -p8080:8080 --net keploy-network --rm --name $containerName gin-mongo"

    Write-Host "Starting Keploy record for iteration ${i}... (logs will stream below)"

    # Start the request sending function in a background job (this part is correct)
    $requestJob = Start-Job -ScriptBlock ${function:Send-Requests}

    Write-Host "Waiting for request sender job to complete..."
    # Start keploy record in the foreground so logs are visible; also write to file
    $recordTask = Start-Job -ScriptBlock {
        param($RecordBin, $Cmd, $ContainerName)
        & $RecordBin record -c $Cmd --container-name $ContainerName --debug
    } -ArgumentList $env:RECORD_BIN, $appCommand, $containerName

    # Give Keploy a moment to spin up before sending requests
    Start-Sleep -Seconds 5

    # Wait for requests to complete
    Wait-Job $requestJob
    Receive-Job $requestJob | Write-Host
    Remove-Job $requestJob

    # Stop Keploy after requests are sent so the record job exits
    Stop-KeployProcess

    # Stream and finalize the record job output
    Wait-Job $recordTask
    Receive-Job $recordTask | Write-Host
    Remove-Job $recordTask


    # Select-String is the PowerShell equivalent of grep.
    if (Select-String -Path $logFile -Pattern "WARNING: DATA RACE") {
        Write-Error "Race condition detected in recording, stopping pipeline..."
        Get-Content $logFile
        exit 1
    }
    # Check for "error" case-insensitively
    if (Select-String -Path $logFile -Pattern "error" -CaseSensitive:$false) {
        Write-Error "Error found in pipeline..."
        Get-Content $logFile
        exit 1
    }
    Start-Sleep -Seconds 5
    Write-Host "Recorded test case and mocks for iteration ${i}"
}

# --- Testing Phase ---
Write-Host "Shutting down mongo before test mode..."
# Prefer compose down to stop the mongo service started earlier
docker compose down 2>$null | Out-Null
# Best-effort extra cleanup if a standalone container exists
docker stop mongoDb 2>$null | Out-Null
docker rm mongoDb 2>$null | Out-Null
Write-Host "MongoDB stopped. Keploy should now use mocks."

$testContainer = "ginApp_test"
$testLogFile = "${testContainer}.txt"
Write-Host "Starting Keploy in test mode..."
& $env:REPLAY_BIN test -c 'docker run -p8080:8080 --net keploy-network --name ginApp_test gin-mongo' --containerName "$testContainer" --apiTimeout 60 --delay 20 --generate-github-actions=$false *>&1 | Set-Content -Path $testLogFile

if (Select-String -Path $testLogFile -Pattern "ERROR") {
    Write-Error "Error found in pipeline..."
    Get-Content $testLogFile
    exit 1
}
if (Select-String -Path $testLogFile -Pattern "WARNING: DATA RACE") {
    Write-Error "Race condition detected in test, stopping pipeline..."
    Get-Content $testLogFile
    exit 1
}

# --- Verification Phase ---
Write-Host "Verifying test reports..."
$all_passed = $true
foreach ($i in 0..1) {
    $reportFile = ".\keploy\reports\test-run-0\test-set-$i-report.yaml"
    if (-not (Test-Path $reportFile)) {
        Write-Error "Report file not found: $reportFile"
        $all_passed = $false
        break
    }
    # Read the YAML file and find the status line
    $statusLine = Get-Content $reportFile | Select-String -Pattern 'status:' | Select-Object -First 1
    # Split the line 'status: PASSED' at the colon and take the second part, then trim whitespace.
    $testStatus = ($statusLine -split ':')[1].Trim()

    Write-Host "Test status for test-set-${i}: $testStatus"

    if ($testStatus -ne "PASSED") {
        $all_passed = $false
        Write-Error "Test-set-$i did not pass."
        break
    }
}

if ($all_passed) {
    Write-Host "All tests passed"
    exit 0
} else {
    Write-Error "One or more tests failed."
    Get-Content $testLogFile
    exit 1
}

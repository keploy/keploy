#!/usr/bin/env pwsh

# Checkout a different branch
git fetch origin
git checkout native-linux

# Note: The sudoers modification is removed as it's not applicable/needed on Windows
# In Windows, administrative permissions are handled differently

# Start mongo before starting keploy.
docker run --rm -d -p 27017:27017 --name mongoDb mongo

# Check if there is a keploy-config file, if there is, delete it.
if (Test-Path "./keploy.yml") {
    Remove-Item -Force "./keploy.yml"
}

# Generate the keploy-config file.
& $env:RECORD_BIN config --generate

# Update the global noise to ts.
$config_file = "./keploy.yml"
$content = Get-Content $config_file -Raw
$content = $content -replace 'global: {}', 'global: {"body": {"ts":[]}}'
$content = $content -replace 'ports: 0', 'ports: 27017'
Set-Content $config_file $content

# Remove any preexisting keploy tests and mocks.
if (Test-Path "./keploy") {
    Remove-Item -Recurse -Force "./keploy"
}

# Build the binary.
go build -cover -coverpkg=./... -o ginApp.exe

function Send-Request {
    param(
        [Parameter(Mandatory=$false)]
        $KpPid
    )
    
    $app_started = $false
    while (-not $app_started) {
        try {
            $response = Invoke-RestMethod -Uri "http://localhost:8080/url" `
                -Method POST `
                -Headers @{'content-type' = 'application/json'} `
                -Body '{"url": "https://facebook.com  "}' `
                -ErrorAction Stop
            $app_started = $true
        } catch {
            Start-Sleep -Seconds 3
        }
    }
    Write-Host "App started"
    
    # Start making curl calls to record the testcases and mocks.
    Invoke-RestMethod -Uri "http://localhost:8080/url" `
        -Method POST `
        -Headers @{'content-type' = 'application/json'} `
        -Body '{"url": "https://google.com  "}'
    
    Invoke-RestMethod -Uri "http://localhost:8080/url" `
        -Method POST `
        -Headers @{'content-type' = 'application/json'} `
        -Body '{"url": "https://facebook.com  "}'
    
    Invoke-RestMethod -Uri "http://localhost:8080/CJBKJd92" -Method GET
    
    # Test email verification endpoint
    Invoke-RestMethod -Uri "http://localhost:8080/verify-email?email=test@gmail.com" `
        -Method GET `
        -Headers @{'Accept' = 'application/json'}
    
    Invoke-RestMethod -Uri "http://localhost:8080/verify-email?email=admin@yahoo.com" `
        -Method GET `
        -Headers @{'Accept' = 'application/json'}
    
    # Wait for 10 seconds for keploy to record the tcs and mocks.
    Start-Sleep -Seconds 10
    
    # Find keploy process and kill it
    $recProcess = Get-Process | Where-Object { $_.ProcessName -like "*keploy*" -or $_.Path -like "*keploy*" }
    if ($recProcess) {
        Write-Host "Killing keploy process: $($recProcess.Id)"
        Stop-Process -Id $recProcess.Id -Force -ErrorAction SilentlyContinue
    } else {
        Write-Host "No keploy process found to kill."
    }
}

# Run two iterations of recording
for ($i = 1; $i -le 2; $i++) {
    $app_name = "javaApp_$i"
    
    # Set environment variables and run keploy record
    $env:Path = $env:Path
    Start-Process -FilePath $env:RECORD_BIN `
        -ArgumentList "record", "-c", "`"./ginApp.exe`"" `
        -RedirectStandardOutput "${app_name}.txt" `
        -RedirectStandardError "${app_name}.txt" `
        -NoNewWindow -PassThru
    
    # Store the process ID
    $KEPLOY_PID = $!
    
    # Drive traffic and stop keploy
    Send-Request -KpPid $KEPLOY_PID
    
    # Check for errors in the output file
    $outputContent = Get-Content "${app_name}.txt" -Raw
    if ($outputContent -match "ERROR") {
        Write-Host "Error found in pipeline..."
        Get-Content "${app_name}.txt"
        exit 1
    }
    if ($outputContent -match "WARNING: DATA RACE") {
        Write-Host "Race condition detected in recording, stopping pipeline..."
        Get-Content "${app_name}.txt"
        exit 1
    }
    
    Start-Sleep -Seconds 5
    Write-Host "Recorded test case and mocks for iteration ${i}"
}

# Shutdown mongo before test mode - Keploy should use mocks for database interactions
Write-Host "Shutting down mongo before test mode..."
docker stop mongoDb 2>$null
docker rm mongoDb 2>$null
Write-Host "MongoDB stopped - Keploy should now use mocks for database interactions"

# Start the gin-mongo app in test mode.
$env:Path = $env:Path
& $env:REPLAY_BIN test -c "./ginApp.exe" --delay 7 2>&1 | Tee-Object -FilePath "test_logs.txt"

# Get test logs content
$testLogs = Get-Content "test_logs.txt" -Raw

# ✅ Extract and validate coverage percentage from log
$coverageLine = $testLogs | Select-String -Pattern "Total Coverage Percentage:\s+([0-9]+(?:\.[0-9]+)?)%" | Select-Object -Last 1

if (-not $coverageLine) {
    Write-Error "::error::No coverage percentage found in test_logs.txt"
    exit 1
}

$coveragePercent = [double]($coverageLine.Matches.Groups[1].Value)
Write-Host "📊 Extracted coverage: ${coveragePercent}%"

# Compare coverage with threshold (50%)
if ($coveragePercent -lt 50) {
    Write-Error "::error::Coverage below threshold (50%). Found: ${coveragePercent}%"
    exit 1
} else {
    Write-Host "✅ Coverage meets threshold (>= 50%)"
}

# Check for errors in test logs
if ($testLogs -match "ERROR") {
    Write-Host "Error found in pipeline..."
    $testLogs
    exit 1
}

if ($testLogs -match "WARNING: DATA RACE") {
    Write-Host "Race condition detected in test, stopping pipeline..."
    $testLogs
    exit 1
}

$all_passed = $true

# Get the test results from the testReport file.
for ($i = 0; $i -le 1; $i++) {
    # Define the report file for each test set
    $report_file = "./keploy/reports/test-run-0/test-set-${i}-report.yaml"
    
    if (Test-Path $report_file) {
        # Extract the test status
        $test_status = Select-String -Path $report_file -Pattern 'status:' | Select-Object -First 1
        if ($test_status) {
            $test_status = $test_status.Line -split ':' | Select-Object -Last 1 | ForEach-Object { $_.Trim() }
        }
        
        # Print the status for debugging
        Write-Host "Test status for test-set-${i}: $test_status"
        
        # Check if any test set did not pass
        if ($test_status -ne "PASSED") {
            $all_passed = $false
            Write-Host "Test-set-${i} did not pass."
            break
        }
    } else {
        Write-Host "Report file not found: $report_file"
        $all_passed = $false
        break
    }
}

# Check the overall test status and exit accordingly
if ($all_passed) {
    Write-Host "All tests passed"
    exit 0
} else {
    $testLogs
    exit 1
}
<# 
  PowerShell equivalent of the provided bash script.

  Notes:
  - Set RECORD_BIN and REPLAY_BIN env vars to your keploy.exe. 
    If not set, we fall back to C:\Users\offic\Downloads\keploy_win\keploy.exe
  - If you had logic in test-iid.sh, create a test-iid.ps1 and dot-source it where indicated.
#>

$ErrorActionPreference = 'Stop'

# --- Optional: dot-source your PS version of test-iid.sh if you have it ---
# $root = Split-Path -Parent $PSScriptRoot
# $testIid = Join-Path $root "..\..\ .github\workflows\test_workflow_scripts\test-iid.ps1"
# if (Test-Path $testIid) { . $testIid } else { Write-Host "Skipping test-iid.ps1 (not found)" }

# --- Resolve Keploy binaries (defaults for your path) ---
$defaultKeploy = 'C:\Users\offic\Downloads\keploy_win\keploy.exe'
if (-not $env:RECORD_BIN) { $env:RECORD_BIN = $defaultKeploy }
if (-not $env:REPLAY_BIN) { $env:REPLAY_BIN = $defaultKeploy }

# --- Build Docker image (compose) ---
Write-Host "Building Docker image(s)..."
docker compose build

# --- Remove any preexisting keploy tests and mocks ---
Write-Host "Cleaning .\keploy\ directory (if exists)..."
Remove-Item -LiteralPath ".\keploy" -Recurse -Force -ErrorAction SilentlyContinue

# --- Generate keploy-config file ---
Write-Host "Generating keploy config..."
& $env:RECORD_BIN config --generate

# --- Update global noise to ts in keploy.yml (sed equivalent) ---
$configFile = ".\keploy.yml"
if (-not (Test-Path $configFile)) {
  throw "Config file '$configFile' not found after generation."
}
# Replace 'global: {}' with 'global: {"body": {"ts":[]}}' (loose on whitespace)
$text = Get-Content $configFile -Raw
$text = $text -replace 'global:\s*\{\s*\}', 'global: {"body": {"ts":[]}}'
Set-Content -Path $configFile -Value $text -Encoding UTF8
Write-Host "Updated global noise in keploy.yml"

function Stop-Keploy {
  try {
    $p = Get-Process -Name 'keploy' -ErrorAction SilentlyContinue | Sort-Object StartTime -Descending | Select-Object -First 1
    if ($null -ne $p) {
      Write-Host "$($p.Id) Keploy PID"
      Write-Host "Killing keploy"
      Stop-Process -Id $p.Id -Force
    } else {
      Write-Host "No keploy process found to kill."
    }
  } catch {
    Write-Warning "Failed to stop keploy: $_"
  }
}

function Send-Request {
  Start-Sleep -Seconds 10
  $appStarted = $false
  while (-not $appStarted) {
    try {
      # Health probe
      Invoke-WebRequest -Method GET -Uri "http://localhost:8082/health" -TimeoutSec 5 | Out-Null
      $appStarted = $true
    } catch {
      Start-Sleep -Seconds 3
    }
  }
  Write-Host "App started"

  # Record some traffic
  $body1 = @{ url = "https://google.com" } | ConvertTo-Json -Compress
  Invoke-RestMethod -Method POST -Uri "http://localhost:8082/url" -ContentType "application/json" -Body $body1 | Out-Null

  $body2 = @{ url = "https://facebook.com" } | ConvertTo-Json -Compress
  Invoke-RestMethod -Method POST -Uri "http://localhost:8082/url" -ContentType "application/json" -Body $body2 | Out-Null

  # final health ping
  try { Invoke-WebRequest -Method GET -Uri "http://localhost:8082/health" -TimeoutSec 5 | Out-Null } catch {}

  # Allow keploy to finish recording, then stop it
  Start-Sleep -Seconds 5
  Stop-Keploy
}

# --- Record twice ---
for ($i = 1; $i -le 2; $i++) {
  $containerName = "echoApp"
  $logPath = "$containerName.txt"

  # Start background request job (equivalent to & in bash)
  $jobName = "SendRequest_$i"
  $job = Start-Job -Name $jobName -ScriptBlock { Send-Request }

  # Run keploy record; capture stdout+stderr and tee to file
  Write-Host "Starting keploy record (iteration $i)..."
  & $env:RECORD_BIN record -c 'docker compose up' --container-name $containerName --generateGithubActions=false 2>&1 |
    Tee-Object -FilePath $logPath

  # Wait for the background job to finish (best-effort)
  try {
    Wait-Job -Name $jobName -Timeout 120 | Out-Null
    Receive-Job -Name $jobName | Out-Null
  } catch {}
  finally {
    if (Get-Job -Name $jobName -ErrorAction SilentlyContinue) { Remove-Job -Name $jobName -Force | Out-Null }
  }

  # Check for race conditions or errors in the log
  $hasRace  = Select-String -Path $logPath -Pattern 'WARNING:\s*DATA\s*RACE' -SimpleMatch
  if ($hasRace) {
    Write-Host "Race condition detected in recording, stopping pipeline..."
    Get-Content $logPath
    exit 1
  }
  $hasError = Select-String -Path $logPath -Pattern 'ERROR' -SimpleMatch
  if ($hasError) {
    Write-Host "Error found in pipeline..."
    Get-Content $logPath
    exit 1
  }

  Start-Sleep -Seconds 5
  Write-Host "Recorded test case and mocks for iteration $i"
}

# --- Shutdown services before test mode ---
Write-Host "Shutting down docker compose services before test mode..."
docker compose down
Write-Host "Services stopped - Keploy should now use mocks for dependency interactions"

# --- Start keploy in test mode ---
$testContainer = "echoApp"
$testLog = "$testContainer.txt"
Write-Host "Starting keploy test..."
& $env:REPLAY_BIN test -c 'docker compose up' --containerName $testContainer --apiTimeout 60 --delay 20 --generate-github-actions=false 2>&1 |
  Tee-Object -FilePath $testLog

# Check test log for errors/races
$testErr = Select-String -Path $testLog -Pattern 'ERROR' -SimpleMatch
if ($testErr) {
  Write-Host "Error found in pipeline..."
  Get-Content $testLog
  exit 1
}
$testRace = Select-String -Path $testLog -Pattern 'WARNING:\s*DATA\s*RACE' -SimpleMatch
if ($testRace) {
  Write-Host "Race condition detected in test, stopping pipeline..."
  Get-Content $testLog
  exit 1
}

# --- Parse reports and ensure both test sets passed ---
$allPassed = $true
for ($idx = 0; $idx -le 1; $idx++) {
  $report = ".\keploy\reports\test-run-0\test-set-$idx-report.yaml"
  if (-not (Test-Path $report)) {
    Write-Host "Missing report file: $report"
    $allPassed = $false
    break
  }

  $line = Select-String -Path $report -Pattern 'status:' | Select-Object -First 1
  $status = ($line.ToString() -replace '.*status:\s*', '').Trim()
  Write-Host "Test status for test-set-${idx}: $status"

  if ($status -ne 'PASSED') {
    $allPassed = $false
    Write-Host "Test-set-$idx did not pass."
    break
  }
}

if ($allPassed) {
  Write-Host "All tests passed"
  exit 0
} else {
  Get-Content $testLog
  exit 1
}

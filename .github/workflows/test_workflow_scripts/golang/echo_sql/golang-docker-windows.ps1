<# 
  PowerShell test runner for Keploy (Windows).
  - Honors RECORD_BIN / REPLAY_BIN (resolved via PATH if only a file name)
  - Honors DOCKER_IMAGE_RECORD / DOCKER_IMAGE_REPLAY via KEPLOY_DOCKER_IMAGE
  - Fixes Stop-Keploy to catch keploy-record.exe as well
  - Standardizes flags to kebab-case
#>



$ErrorActionPreference = 'Stop'
# Force a user home that Docker Desktop can share
$env:USERPROFILE = 'C:\Users\offic'
$env:HOMEDRIVE  = 'C:'
$env:HOMEPATH   = '\Users\offic'
$env:HOME       = $env:USERPROFILE

# Ensure the mount sources exist and are writable
$cfg = Join-Path $env:USERPROFILE '.keploy-config'
$home= Join-Path $env:USERPROFILE '.keploy'
New-Item -ItemType Directory -Force -Path $cfg,$home | Out-Null

# Make sure the runner/Docker can write there (avoid ServiceProfiles ACLs)
icacls $cfg  /grant "Users:(OI)(CI)(M)" /T | Out-Null
icacls $home /grant "Users:(OI)(CI)(M)" /T | Out-Null

# --- Resolve Keploy binaries (defaults for local dev) ---
$defaultKeploy = 'C:\Users\offic\Downloads\keploy_win\keploy.exe'
if (-not $env:RECORD_BIN) { $env:RECORD_BIN = $defaultKeploy }
if (-not $env:REPLAY_BIN) { $env:REPLAY_BIN = $defaultKeploy }

# Optionally parameterize app URLs (kept your current defaults)
$env:APP_HEALTH_URL    = $env:APP_HEALTH_URL    ?? 'http://localhost:8082/health'
$env:APP_POST_URL      = $env:APP_POST_URL      ?? 'http://localhost:8082/url'

Write-Host "Using RECORD_BIN = $env:RECORD_BIN"
Write-Host "Using REPLAY_BIN = $env:REPLAY_BIN"

# --- Build Docker image(s) defined by compose ---
Write-Host "Building Docker image(s) with docker compose..."
docker compose build

# --- Clean previous keploy outputs ---
Write-Host "Cleaning .\keploy\ directory (if exists)..."
Remove-Item -LiteralPath ".\keploy" -Recurse -Force -ErrorAction SilentlyContinue

# --- Generate keploy.yml ---
Write-Host "Generating keploy config..."
& $env:RECORD_BIN config --generate

# --- Update global noise in keploy.yml ---
$configFile = ".\keploy.yml"
if (-not (Test-Path $configFile)) {
  throw "Config file '$configFile' not found after generation."
}
(Get-Content $configFile -Raw) -replace 'global:\s*\{\s*\}', 'global: {"body": {"ts":[]}}' |
  Set-Content -Path $configFile -Encoding UTF8
Write-Host "Updated global noise in keploy.yml"

function Stop-Keploy {
  try {
    # Match both keploy.exe and keploy-record.exe
    $procs = Get-Process -ErrorAction SilentlyContinue | Where-Object {
      $_.Name -match '^keploy(-record)?$' -or $_.Path -match 'keploy(-record)?\.exe$'
    } | Sort-Object StartTime -Descending
    $p = $procs | Select-Object -First 1
    if ($null -ne $p) {
      Write-Host "Stopping Keploy PID $($p.Id) ($($p.ProcessName))"
      Stop-Process -Id $p.Id -Force
    } else {
      Write-Host "No Keploy process found to kill."
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
      Invoke-WebRequest -Method GET -Uri $env:APP_HEALTH_URL -TimeoutSec 5 | Out-Null
      $appStarted = $true
    } catch {
      Start-Sleep -Seconds 3
    }
  }
  Write-Host "App started"

  foreach ($u in @('https://google.com','https://facebook.com')) {
    $body = @{ url = $u } | ConvertTo-Json -Compress
    Invoke-RestMethod -Method POST -Uri $env:APP_POST_URL -ContentType "application/json" -Body $body | Out-Null
  }

  try { Invoke-WebRequest -Method GET -Uri $env:APP_HEALTH_URL -TimeoutSec 5 | Out-Null } catch {}
  Start-Sleep -Seconds 5
  Stop-Keploy
}

# --- Record twice ---
for ($i = 1; $i -le 2; $i++) {
  $containerName = "echoApp"   # adjust per sample if needed
  $logPath = "$containerName.record.$i.txt"

  # Launch traffic generator in background
  $jobName = "SendRequest_$i"
  $job = Start-Job -Name $jobName -ScriptBlock { Send-Request }

  # If the workflow provided an agent image for recording, honor it
  if ($env:DOCKER_IMAGE_RECORD) {
    $env:KEPLOY_DOCKER_IMAGE = $env:DOCKER_IMAGE_RECORD
    Write-Host "Record phase will use agent image: $env:KEPLOY_DOCKER_IMAGE"
  }

  Write-Host "Starting keploy record (iteration $i)..."
  & $env:RECORD_BIN record `
      -c 'docker compose up' `
      --container-name $containerName `
      --generate-github-actions=false --debug 2>&1 | Tee-Object -FilePath $logPath

  # Wait for traffic job to finish
  try {
    Wait-Job -Name $jobName -Timeout 120 | Out-Null
    Receive-Job -Name $jobName | Out-Null
  } catch {}
  finally {
    if (Get-Job -Name $jobName -ErrorAction SilentlyContinue) { Remove-Job -Name $jobName -Force | Out-Null }
  }

  # Guard-rails
  if (Select-String -Path $logPath -Pattern 'WARNING:\s*DATA\s*RACE' -SimpleMatch) {
    Write-Host "Race condition detected in recording."
    Get-Content $logPath
    exit 1
  }
  if (Select-String -Path $logPath -Pattern 'ERROR' -SimpleMatch) {
    Write-Host "Error found in recording."
    Get-Content $logPath
    exit 1
  }

  Start-Sleep -Seconds 5
  Write-Host "Recorded test case and mocks for iteration $i"
}

# --- Stop services before test mode ---
Write-Host "Shutting down docker compose services before test mode..."
docker compose down

# --- Test (replay) ---
$testContainer = "echoApp"
$testLog = "$testContainer.test.txt"

# If the workflow provided an agent image for replay, honor it
if ($env:DOCKER_IMAGE_REPLAY) {
  $env:KEPLOY_DOCKER_IMAGE = $env:DOCKER_IMAGE_REPLAY
  Write-Host "Replay phase will use agent image: $env:KEPLOY_DOCKER_IMAGE"
}

Write-Host "Starting keploy test..."
& $env:REPLAY_BIN test `
    -c 'docker compose up' `
    --container-name $testContainer `
    --api-timeout 60 `
    --delay 20 `
    --generate-github-actions=false 2>&1 | Tee-Object -FilePath $testLog

# Check test log
if (Select-String -Path $testLog -Pattern 'ERROR' -SimpleMatch) {
  Write-Host "Error found during test."
  Get-Content $testLog
  exit 1
}
if (Select-String -Path $testLog -Pattern 'WARNING:\s*DATA\s*RACE' -SimpleMatch) {
  Write-Host "Race condition detected during test."
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

if ($allPassed) { Write-Host "All tests passed"; exit 0 } else { Get-Content $testLog; exit 1 }

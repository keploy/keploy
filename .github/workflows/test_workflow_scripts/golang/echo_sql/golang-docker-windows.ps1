<# 
  PowerShell test runner for Keploy (Windows).
  - Honors RECORD_BIN / REPLAY_BIN (resolved via PATH if only a file name)
  - Honors DOCKER_IMAGE_RECORD / DOCKER_IMAGE_REPLAY via KEPLOY_DOCKER_IMAGE
  - Gracefully stops record once tests exist; avoids multiple test sets
  - Normalizes 0xffffffff / 255 exit after graceful stop
  - Adds --debug for richer logs
#>

$ErrorActionPreference = 'Stop'

# --- Resolve Keploy binaries (defaults for local dev) ---
$defaultKeploy = 'C:\Users\offic\Downloads\keploy_win\keploy.exe'
if (-not $env:RECORD_BIN) { $env:RECORD_BIN = $defaultKeploy }
if (-not $env:REPLAY_BIN) { $env:REPLAY_BIN = $defaultKeploy }

# Ensure USERPROFILE is set (needed for docker volume mounts inside keploy)
if (-not $env:USERPROFILE -or $env:USERPROFILE -eq '') {
  $candidate = "$env:HOMEDRIVE$env:HOMEPATH"
  if ($candidate -and $candidate -ne '') { $env:USERPROFILE = $candidate }
}

# Create keploy config/data dirs so docker doesn't fall back to NetworkService profile
try {
  if ($env:USERPROFILE -and $env:USERPROFILE -ne '') {
    $keployCfg = Join-Path $env:USERPROFILE ".keploy-config"
    $keployHome = Join-Path $env:USERPROFILE ".keploy"
    New-Item -ItemType Directory -Path $keployCfg -Force -ErrorAction SilentlyContinue | Out-Null
    New-Item -ItemType Directory -Path $keployHome -Force -ErrorAction SilentlyContinue | Out-Null
  }
} catch {}

# Defaults for app URLs
$env:APP_HEALTH_URL = if ($env:APP_HEALTH_URL) { $env:APP_HEALTH_URL } else { 'http://localhost:8082/test' }
$env:APP_POST_URL   = if ($env:APP_POST_URL)   { $env:APP_POST_URL }   else { 'http://localhost:8082/url' }

Write-Host "Using RECORD_BIN = $env:RECORD_BIN"
Write-Host "Using REPLAY_BIN = $env:REPLAY_BIN"
Write-Host "Using APP_HEALTH_URL = $env:APP_HEALTH_URL"
Write-Host "Using APP_POST_URL = $env:APP_POST_URL"

# --- Build Docker image(s) defined by compose ---
Write-Host "Building Docker image(s) with docker compose..."
docker compose build

# --- Clean previous keploy outputs ---
Write-Host "Cleaning .\keploy\ directory (if exists)..."
Remove-Item -LiteralPath ".\keploy" -Recurse -Force -ErrorAction SilentlyContinue

# --- Generate keploy.yml ---
Write-Host "Generating keploy config..."
& $env:RECORD_BIN config --generate
if ($LASTEXITCODE -ne 0) { throw "keploy config --generate failed with exit code $LASTEXITCODE" }

# --- Update global noise in keploy.yml ---
$configFile = ".\keploy.yml"
if (-not (Test-Path $configFile)) {
  throw "Config file '$configFile' not found after generation."
}
(Get-Content $configFile -Raw) -replace 'global:\s*\{\s*\}', 'global: {"body": {"ts":[]}}' |
  Set-Content -Path $configFile -Encoding UTF8
Write-Host "Updated global noise in keploy.yml"

# --- Helpers ---
function Get-RunnerWorkPath {
  if ($env:GITHUB_WORKSPACE) { return $env:GITHUB_WORKSPACE }
  for ($i = 0; $i -le 10; $i++) {
    $runnerPath = "C:\actions-runners\runner-$i\_work\keploy\keploy\samples-go\echo-sql"
    if (Test-Path $runnerPath) { return $runnerPath }
  }
  return (Get-Location).Path
}

function Stop-KeployGracefully {
  $id = docker ps --filter "name=^/keploy-v2$" --format "{{.ID}}"
  if (-not $id) { return }
  Write-Host "Gracefully stopping keploy-v2..."
  docker stop --time=25 keploy-v2 2>$null | Out-Null
  Start-Sleep -Seconds 2
  $still = docker ps --filter "name=^/keploy-v2$" --format "{{.ID}}"
  if ($still) {
    docker exec keploy-v2 /bin/sh -lc "kill -INT 1" 2>$null | Out-Null
    docker wait keploy-v2 2>$null | Out-Null
  }
}

function Start-TrafficJob {
  param(
    [string]$HealthUrl = $env:APP_HEALTH_URL,
    [string]$PostUrl   = $env:APP_POST_URL,
    [string]$Name      = "SendRequest"
  )
  Write-Host "Starting background job: $Name"
  $sb = {
    param($HealthUrl, $PostUrl)
    $ErrorActionPreference = 'SilentlyContinue'
    # wait for app
    for ($i=0; $i -lt 60; $i++) {
      try { Invoke-WebRequest -Method GET -Uri 'http://localhost:8082/test' -TimeoutSec 3 -UseBasicParsing | Out-Null; break } catch {}
      Start-Sleep 1
    }
    # send a couple of calls; only once — we only want ONE test set
    foreach ($u in @('https://google.com','https://facebook.com')) {
      try {
        $body = @{ url = $u } | ConvertTo-Json -Compress
        Invoke-RestMethod -Method POST -Uri $PostUrl -ContentType "application/json" -Body $body -TimeoutSec 8 | Out-Null
        Start-Sleep -Milliseconds 400
      } catch {}
    }
  }
  Start-Job -Name $Name -ScriptBlock $sb -ArgumentList $HealthUrl, $PostUrl | Out-Null
}

function Stop-And-DrainJob {
  param([string]$Name = "SendRequest")
  $j = Get-Job -Name $Name -ErrorAction SilentlyContinue
  if ($null -ne $j) {
    Stop-Job -Job $j -Force -ErrorAction SilentlyContinue
    Receive-Job -Job $j -ErrorAction SilentlyContinue | Out-Null
    Remove-Job -Job $j -Force -ErrorAction SilentlyContinue
  }
}

function Normalize-Exit {
  param([int]$Code)
  if ($Code -eq 255 -or $Code -eq -1 -or $Code -eq 4294967295) { return 0 } # 0xffffffff
  return $Code
}

# --- Record (single set) ---
$containerName         = "echoApp"
$expectedTestSetIndex  = 0        # single test set only
$testsDir              = ".\keploy\test-set-$expectedTestSetIndex\tests"
$MinTestsToStop        = 1        # stop as soon as ≥1 test exists
$MaxRecordSeconds      = 300
$PollIntervalSeconds   = 2
$recordLog             = "$containerName.record.txt"

# image for record
if ($env:DOCKER_IMAGE_RECORD) { $env:KEPLOY_DOCKER_IMAGE = $env:DOCKER_IMAGE_RECORD } else { $env:KEPLOY_DOCKER_IMAGE = 'keploy:record' }
Write-Host "Record phase image: $env:KEPLOY_DOCKER_IMAGE"

# start background traffic (single burst)
Start-TrafficJob

# start 'keploy record' in the background, capture PID
$recArgs = @(
  'record',
  '-c', 'docker compose up',
  '--container-name', $containerName,
  '--generate-github-actions=false'
)
Write-Host "Starting 'keploy record'..."
$rec = Start-Process -FilePath $env:RECORD_BIN -ArgumentList $recArgs -RedirectStandardOutput $recordLog -PassThru -NoNewWindow

# wait until tests appear, then stop keploy gracefully
$start = Get-Date
$madeDir = $false
while ((New-TimeSpan -Start $start -End (Get-Date)).TotalSeconds -lt $MaxRecordSeconds) {
  if (-not $madeDir -and -not (Test-Path $testsDir)) {
    # compose may create dirs a bit later
    Start-Sleep -Seconds $PollIntervalSeconds
    continue
  }
  if (Test-Path $testsDir) {
    $madeDir = $true
    $count = (Get-ChildItem -Path $testsDir -Filter "*.yaml" -ErrorAction SilentlyContinue | Measure-Object).Count
    if ($count -ge $MinTestsToStop) {
      Write-Host "Detected $count test file(s) in $testsDir — stopping record."
      break
    }
  }
  Start-Sleep -Seconds $PollIntervalSeconds
}

# stop traffic then keploy container
Stop-And-DrainJob
Stop-KeployGracefully

# wait for record process to exit; nuke if it hangs
$exited = $false
try {
  $exited = $rec | Wait-Process -Timeout 40 -ErrorAction SilentlyContinue
} catch {}
if (-not $exited) {
  Write-Warning "Record process did not exit in time; terminating..."
  try { Stop-Process -Id $rec.Id -Force -ErrorAction SilentlyContinue } catch {}
}

# normalize exit
$rc = 0
try { $rc = Normalize-Exit -Code ($rec.ExitCode) } catch { $rc = 0 }
if ($rc -ne 0) {
  Write-Host "=== Record Log ==="
  Get-Content $recordLog -ErrorAction SilentlyContinue | Select-Object -Last 400
  throw "keploy record failed with exit code $rc"
}

# verify at least one test exists
if (-not (Test-Path $testsDir)) { throw "No tests directory found at $testsDir" }
$testCount = (Get-ChildItem -Path $testsDir -Filter "*.yaml" -ErrorAction SilentlyContinue | Measure-Object).Count
if ($testCount -lt $MinTestsToStop) { throw "Recording incomplete: expected at least $MinTestsToStop test(s), found $testCount" }
Write-Host "Captured $testCount test(s) in $testsDir"

# shutdown app stack before replay (keep volumes)
Write-Host "Shutting down docker compose services before test mode (preserving volumes)..."
docker compose down
Start-Sleep -Seconds 5

# --- Replay (test) ---
$testLog = "$containerName.test.txt"
if ($env:DOCKER_IMAGE_REPLAY) { $env:KEPLOY_DOCKER_IMAGE = $env:DOCKER_IMAGE_REPLAY } else { $env:KEPLOY_DOCKER_IMAGE = 'keploy:replay' }
Write-Host "Replay phase image: $env:KEPLOY_DOCKER_IMAGE"

$testArgs = @(
  'test',
  '-c', 'docker compose up',
  '--container-name', $containerName,
  '--set', "test-set-$expectedTestSetIndex",
  '--api-timeout', '60',
  '--delay', '20',
  '--generate-github-actions=false'
)
Write-Host "Starting 'keploy test'..."
& $env:REPLAY_BIN @testArgs 2>&1 | Tee-Object -FilePath $testLog

if ($LASTEXITCODE -ne 0) {
  Write-Host "=== Test Log ==="
  Get-Content $testLog -ErrorAction SilentlyContinue | Select-Object -Last 400
  throw "keploy test failed with exit code $LASTEXITCODE"
}

# quick status read (optional)
$report = ".\keploy\reports\test-run-0\test-set-$expectedTestSetIndex-report.yaml"
if (Test-Path $report) {
  $line = Select-String -Path $report -Pattern 'status:' | Select-Object -First 1
  if ($line) {
    $status = ($line.ToString() -replace '.*status:\s*', '').Trim()
    Write-Host "Test status for test-set-${expectedTestSetIndex}: $status"
  }
}

Write-Host "Replay succeeded ✅"

# final tidy (safe to remove now)
docker rm -f keploy-v2 2>$null | Out-Null
docker volume rm -f debugfs 2>$null | Out-Null

Write-Host "Done. (Tip: remove 'version:' from docker-compose to silence the deprecation warning.)"

 <#
  PowerShell test runner for Keploy (Windows) - Flask-MySQL sample

  - Cleans up previous Docker containers, networks, and Keploy files.
  - Sets up the Docker network and starts the database.
  - Builds the application's Docker image.
  - Runs the record phase, automatically making all API calls to generate tests.
  - Kills the record process cleanly.
  - MODIFIES keploy.yml to add a global noise rule for dynamic JWTs.
  - Runs the replay phase and validates the test report.
#>

$ErrorActionPreference = 'Stop'

# --- Resolve Keploy binaries (or use a default path) ---
if (-not $env:RECORD_BIN) { Write-Warning "RECORD_BIN not set. Using default."; $env:RECORD_BIN = "keploy" }
if (-not $env:REPLAY_BIN) { Write-Warning "REPLAY_BIN not set. Using default."; $env:REPLAY_BIN = "keploy" }

# --- App-specific configuration ---
$appName = "flask-mysql-app"
$appImage = "flask-mysql-app:1.0"
$appNetwork = "keploy-network"
$appBaseUrl = "http://localhost:5000"

Write-Host "Using RECORD_BIN = $env:RECORD_BIN"
Write-Host "Using REPLAY_BIN = $env:REPLAY_BIN"
Write-Host "Using APP_BASE_URL = $appBaseUrl"

# --- Helper: remove keploy dirs robustly ---
function Remove-KeployDirs {
    param([string[]]$Candidates)
    foreach ($p in $Candidates) {
        if (-not $p -or -not (Test-Path -LiteralPath $p)) { continue }
        Write-Host "Cleaning keploy directory: $p"
        try {
            Remove-Item -LiteralPath $p -Recurse -Force -ErrorAction Stop
        } catch {
            Write-Warning "Remove-Item failed for $p, using rmdir fallback: $_"
            cmd /c "rmdir /S /Q `"$p`"" 2>$null | Out-Null
        }
    }
}

# --- Helper: Kill a process tree by root PID ---
function Kill-Tree {
    param([int]$ProcessId)
    try {
        Write-Host "Stopping Keploy process tree (root PID $ProcessId)…"
        cmd /c "taskkill /PID $ProcessId /T /F" | Out-Null
    } catch {
        Write-Warning "taskkill failed for $ProcessId : $_"
    }
}

# =========================
# ========== SETUP ========
# =========================

# Clean up previous runs to ensure a fresh start
Write-Host "Performing cleanup of Docker environment..."
try { docker compose down 2>$null | Out-Null } catch {}
try { docker rm -f $appName 2>$null | Out-Null } catch {}
try { docker network rm $appNetwork 2>$null | Out-Null } catch {}
# Forcefully remove the debugfs volume that can get stuck
try { docker volume rm -f debugfs 2>$null | Out-Null } catch {}
Remove-KeployDirs -Candidates @(".\keploy")
Remove-Item -LiteralPath ".\keploy.yml" -Force -ErrorAction SilentlyContinue
Write-Host "Cleanup complete."

# Set up the environment
Write-Host "Creating Docker network: $appNetwork"
try { docker network create $appNetwork 2>$null | Out-Null } catch {}

function Wait-MySQLReady {
  param([string]$Name, [int]$TimeoutSec=180, [string]$RootPass="rootpass")
  $deadline = (Get-Date).AddSeconds($TimeoutSec)
  do {
    $state  = docker inspect -f "{{.State.Status}}" $Name 2>$null
    $health = docker inspect -f "{{if .State.Health}}{{.State.Health.Status}}{{end}}" $Name 2>$null
    if ($state -eq "running") {
      if ($health -eq "healthy") { return }
      if ([string]::IsNullOrEmpty($health)) {
        docker exec $Name mysqladmin ping -uroot -p$RootPass --silent 2>$null
        if ($LASTEXITCODE -eq 0) { return }
      }
    }
    Start-Sleep 2
  } while ((Get-Date) -lt $deadline)
  docker logs $Name --tail 200
  throw "$Name not ready in $TimeoutSec s (state=$state health=$health)"
}

Write-Host "Starting MySQL database..."
docker compose up -d db

Write-Host "Waiting for MySQL (simple-demo-db) to become ready…"
Wait-MySQLReady simple-demo-db 180 "rootpass"

Write-Host "Building Docker image: $appImage"
docker build -t $appImage .

# =========================
# ========== RECORD =======
# =========================
$logPath = "$appName.record.txt"
$expectedTestSetIndex = 0

$dockerRunCommand = "docker run -p 5000:5000 --name $appName --network $appNetwork " +
                    "-e LD_PRELOAD= " +
                    "-e DB_HOST=db -e DB_USER=demo -e DB_PASSWORD=demopass -e DB_NAME=demo " +
                    "-e DB_SSL_DISABLED=1 " +
                    "$appImage"

# Build the entire argument list as a single string, using backticks `` to embed the quotes
# This ensures Keploy receives -c "<command>" as it expects.
$recArgs = "record -c `"$dockerRunCommand`" --container-name $appName"

Write-Host "Starting keploy record (expecting test-set-$expectedTestSetIndex)…"
Write-Host "Executing: $env:RECORD_BIN $($recArgs -join ' ')"

# Start Keploy in the background and stream its logs
$proc = Start-Process -FilePath $env:RECORD_BIN -ArgumentList $recArgs -PassThru -NoNewWindow -RedirectStandardOutput $logPath
$logJob = Start-Job { Get-Content -Path $using:logPath -Wait -Tail 10 }
Write-Host "Tailing Keploy logs from $logPath ..."

# Wait for app readiness
Write-Host "Waiting for app to respond on $appBaseUrl/health …"
$deadline = (Get-Date).AddMinutes(2)
do {
  try {
    $r = Invoke-WebRequest -Method GET -Uri "$appBaseUrl/health" -TimeoutSec 5 -UseBasicParsing -ErrorAction Stop
    if ($r.StatusCode -eq 200) { break }
  } catch { Start-Sleep 3 }
} while ((Get-Date) -lt $deadline)
if ((Get-Date) -ge $deadline) { throw "App failed to start in time." }

# Send traffic to generate tests
Write-Host "Sending HTTP requests to generate tests…"
$sent = 0
try {
    # 1. Login to get JWT
    Write-Host "Logging in to get JWT token..."
    $loginResponse = Invoke-RestMethod -Method Post -Uri "$appBaseUrl/login" -ContentType "application/json" -Body '{"username": "admin", "password": "admin123"}'
    $JWT_TOKEN = $loginResponse.access_token
    $headers = @{ "Authorization" = "Bearer $JWT_TOKEN" }
    $sent++

    # 2. Health check
    Invoke-RestMethod -Uri "$appBaseUrl/health"; $sent++

    # 3. Create data
    Invoke-RestMethod -Method Post -Uri "$appBaseUrl/data" -Headers $headers -ContentType "application/json" -Body '{"message": "First data log"}'; $sent++

    # 4. Get data
    Invoke-RestMethod -Uri "$appBaseUrl/data" -Headers $headers; $sent++

    # 5. Complex queries
    Invoke-RestMethod -Uri "$appBaseUrl/generate-complex-queries" -Headers $headers; $sent++

    # 6. System status
    Invoke-RestMethod -Uri "$appBaseUrl/system/status" -Headers $headers; $sent++

    # 7. Migrations
    Invoke-RestMethod -Uri "$appBaseUrl/system/migrations" -Headers $headers; $sent++

    # 8. Check blacklisted token
    Invoke-RestMethod -Uri "$appBaseUrl/auth/check-token/9522d59c56404995af98d4c30bde72b3" -Headers $headers; $sent++

    # 9. Create log entry
    Invoke-RestMethod -Method Post -Uri "$appBaseUrl/logs" -Headers $headers -ContentType "application/json" -Body '{"event": "user_action", "details": "testing log endpoint"}'; $sent++

    # 10. Client summary
    Invoke-RestMethod -Uri "$appBaseUrl/reports/client-summary" -Headers $headers; $sent++

    # 11. Financial summary
    Invoke-RestMethod -Uri "$appBaseUrl/reports/full-financial-summary" -Headers $headers; $sent++

    # 12. Search clients
    Invoke-RestMethod -Uri "$appBaseUrl/search/clients?q=Global" -Headers $headers; $sent++
    Invoke-RestMethod -Uri "$appBaseUrl/search/clients?q=F12345" -Headers $headers; $sent++

    # 13. Fund transfer
    Invoke-RestMethod -Method Post -Uri "$appBaseUrl/transactions/transfer" -Headers $headers -ContentType "application/json" -Body '{"from_account_id": 1, "to_account_id": 2, "amount": "100.00"}'; $sent++
} catch { Write-Warning "A request failed: $_" }

Write-Host "Sent $sent request(s). Waiting for tests to flush to disk…"
Start-Sleep -Seconds 10 # Give Keploy time to write files

# Stop Keploy and the application
Kill-Tree -ProcessId $proc.Id
Stop-Job $logJob

# Clean up the containers from the record session
Write-Host "Cleaning up record session containers..."
try { docker rm -f $appName 2>$null | Out-Null } catch {}
# Forcefully remove the Keploy agent container to release the debugfs volume lock
try { docker rm -f keploy-v2 2>$null | Out-Null } catch {}


# Verify recording
$testSetPath = ".\keploy\test-set-$expectedTestSetIndex"
if (-not (Test-Path $testSetPath)) { Write-Error "Test directory not found at $testSetPath"; exit 1 }
$testCount = (Get-ChildItem -Path "$testSetPath\tests" -Filter "test-*.yaml").Count
if ($testCount -lt 12) { Write-Error "Expected at least 12 test files, but found $testCount."; exit 1 }

Write-Host "Successfully recorded $testCount test file(s) in test-set-$expectedTestSetIndex"


Write-Host "Adding global noise rule to keploy.yml to ignore 'access_token' field..."
$configPath = ".\keploy.yml"
try {
    # Read the entire file content as a single string
    $configContent = Get-Content -Path $configPath -Raw

    # Define the exact string to find (with correct indentation)
    $placeholder = '        global: {}'
    
    # Define the new configuration block to insert.
    # The indentation in this here-string is critical for valid YAML.
    $replacement = @"
        global: {
            body: {
                "access_token": [],
            }
        }
"@

    # Replace the placeholder with the actual noise configuration
    $newConfigContent = $configContent.Replace($placeholder, $replacement)

    # Write the modified content back to the file
    Set-Content -Path $configPath -Value $newConfigContent
    Write-Host "Successfully updated keploy.yml with global noise rule."
} catch {
    Write-Error "Failed to update keploy.yml with noise configuration: $_"
    exit 1
}

# =========================
# ========== REPLAY =======
# =========================

Write-Host "Shutting down database for test mode..."
docker compose down

$testLog = "$appName.test.txt"
$testCommand = "$($env:REPLAY_BIN) test -c `"$dockerRunCommand`" --delay 10 --container-name $appName"

Write-Host "Starting keploy replay…"
Write-Host "Executing: $testCommand"

# Use Invoke-Expression to run the command string, which correctly handles piping to Tee-Object
Invoke-Expression $testCommand 2>&1 | Tee-Object -FilePath $testLog


# Validate replay report
$report = ".\keploy\reports\test-run-0\test-set-0-report.yaml"
if (-not (Test-Path $report)) {
  Write-Error "Missing report file: $report"
  Get-Content $testLog -ErrorAction SilentlyContinue | Select-Object -Last 200
  exit 1
}

$line = Select-String -Path $report -Pattern 'status:' | Select-Object -First 1
$status = ($line.ToString() -replace '.*status:\s*', '').Trim()
Write-Host "Test status for test-set-0: $status"

if ($status -ne 'PASSED') {
  Write-Error "Replay failed (status: $status). See logs in $testLog"
  exit 1
}

Write-Host "All tests passed successfully!"
exit 0

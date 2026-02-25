# Cleanup script moved from workflow: stops long-running containers (>30m) and prunes Docker resources when safe.
$ErrorActionPreference = 'Stop'

# Resolve lock directory and files
$lockDir = Join-Path $env:USERPROFILE '.github-workflow-locks'
$lockFiles = @()
if (Test-Path -LiteralPath $lockDir) { $lockFiles = Get-ChildItem -Path $lockDir -Filter '*.lock' -ErrorAction SilentlyContinue }

# Ensure Docker CLI available
$dockerCmd = Get-Command docker -ErrorAction SilentlyContinue
if (-not $dockerCmd) {
  Write-Warning "Docker CLI not found on PATH. Skipping Docker cleanup."
  return
}

# Find and remove containers older than 30 minutes
$stoppedAny = $false
try {
  $cutoff = (Get-Date).AddMinutes(-30)
  $running = @(docker ps -a -q 2>$null)
  if ($running.Count -gt 0) {
    Write-Host "Checking running containers for uptime > 30 minutes..."
    foreach ($cid in $running) {
      if (-not $cid) { continue }
      try {
        $started = docker inspect --format "{{.State.StartedAt}}" $cid 2>$null
        if (-not $started) { continue }
        $startedDt = [DateTime]::Parse($started, $null, [System.Globalization.DateTimeStyles]::AssumeUniversal)
        if ($startedDt -lt $cutoff) {
          Write-Host "Container $cid started at $startedDt (older than 30m). Stopping and removing..."
          try { docker rm -f $cid 2>&1 | Write-Host; $stoppedAny = $true } catch { Write-Warning "Failed to remove container ${cid}: $($_.Exception.Message)" }
        }
      } catch { Write-Warning "Failed to inspect/parse start time for container ${cid}: $($_.Exception.Message)" }
    }
  } else { Write-Host "No running containers found." }
} catch { Write-Warning "Docker check for long-running containers failed: $($_.Exception.Message)" }

# Decide whether to prune
$shouldPrune = $false
if (-not $lockFiles -or $lockFiles.Count -eq 0) {
  Write-Host "No lock files found. Will perform Docker cleanup."
  if (Test-Path -LiteralPath $lockDir) { Remove-Item -Path $lockDir -Recurse -Force -ErrorAction SilentlyContinue }
  $shouldPrune = $true
} elseif ($stoppedAny) {
  Write-Host "Stopped long-running containers. Will perform Docker cleanup despite lock files."
  $shouldPrune = $true
} else {
  $count = $lockFiles.Count
  Write-Host "Lock files still present ($count). Skipping Docker cleanup."
}

# Perform prune if needed
if ($shouldPrune) {
  Write-Host "Pruning Docker images (this will remove dangling and unused images)..."
  try { docker image prune -af 2>&1 | Write-Host } catch { Write-Warning "docker image prune failed: $($_.Exception.Message)" }
  Write-Host "Pruning Docker volumes (this will remove unused volumes)..."
  try { docker volume prune -f 2>&1 | Write-Host } catch { Write-Warning "docker volume prune failed: $($_.Exception.Message)" }
  Write-Host "Removing unused builder cache and performing system prune (including containers/networks)..."
  try { docker system prune -af --volumes 2>&1 | Write-Host } catch { Write-Warning "docker system prune failed: $($_.Exception.Message)" }
}

# Output whether we stopped any containers (for workflow conditionals)
# If running inside GitHub Actions, write to the GITHUB_OUTPUT file so the step can expose outputs
if ($env:GITHUB_OUTPUT) {
  try {
    "stopped_any=$stoppedAny" | Out-File -FilePath $env:GITHUB_OUTPUT -Encoding utf8 -Append
  } catch {
    Write-Warning "Failed to write to GITHUB_OUTPUT: $($_.Exception.Message)"
    Write-Output "stopped_any=$stoppedAny"
  }
} else {
  Write-Output "stopped_any=$stoppedAny"
}

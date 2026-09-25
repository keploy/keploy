# Post-run cleanup of the self-hosted Windows runners' shared Docker VM:
# remove containers no live job can own, then prune Docker resources once no
# job is using them.
#
# win-runner-1..4 run on ONE machine against ONE Docker daemon, and this runs
# (from cleanup_windows) while sibling runners are mid-job, so neither half may
# touch what a live job needs:
#
# - Containers: the same ownership rule as the pre-job reap, by running it over
#   every container (reap-keploy-containers.ps1 -NamePrefix ''). This used to
#   `docker rm -f` any container whose StartedAt was before the host's clock
#   minus 30 minutes. A created-but-not-started container reports StartedAt
#   0001-01-01, so that removed every sibling container waiting in the Created
#   state - an application container sits there for ~6s on every `compose up`
#   while compose waits for its keploy agent to turn healthy.
#
# - Prune: `docker system prune -af --volumes` removes every stopped (including
#   Created) container and every image, network and volume no container uses,
#   so it runs only when no lock is live, under the marker handshake that keeps
#   a job from starting on Docker mid-prune. docker-locks.ps1 describes both.
#   The API calls come before the marker goes down, so a slow API does not keep
#   starting jobs waiting on a prune that live locks will call off anyway.
#   Stale locks are deleted. Removing a container never forces a prune past
#   live locks.
[CmdletBinding()]
param(
    [string]$LockDir = '',
    [int]$MinAgeMinutes = 45,
    [int]$RunLockMaxAgeHours = 24,
    # take-docker-job-lock.ps1's bound: a job stops waiting for a marker this
    # old, because only a prune or Docker restart killed before its finally
    # block leaves one that old behind (cleanup_windows has a 10-minute
    # timeout, and ensure-docker.ps1's restart is bounded well below this).
    # Such markers are deleted here.
    [int]$PruneMaxMinutes = 15,
    # The repository whose runs a lock names, for locks that do not say.
    [string]$Repository = $env:GITHUB_REPOSITORY,
    # Only the tests override these. GetRunStatus takes (repository, run ID,
    # attempt or '') and returns the run's status, or $null when it could not
    # find out.
    [string]$DockerExe = 'docker',
    [string]$WedgeDir = '',
    [scriptblock]$GetRunStatus = $null
)
$ErrorActionPreference = 'Continue'

if (-not $LockDir) {
    $base = $env:USERPROFILE
    if (-not $base) { $base = $HOME }
    $LockDir = Join-Path $base '.github-workflow-locks'
}

if (-not (Get-Command $DockerExe -ErrorAction SilentlyContinue)) {
    Write-Warning "Docker CLI not found on PATH. Skipping Docker cleanup."
    return
}

# Containers. Warn rather than fail: a teardown that fails the workflow would
# hide the real result of the run.
try {
    $reaper = Join-Path $PSScriptRoot 'reap-keploy-containers.ps1'
    $reapArgs = @{ NamePrefix = ''; MinAgeMinutes = $MinAgeMinutes; DockerExe = $DockerExe }
    if ($WedgeDir) { $reapArgs.WedgeDir = $WedgeDir }
    if (Test-Path $reaper) { & $reaper @reapArgs } else { Write-Warning "reaper script not found at $reaper" }
} catch {
    Write-Warning "container reap failed: $($_.Exception.Message)"
}

. (Join-Path $PSScriptRoot 'docker-locks.ps1')

New-Item -ItemType Directory -Force -Path $LockDir | Out-Null
foreach ($old in @(Get-ChildItem -LiteralPath $LockDir -Filter 'docker-prune-*.inprogress' -ErrorAction SilentlyContinue)) {
    $oldAge = [DateTime]::UtcNow - $old.LastWriteTimeUtc
    if ($oldAge.TotalMinutes -ge $PruneMaxMinutes) {
        Write-Host "Deleting $($old.Name), left $([int]$oldAge.TotalMinutes) min ago by a prune or Docker restart that was killed; jobs no longer wait for it."
        Remove-Item -LiteralPath $old.FullName -Force -ErrorAction SilentlyContinue
    }
}

# 1. Judge the locks, asking the Actions API, with no marker down.
$locks = Resolve-DockerLocks -LockDir $LockDir -JobLockMaxMinutes $MinAgeMinutes -RunLockMaxAgeHours $RunLockMaxAgeHours -Repository $Repository -GetRunStatus $GetRunStatus
if ($locks.Live.Count -gt 0) {
    Write-Host "Skipping Docker prune: $($locks.Live.Count) live lock(s) - $($locks.Live -join ', ')."
    return
}

# 2. Marker down, then one more look at the locks, with no API call. The
# marker is down only for that look and the prune.
try {
    $exclusive = Enter-DockerExclusive -LockDir $LockDir -Stale $locks.Stale -Operation 'a Docker prune'
} catch {
    Write-Warning "could not put down the prune marker ($($_.Exception.Message)); not pruning."
    return
}
if (-not $exclusive.Marker) {
    Write-Host "Skipping Docker prune: $($exclusive.Taken.Count) lock(s) taken while the others were judged - $($exclusive.Taken -join ', ')."
    return
}
try {
    Write-Host "No live locks. Pruning Docker images, volumes, networks and build cache..."
    try { & $DockerExe image prune -af 2>&1 | Write-Host } catch { Write-Warning "docker image prune failed: $($_.Exception.Message)" }
    try { & $DockerExe volume prune -f 2>&1 | Write-Host } catch { Write-Warning "docker volume prune failed: $($_.Exception.Message)" }
    try { & $DockerExe system prune -af --volumes 2>&1 | Write-Host } catch { Write-Warning "docker system prune failed: $($_.Exception.Message)" }
} finally {
    Remove-Item -LiteralPath $exclusive.Marker -Force -ErrorAction SilentlyContinue
}

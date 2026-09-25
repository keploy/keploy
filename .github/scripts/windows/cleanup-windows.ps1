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
#   so it runs only when no lock file is live. A lock is live until it is older
#   than the longest its holder can legitimately run:
#     docker-job-*.lock   held by each golang_docker_windows job from before it
#                         loads its images until its teardown; the job cannot
#                         outlive its timeout, so older than -MinAgeMinutes
#                         (the reaper's threshold, >= that timeout) is stale.
#     anything else       the run-level prepare-windows-workflow-<run>.lock,
#                         held from build-windows-amd64 until this job; stale
#                         after -RunLockMaxAgeHours, the age the macOS twin's
#                         lock sweep also uses.
#   Stale locks are deleted, so a run that never reached cleanup cannot block
#   pruning forever. Removing a container no longer forces a prune past live
#   locks. Lock ages compare the file's write time with the host clock that
#   wrote it.
[CmdletBinding()]
param(
    [string]$LockDir = '',
    [int]$MinAgeMinutes = 40,
    [int]$RunLockMaxAgeHours = 24,
    # Only the tests override these.
    [string]$DockerExe = 'docker',
    [string]$WedgeDir = ''
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

# Locks.
$live = @()
$nowUtc = [DateTime]::UtcNow
if (Test-Path -LiteralPath $LockDir) {
    foreach ($lock in @(Get-ChildItem -LiteralPath $LockDir -Filter '*.lock' -ErrorAction SilentlyContinue)) {
        $maxAge = [TimeSpan]::FromHours($RunLockMaxAgeHours)
        if ($lock.Name -like 'docker-job-*') { $maxAge = [TimeSpan]::FromMinutes($MinAgeMinutes) }
        $age = $nowUtc - $lock.LastWriteTimeUtc
        if ($age -ge $maxAge) {
            Write-Host "Deleting stale lock $($lock.Name) ($([int]$age.TotalMinutes) min old; its holder cannot run longer than $([int]$maxAge.TotalMinutes) min)."
            Remove-Item -LiteralPath $lock.FullName -Force -ErrorAction SilentlyContinue
        } else {
            $live += $lock.Name
        }
    }
}
if ($live.Count -gt 0) {
    Write-Host "Skipping Docker prune: $($live.Count) live lock(s) - $($live -join ', ')."
    return
}

Write-Host "No live locks. Pruning Docker images, volumes, networks and build cache..."
try { & $DockerExe image prune -af 2>&1 | Write-Host } catch { Write-Warning "docker image prune failed: $($_.Exception.Message)" }
try { & $DockerExe volume prune -f 2>&1 | Write-Host } catch { Write-Warning "docker volume prune failed: $($_.Exception.Message)" }
try { & $DockerExe system prune -af --volumes 2>&1 | Write-Host } catch { Write-Warning "docker system prune failed: $($_.Exception.Message)" }

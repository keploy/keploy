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
#   so it runs only when no lock file is live. A lock is live while the run
#   that holds it is not over, and never longer than its holder can
#   legitimately run:
#     docker-job-<run>-<attempt>-*.lock   held by each golang_docker_windows
#                         job from before it builds or loads its images until
#                         its teardown. Stale once that run attempt has
#                         completed, or once older than -MinAgeMinutes (the
#                         reaper's threshold, above the job's timeout).
#     prepare-windows-workflow-<run>.lock the run-level lock, held from
#                         build-windows-amd64 until cleanup_windows. Stale once
#                         that run has completed, or once older than
#                         -RunLockMaxAgeHours (the age the macOS twin's lock
#                         sweep also uses).
#   Whether a run is over is asked of the Actions API (the run ID is in the
#   file name, the repository in the file), not inferred from the lock still
#   being there. Only the run's own cleanup_windows deletes its run lock, and a
#   run cancelled by a newer push can have that job cancelled at "Set up job",
#   before any step (run 35707841215); its lock then blocked every prune on
#   the machine. Nothing used to delete such locks, and 42-45 of them sat in the
#   directory from at least 2026-09-20, so the no-lock prune never ran. When
#   the API cannot answer, the age bound alone decides. Stale locks are
#   deleted. Removing a container never forces a prune past live locks. Lock
#   ages compare the file's write time with the host clock that wrote it.
#
#   A job that takes its lock while a prune is already under way is not saved
#   by the lock alone, since the check came first. So the prune announces
#   itself with a marker (docker-prune-*.inprogress) BEFORE it checks the locks,
#   and a job checks for that marker AFTER writing its lock
#   (take-docker-job-lock.ps1) and waits until the prune is over. Whichever
#   comes second sees the other: either this script sees the job's lock and
#   does not prune, or the job sees the marker and waits.
[CmdletBinding()]
param(
    [string]$LockDir = '',
    [int]$MinAgeMinutes = 45,
    [int]$RunLockMaxAgeHours = 24,
    # take-docker-job-lock.ps1's bound: a job stops waiting for a prune marker
    # this old, because only a cleanup killed before its finally block (past
    # cleanup_windows' 10-minute timeout) leaves one that old behind. Such
    # markers are deleted here.
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

# Whether a run (or one attempt of it) is still going, from the Actions API.
# keploy/keploy is public, so this needs no token; GITHUB_TOKEN is sent when
# the step provides one, for the higher rate limit.
function Get-RunStatusFromApi([string]$Repo, [string]$RunId, [string]$Attempt) {
    if (-not $Repo) { return $null }
    $uri = "https://api.github.com/repos/$Repo/actions/runs/$RunId"
    if ($Attempt) { $uri = "$uri/attempts/$Attempt" }
    $headers = @{ Accept = 'application/vnd.github+json'; 'User-Agent' = 'keploy-cleanup-windows' }
    if ($env:GITHUB_TOKEN) { $headers.Authorization = "Bearer $($env:GITHUB_TOKEN)" }
    try {
        # Windows PowerShell 5.1 on .NET Framework may not offer TLS 1.2 by default.
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
        return "$((Invoke-RestMethod -Uri $uri -Headers $headers -TimeoutSec 15 -UseBasicParsing).status)"
    } catch {
        # A 404 is no answer either: besides a deleted run, it is what a token
        # that cannot see $Repo gets, or a repository misread from the lock.
        # Taking it for "over" would delete a live lock; a deleted run's lock
        # ages out instead.
        Write-Host "  could not ask the Actions API about run $RunId of $Repo ($($_.Exception.Message)); judging its lock by age."
        return $null
    }
}
if (-not $GetRunStatus) { $GetRunStatus = ${function:Get-RunStatusFromApi} }

# Locks. The marker goes down first: see the header.
New-Item -ItemType Directory -Force -Path $LockDir | Out-Null
foreach ($old in @(Get-ChildItem -LiteralPath $LockDir -Filter 'docker-prune-*.inprogress' -ErrorAction SilentlyContinue)) {
    $oldAge = [DateTime]::UtcNow - $old.LastWriteTimeUtc
    if ($oldAge.TotalMinutes -ge $PruneMaxMinutes) {
        Write-Host "Deleting $($old.Name), left $([int]$oldAge.TotalMinutes) min ago by a cleanup that was killed mid-prune; jobs no longer wait for it."
        Remove-Item -LiteralPath $old.FullName -Force -ErrorAction SilentlyContinue
    }
}
# One marker per cleanup, so one cleanup finishing does not take down the
# marker of another that is still pruning.
$marker = Join-Path $LockDir ("docker-prune-{0}.inprogress" -f [guid]::NewGuid().ToString('N'))
Set-Content -LiteralPath $marker -Value "$env:GITHUB_REPOSITORY run $env:GITHUB_RUN_ID $env:RUNNER_NAME"
try {
    $live = @()
    $nowUtc = [DateTime]::UtcNow
    foreach ($lock in @(Get-ChildItem -LiteralPath $LockDir -Filter '*.lock' -ErrorAction SilentlyContinue)) {
        $maxAge = [TimeSpan]::FromHours($RunLockMaxAgeHours)
        $runId = ''; $attempt = ''
        if ($lock.Name -match '^docker-job-(\d+)-(\d+)-') {
            $maxAge = [TimeSpan]::FromMinutes($MinAgeMinutes)
            $runId = $Matches[1]; $attempt = $Matches[2]
        } elseif ($lock.Name -match '^prepare-windows-workflow-(\d+)\.lock$') {
            $runId = $Matches[1]
        }
        $age = $nowUtc - $lock.LastWriteTimeUtc
        $why = ''
        if ($age -ge $maxAge) {
            $why = "$([int]$age.TotalMinutes) min old; its holder cannot run longer than $([int]$maxAge.TotalMinutes) min"
        } elseif ($runId) {
            # Both kinds of lock start with their repository (older run
            # locks do not, and were only ever written by this repository).
            $repo = $Repository
            $first = "$(Get-Content -LiteralPath $lock.FullName -TotalCount 1 -ErrorAction SilentlyContinue)".Trim().Split(' ')[0]
            if ($first -match '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') { $repo = $first }
            $status = & $GetRunStatus $repo $runId $attempt
            if ($status -eq 'completed') {
                $what = "run $runId"
                if ($attempt) { $what = "attempt $attempt of run $runId" }
                $why = "$what of $repo is $status"
            }
        }
        if ($why) {
            Write-Host "Deleting stale lock $($lock.Name) ($why)."
            Remove-Item -LiteralPath $lock.FullName -Force -ErrorAction SilentlyContinue
        } else {
            $live += $lock.Name
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
} finally {
    Remove-Item -LiteralPath $marker -Force -ErrorAction SilentlyContinue
}

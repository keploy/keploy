# Dot-sourced by cleanup-windows.ps1 and ensure-docker.ps1: which of this
# machine's Docker locks are live, and the handshake a whole-daemon operation
# uses so that no job starts on Docker under it. There are two such
# operations: cleanup-windows.ps1's prune and ensure-docker.ps1's restart of
# Docker Desktop. Both destroy what live jobs need, on the ONE daemon that
# win-runner-1..4 share.
#
# THE LOCKS, in -LockDir (%USERPROFILE%\.github-workflow-locks, which every
# runner on the machine shares). A lock is live while the run that holds it is
# not over. It is never live for longer than its holder can legitimately run:
#   docker-job-<run>-<attempt>-*.lock   held by each golang_docker_windows
#                       job, from before it builds or loads its images until
#                       its teardown (take-docker-job-lock.ps1). Stale once that
#                       run attempt has completed, or once older than
#                       -JobLockMaxMinutes (the reaper's MinAgeMinutes, above the
#                       job's timeout).
#   prepare-windows-workflow-<run>.lock the run-level lock, held from
#                       build-windows-amd64 until cleanup_windows. Stale once
#                       that run has completed, or once older than
#                       -RunLockMaxAgeHours (the age the macOS twin's lock sweep
#                       also uses).
# Whether a run is over is asked of the Actions API: the run ID is in the file
# name and the repository is in the file. It is not inferred from the lock
# still being there. Only the run's own cleanup_windows deletes its run lock,
# and a run cancelled by a newer push can have that job cancelled at "Set up
# job", before any step runs (run 35707841215). Its lock then blocked every
# prune on the machine: 42-45 such locks sat in the directory from at least
# 2026-09-20. When the API cannot answer, the age bound alone decides. Lock
# ages compare the file's write time with the host clock that wrote it.
#
# THE HANDSHAKE. A job that takes its lock after the operation has checked the
# locks is not protected by the lock alone. So the operation puts down a
# docker-prune-*.inprogress marker BEFORE its final look at the locks. The job
# looks for markers AFTER writing its lock and waits while one is down
# (take-docker-job-lock.ps1). Whichever comes second sees the other.
#   1. Resolve-DockerLocks judges every lock and deletes the stale ones, with
#      no marker down. Asking the Actions API can take 15s per lock, and a
#      marker down for that long would keep every job that starts meanwhile
#      waiting on an operation that has not even decided to run.
#   2. Enter-DockerExclusive puts the marker down and lists the locks again,
#      without asking anyone. Any lock that step 1 did not judge stale is
#      live, because it was taken since.
# The marker is named for the prune, which was its first user; a restart puts
# down the same kind, since jobs must wait for either.

# Whether a run (or one attempt of it) is still going, from the Actions API.
# keploy/keploy is public, so this needs no token. GITHUB_TOKEN is sent when
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
        # A 404 is no answer either. Besides a deleted run, it is what a token
        # that cannot see $Repo gets, or what a repository misread from the lock
        # gets. Taking it for "over" would delete a live lock. A deleted run's
        # lock ages out instead.
        Write-Host "  could not ask the Actions API about run $RunId of $Repo ($($_.Exception.Message)); judging its lock by age."
        return $null
    }
}

# Step 1: judges every lock in $LockDir that matches $Filter and deletes the
# stale ones. Returns the names of the live locks, and the stale ones' identity
# (full path -> write time). Enter-DockerExclusive uses that identity to tell
# "judged stale here but not deleted" from "taken since".
# GetRunStatus takes (repository, run ID, attempt or '') and returns the run's
# status, or $null when it could not find out. Only the tests pass their own.
function Resolve-DockerLocks {
    param(
        [string]$LockDir,
        [string]$Filter = '*.lock',
        [int]$JobLockMaxMinutes = 45,
        [int]$RunLockMaxAgeHours = 24,
        # The repository whose runs a lock names, for locks that do not say.
        [string]$Repository = $env:GITHUB_REPOSITORY,
        [scriptblock]$GetRunStatus = $null
    )
    if (-not $GetRunStatus) { $GetRunStatus = ${function:Get-RunStatusFromApi} }
    $live = @()
    $stale = @{}
    $nowUtc = [DateTime]::UtcNow
    foreach ($lock in @(Get-ChildItem -LiteralPath $LockDir -Filter $Filter -ErrorAction SilentlyContinue)) {
        $maxAge = [TimeSpan]::FromHours($RunLockMaxAgeHours)
        $runId = ''; $attempt = ''
        if ($lock.Name -match '^docker-job-(\d+)-(\d+)-') {
            $maxAge = [TimeSpan]::FromMinutes($JobLockMaxMinutes)
            $runId = $Matches[1]; $attempt = $Matches[2]
        } elseif ($lock.Name -match '^prepare-windows-workflow-(\d+)\.lock$') {
            $runId = $Matches[1]
        }
        $age = $nowUtc - $lock.LastWriteTimeUtc
        $why = ''
        if ($age -ge $maxAge) {
            $why = "$([int]$age.TotalMinutes) min old; its holder cannot run longer than $([int]$maxAge.TotalMinutes) min"
        } elseif ($runId) {
            # Both kinds of lock start with their repository. Older run locks
            # do not, but only this repository ever wrote those.
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
            $stale[$lock.FullName] = $lock.LastWriteTimeUtc.Ticks
            Remove-Item -LiteralPath $lock.FullName -Force -ErrorAction SilentlyContinue
        } else {
            $live += $lock.Name
        }
    }
    [pscustomobject]@{ Live = $live; Stale = $stale }
}

# Step 2: puts this operation's marker down, then lists the locks again with
# no API call. A lock is live unless step 1 judged that same file stale: same
# path and same write time. A lock re-created under a judged name (a re-run of
# that run) is new. Returns the marker's path when nothing is live. Otherwise
# it takes the marker back down and returns the live locks' names in Taken.
# Throws when the marker cannot be written: without it the operation is not
# safe to run.
function Enter-DockerExclusive {
    param(
        [string]$LockDir,
        [string]$Filter = '*.lock',
        [hashtable]$Stale = @{},
        [string]$Operation = 'a Docker prune'
    )
    $marker = Join-Path $LockDir ("docker-prune-{0}.inprogress" -f [guid]::NewGuid().ToString('N'))
    Set-Content -LiteralPath $marker -Value "$Operation by $env:GITHUB_REPOSITORY run $env:GITHUB_RUN_ID $env:RUNNER_NAME" -ErrorAction Stop
    $taken = @(Get-ChildItem -LiteralPath $LockDir -Filter $Filter -ErrorAction SilentlyContinue |
        Where-Object { -not ($Stale.ContainsKey($_.FullName) -and ($Stale[$_.FullName] -eq $_.LastWriteTimeUtc.Ticks)) } |
        ForEach-Object { $_.Name })
    if ($taken.Count -gt 0) {
        Remove-Item -LiteralPath $marker -Force -ErrorAction SilentlyContinue
        return [pscustomobject]@{ Marker = $null; Taken = $taken }
    }
    [pscustomobject]@{ Marker = $marker; Taken = @() }
}

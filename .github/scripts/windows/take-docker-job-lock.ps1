# Takes a golang_docker_windows job's Docker lock and prints its path.
#
# While the lock exists, cleanup-windows.ps1 (cleanup_windows of any run on
# this machine) does not prune the images, volumes, networks, build cache and
# stopped containers the job builds and needs between its record and replay
# phases. Per job rather than per run, so a re-run of just this job (which
# gets no run-level lock) is covered too. The file name carries the run ID and
# attempt, and the file starts with the repository, so the cleanup can ask the
# Actions API whether the holder is over instead of guessing from its age.
#
# A prune (cleanup-windows.ps1), or a start or restart of Docker Desktop
# (ensure-docker.ps1), that is already under way when the lock is written does
# not look at locks again, so after writing the lock this waits for any such
# operation to finish. The operation puts its docker-prune-*.inprogress marker
# down before its final look at the locks (docker-locks.ps1), and this checks
# for markers after writing the lock, so one of the two always sees the other. A marker older
# than -PruneMaxMinutes was left behind by an operation that was killed, and is
# not waited for (Get-DockerMarkers, the rule ensure-docker.ps1 waits by too).
[CmdletBinding()]
param(
    [string]$LockDir = '',
    [int]$PruneMaxMinutes = 15,
    # Only the tests override these.
    [string]$RunId = $env:GITHUB_RUN_ID,
    [string]$RunAttempt = $env:GITHUB_RUN_ATTEMPT,
    [int]$PollSeconds = 2
)

# Under the tests, no default may reach the shared machine (test-overrides.ps1).
. (Join-Path $PSScriptRoot 'test-overrides.ps1')
Assert-TestOverrides 'take-docker-job-lock.ps1' $PSBoundParameters @('LockDir')
$ErrorActionPreference = 'Stop'

if (-not $LockDir) {
    $base = $env:USERPROFILE
    if (-not $base) { $base = $HOME }
    $LockDir = Join-Path $base '.github-workflow-locks'
}
New-Item -Path $LockDir -ItemType Directory -Force | Out-Null
$lock = Join-Path $LockDir ("docker-job-{0}-{1}-{2}.lock" -f $RunId, $RunAttempt, [guid]::NewGuid().ToString('N'))
Set-Content -LiteralPath $lock -Value "$env:GITHUB_REPOSITORY $env:GITHUB_WORKFLOW $env:RUNNER_NAME"

. (Join-Path $PSScriptRoot 'docker-locks.ps1')
$announced = $false
while ($true) {
    $pruning = @(Get-DockerMarkers -LockDir $LockDir -MaxMinutes $PruneMaxMinutes)
    if ($pruning.Count -eq 0) { break }
    if (-not $announced) {
        Write-Host "A Docker prune, or a start or restart of Docker Desktop, is in progress ($($pruning[0].Name)); waiting for it to finish before using Docker."
        $announced = $true
    }
    Start-Sleep -Seconds $PollSeconds
}
if ($announced) { Write-Host "The Docker prune, start or restart has finished." }
Write-Output $lock

# precheck-windows' "Ensure Docker is running": make sure the Docker engine on
# this self-hosted Windows machine answers before the Windows lanes start, and
# do it without killing the containers of jobs that are already using it.
#
# win-runner-1..4 share ONE Docker Desktop. Restarting it kills every container
# on the machine, including a sibling job's live keploy agent and application,
# which is the failure class reap-keploy-containers.ps1 describes. This script
# used to force-kill Docker Desktop and com.docker* after a single failed
# `docker info` or `docker ps`. A daemon that is only busy (four jobs loading
# images and starting eBPF agents at once) fails a check like that. Now:
#   1. A failed check is retried -Attempts times, with the wait doubling each
#      time from -BackoffSeconds.
#   2. If Docker Desktop is not running at all, it is started. Nothing is
#      running on it, so nothing is lost.
#   3. If it is running but still does not answer, only a restart helps. The
#      restart happens only when no golang_docker_windows job holds a live
#      Docker lock, judged the way cleanup-windows.ps1 judges its locks, and
#      it runs under the same marker handshake (docker-locks.ps1). A job that
#      starts meanwhile therefore waits for the restart instead of starting its
#      containers on a daemon about to be killed. Run locks do not block the
#      restart. They mark a run in flight, not a container on the daemon, and
#      a restart keeps images and volumes, unlike a prune. Counting them would
#      forbid the restart whenever any Windows run is in flight, which is most
#      of the day.
#   4. Otherwise the job fails. The error names the jobs using Docker and the
#      remediation.
[CmdletBinding()]
param(
    [int]$Attempts = 5,
    [int]$BackoffSeconds = 5,
    # After starting Docker Desktop: how long it may take to answer, how often
    # to ask, and how long to let it settle once it does. A restart holds the
    # marker for about PollSeconds + ReadyTimeoutSeconds + StabilizeSeconds,
    # and reap-keploy-containers.tests.ps1 keeps that well under PruneMaxMinutes
    # so that a job never stops waiting for a restart that is still going.
    [int]$ReadyTimeoutSeconds = 120,
    [int]$PollSeconds = 5,
    [int]$StabilizeSeconds = 30,
    [string]$LockDir = '',
    # A Docker lock older than this is stale: the reaper's MinAgeMinutes.
    [int]$MinAgeMinutes = 45,
    # take-docker-job-lock.ps1's bound on a marker. See ReadyTimeoutSeconds.
    [int]$PruneMaxMinutes = 15,
    [string]$Repository = $env:GITHUB_REPOSITORY,
    [string]$DesktopExe = 'C:\Program Files\Docker\Docker\Docker Desktop.exe',
    # Only the tests override these. They must override all of the Desktop
    # ones: the defaults act on the real Docker Desktop of the shared machine.
    [string]$DockerExe = 'docker',
    [scriptblock]$GetRunStatus = $null,
    [scriptblock]$TestDesktopRunning = { [bool](Get-Process -Name 'Docker Desktop' -ErrorAction SilentlyContinue) },
    [scriptblock]$StopDesktop = {
        Get-Process -Name 'Docker Desktop' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
        Get-Process -Name 'com.docker*' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    },
    [scriptblock]$StartDesktop = { Start-Process -FilePath $DesktopExe -WindowStyle Hidden }
)
# Not 'Stop': under Windows PowerShell 5.1 a native command's stderr then
# throws NativeCommandError. Every outcome below is an explicit exit code.
$ErrorActionPreference = 'Continue'

if (-not $LockDir) {
    $base = $env:USERPROFILE
    if (-not $base) { $base = $HOME }
    $LockDir = Join-Path $base '.github-workflow-locks'
}

function Test-DockerHealthy {
    if (-not (Get-Command $DockerExe -ErrorAction SilentlyContinue)) { return $false }
    & $DockerExe info --format '{{.ServerVersion}}' *> $null
    if ($LASTEXITCODE -ne 0) { return $false }
    & $DockerExe ps -q *> $null
    return ($LASTEXITCODE -eq 0)
}

# After starting Docker Desktop: until it answers, then again once it settles.
function Wait-DockerReady {
    $deadline = [DateTime]::UtcNow.AddSeconds($ReadyTimeoutSeconds)
    while (-not (Test-DockerHealthy)) {
        if ([DateTime]::UtcNow -ge $deadline) { return $false }
        Write-Host "Waiting for Docker to be ready..."
        Start-Sleep -Seconds $PollSeconds
    }
    if ($StabilizeSeconds -gt 0) {
        Write-Host "Docker answers; giving it $StabilizeSeconds s to settle."
        Start-Sleep -Seconds $StabilizeSeconds
    }
    return (Test-DockerHealthy)
}

$delay = $BackoffSeconds
for ($i = 1; $i -le $Attempts; $i++) {
    if (Test-DockerHealthy) {
        Write-Host "Docker engine is running and healthy."
        exit 0
    }
    if ($i -lt $Attempts) {
        Write-Host "Docker did not answer 'docker info' / 'docker ps' (attempt $i of $Attempts); asking again in $delay s."
        Start-Sleep -Seconds $delay
        $delay *= 2
    }
}
$failed = "it did not answer 'docker info' / 'docker ps' in $Attempts attempts"

if (-not (& $TestDesktopRunning)) {
    Write-Host "Docker Desktop is not running ($failed); starting it. No container can be running on it."
    & $StartDesktop
    if (Wait-DockerReady) {
        Write-Host "Docker is ready."
        exit 0
    }
    Write-Host "::error::Docker did not become ready within $ReadyTimeoutSeconds s of starting Docker Desktop. Check Docker Desktop on the host; if it is stuck, quit it, run 'wsl --shutdown', and start it again."
    exit 1
}

# Running but not answering. Only a restart helps, and a restart kills every
# container on the daemon.
. (Join-Path $PSScriptRoot 'docker-locks.ps1')
function Stop-Refusing([string[]]$Holders, [string]$How) {
    Write-Host ("::error::Docker Desktop is running but $failed, so it needs a restart. A restart kills every " +
        "container on the Docker daemon that the four runners of this machine share, and $($Holders.Count) job(s) " +
        "$How ($($Holders -join ', ')), so Docker Desktop was NOT restarted. Re-run this job " +
        "once they have finished: this step then restarts Docker Desktop itself. If Docker still does not answer " +
        "when no job is using it, restart it on the host: quit Docker Desktop, run 'wsl --shutdown', and start " +
        "Docker Desktop again.")
    exit 1
}

New-Item -ItemType Directory -Force -Path $LockDir | Out-Null
$locks = Resolve-DockerLocks -LockDir $LockDir -Filter 'docker-job-*.lock' -JobLockMaxMinutes $MinAgeMinutes -Repository $Repository -GetRunStatus $GetRunStatus
# Live locks call the restart off before the marker goes down: a starting job
# should not wait on a restart that will not happen.
if ($locks.Live.Count -gt 0) { Stop-Refusing $locks.Live 'hold a live Docker lock' }
try {
    $exclusive = Enter-DockerExclusive -LockDir $LockDir -Filter 'docker-job-*.lock' -Stale $locks.Stale -Operation 'a Docker Desktop restart'
} catch {
    Write-Host "::error::Docker Desktop is running but $failed, and it cannot be restarted safely: the marker that keeps jobs from starting on Docker during the restart could not be written ($($_.Exception.Message))."
    exit 1
}
if (-not $exclusive.Marker) { Stop-Refusing $exclusive.Taken 'took a Docker lock while the others were being judged' }

$ready = $false
try {
    Write-Host "Docker Desktop is running but $failed, and no job is using Docker; restarting Docker Desktop."
    & $StopDesktop
    Start-Sleep -Seconds $PollSeconds
    & $StartDesktop
    $ready = Wait-DockerReady
} finally {
    Remove-Item -LiteralPath $exclusive.Marker -Force -ErrorAction SilentlyContinue
}
if ($ready) {
    Write-Host "Docker is ready after the restart."
    exit 0
}
Write-Host "::error::Docker did not become ready within $ReadyTimeoutSeconds s of restarting Docker Desktop. Restart it on the host: quit Docker Desktop, run 'wsl --shutdown', and start Docker Desktop again."
exit 1

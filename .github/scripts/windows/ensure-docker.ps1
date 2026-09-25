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
#   1. Without a docker CLI there is nothing to check, and starting or
#      restarting Docker Desktop cannot put one on the PATH: the job fails.
#   2. A failed check is retried -Attempts times, with the wait doubling each
#      time from -BackoffSeconds. A call that has not returned after
#      -CallTimeoutSeconds is killed and counts as failed: a wedged Docker
#      Desktop often hangs `docker info` instead of failing it.
#   3. If another runner's prune, start or restart of Docker is under way (its
#      docker-prune-*.inprogress marker is down, docker-locks.ps1), this waits
#      for it and checks again from the top. When Docker is wedged, every
#      run's precheck-windows gets here at about the same time, and a restart
#      now would kill the other runner's start that is still coming up.
#      Waits end at -MaxWaitMinutes after this started: the job then fails
#      with the remediation, instead of being killed at its timeout-minutes.
#   4. If Docker Desktop is not running at all, it is started under a marker.
#      Nothing is running on it, so nothing is lost.
#   5. If it is running but still does not answer, only a restart helps. The
#      restart happens only when no golang_docker_windows job holds a live
#      Docker lock, judged the way cleanup-windows.ps1 judges its locks, and
#      it runs under the same marker handshake. A job that starts meanwhile
#      therefore waits for the restart instead of starting its containers on a
#      daemon about to be killed. Run locks do not block the restart. They
#      mark a run in flight, not a container on the daemon, and a restart
#      keeps images and volumes, unlike a prune. Counting them would forbid
#      the restart whenever any Windows run is in flight, which is most of the
#      day.
#   6. Otherwise the job fails. The error names the jobs using Docker and the
#      remediation.
# Two starts or restarts that put their markers down at the same moment settle
# it by name (Resolve-MarkerRace in docker-locks.ps1): exactly one of them goes
# on, and the others wait for it and check Docker again.
# A start or restart acts only on a check (with every retry) that no other
# start or restart of Docker Desktop followed, whether it waited for that one
# or it happened just before its marker went down (THE LAST START in
# docker-locks.ps1). Otherwise Docker may still be coming up from it, and this
# checks again from the top instead.
[CmdletBinding()]
param(
    [int]$Attempts = 5,
    [int]$BackoffSeconds = 5,
    # How long one `docker info` or `docker ps` may take.
    [int]$CallTimeoutSeconds = 20,
    # After starting Docker Desktop: how long it may take to answer, how often
    # to ask, and how long to let it settle once it does. A start or restart
    # holds its marker for at most
    #   2 x PollSeconds + ReadyTimeoutSeconds + StabilizeSeconds + 6 x CallTimeoutSeconds
    # (a check is two calls: the one under the marker before starting, the one
    # the ready deadline cuts short, and the one after settling). A wait for
    # another runner's marker does not count: the marker is written again
    # after it. reap-keploy-containers.tests.ps1 keeps that well under
    # PruneMaxMinutes, so that a job never stops waiting for a start or
    # restart still going.
    [int]$ReadyTimeoutSeconds = 120,
    [int]$PollSeconds = 5,
    [int]$StabilizeSeconds = 30,
    # How long after it starts this may still wait for other runners' Docker
    # prunes, starts and restarts, or go round again after one. Past it, the
    # job fails with the remediation. reap-keploy-containers.tests.ps1 keeps
    # this, plus one more full check of Docker and one restart, plus the
    # timeout-minutes of the step before this one that runs those tests,
    # under precheck-windows' timeout-minutes: a job killed at its timeout
    # says nothing of why.
    [int]$MaxWaitMinutes = 10,
    [string]$LockDir = '',
    # A Docker lock older than this is stale: the reaper's MinAgeMinutes.
    [int]$MinAgeMinutes = 45,
    # take-docker-job-lock.ps1's bound on a marker. See ReadyTimeoutSeconds.
    [int]$PruneMaxMinutes = 15,
    [string]$Repository = $env:GITHUB_REPOSITORY,
    [string]$DesktopExe = 'C:\Program Files\Docker\Docker\Docker Desktop.exe',
    # Only the tests override these, and under the tests they must, like
    # LockDir (test-overrides.ps1): the defaults act on the shared machine.
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

# Under the tests, no default may reach the shared machine (test-overrides.ps1):
# the lock directory, the docker CLI, and every scriptblock, whose defaults act
# on the real Docker Desktop and ask the real Actions API.
. (Join-Path $PSScriptRoot 'test-overrides.ps1')
Assert-TestOverrides 'ensure-docker.ps1' $PSBoundParameters (@('LockDir', 'DockerExe') + @($MyInvocation.MyCommand.Parameters.Values |
        Where-Object { $_.ParameterType -eq [scriptblock] } | ForEach-Object { $_.Name }))
$deadline = [DateTime]::UtcNow.AddMinutes($MaxWaitMinutes)

if (-not $LockDir) {
    $base = $env:USERPROFILE
    if (-not $base) { $base = $HOME }
    $LockDir = Join-Path $base '.github-workflow-locks'
}

$docker = @(Get-Command $DockerExe -ErrorAction SilentlyContinue)
if ($docker.Count -eq 0) {
    Write-Host ("::error::docker CLI not found on PATH for this runner ('$DockerExe'). Docker Desktop was neither " +
        "started nor restarted: that cannot put the CLI on the PATH, and a restart kills the containers of every job " +
        "on the machine. Put Docker Desktop's resources\bin on the PATH of this runner's service, restart the runner, " +
        "and re-run the job.")
    exit 1
}
# Each call runs as a process of its own, so that one that hangs can be
# killed. A script (the tests' fake docker) runs under this same PowerShell.
$docker = $docker[0]
$dockerFile = $docker.Path
$dockerArgs = ''
if ($docker.CommandType -eq 'ExternalScript') {
    $dockerFile = [System.Diagnostics.Process]::GetCurrentProcess().MainModule.FileName
    $dockerArgs = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$($docker.Path)`" "
}

# One docker call, $true when it exits 0 within CallTimeoutSeconds. Its output
# is read and dropped, so a long `docker ps` cannot block on a full pipe. A
# call that fails says how: its exit code and the first line of its stderr,
# which is the daemon's own error ("error during connect: ...", say). It says
# so whenever that differs from how the same call last failed, so that a
# Docker that keeps failing the same way says it once, not on every poll.
$lastFailure = @{}
function Invoke-DockerCall([string]$Arguments) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $dockerFile
    $psi.Arguments = $dockerArgs + $Arguments
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    try {
        $p = [System.Diagnostics.Process]::Start($psi)
    } catch {
        Write-Host "could not run 'docker $Arguments': $($_.Exception.Message)"
        return $false
    }
    [void]$p.StandardOutput.ReadToEndAsync()
    $stderr = $p.StandardError.ReadToEndAsync()
    if (-not $p.WaitForExit($CallTimeoutSeconds * 1000)) {
        Write-Host "'docker $Arguments' did not return within $CallTimeoutSeconds s; killing it and counting the check as failed."
        try { $p.Kill() } catch { }
        [void]$p.WaitForExit(5000)
        return $false
    }
    $p.WaitForExit()
    if ($p.ExitCode -eq 0) { return $true }
    # Its stderr ends when it does, unless a process it started still holds
    # it: a moment for that, no more.
    $said = 'nothing on stderr'
    try {
        if ($stderr.Wait(5000)) {
            $lines = @("$($stderr.Result)" -split "`r?`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ })
            if ($lines.Count) { $said = $lines[0] }
        }
    } catch { }
    if ($said.Length -gt 300) { $said = $said.Substring(0, 300) + '...' }
    $failure = "'docker $Arguments' failed (exit $($p.ExitCode)): $said"
    if ($failure -ne $lastFailure[$Arguments]) { Write-Host $failure }
    $lastFailure[$Arguments] = $failure
    return $false
}

function Test-DockerHealthy {
    if (-not (Invoke-DockerCall 'info --format "{{.ServerVersion}}"')) { return $false }
    return (Invoke-DockerCall 'ps -q')
}

# Up to -Attempts checks, with backoff between them.
function Test-DockerAnswers {
    $delay = $BackoffSeconds
    for ($i = 1; $i -le $Attempts; $i++) {
        if (Test-DockerHealthy) { return $true }
        if ($i -lt $Attempts) {
            Write-Host "Docker did not answer 'docker info' / 'docker ps' (attempt $i of $Attempts); asking again in $delay s."
            Start-Sleep -Seconds $delay
            $delay *= 2
        }
    }
    return $false
}

# After starting Docker Desktop: until it answers, then again once it settles.
function Wait-DockerReady {
    $deadline = [DateTime]::UtcNow.AddSeconds($ReadyTimeoutSeconds)
    while (-not (Test-DockerHealthy)) {
        if ([DateTime]::UtcNow -ge $deadline) { return $false }
        Write-Host "Waiting for Docker to be ready..."
        Start-Sleep -Seconds $PollSeconds
    }
    if ($StabilizeSeconds -le 0) { return $true }
    Write-Host "Docker answers; giving it $StabilizeSeconds s to settle."
    Start-Sleep -Seconds $StabilizeSeconds
    return (Test-DockerHealthy)
}

. (Join-Path $PSScriptRoot 'docker-locks.ps1')
New-Item -ItemType Directory -Force -Path $LockDir | Out-Null

# Ends the job once MaxWaitMinutes have passed. $Others names what it was
# waiting for, when it was waiting.
function Stop-PastDeadline([string]$Others = '') {
    if ($Others) { $Others = " ($Others)" }
    Write-Host ("::error::Docker does not answer, and $MaxWaitMinutes min after this step started, other runners' " +
        "Docker prunes, starts or restarts$Others still kept this one from starting or restarting Docker Desktop, so " +
        "it did NOT. Re-run this job. If Docker still does not answer, restart it on the host: quit Docker Desktop, run " +
        "'wsl --shutdown', and start Docker Desktop again.")
    exit 1
}

# $true once it has waited for other runners' markers to go; $false when there
# are none.
function Wait-OtherOperations {
    $others = @(Get-DockerMarkers -LockDir $LockDir -MaxMinutes $PruneMaxMinutes)
    if ($others.Count -eq 0) { return $false }
    $names = ($others | ForEach-Object { $_.Name }) -join ', '
    Write-Host ("Docker does not answer, and another runner has a Docker prune, start or restart under way ($names). " +
        "Starting or restarting Docker Desktop now would kill it; waiting for it to finish, then checking Docker again.")
    while (@(Get-DockerMarkers -LockDir $LockDir -MaxMinutes $PruneMaxMinutes).Count -gt 0) {
        if ([DateTime]::UtcNow -ge $deadline) { Stop-PastDeadline $names }
        Start-Sleep -Seconds $PollSeconds
    }
    return $true
}

function Stop-Refusing([string[]]$Holders, [string]$How) {
    Write-Host ("::error::Docker Desktop is running but $failed, so it needs a restart. A restart kills every " +
        "container on the Docker daemon that the four runners of this machine share, and $($Holders.Count) job(s) " +
        "$How ($($Holders -join ', ')), so Docker Desktop was NOT restarted. Re-run this job " +
        "once they have finished: this step then restarts Docker Desktop itself. If Docker still does not answer " +
        "when no job is using it, restart it on the host: quit Docker Desktop, run 'wsl --shutdown', and start " +
        "Docker Desktop again.")
    exit 1
}

# With $Marker down and the race settled: (re)starts Docker Desktop and waits
# for Docker, then takes the marker back down and ends the script. It does not
# act when Docker Desktop was started or restarted since $Started was read,
# before this round's check (THE LAST START in docker-locks.ps1): Docker may
# still be coming up from that, and the check that failed does not count. It
# then takes the marker back down and returns, and the caller checks again
# from the top, with every retry. It also checks Docker once more before
# acting: an operation this one waited for can have found Docker answering.
function Invoke-UnderMarker([string]$Marker, [string]$Started, [switch]$Restart) {
    $what = 'starting'; $done = 'started'; $operation = 'a Docker Desktop start'
    if ($Restart) { $what = 'restarting'; $done = 'restarted'; $operation = 'a Docker Desktop restart' }
    $now = Get-DesktopStarted -LockDir $LockDir
    if ($now -ne $Started) {
        Remove-Item -LiteralPath $Marker -Force -ErrorAction SilentlyContinue
        Write-Host "Docker Desktop was started or restarted by another runner since this one checked Docker ($now). Docker may still be coming up, so this is not $what it; checking Docker again, with every retry."
        return
    }
    $ready = $false
    try {
        if (Test-DockerHealthy) {
            Write-Host "Docker answers now (an operation this one waited for may have found it answering); not $what Docker Desktop."
            $ready = $true
        } else {
            try {
                Set-DesktopStarted -LockDir $LockDir -Operation $operation
            } catch {
                Write-Host "::error::Docker Desktop was NOT $($done): docker-desktop.started, which keeps other runners from $what it again while it comes up, could not be written ($($_.Exception.Message))."
                exit 1
            }
            if ($Restart) {
                Write-Host "Docker Desktop is running but $failed, and no job is using Docker; restarting Docker Desktop."
                & $StopDesktop
                Start-Sleep -Seconds $PollSeconds
            } else {
                Write-Host "Docker Desktop is not running ($failed); starting it. No container can be running on it."
            }
            & $StartDesktop
            $ready = Wait-DockerReady
        }
    } finally {
        Remove-Item -LiteralPath $Marker -Force -ErrorAction SilentlyContinue
    }
    if ($ready) {
        Write-Host "Docker is ready."
        exit 0
    }
    Write-Host "::error::Docker did not become ready within $ReadyTimeoutSeconds s of $what Docker Desktop. Restart it on the host: quit Docker Desktop, run 'wsl --shutdown', and start Docker Desktop again."
    exit 1
}

$failed = "it did not answer 'docker info' / 'docker ps' in $Attempts attempts"
$round = 0
while ($true) {
    $round++
    # Read before the check, so that a start or restart after it shows.
    $started = Get-DesktopStarted -LockDir $LockDir
    if (Test-DockerAnswers) {
        Write-Host "Docker engine is running and healthy."
        exit 0
    }
    # Every round after the first follows another runner's operation.
    if (($round -gt 1) -and ([DateTime]::UtcNow -ge $deadline)) { Stop-PastDeadline }
    if (Wait-OtherOperations) { continue }

    if (-not (& $TestDesktopRunning)) {
        $marker = $null
        try {
            $marker = New-DockerMarker -LockDir $LockDir -Operation 'a Docker Desktop start'
            $race = Resolve-MarkerRace -LockDir $LockDir -Marker $marker -MaxMinutes $PruneMaxMinutes -Wait -PollSeconds $PollSeconds -Until $deadline
        } catch {
            if ($marker) { Remove-Item -LiteralPath $marker -Force -ErrorAction SilentlyContinue }
            Write-Host "::error::Docker Desktop is not running, and it cannot be started safely: the marker that keeps other runners from restarting it while it comes up could not be written ($($_.Exception.Message))."
            exit 1
        }
        if ($race.TimedOut) { Stop-PastDeadline ($race.Others -join ', ') }
        if (-not $race.Go) {
            Write-Host "Another runner's Docker start or restart has its marker down ($($race.Others -join ', ')); leaving the start to it."
            continue
        }
        Invoke-UnderMarker -Marker $marker -Started $started
        continue
    }

    # Running but not answering. Only a restart helps, and a restart kills
    # every container on the daemon. Live locks call it off before the marker
    # goes down: a starting job should not wait on a restart that will not
    # happen.
    $locks = Resolve-DockerLocks -LockDir $LockDir -Filter 'docker-job-*.lock' -JobLockMaxMinutes $MinAgeMinutes -Repository $Repository -GetRunStatus $GetRunStatus
    if ($locks.Live.Count -gt 0) { Stop-Refusing $locks.Live 'hold a live Docker lock' }
    try {
        $exclusive = Enter-DockerExclusive -LockDir $LockDir -Filter 'docker-job-*.lock' -Stale $locks.Stale -Operation 'a Docker Desktop restart' -MaxMinutes $PruneMaxMinutes -Wait -PollSeconds $PollSeconds -Until $deadline
    } catch {
        Write-Host "::error::Docker Desktop is running but $failed, and it cannot be restarted safely: the marker that keeps jobs from starting on Docker during the restart could not be written ($($_.Exception.Message))."
        exit 1
    }
    if ($exclusive.TimedOut) { Stop-PastDeadline ($exclusive.Others -join ', ') }
    if (@($exclusive.Others).Count -gt 0) {
        Write-Host "Another runner's Docker prune, start or restart has its marker down ($($exclusive.Others -join ', ')); leaving the restart to it."
        continue
    }
    if (-not $exclusive.Marker) {
        # Jobs that start once another runner's restart has brought Docker
        # back take their locks while this one waits for that restart to end.
        # Docker may well answer now: check it again from the top.
        if ($exclusive.Waited) {
            Write-Host "Job(s) took a Docker lock while this waited for another runner's start or restart ($($exclusive.Taken -join ', ')); checking Docker again."
            continue
        }
        Stop-Refusing $exclusive.Taken 'took a Docker lock while the others were being judged'
    }
    Invoke-UnderMarker -Marker $exclusive.Marker -Started $started -Restart
}

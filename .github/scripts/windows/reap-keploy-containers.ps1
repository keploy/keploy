# Remove containers left behind on the self-hosted Windows runners' Docker VM,
# and fail loudly if any of them cannot be removed.
#
# WHY THIS EXISTS
# Every keploy record/replay run creates a keploy-v3-<hash> agent container.
# Nothing removed them: "Recover stale runner state" only kills Windows
# keploy.exe processes and resets the WinDivert driver, and the only `docker rm`
# sweep in this repo is clean_up_docker_macos, which is macOS-only and filters
# on app-container prefixes. They accumulated indefinitely - eight were found
# alive on one runner, spanning six hours.
#
# ONLY CONTAINERS NO LIVE JOB CAN OWN
# win-runner-1..4 are org-level runners (Default group, visible to every keploy
# repo) running as separate processes on ONE machine and talking to ONE Docker
# daemon, so any container on it is as likely to be a sibling job's live
# container as a leftover. The sweep used to remove every keploy-v3-* container,
# and each caller runs while other runners are mid-job: the pre-job reap at the
# start of every golang_docker_windows job, and cleanup_windows at the end of
# every run. Run 36092000545 lost its replay that way: 20s into the replay, run
# 36091939524's cleanup_windows (win-runner-3) removed its live agent
# keploy-v3-ee1c (win-runner-2). The application shares that container's pid
# namespace, so it was SIGKILLed with it (exit 137), keploy reported "exit
# status 137" and no report was written.
#
# A container is removed only by a caller that owns it, and there are two:
#   -ComposeProject  the job that started the containers, at its own teardown.
#                    Every container a golang_docker_windows job starts
#                    (application and keploy agent) carries its job-unique
#                    compose project label, so the job removes exactly its own,
#                    at any age.
#   the age sweep    anyone else. A job cannot outlive its timeout-minutes and
#                    creates its containers after it starts, so while it is
#                    alive none of its containers is older than that. A
#                    container whose most recent lifecycle event (created,
#                    started or stopped) is at least -MinAgeMinutes old belongs
#                    to no live job; a younger one might, and is left alone. A
#                    never-started container reports StartedAt/FinishedAt as
#                    year 1, so it is aged from its creation - it is NOT ancient.
#                    Age is measured on the Docker daemon's own clock (`docker
#                    info` SystemTime) against the timestamps that same daemon
#                    recorded, because the WSL2 VM's clock drifts from the
#                    Windows host's.
#
# THE TWO OUTCOMES, AND WHY THE SECOND ONE FAILS THE JOB
# A leftover from a finished run is ordinary garbage; delete it and move on.
# A container that will NOT die is something else: a task stuck in
# uninterruptible kernel sleep. The observed cause is a stalled RCU-tasks grace
# period (dmesg: tasks_rcu_exit_srcu_stall), which makes bpf(BPF_PROG_LOAD)
# block forever at 0% CPU. The keploy agent then never becomes ready, compose
# aborts the application on an unhealthy dependency, and every test case is
# reported as a connection failure. Once the VM reaches that state EVERY
# subsequent Docker job on this runner fails the same way, each burning its full
# budget and reporting a misleading result. One accurate failure naming the
# remediation is worth more than a day of those.
#
# That detection must not wait for the age threshold, so a container that
# survives removal is recorded in -WedgeDir (one <id>.wedged file, shared by
# every runner on the machine). A recorded container was already given up by
# its owner, so every later sweep - pre-job included, owner teardowns excepted -
# retries its removal whatever its age: still there means the VM is still
# wedged (fail the pre-job reap at once); gone means the VM was restarted, and
# the record is deleted. A wedged job's own teardown records its agent (and
# only warns), so the very next job on any runner refuses to start instead of
# burning its budget.
[CmdletBinding()]
param(
    # Exit non-zero when a container survives removal. Only the pre-job caller
    # wants that. The owning job's teardown and the post-run cleanup warn
    # instead: failing teardown would mask the real result of the run, and the
    # next pre-job reap retries the container and fails then.
    [switch]$FailOnStuck,

    # The age sweep leaves any container whose last lifecycle event is younger
    # than this, because it may belong to a job still running on another runner
    # of this machine. Must be at least the timeout-minutes of every job that
    # can run on these runners: keploy/windows-redirector's build job (40, no
    # Docker today) shares them with this repo's self-hosted Windows jobs (at
    # most 30). reap-keploy-containers.tests.ps1 enforces this repo's half.
    [int]$MinAgeMinutes = 40,

    # Which containers the age sweep considers: names starting with this.
    # Empty means every container on the daemon (cleanup-windows.ps1).
    [string]$NamePrefix = 'keploy-v3-',

    # Owner teardown: remove every container of this compose project now,
    # whatever its age. Only the job that owns the project passes it.
    [string]$ComposeProject = '',

    # Where containers that survived `docker rm -f` are recorded.
    [string]$WedgeDir = '',

    # How long a removed container may stay listed before it counts as having
    # survived. `docker rm -f` of a container whose removal is already under
    # way (keploy's own teardown, or a sweep on another runner) returns "removal
    # ... is already in progress" at once while the container is still listed
    # in state "removing"; that finishes in seconds, a wedged task never does.
    [int]$SettleSeconds = 60,

    # The docker CLI to drive. Only the tests override it.
    [string]$DockerExe = 'docker'
)

$ErrorActionPreference = 'Continue'

if (-not $WedgeDir) {
    $base = $env:USERPROFILE
    if (-not $base) { $base = $HOME }
    $WedgeDir = Join-Path $base '.keploy-docker-wedged'
}

# Docker prints RFC 3339 timestamps with nanoseconds, which both .NET
# Framework (Windows PowerShell 5.1) and .NET (pwsh) parse. Returns $null for
# anything unparseable rather than guessing an age.
function ConvertFrom-DockerTime([string]$Raw) {
    if ([string]::IsNullOrWhiteSpace($Raw)) { return $null }
    $parsed = [DateTimeOffset]::MinValue
    $styles = [Globalization.DateTimeStyles]::AssumeUniversal
    if ([DateTimeOffset]::TryParse($Raw.Trim(), [Globalization.CultureInfo]::InvariantCulture, $styles, [ref]$parsed)) {
        return $parsed
    }
    return $null
}

if (-not (Get-Command $DockerExe -ErrorAction SilentlyContinue)) {
    Write-Host "docker not on PATH; nothing to reap."
    exit 0
}
$nowRaw = & $DockerExe info --format '{{.SystemTime}}' 2>$null
if ($LASTEXITCODE -ne 0) {
    Write-Host "Docker daemon not reachable; skipping reap."
    exit 0
}
$now = ConvertFrom-DockerTime "$nowRaw"

$requested = New-Object System.Collections.Generic.List[string]
$names = @{}
function Request-Removal([string]$Id, [string]$Name, [string]$Why) {
    if ($requested.Contains($Id)) { return }
    & $DockerExe rm -f $Id *> $null
    # "requested", not "removed": whether it actually went is established by
    # the re-query below, not by this call returning.
    Write-Host "  requested removal of $Name ($Why)"
    $requested.Add($Id)
    $names[$Id] = $Name
}

# 1. Containers an earlier run recorded as surviving removal. Not the owner's
# teardown's business: it is about this job's own containers, and waiting out
# another runner's wedge there would only fail a job whose result is known.
$recorded = @()
if ((-not $ComposeProject) -and (Test-Path -LiteralPath $WedgeDir)) {
    $recorded = @(Get-ChildItem -LiteralPath $WedgeDir -Filter '*.wedged' -ErrorAction SilentlyContinue)
}
foreach ($f in $recorded) {
    $label = (Get-Content -LiteralPath $f.FullName -TotalCount 1 -ErrorAction SilentlyContinue)
    if (-not $label) { $label = $f.BaseName }
    Request-Removal $f.BaseName $label "it survived removal before"
}

if ($ComposeProject) {
    # 2a. The calling job's own containers.
    # @() so a single container stays an array rather than a bare string that
    # would then be iterated one character at a time.
    $ids = @(& $DockerExe ps -aq --filter "label=com.docker.compose.project=$ComposeProject" 2>$null | Where-Object { $_ })
    Write-Host "Removing the $($ids.Count) container(s) of this job's compose project $ComposeProject."
    foreach ($id in $ids) {
        $name = "$(& $DockerExe inspect --format '{{.Name}}' $id 2>$null)".TrimStart('/')
        if (-not $name) { continue }  # its owner (keploy) removed it meanwhile
        Request-Removal $id $name "owned by this job"
    }
} elseif ($null -eq $now) {
    # Without the daemon's clock no container's age is known, and removing one
    # whose age is unknown is exactly how a sibling's live container gets killed.
    Write-Host "::warning::could not read the Docker daemon's clock (got '$nowRaw'); sweeping nothing by age."
} else {
    # 2b. The age sweep.
    $psArgs = @('ps', '-aq')
    if ($NamePrefix) { $psArgs += @('--filter', "name=$NamePrefix") }
    $ids = @(& $DockerExe @psArgs 2>$null | Where-Object { $_ })
    $what = 'container(s)'
    if ($NamePrefix) { $what = "$NamePrefix* container(s)" }
    Write-Host "Found $($ids.Count) $what; removing those idle for at least $MinAgeMinutes minutes."
    foreach ($id in $ids) {
        $meta = & $DockerExe inspect --format '{{.Name}}|{{.Created}}|{{.State.StartedAt}}|{{.State.FinishedAt}}' $id 2>$null
        if (-not $meta) { continue }  # gone between `ps` and `inspect`: its owner removed it
        $fields = "$meta".Split('|')
        $name = $fields[0].TrimStart('/')
        if (-not $name) { $name = $id }
        # docker's name filter is an unanchored match, so "name=keploy-v3-" also
        # returns e.g. my-keploy-v3-db. The prefix is the contract; check it here.
        if ($NamePrefix -and -not $name.StartsWith($NamePrefix, [StringComparison]::Ordinal)) { continue }
        # The most recent of created / started / stopped. Year-1 StartedAt and
        # FinishedAt (never started, never stopped) lose to Created.
        $last = $null
        foreach ($raw in $fields[1..3]) {
            $t = ConvertFrom-DockerTime $raw
            if (($null -ne $t) -and (($null -eq $last) -or ($t -gt $last))) { $last = $t }
        }
        if ($null -eq $last) {
            Write-Host "  leaving $name - its age cannot be determined (inspect said '$meta')."
            continue
        }
        $idle = ($now - $last).TotalMinutes
        $idleMinutes = [int][Math]::Floor($idle)
        if ($idle -lt $MinAgeMinutes) {
            Write-Host "  leaving $name - last active $idleMinutes minute(s) ago, so it may be a live job's on another runner of this machine."
            continue
        }
        Request-Removal $id $name "last active $idleMinutes minute(s) ago"
    }
}

if ($requested.Count -eq 0) {
    Write-Host "Nothing to remove."
    exit 0
}

# Re-query rather than trust exit codes. `docker rm -f` returns 0 even when it
# prints "No such container" and non-zero when another removal of the same
# container is in progress, so its status does not distinguish "removed" from
# "could not remove" - but whether the container is still listed once removal
# has had -SettleSeconds to finish does, and that is the fact this check is
# actually about. Only the containers this run asked to remove count: one
# deliberately left alone is not stuck.
$deadline = [DateTime]::UtcNow.AddSeconds($SettleSeconds)
while ($true) {
    $listed = @(& $DockerExe ps -aq 2>$null | Where-Object { $_ })
    if ($LASTEXITCODE -ne 0) {
        Write-Host "::warning::could not list containers after removal; cannot tell whether any survived."
        exit 0
    }
    $remaining = @($requested | Where-Object { $listed -contains $_ })
    if (($remaining.Count -eq 0) -or ([DateTime]::UtcNow -ge $deadline)) { break }
    Start-Sleep -Seconds 1
}
if (-not (Test-Path -LiteralPath $WedgeDir)) { New-Item -ItemType Directory -Force -Path $WedgeDir | Out-Null }
$stuck = @()
foreach ($id in $requested) {
    $marker = Join-Path $WedgeDir "$id.wedged"
    if ($listed -contains $id) {
        $stuck += $names[$id]
        if (-not (Test-Path -LiteralPath $marker)) {
            Set-Content -LiteralPath $marker -Encoding ASCII -Value @($names[$id], "survived 'docker rm -f' at $nowRaw (daemon clock)")
        }
    } elseif (Test-Path -LiteralPath $marker) {
        Remove-Item -LiteralPath $marker -Force -ErrorAction SilentlyContinue
        Write-Host "  $($names[$id]) is gone now; the Docker VM has recovered from the wedge recorded for it."
    }
}
if ($stuck.Count -eq 0) {
    Write-Host "All $($requested.Count) container(s) removed."
    exit 0
}

$msg = "Docker VM is wedged: $($stuck.Count) container(s) survived 'docker rm -f' - $($stuck -join ', '). " +
       "A container that outlives SIGKILL means a task is in uninterruptible kernel sleep, so every Docker job on this " +
       "runner will keep failing until the VM is restarted. Remediation: run 'wsl --shutdown' on this host. " +
       "NOT 'wsl --terminate docker-desktop' - WSL2 shares one utility VM, so terminating the distro comes back on the " +
       "same wedged kernel. Confirm the restart took with: docker run --rm --entrypoint cat <image> /proc/uptime"

if ($FailOnStuck) {
    Write-Host "::error::$msg"
    exit 1
}
Write-Host "::warning::$msg"
exit 0

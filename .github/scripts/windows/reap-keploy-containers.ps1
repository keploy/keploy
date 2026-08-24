# Remove keploy agent containers left behind on this runner's Docker VM, and
# fail loudly if any of them cannot be removed.
#
# WHY THIS EXISTS
# Every keploy record/replay run creates a keploy-v3-<hash> agent container.
# Nothing removed them: "Recover stale runner state" only kills Windows
# keploy.exe processes and resets the WinDivert driver, and the only `docker rm`
# sweep in this repo is clean_up_docker_macos, which is macOS-only and filters
# on app-container prefixes. They accumulated indefinitely — eight were found
# alive on one runner, spanning six hours.
#
# THE TWO CASES, AND WHY THE SECOND ONE FAILS THE JOB
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
[CmdletBinding()]
param(
    # Exit non-zero when containers survive removal. The pre-job caller wants
    # that (do not start work on a wedged VM); the post-run cleanup caller does
    # not, because failing teardown would mask the real result of the run.
    [switch]$FailOnStuck
)

$ErrorActionPreference = 'Continue'

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Host "docker not on PATH; nothing to reap."
    exit 0
}
docker info *> $null
if ($LASTEXITCODE -ne 0) {
    Write-Host "Docker daemon not reachable; skipping reap."
    exit 0
}

# @() so a single leftover stays an array rather than a bare string that would
# then be iterated one character at a time.
$ids = @(docker ps -aq --filter "name=keploy-v3-" 2>$null | Where-Object { $_ })
if ($ids.Count -eq 0) {
    Write-Host "No leftover keploy-v3-* containers."
    exit 0
}

Write-Host "Found $($ids.Count) leftover keploy-v3-* container(s); removing."
foreach ($id in $ids) {
    $name = (docker inspect --format '{{.Name}}' $id 2>$null)
    if (-not $name) { $name = $id }
    docker rm -f $id *> $null
    # "requested", not "removed": whether it actually went is established by the
    # re-query below, not by this call returning.
    Write-Host "  requested removal of $name"
}

# Re-query rather than trust exit codes. `docker rm -f` returns 0 even when it
# prints "No such container", so its status does not distinguish "removed" from
# "could not remove" — but whether the container is still listed does, and that
# is the fact this check is actually about.
$stuck = @(docker ps -aq --filter "name=keploy-v3-" 2>$null | Where-Object { $_ })
if ($stuck.Count -eq 0) {
    Write-Host "All leftover keploy agent containers removed."
    exit 0
}

$names = @()
foreach ($id in $stuck) {
    $n = (docker inspect --format '{{.Name}}' $id 2>$null)
    if (-not $n) { $n = $id }
    $names += $n
}

$msg = "Docker VM is wedged: $($stuck.Count) keploy agent container(s) survived 'docker rm -f' - $($names -join ', '). " +
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

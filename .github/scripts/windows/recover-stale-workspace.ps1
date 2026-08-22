# recover-stale-workspace.ps1
#
# Job-scoped recovery of a Windows runner's workspace. Runs BEFORE
# actions/checkout so that checkout can succeed.
#
# WHY THIS EXISTS
# ---------------
# 1. keploy processes left over from a prior run on this runner hold open
#    file handles into the workspace bin/ (notably WinDivert64.sys, the
#    kernel driver keploy uses for Windows traffic redirection). Those
#    handles make actions/checkout's clean step fail with EPERM on unlink.
# 2. keploy registers WinDivert with the SCM as a kernel-driver service named
#    "WinDivert" (or "WinDivert64" on some versions). While that service is
#    running it pins the .sys file inside the workspace, which produces the
#    same EPERM observed at checkout-clean time.
# 3. If a prior run was interrupted partway through cleanup, the workspace
#    can end up with files present but the .git subdirectory missing or
#    corrupt. In that state actions/checkout@v4 with clean:true fails at
#    "git config --local" with "not a git repository", because clean tries
#    to operate on a non-git tree. Wiping the workspace contents in that
#    case lets checkout start from a fresh init. Healthy workspaces (with a
#    valid .git) are left untouched so the existing checkout fast path
#    (clean+fetch over an existing clone) keeps working.
#
# WHY IT IS JOB-SCOPED
# --------------------
# Four self-hosted runner installs (win-runner-1..4) share ONE physical VM
# (see the header of .github/workflows/golang_docker_windows.yml). The
# original machine-wide form of this recovery
#     Get-Process -Name 'keploy' | Stop-Process -Force
#     foreach ($n in 'WinDivert','WinDivert64') { Stop-Service $n -Force }
# is hostile to that layout in two ways:
#   * the kill is unscoped by path, so a job on win-runner-1 kills the HOST
#     keploy CLI of a concurrent job on win-runner-2 (the process
#     orchestrating that job's run), aborting that job's whole run;
#   * WinDivert/WinDivert64 is a SINGLE machine-wide kernel (WFP) driver
#     service, so stopping it unconditionally yanks packet interception out
#     from under a sibling job that is mid-capture.
# Each runner install has its own _work directory (the default layout,
# <runner-install>\_work\<repo>\<repo>), so the four installs on this VM are
# expected to yield four distinct $GITHUB_WORKSPACE values, and every keploy
# binary a job uses lives under its own workspace ($ws\bin). The executable
# path is therefore the ownership key. (If two installs were ever pointed at
# the same _work directory this scoping would not separate them - but then
# concurrent jobs would already be corrupting each other's checkouts, which
# no cleanup step can paper over.) This script:
#   * kills only keploy processes whose .Path is under $GITHUB_WORKSPACE
#     (prefixed with a trailing separator so `...\keploy` cannot be mistaken
#     for a sibling `...\keploy2`), and
#   * resets the WinDivert services only when NO keploy process outside this
#     workspace is running. A process whose .Path cannot be read counts as
#     "other", so the ambiguous case errs towards leaving the driver alone.
# In the docker-compose lane (golang_docker_windows.yml) the driver reset is
# purely defensive: that lane intercepts inside the compose network via a
# per-job eBPF keploy-agent container and never loads the host WinDivert
# driver. The native Windows lane is the one that does load it.
#
# If $GITHUB_WORKSPACE is unset or empty there is no ownership key, so the
# script does NOTHING - it never falls back to machine-wide behaviour.
#
# This is runner hygiene only. It deliberately does nothing that could take
# the runner itself down: no com.docker* kills, no `wsl --shutdown`, no
# network adapter resets, no Docker Desktop start/stop. It is idempotent and
# exits 0 even when there is nothing stale.
#
# NOTE ON DUPLICATION
# -------------------
# Every call site runs BEFORE actions/checkout - this recovery is precisely
# what makes that checkout able to succeed, so it cannot be loaded from the
# checkout it repairs (the repo, and therefore this file, is not on disk yet;
# and in the "files but no .git" case the on-disk copy is exactly what may be
# missing or stale). The steps named "Recover stale runner state" in
#   .github/workflows/prepare_and_run.yml       (build-windows-amd64,
#                                                precheck-windows,
#                                                cleanup_windows)
#   .github/workflows/golang_docker_windows.yml (golang_docker_windows)
#   .github/workflows/golang_native_windows.yml (golang_native_windows)
# therefore inline the region below verbatim.
#
# golang_native_windows runs on GitHub-hosted `windows-latest`, a fresh
# single-tenant VM per job, so it has neither sibling jobs for the scoping to
# protect nor, on a VM that never ran anything before, leftovers to recover.
# It shares the same body anyway, and the reason is the drift check itself: a
# carve-out for one lane would be a sixth variant of this code, and variants
# drifting apart is the bug this arrangement exists to prevent. One body with
# no exceptions is what keeps the check below total. The scoping costs that
# lane nothing (every keploy it starts lives under $GITHUB_WORKSPACE\bin, so
# nothing is ever classified as another job's), and the body's
# `-like 'keploy*'` match - which the older `-Name 'keploy'` form lacked -
# would catch that lane's keploy-record.exe / keploy-replay.exe if this lane
# were ever moved to a reused or self-hosted VM where leftovers can exist.
#
# THIS FILE IS THE SINGLE SOURCE OF TRUTH. Change it here first, then re-sync
# every call site.
# INVARIANT: each call site's `run:` body is the region between the BEGIN/END
# markers below indented by exactly ten spaces and nothing else. This is
# ENFORCED, not merely documented:
#   .github/scripts/check-inline-regions.py
# extracts the region, parses the repository's YAML, walks both
# `jobs.<id>.steps` (workflows) and `runs.steps` (composite actions), dedents
# each "Recover stale runner state" body and requires byte-equality - and
# fails on any such step it does not already know about, so a new divergent
# copy cannot be added silently. Renaming the step does not help: it also
# fails any OTHER `run:` body that carries this region's fingerprint (a keploy
# kill together with WinDivert service handling, or one of this region's log
# strings) and is not a registered call site. It also fails if the region
# BELOW loses its job-scoping, so a careless edit here cannot be propagated to
# every call site by `--fix`. Deleting the `inline-regions` job in
# .github/workflows/golangci-lint.yml is caught; disabling it in place
# (`if: false`, `continue-on-error`, narrowing the workflow's triggers) is
# not - the check guards against accidental drift, not against someone
# deliberately switching it off, which review is for.
# To list the call sites by hand:
#   git grep -n 'name: Recover stale runner state' -- .github
# To re-sync them after editing the region:
#   python3 .github/scripts/check-inline-regions.py --fix

# ===== BEGIN INLINE REGION (keep byte-identical with every call site) =====
$ErrorActionPreference = 'Continue'

# Ownership key for everything below: this job's workspace, with a
# trailing separator so `...\keploy` cannot match a sibling `...\keploy2`.
$ws = $env:GITHUB_WORKSPACE
$wsPrefix = if ([string]::IsNullOrWhiteSpace($ws)) { $null } else { $ws.TrimEnd('\', '/') + '\' }

if (-not $wsPrefix) {
  # No workspace means no way to tell our processes from a sibling job's. On
  # the self-hosted lanes four runner installs share one VM, so a machine-wide
  # fallback would abort other jobs. Do nothing instead.
  Write-Host "GITHUB_WORKSPACE is unset or empty; skipping recovery entirely (refusing to fall back to machine-wide cleanup)."
  exit 0
}

Write-Host "Recovering stale runner state for workspace $ws"

# Each numbered section below carries its own try/catch. One try around all
# three would let a terminating error in section 1 or 2 skip section 3, which
# is the part that unblocks checkout.

# Identity of a process for section 2's exclusion list. A PID is NOT an
# identity on Windows: the kernel recycles PIDs, and section 1 waits up to
# five seconds for its kills to land - long enough for a freed PID to be
# handed to a sibling job's brand-new process. Pairing the PID with the
# process start time makes a recycled PID a different key. A process whose
# start time cannot be read gets no key and is therefore never excluded,
# which errs towards leaving the machine-wide driver alone.
function Get-KeployProcKey($proc) {
  # Unreadable start times surface either as a thrown getter exception or, on
  # some hosts, as a null with an error on the stream; treat both as "no key"
  # rather than letting an empty tick count become a matchable string.
  $ticks = $null
  try { $ticks = $proc.StartTime.Ticks } catch { $ticks = $null }
  if ($null -eq $ticks) { return $null }
  return "$($proc.Id):$ticks"
}

# 1. Kill keploy left over from a prior run IN THIS WORKSPACE only. Such
#    processes pin workspace files (bin\*.exe, WinDivert64.sys) and make
#    actions/checkout's clean fail with EPERM on unlink. A sibling job's
#    keploy, running from another runner install on this same VM, is not
#    ours to kill.
$killedKeys = @()
$killedProcs = @()
try {
  $ownCount = 0
  foreach ($p in @(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -like 'keploy*' })) {
    # .Path is MainModule.FileName underneath, and is unavailable for a
    # process that has exited or that this session cannot open. Read it
    # defensively and warn on the miss: an unreadable path silently changes
    # what happens in BOTH sections (not killed here, counted as another
    # job's below). If such a process belongs to THIS workspace it keeps
    # pinning bin\WinDivert64.sys and also suppresses the driver reset, so
    # the original EPERM-at-checkout failure can recur - that must surface as
    # a job annotation, not scroll past in the log.
    $path = $null
    try { $path = $p.Path } catch { $path = $null }
    if (-not $path) {
      Write-Host "::warning::keploy pid=$($p.Id) name=$($p.ProcessName): executable path unreadable (exited, or this session cannot open it); not killing it, and section 2 will count it as another job's. If it is in fact this workspace's, it may still pin bin\WinDivert64.sys and fail checkout with EPERM."
      continue
    }
    if (-not $path.StartsWith($wsPrefix, [System.StringComparison]::OrdinalIgnoreCase)) { continue }
    $ownCount++
    Write-Host "Stopping this workspace's leftover keploy pid=$($p.Id) path=$path"
    # Take the identity BEFORE stopping it: the start time comes off a live
    # process handle, and a process already in rundown may not answer.
    $key = Get-KeployProcKey $p
    try {
      $p | Stop-Process -Force -ErrorAction SilentlyContinue
      if ($key) { $killedKeys += $key }
      $killedProcs += $p
    } catch {}
  }
  if ($ownCount -eq 0) {
    Write-Host "No leftover keploy process under $wsPrefix; nothing to kill."
  }
  # Stop-Process calls TerminateProcess, which returns once termination has
  # been requested - not once the process is gone. Give each one a bounded
  # moment to finish exiting so its handles on workspace files are released
  # before checkout runs. Section 2 does not depend on this wait; it excludes
  # $killedKeys outright.
  foreach ($p in $killedProcs) { try { [void]$p.WaitForExit(5000) } catch {} }
} catch {
  Write-Host "::warning::Recover stale runner state (section 1, kill own keploy): $_"
}

# 2. WinDivert/WinDivert64 is ONE machine-wide kernel driver service, so
#    reset it only when no keploy outside this workspace is running -
#    otherwise a sibling job mid-capture loses interception. This
#    re-enumerates instead of reusing section 1's snapshot so a keploy
#    started in the meantime is seen. Processes killed above are excluded by
#    (PID, start time) rather than by PID: a process still in rundown can be
#    listed here with an unreadable .Path, which would otherwise count it as
#    "other" and block the reset - but Windows recycles PIDs, so a bare PID
#    could just as well match a sibling job's brand-new process. Every
#    excluded (PID, start time) pair was classified as this workspace's while
#    that process was alive, so the exclusion cannot hide a sibling job's
#    process. A key that cannot be computed excludes nothing, and a path that
#    cannot be read still counts as "other", so both ambiguous cases leave the
#    driver alone.
try {
  $otherKeploy = @()
  foreach ($p in @(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -like 'keploy*' })) {
    if ($killedKeys -contains (Get-KeployProcKey $p)) { continue }
    $path = $null
    try { $path = $p.Path } catch { $path = $null }
    if (-not $path) {
      Write-Host "keploy pid=$($p.Id) name=$($p.ProcessName): executable path unreadable and this job did not kill it; counting it as another job's."
      $otherKeploy += $p
    } elseif (-not $path.StartsWith($wsPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
      $otherKeploy += $p
    }
  }
  if ($otherKeploy.Count -gt 0) {
    Write-Host "Skipping WinDivert driver reset: $($otherKeploy.Count) keploy process(es) outside this workspace are running on this VM (a sibling job may be capturing)."
  } else {
    $answered = @()
    foreach ($name in @('WinDivert', 'WinDivert64')) {
      # The name is passed literally, with no wildcard, on purpose. For a
      # literal name PowerShell resolves the service with
      # ServiceController(name) + .Status (i.e. OpenService), which sees
      # kernel-driver services too; a wildcard would instead filter
      # ServiceController.GetServices(), which by contract excludes device
      # drivers and so would never match WinDivert.
      $svc = Get-Service -Name $name -ErrorAction SilentlyContinue
      if (-not $svc) { continue }
      $answered += $name
      if ($svc.Status -ne 'Stopped') {
        Write-Host "No keploy outside this workspace is running; resetting stale kernel driver service $name (status=$($svc.Status))"
        try { Stop-Service -Name $name -Force -ErrorAction SilentlyContinue } catch {}
      } else {
        Write-Host "Kernel driver service $name is registered and already Stopped; nothing to reset."
      }
    }
    if ($answered.Count -eq 0) {
      Write-Host "No keploy outside this workspace is running, and neither WinDivert nor WinDivert64 answered a service lookup; nothing to reset."
    }
  }
} catch {
  Write-Host "::warning::Recover stale runner state (section 2, WinDivert reset): $_"
}

# 3. Files but no .git means a prior run was interrupted partway through
#    cleanup, and actions/checkout's clean:true would fail with "not a
#    git repository". Wipe so checkout starts fresh. Healthy workspaces
#    (valid .git) are left alone so the clean+fetch fast path keeps
#    working.
try {
  if (Test-Path $ws) {
    $gitDir = Join-Path $ws '.git'
    $lsErr = $null
    $items = @(Get-ChildItem -Path $ws -Force -ErrorAction SilentlyContinue -ErrorVariable lsErr)
    $hasFiles = $items.Count -gt 0
    $hasGit = Test-Path $gitDir
    if ($hasFiles -and -not $hasGit) {
      Write-Host "::warning::Workspace $ws has files but no .git -- wiping for fresh checkout (prior run was interrupted)"
      Remove-Item -Path (Join-Path $ws '*') -Recurse -Force -ErrorAction SilentlyContinue
    } elseif ($lsErr) {
      # The listing was incomplete, so hasFiles is only a lower bound and no
      # claim about the layout is warranted. Leave the workspace alone.
      Write-Host "::warning::Could not fully list $ws ($($lsErr.Count) suppressed error(s), hasGit=$hasGit); leaving it for checkout without judging its layout."
    } else {
      Write-Host "Workspace listed cleanly (hasFiles=$hasFiles, hasGit=$hasGit); no wipe needed, leaving it for checkout."
    }
  } else {
    Write-Host "Workspace $ws does not exist; nothing to wipe, checkout will create it."
  }
} catch {
  Write-Host "::warning::Recover stale runner state (section 3, workspace layout): $_"
}
exit 0
# ===== END INLINE REGION =====

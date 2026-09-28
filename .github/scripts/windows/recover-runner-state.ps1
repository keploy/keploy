# "Recover stale runner state": the step before actions/checkout in every job
# on the self-hosted Windows runners. It runs before the checkout, so it cannot
# call a script from the repository: each such step's run: block is this
# file's text, and reap-keploy-containers.tests.ps1 fails if one differs.
#
# win-runner-1..4 run on ONE machine, so this acts only on what THIS job's
# runner left behind. It used to stop every process named keploy and the
# WinDivert driver unconditionally, which kills a sibling job's keploy
# mid-run and pulls the driver from under the one capturing through it.
$ErrorActionPreference = 'Continue'
try {
  $ws = $env:GITHUB_WORKSPACE
  # Each runner has a workspace of its own, so a process's executable path
  # tells whose it is. The trailing separator keeps ...\keploy from matching a
  # sibling ...\keploy2.
  $wsPrefix = $null
  if ($ws) { $wsPrefix = $ws.TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar }
  $keploy = @(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -like 'keploy*' })
  $own = @($keploy | Where-Object {
      $_.Path -and $wsPrefix -and $_.Path.StartsWith($wsPrefix, [System.StringComparison]::OrdinalIgnoreCase)
  })

  # 1. This workspace's leftover keploy processes (from an interrupted run of
  #    this runner). They hold files open under the workspace's bin/, notably
  #    WinDivert64.sys, and actions/checkout's clean then fails with EPERM.
  foreach ($p in $own) {
    Write-Host "Stopping this workspace's leftover $($p.ProcessName) pid=$($p.Id) path=$($p.Path)"
    try { $p | Stop-Process -Force -ErrorAction SilentlyContinue } catch {}
  }

  # 2. The WinDivert driver keploy registers ("WinDivert", or "WinDivert64")
  #    pins its .sys the same way. It is ONE service for the whole machine,
  #    so it is reset only when no other keploy is running. A keploy whose
  #    path cannot be read counts as another's.
  $others = @($keploy | Where-Object { $own -notcontains $_ })
  if ($others.Count -eq 0) {
    foreach ($name in @('WinDivert', 'WinDivert64')) {
      $svc = Get-Service -Name $name -ErrorAction SilentlyContinue
      if ($svc -and $svc.Status -ne 'Stopped') {
        Write-Host "No other keploy is running on this machine; stopping the kernel driver service $name (status=$($svc.Status))"
        try { Stop-Service -Name $name -Force -ErrorAction SilentlyContinue } catch {}
      }
    }
  } else {
    Write-Host "Not resetting the WinDivert driver: $($others.Count) other keploy process(es) are running on this machine, and a sibling job may be capturing through it."
  }

  # 3. A prior run interrupted mid-cleanup can leave the workspace with files
  #    but no .git, and actions/checkout's clean then fails with "not a git
  #    repository". Wipe it so checkout starts fresh; a workspace with a .git
  #    is left alone, so checkout's clean+fetch fast path keeps working.
  if ($ws -and (Test-Path -LiteralPath $ws)) {
    $items = @(Get-ChildItem -LiteralPath $ws -Force -ErrorAction SilentlyContinue)
    if (($items.Count -gt 0) -and -not (Test-Path -LiteralPath (Join-Path $ws '.git'))) {
      Write-Host "::warning::Workspace $ws has files but no .git -- wiping for fresh checkout (prior run was interrupted)"
      Remove-Item -Path (Join-Path $ws '*') -Recurse -Force -ErrorAction SilentlyContinue
    }
  }
} catch {
  Write-Host "::warning::Recover stale runner state: $_"
}
exit 0

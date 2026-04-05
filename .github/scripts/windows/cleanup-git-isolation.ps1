<#
.SYNOPSIS
  Prunes stale git isolation directories from previous runs.

.DESCRIPTION
  Does NOT delete the current run's directory because the
  actions/checkout post-job hook still needs GIT_CONFIG_GLOBAL.
  Only removes directories from OTHER runs that are older than 2 hours
  AND do not have an .active marker file (indicating a still-running job).
  The current run's directory will be cleaned by a future run's pruning
  once it ages past the 2-hour threshold.
#>

param(
    [string]$IsoRoot = '.git-isolation'
)

$root = Join-Path $env:USERPROFILE $IsoRoot
if (-not (Test-Path $root)) { return }

$staleCutoff = (Get-Date).AddHours(-2)
Get-ChildItem -Path $root -Directory | Where-Object {
    # Skip current run
    $_.Name -ne "$env:GITHUB_RUN_ID" -and
    # Only prune dirs older than 2 hours
    $_.LastWriteTime -lt $staleCutoff -and
    # Skip dirs with a recent .active marker (job may still be running)
    (-not (Test-Path (Join-Path $_.FullName '.active')) -or
     (Get-Item (Join-Path $_.FullName '.active')).LastWriteTime -lt $staleCutoff)
} | ForEach-Object {
    Remove-Item $_.FullName -Recurse -Force -ErrorAction SilentlyContinue
    Write-Host "Pruned stale isolation dir: $($_.Name)"
}

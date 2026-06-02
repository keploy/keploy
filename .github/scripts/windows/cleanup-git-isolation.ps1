<#
.SYNOPSIS
  Prunes stale git isolation directories from previous runs.

.DESCRIPTION
  Does NOT delete directories from the current workflow run because
  actions/checkout post-job hooks still need GIT_CONFIG_GLOBAL.
  Only removes directories from OTHER runs that are both:
    (a) older than 2 hours, AND
    (b) either lacking an .active marker or having a marker whose
        LastWriteTime is also older than the 2-hour cutoff.
  Current run directories will be cleaned by a future run's
  pruning once it ages past the threshold.
#>

param(
    [string]$IsoRoot = '.git-isolation'
)

$root = Join-Path $env:USERPROFILE $IsoRoot
if (-not (Test-Path $root)) { return }

$staleCutoff = (Get-Date).AddHours(-2)
$currentRunPrefix = "$env:GITHUB_RUN_ID-"
$currentIsoName = $env:GIT_ISOLATION_ID
Get-ChildItem -Path $root -Directory | Where-Object {
    # Skip current run/job dirs; actions/checkout post-job hooks may still
    # need the exported GIT_CONFIG_GLOBAL after this cleanup step completes.
    $_.Name -ne "$env:GITHUB_RUN_ID" -and
    ($_.Name -notlike "$currentRunPrefix*") -and
    ([string]::IsNullOrWhiteSpace($currentIsoName) -or $_.Name -ne $currentIsoName) -and
    # Only prune dirs older than 2 hours
    $_.LastWriteTime -lt $staleCutoff -and
    # Skip dirs with a recent .active marker (job may still be running)
    (-not (Test-Path (Join-Path $_.FullName '.active')) -or
     (Get-Item (Join-Path $_.FullName '.active')).LastWriteTime -lt $staleCutoff)
} | ForEach-Object {
    Remove-Item $_.FullName -Recurse -Force -ErrorAction SilentlyContinue
    Write-Host "Pruned stale isolation dir: $($_.Name)"
}

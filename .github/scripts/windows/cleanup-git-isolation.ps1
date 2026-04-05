<#
.SYNOPSIS
  Prunes stale git isolation directories from previous runs.

.DESCRIPTION
  Does NOT delete the current run's directory (checkout post-job hook
  still needs GIT_CONFIG_GLOBAL). Only removes dirs from other runs
  that are older than 2 hours. Current run's dir will be cleaned by
  a future run's pruning.
#>

param(
    [string]$IsoRoot = '.git-isolation'
)

$root = Join-Path $env:USERPROFILE $IsoRoot
if (-not (Test-Path $root)) { return }

$staleCutoff = (Get-Date).AddHours(-2)
Get-ChildItem -Path $root -Directory | Where-Object {
    $_.Name -ne "$env:GITHUB_RUN_ID" -and $_.LastWriteTime -lt $staleCutoff
} | ForEach-Object {
    Remove-Item $_.FullName -Recurse -Force -ErrorAction SilentlyContinue
    Write-Host "Pruned stale isolation dir: $($_.Name)"
}

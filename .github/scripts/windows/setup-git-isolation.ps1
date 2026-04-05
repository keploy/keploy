<#
.SYNOPSIS
  Creates per-GITHUB_RUN_ID isolated git config and SSH known_hosts on
  Windows self-hosted runners. Exports GIT_CONFIG_GLOBAL and GIT_SSH_COMMAND
  so concurrent jobs on the same machine cannot interfere with each other.

.DESCRIPTION
  Called at the start of every self-hosted Windows job. Seeds a per-run
  gitconfig from the global one (if present), removes any inherited
  SSH redirect settings that would break HTTPS-based checkout, populates
  a per-run known_hosts via ssh-keyscan, and exports both env vars.

  GIT_SSH_COMMAND still honors SSH_AUTH_SOCK, so webfactory/ssh-agent
  keys remain accessible.
#>

param(
    [string]$IsoRoot = '.git-isolation'
)

$ErrorActionPreference = 'Stop'

$isoDir = Join-Path $env:USERPROFILE $IsoRoot $env:GITHUB_RUN_ID
New-Item -Path $isoDir -ItemType Directory -Force | Out-Null

# --- Per-run gitconfig ---
$gitConfig = Join-Path $isoDir 'gitconfig'
$globalCfg = Join-Path $env:USERPROFILE '.gitconfig'
if (Test-Path $globalCfg) {
    Copy-Item $globalCfg $gitConfig -Force
} else {
    New-Item -Path $gitConfig -ItemType File -Force | Out-Null
}

# Remove inherited SSH redirect settings so that the initial
# actions/checkout always uses HTTPS (before ssh-agent is loaded).
$staleKeys = @(
    'url."git@github.com:".insteadOf',
    'url."ssh://git@github.com/".insteadOf',
    'core.sshCommand'
)
foreach ($key in $staleKeys) {
    git config --file $gitConfig --unset-all $key 2>$null
    # exit code 5 = key not found (OK), anything > 1 other than 5 is a real error
}

# Set EOL handling
git config --file $gitConfig core.autocrlf false
git config --file $gitConfig core.eol lf

# --- Per-run SSH known_hosts ---
$knownHosts = Join-Path $isoDir 'known_hosts'
$hostKeys = ssh-keyscan github.com 2>$null
if ([string]::IsNullOrWhiteSpace(($hostKeys | Out-String))) {
    throw "ssh-keyscan did not return any host keys for github.com. Check network access from this runner."
}
$hostKeys | Out-File -FilePath $knownHosts -Encoding ascii

# --- Export env vars ---
# GIT_SSH_COMMAND with per-run known_hosts. SSH_AUTH_SOCK is still
# honored so webfactory/ssh-agent keys remain accessible.
"GIT_CONFIG_GLOBAL=$gitConfig" | Out-File -FilePath $env:GITHUB_ENV -Encoding utf8 -Append
"GIT_SSH_COMMAND=ssh -o UserKnownHostsFile=`"$knownHosts`" -o StrictHostKeyChecking=yes" | Out-File -FilePath $env:GITHUB_ENV -Encoding utf8 -Append

Write-Host "Git isolation setup complete for run $env:GITHUB_RUN_ID"
Write-Host "  GIT_CONFIG_GLOBAL = $gitConfig"
Write-Host "  GIT_SSH_COMMAND   = ssh -o UserKnownHostsFile=`"$knownHosts`" -o StrictHostKeyChecking=yes"

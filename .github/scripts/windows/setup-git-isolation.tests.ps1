# Tests for the writers of the SSH known_hosts that Windows jobs check
# github.com against: setup-git-isolation.ps1; prepare_and_run.yml's "Setup per-job
# git isolation" step, which runs before the checkout and so carries the
# script's logic inline; and the setup-private-parsers action's Windows step.
# Each must write GitHub's pinned host keys (.github/ssh/github_known_hosts)
# with no network call. Here an ssh-keyscan first on PATH fails as one did on
# a self-hosted runner whose DNS blipped ("getaddrinfo github.com: No such
# host is known"), and notes each call. Plain PowerShell, no Pester, so it
# runs unchanged under Windows PowerShell 5.1 (Windows CI scripts on
# windows-latest) and PowerShell 7 on Linux (Windows CI scripts on ubuntu):
#
#   pwsh -NoProfile -File .github/scripts/windows/setup-git-isolation.tests.ps1
#
# Each case runs its writer in a PowerShell of its own, as a workflow step
# does, with USERPROFILE and GITHUB_ENV in a directory of its own: nothing here
# touches the machine's own git config, known_hosts or isolation directories.
# It needs git and ssh-keygen on PATH (OpenSSH reads each file written).
$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = Split-Path -Parent (Split-Path -Parent (Split-Path -Parent $here))
$setup = Join-Path $here 'setup-git-isolation.ps1'
$pinnedPath = Join-Path (Join-Path (Join-Path $repoRoot '.github') 'ssh') 'github_known_hosts'
$actionDir = Join-Path (Join-Path (Join-Path $repoRoot '.github') 'actions') 'setup-private-parsers'
$tempBase = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { [IO.Path]::GetTempPath() }
$work = Join-Path $tempBase ("git-isolation-tests-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null
$onWindows = [IO.Path]::DirectorySeparatorChar -eq '\'
$failures = 0

function Report([string]$what, [object[]]$problems, [string[]]$output) {
    $problems = @($problems | Where-Object { $_ })
    if ($problems.Count -eq 0) { Write-Host "ok   - $what"; return }
    $script:failures++
    Write-Host "FAIL - $what"
    foreach ($p in $problems) { Write-Host "       $p" }
    foreach ($l in @($output)) { Write-Host "       | $l" }
}

$pinnedKeys = @(Get-Content -LiteralPath $pinnedPath | Where-Object { $_ -match '^github\.com ' })
$publishedPrints = @(Get-Content -LiteralPath $pinnedPath | Where-Object { $_ -match '^# fingerprint (SHA256:\S+) ' } | ForEach-Object { ($_ -split ' ')[2] } | Sort-Object)

# The stand-in ssh-keyscan, in a directory put first on each case's PATH.
$fakeBin = Join-Path $work 'fakebin'
New-Item -ItemType Directory -Path $fakeBin | Out-Null
if ($onWindows) {
    [IO.File]::WriteAllText((Join-Path $fakeBin 'ssh-keyscan.cmd'),
        "@echo off`r`necho ssh-keyscan %*>>`"%FAKE_KEYSCAN_LOG%`"`r`necho getaddrinfo github.com: No such host is known. 1>&2`r`nexit /b 255`r`n")
} else {
    $fake = Join-Path $fakeBin 'ssh-keyscan'
    [IO.File]::WriteAllText($fake,
        "#!/bin/sh`necho `"ssh-keyscan `$*`" >>`"`$FAKE_KEYSCAN_LOG`"`necho 'getaddrinfo github.com: No such host is known.' >&2`nexit 255`n")
    & chmod +x $fake
    if ($LASTEXITCODE -ne 0) { throw "chmod +x $fake failed" }
}

function New-Case([string]$name) {
    $d = Join-Path $work $name
    $userProfile = Join-Path $d 'home'
    New-Item -ItemType Directory -Path $userProfile -Force | Out-Null
    $githubEnv = Join-Path $d 'github_env'
    New-Item -ItemType File -Path $githubEnv -Force | Out-Null
    [pscustomobject]@{ Dir = $d; Home = $userProfile; GithubEnv = $githubEnv; Calls = (Join-Path $d 'keyscan-calls') }
}

# Invoke-Writer CASE SCRIPT [ENV]: runs SCRIPT in a PowerShell of its own, as
# a workflow step does, with the case's USERPROFILE, GITHUB_ENV and PATH (the
# stand-in ssh-keyscan first). Windows PowerShell 5.1 (and pwsh before 7.2)
# turns a native command's stderr into errors, which 'Stop' would throw on.
function Invoke-Writer($case, [string]$scriptPath, [hashtable]$extra = @{}) {
    $vars = @{
        USERPROFILE      = $case.Home
        GITHUB_ENV       = $case.GithubEnv
        GIT_ISOLATION_ID = 'case'
        FAKE_KEYSCAN_LOG = $case.Calls
        PATH             = $fakeBin + [IO.Path]::PathSeparator + $env:PATH
    }
    foreach ($k in $extra.Keys) { $vars[$k] = $extra[$k] }
    $saved = @{}
    foreach ($k in $vars.Keys) {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k)
        [Environment]::SetEnvironmentVariable($k, $vars[$k])
    }
    $exe = (Get-Process -Id $PID).Path
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $out = & $exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File $scriptPath 2>&1 | ForEach-Object { "$_" }
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $prev
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
    }
    [pscustomobject]@{ Code = $code; Out = @($out) }
}

# Check-KnownHosts CASE RESULT PATH: the problems with a writer's run whose
# known_hosts is PATH: it must exit 0, never call ssh-keyscan, and write the
# pinned keys, in ASCII with no BOM, which OpenSSH reads as the keys whose
# fingerprints GitHub publishes.
function Check-KnownHosts($case, $r, [string]$path) {
    if ($r.Code -ne 0) { "exit $($r.Code), want 0" }
    if (Test-Path -LiteralPath $case.Calls) { "it ran ssh-keyscan: $((Get-Content -LiteralPath $case.Calls) -join '; ')" }
    if (-not (Test-Path -LiteralPath $path)) { "no known_hosts at $path"; return }
    $got = @(Get-Content -LiteralPath $path)
    if (($got -join "`n") -ne ($pinnedKeys -join "`n")) { "known_hosts holds [$($got -join ' | ')], want the $($pinnedKeys.Count) pinned keys" }
    $bytes = [IO.File]::ReadAllBytes($path)
    if (@($bytes | Where-Object { $_ -gt 127 }).Count) { "known_hosts is not ASCII (first bytes $(($bytes[0..2] | ForEach-Object { '{0:X2}' -f $_ }) -join ' '))" }
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $lines = @(& ssh-keygen -l -f $path 2>&1 | ForEach-Object { "$_" })
        $kgCode = $LASTEXITCODE
    } finally { $ErrorActionPreference = $prev }
    $prints = @($lines | ForEach-Object { ($_ -split ' ')[1] } | Where-Object { $_ -like 'SHA256:*' } | Sort-Object)
    if ($kgCode -ne 0) { "ssh-keygen -l could not read it (exit $kgCode): $($lines -join ' | ')" }
    elseif (($prints -join ',') -ne ($publishedPrints -join ',')) { "OpenSSH reads fingerprints [$($prints -join ', ')], want the published [$($publishedPrints -join ', ')]" }
}

# Get-RunBlocks FILE STEP: each run: | block of a step named STEP in FILE,
# dedented, with the line it starts at.
function Get-RunBlocks([string]$file, [string]$step) {
    $lines = [string[]]@(Get-Content -LiteralPath $file)
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -notmatch ('^(\s*)- name: ' + [regex]::Escape($step) + '\s*$')) { continue }
        $stepIndent = $Matches[1].Length
        $runIndent = -1
        for ($j = $i + 1; $j -lt $lines.Count; $j++) {
            if ($lines[$j] -match '^(\s*)run: \|\s*$') { $runIndent = $Matches[1].Length; break }
            if (($lines[$j] -match '^(\s*)-\s') -and ($Matches[1].Length -le $stepIndent)) { break }
        }
        if ($runIndent -lt 0) { continue }
        $body = New-Object System.Collections.ArrayList
        for ($k = $j + 1; $k -lt $lines.Count; $k++) {
            if (($lines[$k].Trim() -ne '') -and (($lines[$k] -replace '^(\s*).*$', '$1').Length -le $runIndent)) { break }
            [void]$body.Add($lines[$k])
        }
        $cut = $runIndent + 2
        [pscustomobject]@{
            Where = "$(Split-Path -Leaf $file):$($i + 1)"
            Text  = (@($body | ForEach-Object { if ($_.Length -ge $cut) { $_.Substring($cut) } else { $_.TrimStart() } }) -join "`n").TrimEnd()
        }
    }
}

try {
    if ($pinnedKeys.Count -eq 0 -or $publishedPrints.Count -ne $pinnedKeys.Count) {
        throw "$pinnedPath holds $($pinnedKeys.Count) key(s) and $($publishedPrints.Count) published fingerprint(s)"
    }
    foreach ($tool in 'git', 'ssh-keygen') {
        if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "$tool is not on PATH; these cases need it" }
    }

    # 1. The script, as golang_docker_windows and prepare_and_run's Windows
    #    jobs run it after their checkout (run 44095089090 failed here).
    $c = New-Case 'script'
    $r = Invoke-Writer $c $setup
    $kh = Join-Path (Join-Path (Join-Path $c.Home '.git-isolation') 'case') 'known_hosts'
    $envText = Get-Content -Raw -LiteralPath $c.GithubEnv
    Report "setup-git-isolation.ps1 writes the pinned keys while ssh-keyscan fails, and never calls it" @(
        (Check-KnownHosts $c $r $kh),
        $(if ($envText -notmatch [regex]::Escape("GIT_SSH_COMMAND=ssh -o UserKnownHostsFile=`"$kh`" -o GlobalKnownHostsFile=NUL -o StrictHostKeyChecking=yes")) {
            "GITHUB_ENV does not point ssh at only that known_hosts with StrictHostKeyChecking=yes: $envText" })
    ) $r.Out

    # 2. The same with no keys to write: it fails and says why, and writes no
    #    known_hosts that would leave ssh to fail later with "Host key
    #    verification failed".
    foreach ($how in 'no pinned file', 'a pinned file with no key') {
        $c = New-Case ($how -replace ' ', '-')
        $tree = Join-Path $c.Dir 'tree'
        $scripts = Join-Path (Join-Path (Join-Path $tree '.github') 'scripts') 'windows'
        New-Item -ItemType Directory -Path $scripts -Force | Out-Null
        Copy-Item -LiteralPath $setup -Destination $scripts
        if ($how -eq 'a pinned file with no key') {
            $sshDir = Join-Path (Join-Path $tree '.github') 'ssh'
            New-Item -ItemType Directory -Path $sshDir -Force | Out-Null
            Set-Content -LiteralPath (Join-Path $sshDir 'github_known_hosts') -Encoding ASCII -Value '# no keys'
        }
        $r = Invoke-Writer $c (Join-Path $scripts 'setup-git-isolation.ps1')
        $kh = Join-Path (Join-Path (Join-Path $c.Home '.git-isolation') 'case') 'known_hosts'
        Report "setup-git-isolation.ps1 with $how fails, says so, and writes no known_hosts" @(
            $(if ($r.Code -eq 0) { "exit 0, want a failure" }),
            $(if (($r.Out -join "`n") -notmatch 'github_known_hosts') { "its output does not name github_known_hosts" }),
            $(if (Test-Path -LiteralPath $kh) { "it wrote $kh" }),
            $(if (Test-Path -LiteralPath $c.Calls) { "it ran ssh-keyscan" })
        ) $r.Out
    }

    # 3. prepare_and_run.yml's inline copy, which runs before the checkout
    #    (build-windows-amd64), and any other step of that name that does not
    #    just run the script.
    $inline = @()
    $callers = @()
    foreach ($wf in @(Get-ChildItem -LiteralPath (Join-Path (Join-Path $repoRoot '.github') 'workflows') -File | Where-Object { $_.Extension -in '.yml', '.yaml' })) {
        $wfText = Get-Content -Raw -LiteralPath $wf.FullName
        foreach ($b in @(Get-RunBlocks $wf.FullName 'Setup per-job git isolation')) {
            if ($b.Text -match '^\s*&\s*"\$env:GITHUB_WORKSPACE\\\.github\\scripts\\windows\\setup-git-isolation\.ps1"\s*$') { $callers += $b.Where; continue }
            $isoRoot = if ($wfText -match '(?m)^\s*_ISO_ROOT:\s*(\S+)\s*$') { $Matches[1] } else { '' }
            $inline += [pscustomobject]@{ Where = $b.Where; Text = $b.Text.Replace('${{ env._ISO_ROOT }}', $isoRoot); IsoRoot = $isoRoot }
        }
    }
    if ($inline.Count -eq 0) {
        Report "prepare_and_run.yml's pre-checkout git isolation step writes the pinned keys" @("found no inline 'Setup per-job git isolation' step (callers of the script: $($callers -join ', '))") @()
    }
    foreach ($b in $inline) {
        $c = New-Case ('inline-' + ($b.Where -replace '[^A-Za-z0-9]', '-'))
        $problems = @()
        if (-not $b.IsoRoot) { $problems += "its workflow sets no _ISO_ROOT" }
        if ($b.Text -match '\$\{\{') { $problems += "it has an expression this case does not fill in: $([regex]::Match($b.Text, '\$\{\{[^}]*\}\}').Value)" }
        $stepFile = Join-Path $c.Dir 'step.ps1'
        [IO.File]::WriteAllText($stepFile, $b.Text + "`n")
        $r = Invoke-Writer $c $stepFile
        $kh = Join-Path (Join-Path (Join-Path $c.Home $b.IsoRoot) 'case') 'known_hosts'
        $problems += @(Check-KnownHosts $c $r $kh)
        Report "the inline git isolation step at $($b.Where) writes the pinned keys while ssh-keyscan fails, and never calls it" $problems $r.Out
    }

    # 4. The setup-private-parsers action's Windows step (jobs with no
    #    per-job isolation): it appends the pinned keys to ~/.ssh/known_hosts.
    $blocks = @(Get-RunBlocks (Join-Path $actionDir 'action.yaml') 'Add GitHub to known hosts (Windows)')
    $c = New-Case 'action'
    if ($blocks.Count -ne 1) {
        Report "the setup-private-parsers Windows step writes the pinned keys" @("found $($blocks.Count) 'Add GitHub to known hosts (Windows)' run blocks in action.yaml, want 1") @()
    } else {
        $stepFile = Join-Path $c.Dir 'step.ps1'
        [IO.File]::WriteAllText($stepFile, $blocks[0].Text + "`n")
        # The action's path as the runner sets it for "uses: ./.github/actions/...".
        $r = Invoke-Writer $c $stepFile @{ GITHUB_ACTION_PATH = ($repoRoot + [IO.Path]::DirectorySeparatorChar + './.github/actions/setup-private-parsers') }
        Report "the setup-private-parsers Windows step writes the pinned keys while ssh-keyscan fails, and never calls it" @(
            (Check-KnownHosts $c $r (Join-Path (Join-Path $c.Home '.ssh') 'known_hosts'))
        ) $r.Out
    }
} finally {
    Remove-Item -Recurse -Force -LiteralPath $work -ErrorAction SilentlyContinue
}

if ($failures -gt 0) {
    Write-Host "$failures case(s) failed"
    exit 1
}
Write-Host "all git isolation cases passed"
exit 0

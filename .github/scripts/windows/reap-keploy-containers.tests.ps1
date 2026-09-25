# Tests for the scripts in this directory that act on the self-hosted Windows
# runners' shared Docker daemon: reap-keploy-containers.ps1,
# cleanup-windows.ps1, take-docker-job-lock.ps1, remove-job-containers.ps1 (with
# register-job-compose-project.ps1), ensure-docker.ps1 and
# recover-runner-state.ps1. Plain PowerShell, no
# Pester, so it runs unchanged under Windows PowerShell 5.1 (what the
# self-hosted runners use) and PowerShell 7 on Linux (what the ubuntu CI job
# uses):
#
#   pwsh -NoProfile -File .github/scripts/windows/reap-keploy-containers.tests.ps1
#
# The scripts drive a fake docker CLI (-DockerExe) backed by a JSON state file,
# so every case pins the daemon clock and each container's timestamps exactly.
# Nothing here touches the real Docker daemon, Docker Desktop, processes or
# services: the scripts' actions on those go to fakes too. These tests run on
# the shared Windows machine itself, so every script refuses to run here with
# a parameter left at a default that reaches the machine's real Docker, Docker
# Desktop, lock directory or wedge records (KEPLOY_WINDOWS_SCRIPT_TESTS,
# test-overrides.ps1).
$ErrorActionPreference = 'Stop'
$savedTestsFlag = $env:KEPLOY_WINDOWS_SCRIPT_TESTS
$env:KEPLOY_WINDOWS_SCRIPT_TESTS = '1'

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$reaper = Join-Path $here 'reap-keploy-containers.ps1'
$cleanup = Join-Path $here 'cleanup-windows.ps1'
$takeLock = Join-Path $here 'take-docker-job-lock.ps1'
$ensure = Join-Path $here 'ensure-docker.ps1'
$recover = Join-Path $here 'recover-runner-state.ps1'
$removeJob = Join-Path $here 'remove-job-containers.ps1'
$register = Join-Path $here 'register-job-compose-project.ps1'
$repoRoot = Split-Path -Parent (Split-Path -Parent (Split-Path -Parent $here))
$work = Join-Path ([IO.Path]::GetTempPath()) ("reap-tests-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null
$fake = Join-Path $work 'fake-docker.ps1'
$stateFile = Join-Path $work 'state.json'

# The fake docker. Understands exactly the calls the scripts make, with
# docker's semantics (the name filter is an unanchored match); anything else
# fails loudly so a new call cannot silently get an empty answer.
Set-Content -Path $fake -Encoding ASCII -Value @'
# FAKE_DOCKER_CALLS, when set, gets a line per call.
if ($env:FAKE_DOCKER_CALLS) { Add-Content -LiteralPath $env:FAKE_DOCKER_CALLS -Value "$args" }
$state = Get-Content -Raw -Path $env:FAKE_DOCKER_STATE | ConvertFrom-Json
function Save { $state | ConvertTo-Json -Depth 5 | Set-Content -Path $env:FAKE_DOCKER_STATE -Encoding ASCII }
function Find($id) { @($state.containers | Where-Object { $_.id -eq $id }) }
# A down daemon comes up once a fake Docker Desktop start writes <state>.up.
# <state>.hang makes every call hang, the way a wedged Docker Desktop's do,
# and hangInfo hangs just the next that many `info` calls: the caller has to
# kill them.
function Test-Down { $state.daemonDown -and -not (Test-Path -LiteralPath "$env:FAKE_DOCKER_STATE.up") }
if (Test-Path -LiteralPath "$env:FAKE_DOCKER_STATE.hang") { Start-Sleep -Seconds 120 }
# stampOnCall counts down the calls until another runner's start or restart of
# Docker Desktop lands: that call first writes its docker-desktop.started to
# stampFile.
if ($state.stampOnCall -gt 0) {
    $state.stampOnCall--; Save
    if ($state.stampOnCall -eq 0) { Set-Content -LiteralPath $state.stampFile -Value "$([guid]::NewGuid().ToString('N')) another runner's restart" }
}
switch ($args[0]) {
    'info' {
        if ($state.hangInfo -gt 0) { $state.hangInfo--; Save; Start-Sleep -Seconds 120 }
        if (Test-Down) { exit 1 }
        # A daemon too busy to answer the next failInfo calls.
        if ($state.failInfo -gt 0) { $state.failInfo--; Save; exit 1 }
        Write-Output $state.systemTime; exit 0
    }
    'ps' {
        if (Test-Down) { exit 1 }
        # psDown: `info` answers but `ps` does not, until Docker Desktop starts.
        if ($state.psDown -and -not (Test-Path -LiteralPath "$env:FAKE_DOCKER_STATE.up")) { exit 1 }
        # A container whose removal is already under way leaves the list after
        # its countdown of `ps` calls, the way the daemon finishes it.
        # Written back only when that changed it: the background cases run
        # several scripts against one state, and must never read it half
        # written.
        $counting = @($state.containers | Where-Object { $_.removing })
        foreach ($c in $counting) { $c.removing--; if ($c.removing -eq 0) { $c.gone = $true } }
        if ($counting.Count) { $state.containers = @($state.containers | Where-Object { -not $_.gone }); Save }
        $filter = "$($args | Where-Object { "$_" -like 'name=*' -or "$_" -like 'label=*' } | Select-Object -First 1)"
        foreach ($c in @($state.containers)) {
            if ($filter -like 'name=*') { if ($c.name -notlike ("*" + $filter.Substring(5) + "*")) { continue } }
            elseif ($filter -like 'label=com.docker.compose.project=*') { if ($c.project -ne ($filter -replace '^label=com\.docker\.compose\.project=', '')) { continue } }
            elseif ($filter) { Write-Error "fake docker: unexpected filter $filter"; exit 2 }
            Write-Output $c.id
        }
        exit 0
    }
    'inspect' {
        $format = $args[2]; $c = Find $args[3]
        if ($c.Count -eq 0) { exit 1 }
        $c = $c[0]
        if ($format -eq '{{.Name}}') { Write-Output "/$($c.name)"; exit 0 }
        if ($format -eq '{{.Name}}|{{.Created}}|{{.State.StartedAt}}|{{.State.FinishedAt}}') {
            Write-Output "/$($c.name)|$($c.created)|$($c.started)|$($c.finished)"; exit 0
        }
        Write-Error "fake docker: unexpected inspect format $format"; exit 2
    }
    'rm' {
        $id = $args[2]
        $state.removed = @($state.removed) + @($id)
        $c = Find $id
        if (($c.Count -gt 0) -and $c[0].removing) {
            Save; Write-Error "Error response from daemon: removal of container $id is already in progress"; exit 1
        }
        if (($c.Count -gt 0) -and -not $c[0].stuck) {
            $state.containers = @($state.containers | Where-Object { $_.id -ne $id })
        }
        Save; exit 0
    }
    { $_ -in 'image', 'volume', 'system' } {
        # Record each prune, and whether a cleanup's in-progress marker was
        # down while it ran (FAKE_LOCK_DIR is the lock directory of the case).
        $what = $args[0]
        if ($env:FAKE_LOCK_DIR -and -not @(Get-ChildItem -LiteralPath $env:FAKE_LOCK_DIR -Filter 'docker-prune-*.inprogress' -ErrorAction SilentlyContinue).Count) {
            $what = "$what-unannounced"
        }
        $state.pruned = @($state.pruned) + @($what); Save; exit 0
    }
    default { Write-Error "fake docker: unexpected command $($args -join ' ')"; exit 2 }
}
'@

# A script parameter's default, so the cases follow the thresholds the
# scripts actually ship with.
function Get-Default($file, $param) {
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($file, [ref]$null, [ref]$null)
    [int](($ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq $param }).DefaultValue.Value)
}
$minAge = Get-Default $reaper 'MinAgeMinutes'

# What Invoke-RestMethod throws on an HTTP error, as far as the scripts can
# tell: an exception whose Response.StatusCode is the code.
Add-Type -TypeDefinition @'
public class FakeHttpResponse { public int StatusCode; }
public class FakeHttpException : System.Exception {
    public FakeHttpResponse Response = new FakeHttpResponse();
    public FakeHttpException(int code) : base("Response status code does not indicate success: " + code) { Response.StatusCode = code; }
}
'@

$NEVER = '0001-01-01T00:00:00Z'
# The daemon clock of the failing run: the moment run 36091939524's
# cleanup_windows began the sweep that removed keploy-v3-ee1c.
$NOW = '2026-09-25T04:05:33.163094100Z'
function Ago([double]$minutes) {
    # Nine fractional digits, as docker prints them.
    ([DateTimeOffset]::Parse('2026-09-25T04:05:33Z')).AddMinutes(-$minutes).UtcDateTime.ToString("yyyy-MM-dd'T'HH:mm:ss'.'fffffff'00Z'")
}
function Container($id, $name, $created, $started = $NEVER, $finished = $NEVER, $project = '', [switch]$Stuck, [int]$RemovingFor = 0) {
    [pscustomobject]@{ id = $id; name = $name; created = $created; started = $started; finished = $finished; project = $project; stuck = [bool]$Stuck; removing = $RemovingFor; gone = $false }
}
function Set-State($containers, $systemTime = $NOW, [switch]$DaemonDown, [switch]$PsDown, [int]$FailInfo = 0, [int]$HangInfo = 0) {
    [pscustomobject]@{ systemTime = $systemTime; daemonDown = [bool]$DaemonDown; psDown = [bool]$PsDown; failInfo = $FailInfo; hangInfo = $HangInfo; stampOnCall = 0; stampFile = ''; containers = @($containers); removed = @(); pruned = @() } |
        ConvertTo-Json -Depth 5 | Set-Content -Path $stateFile -Encoding ASCII
    Remove-Item -LiteralPath "$stateFile.up", "$stateFile.hang" -Force -ErrorAction SilentlyContinue
    $env:FAKE_DOCKER_STATE = $stateFile
}
function Get-State { Get-Content -Raw -Path $stateFile | ConvertFrom-Json }
function New-Dir($name) {
    $d = Join-Path $work ($name + '-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $d | Out-Null
    $d
}
# Runs a script; returns its output, exit code, the error records it wrote,
# and what the fake saw. An error the script throws ends up in Errors too, so
# it fails the case that caused it instead of aborting the whole run.
function Invoke-Script($script, [hashtable]$scriptArgs) {
    $ErrorActionPreference = 'Continue'
    if (-not $scriptArgs.ContainsKey('DockerExe')) { $scriptArgs.DockerExe = $fake }
    $global:LASTEXITCODE = 0
    $records = New-Object System.Collections.ArrayList
    try {
        & $script @scriptArgs *>&1 | ForEach-Object { [void]$records.Add($_) }
    } catch {
        [void]$records.Add($_)
    }
    $code = $LASTEXITCODE
    $after = Get-State
    [pscustomobject]@{
        Output  = ($records | Out-String)
        Errors  = @($records | Where-Object { $_ -is [System.Management.Automation.ErrorRecord] })
        Code    = $code
        Removed = @($after.removed | Where-Object { $_ })
        Pruned  = @($after.pruned | Where-Object { $_ })
    }
}
# Every -WedgeDir starts empty unless a case shares one on purpose.
# -SettleSeconds 0: a stuck container is reported on the first re-query
# instead of after the real 60s deadline.
function Invoke-Reaper([hashtable]$reapArgs = @{}) {
    if (-not $reapArgs.ContainsKey('WedgeDir')) { $reapArgs.WedgeDir = New-Dir 'wedge' }
    if (-not $reapArgs.ContainsKey('SettleSeconds')) { $reapArgs.SettleSeconds = 0 }
    Invoke-Script $reaper $reapArgs
}

# Runs $script in the background, in a runspace of its own in this process:
# no child process and no job infrastructure, which a runner service's Windows
# PowerShell may not provide (Start-Job there can fail with "The background
# process reported an error"). $blocks maps a parameter name to the text of a
# scriptblock, built inside that runspace, since a scriptblock belongs to the
# runspace that made it. $prelude is dot-sourced there first. Everything the
# script writes, on any stream, goes to Out, then "exit=<its exit code>".
$bgWrapper = {
    param($script, [hashtable]$params, [hashtable]$blocks, [string]$prelude)
    $p = @{}
    foreach ($k in @($params.Keys)) { $p[$k] = $params[$k] }
    foreach ($k in @($blocks.Keys)) { $p[$k] = [scriptblock]::Create($blocks[$k]) }
    if ($prelude) { . ([scriptblock]::Create($prelude)) }
    & $script @p *>&1 | ForEach-Object { "$_" }
    "exit=$LASTEXITCODE"
}.ToString()
function Start-Background($script, [hashtable]$params, [hashtable]$blocks = @{}, [string]$prelude = '') {
    $ps = [powershell]::Create()
    [void]$ps.AddScript($bgWrapper).AddArgument($script).AddArgument($params).AddArgument($blocks).AddArgument($prelude)
    $in = New-Object 'System.Management.Automation.PSDataCollection[psobject]'
    $in.Complete()
    $out = New-Object 'System.Management.Automation.PSDataCollection[psobject]'
    [pscustomobject]@{ PS = $ps; Out = $out; Handle = $ps.BeginInvoke($in, $out) }
}
# What it has written so far, and anything that stopped it. These helpers
# also take a script started in a process of its own (Start-Ensure below):
# Proc is that process, and Log the file it writes to.
function Read-Background($bg) {
    if ($bg.Proc) {
        $text = ''
        try {
            $fs = [IO.File]::Open($bg.Log, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::ReadWrite)
            try { $text = (New-Object IO.StreamReader($fs)).ReadToEnd() } finally { $fs.Dispose() }
        } catch { }
        if ($bg.Proc.HasExited) { $text += $bg.Err.Result + $bg.StdOut.Result }
        return $text
    }
    $lines = @(for ($i = 0; $i -lt $bg.Out.Count; $i++) { "$($bg.Out[$i])" })
    if ($bg.Handle.IsCompleted) {
        $lines += @($bg.PS.Streams.Error | ForEach-Object { "ERROR: $_" })
        if ($bg.PS.InvocationStateInfo.Reason) { $lines += "FAILED: $($bg.PS.InvocationStateInfo.Reason.Message)" }
    }
    $lines -join "`n"
}
function Test-BackgroundDone($bg) {
    if ($bg.Proc) { return $bg.Proc.HasExited }
    $bg.Handle.IsCompleted
}
# $true once it has finished; $false if it is still going after $seconds.
function Wait-Background($bg, [int]$seconds) {
    if ($bg.Proc) { return $bg.Proc.WaitForExit($seconds * 1000) }
    $deadline = [DateTime]::UtcNow.AddSeconds($seconds)
    while (-not $bg.Handle.IsCompleted) {
        if ([DateTime]::UtcNow -ge $deadline) { return $false }
        Start-Sleep -Milliseconds 100
    }
    $true
}
function Get-BackgroundExit($bg) {
    if ($bg.Proc) { if ($bg.Proc.HasExited) { return $bg.Proc.ExitCode } else { return $null } }
    if ((Read-Background $bg) -match '(?m)^exit=(\d+)\s*$') { [int]$Matches[1] } else { $null }
}
function Stop-Background($bg) {
    if ($bg.Proc) {
        if (-not $bg.Proc.HasExited) { try { $bg.Proc.Kill() } catch { } }
        $bg.Proc.Dispose()
        return
    }
    if (-not $bg.Handle.IsCompleted) { $bg.PS.Stop() }
    $bg.PS.Dispose()
}

$failures = 0
function Report($title, $result, [object[]]$problems) {
    $problems = @($problems | Where-Object { $_ })
    if ($problems.Count -eq 0) {
        Write-Host "ok   - $title"
    } else {
        $script:failures++
        Write-Host "FAIL - $title"
        foreach ($p in $problems) { Write-Host "       $p" }
        Write-Host ($result.Output -replace '(?m)^', '       | ')
    }
}
function Removed($result, [string[]]$want) {
    $got = @($result.Removed | Sort-Object -Unique) -join ','
    $exp = @($want | Sort-Object) -join ','
    if ($got -ne $exp) { "removed [$got], want [$exp]" }
}
function Code($result, $want) { if ($result.Code -ne $want) { "exit $($result.Code), want $want" } }
function Says($result, [string]$pattern, [string]$what) { if ($result.Output -notmatch $pattern) { "missing $what" } }
function NoErrors($result) {
    if ($result.Errors.Count) { "wrote $($result.Errors.Count) error record(s): $(($result.Errors | ForEach-Object { $_.Exception.GetType().Name + ': ' + $_.Exception.Message }) -join ' | ')" }
}

try {
    # ---- the age sweep ------------------------------------------------------

    Set-State @((Container 'ee1c' 'keploy-v3-ee1c' (Ago 0.5) (Ago 0.4)))
    $r = Invoke-Reaper
    Report "a sibling job's live agent (the run 36092000545 case) is left alone" $r @((Removed $r @()), (Code $r 0))

    Set-State @(
        (Container 'live' 'keploy-v3-live' (Ago 3) (Ago 3)),
        (Container 'old' 'keploy-v3-old' (Ago 360) (Ago 360))
    )
    $r = Invoke-Reaper
    Report "a leftover is removed while a live agent beside it is kept" $r @((Removed $r @('old')), (Code $r 0))

    Set-State @(
        (Container 'restarted' 'keploy-v3-restarted' (Ago 180) (Ago 1)),
        (Container 'justexited' 'keploy-v3-justexited' (Ago 45) (Ago 45) (Ago 2)),
        (Container 'longexited' 'keploy-v3-longexited' (Ago 90) (Ago 90) (Ago 60)),
        (Container 'neverstarted' 'keploy-v3-neverstarted' (Ago ($minAge + 5))),
        (Container 'justcreated' 'keploy-v3-justcreated' (Ago 0.01))
    )
    $r = Invoke-Reaper
    Report "age is the most recent lifecycle event, and a never-started container is aged from creation" $r @(Removed $r @('longexited', 'neverstarted'))

    Set-State @(
        (Container 'inside' 'keploy-v3-inside' (Ago ($minAge - 0.1)) (Ago ($minAge - 0.1))),
        (Container 'past' 'keploy-v3-past' (Ago ($minAge + 0.1)) (Ago ($minAge + 0.1)))
    )
    $r = Invoke-Reaper
    Report "just inside and just past the threshold" $r @(Removed $r @('past'))

    # docker's own name filter returns my-keploy-v3-db for "name=keploy-v3-";
    # the fake does too, so only the reaper's prefix check keeps it.
    Set-State @(
        (Container 'app' 'dedup-go-5e7e50f3' (Ago 300) (Ago 300)),
        (Container 'db' 'my-keploy-v3-db' (Ago 300) (Ago 300))
    )
    $r = Invoke-Reaper
    Report "only containers whose name starts with keploy-v3- are candidates" $r @(Removed $r @())

    # cleanup-windows sweeps every container. dedup-go stays Created for ~6s
    # on every compose up while compose waits for the agent to turn healthy.
    Set-State @(
        (Container 'app' 'dedup-go-5e7e50f3' (Ago 0.1)),
        (Container 'agent' 'keploy-v3-7a1b' (Ago 0.01)),
        (Container 'oldapp' 'dedup-go-0badf00d' (Ago 120) (Ago 120) (Ago 90))
    )
    $r = Invoke-Reaper @{ NamePrefix = '' }
    Report "sweeping every container keeps a sibling's Created-state app and agent" $r @(Removed $r @('oldapp'))

    Set-State @((Container 'garbled' 'keploy-v3-garbled' 'not-a-time' 'also-not' ''))
    $r = Invoke-Reaper
    Report "a container whose age cannot be read is left alone" $r @(Removed $r @())

    Set-State @((Container 'old' 'keploy-v3-old' (Ago 360) (Ago 360))) -systemTime 'garbage'
    $r = Invoke-Reaper
    # Guarded, not left to `$null - [DateTimeOffset]` throwing and a $null idle
    # time happening to compare below the threshold.
    Report "an unreadable daemon clock reaps nothing by age" $r @(
        (Removed $r @()), (Code $r 0), (NoErrors $r),
        (Says $r "::warning::could not read the Docker daemon's clock \(got 'garbage'\); sweeping nothing by age" "the unreadable-clock warning")
    )

    Set-State @((Container 'old' 'keploy-v3-old' (Ago 360) (Ago 360))) -DaemonDown
    $r = Invoke-Reaper
    Report "an unreachable daemon reaps nothing" $r @((Removed $r @()), (Code $r 0))

    Set-State @(
        (Container 'wedged' 'keploy-v3-wedged' (Ago 120) (Ago 120) -Stuck),
        (Container 'live' 'keploy-v3-live' (Ago 1) (Ago 1))
    )
    $r = Invoke-Reaper @{ FailOnStuck = $true }
    Report "a leftover that survives removal fails the pre-job reap" $r @(
        (Removed $r @('wedged')), (Code $r 1),
        $(if ($r.Output -notmatch 'Docker VM is wedged: 1 container\(s\) survived .* - keploy-v3-wedged\. ') { "missing the wedged-VM error naming exactly keploy-v3-wedged" })
    )

    Set-State @((Container 'wedged' 'keploy-v3-wedged' (Ago 120) (Ago 120) -Stuck))
    $r = Invoke-Reaper
    Report "the post-run reap only warns about a wedged VM" $r @(
        (Code $r 0), $(if ($r.Output -notmatch '::warning::Docker VM is wedged') { "missing the wedged-VM warning" })
    )

    # keploy's own teardown is still removing its agent when the job's
    # teardown step asks: rm -f says "already in progress" and the container
    # is listed (state "removing") for a moment. That is not a wedge.
    Set-State @((Container 'mine' 'keploy-v3-eeee' (Ago 2) (Ago 2) $NEVER 'keploy-5e7e50f3' -RemovingFor 3))
    $shared = New-Dir 'wedge'
    $r = Invoke-Reaper @{ ComposeProject = 'keploy-5e7e50f3'; FailOnStuck = $true; SettleSeconds = 10; WedgeDir = $shared }
    Report "a removal already in progress elsewhere is waited for, not reported as a wedge" $r @(
        (Code $r 0), $(if (@(Get-ChildItem $shared).Count -ne 0) { "wrote a wedge record for a healthy removal" })
    )

    # ---- the owner's teardown, and wedge detection that does not wait -------

    Set-State @(
        (Container 'mine1' 'keploy-v3-aaaa' (Ago 2) (Ago 2) $NEVER 'keploy-5e7e50f3'),
        (Container 'mine2' 'dedup-go-5e7e50f3' (Ago 2) (Ago 2) (Ago 0.2) 'keploy-5e7e50f3'),
        (Container 'sib' 'keploy-v3-bbbb' (Ago 1) (Ago 1) $NEVER 'keploy-0c0ffee0'),
        (Container 'sibapp' 'dedup-go-0c0ffee0' (Ago 1) $NEVER $NEVER 'keploy-0c0ffee0')
    )
    $r = Invoke-Reaper @{ ComposeProject = 'keploy-5e7e50f3'; FailOnStuck = $true }
    Report "the owning job removes exactly its own containers, young as they are" $r @((Removed $r @('mine1', 'mine2')), (Code $r 0))

    # Another runner recorded a wedge during this job. The owner's teardown
    # neither retries it nor fails for it; the next pre-job reap does.
    $shared = New-Dir 'wedge'
    Set-Content -Path (Join-Path $shared 'other.wedged') -Value 'keploy-v3-9999'
    Set-State @(
        (Container 'mine' 'keploy-v3-aaaa' (Ago 2) (Ago 2) $NEVER 'keploy-5e7e50f3'),
        (Container 'other' 'keploy-v3-9999' (Ago 5) (Ago 5) $NEVER 'keploy-0c0ffee0' -Stuck)
    )
    $r = Invoke-Reaper @{ ComposeProject = 'keploy-5e7e50f3'; WedgeDir = $shared }
    Report "the owner's teardown leaves another runner's recorded wedge to the pre-job reap" $r @(
        (Removed $r @('mine')), (Code $r 0),
        $(if (-not (Test-Path (Join-Path $shared 'other.wedged'))) { "dropped the other runner's wedge record" })
    )

    $shared = New-Dir 'wedge'
    Set-State @(
        (Container 'mine' 'keploy-v3-cccc' (Ago 3) (Ago 3) $NEVER 'keploy-5e7e50f3' -Stuck),
        (Container 'sib' 'keploy-v3-dddd' (Ago 1) (Ago 1) $NEVER 'keploy-0c0ffee0')
    )
    $r = Invoke-Reaper @{ ComposeProject = 'keploy-5e7e50f3'; WedgeDir = $shared }
    $marker = Join-Path $shared 'mine.wedged'
    Report "the owning job warns, and records its agent, when it survives removal" $r @(
        (Removed $r @('mine')), (Code $r 0),
        $(if ($r.Output -notmatch '::warning::Docker VM is wedged') { "missing the wedged-VM warning" }),
        $(if (-not (Test-Path $marker)) { "no wedge record written" })
    )

    # The next job, on any runner: the wedged agent is 3 minutes old, far
    # below the age threshold, and must still stop the job from starting.
    $state = Get-State; $state.removed = @(); $state | ConvertTo-Json -Depth 5 | Set-Content -Path $stateFile -Encoding ASCII
    $r = Invoke-Reaper @{ FailOnStuck = $true; WedgeDir = $shared }
    Report "a young wedged container still fails the next pre-job reap" $r @(
        (Removed $r @('mine')), (Code $r 1),
        $(if ($r.Output -notmatch '::error::Docker VM is wedged: 1 container\(s\) survived .* - keploy-v3-cccc\. ') { "missing the wedged-VM error naming keploy-v3-cccc" }),
        $(if (-not (Test-Path $marker)) { "the wedge record was dropped while the container is still stuck" })
    )

    # `wsl --shutdown`: the container survives the restart as an exited
    # container that can be removed now.
    $state = Get-State; $state.removed = @()
    foreach ($c in $state.containers) { if ($c.id -eq 'mine') { $c.stuck = $false } }
    $state | ConvertTo-Json -Depth 5 | Set-Content -Path $stateFile -Encoding ASCII
    $r = Invoke-Reaper @{ FailOnStuck = $true; WedgeDir = $shared }
    Report "after the VM restart the recorded container is removed and jobs start again" $r @(
        (Removed $r @('mine')), (Code $r 0),
        $(if (Test-Path $marker) { "the wedge record outlived the wedge" })
    )

    # ---- remove-job-containers.ps1: the owner's teardown step ---------------

    # The plumbing end to end: the run step records its project through
    # register-job-compose-project.ps1 into GITHUB_ENV; the runner hands
    # GITHUB_ENV's lines to later steps as environment; the teardown step
    # reads it and removes exactly that project's containers.
    function Read-GitHubEnv($file) {
        $vars = @{}
        foreach ($l in @(Get-Content -Path $file -Encoding UTF8)) {
            $l = $l.TrimStart([char]0xFEFF)
            if ($l -match '^([^=]+)=(.*)$') { $vars[$Matches[1]] = $Matches[2] }
        }
        $vars
    }
    $saved = @{}
    foreach ($v in 'GITHUB_ENV', 'EARLIER', 'COMPOSE_PROJECT_NAME', 'KEPLOY_JOB_COMPOSE_PROJECT', 'KEPLOY_DOCKER_JOB_LOCK', 'KEPLOY_START_STEP_OUTCOME') {
        $saved[$v] = [Environment]::GetEnvironmentVariable($v)
    }
    function Clear-JobEnv { foreach ($v in 'COMPOSE_PROJECT_NAME', 'KEPLOY_JOB_COMPOSE_PROJECT', 'KEPLOY_DOCKER_JOB_LOCK', 'KEPLOY_START_STEP_OUTCOME') { [Environment]::SetEnvironmentVariable($v, $null) } }
    try {
        Clear-JobEnv
        $envFile = Join-Path (New-Dir 'ghenv') 'env'
        Set-Content -Path $envFile -Value 'EARLIER=1'
        $env:GITHUB_ENV = $envFile
        & $register -Project 'keploy-5e7e50f3' 6>$null
        $problems = @()
        if ($env:COMPOSE_PROJECT_NAME -ne 'keploy-5e7e50f3') { $problems += "COMPOSE_PROJECT_NAME is '$env:COMPOSE_PROJECT_NAME' in the run step" }
        $vars = Read-GitHubEnv $envFile
        Clear-JobEnv; $env:GITHUB_ENV = $saved['GITHUB_ENV']
        foreach ($k in $vars.Keys) { [Environment]::SetEnvironmentVariable($k, $vars[$k]) }
        $lockDir = New-Dir 'locks'
        $env:KEPLOY_DOCKER_JOB_LOCK = Join-Path $lockDir 'docker-job-40-1-a.lock'
        Set-Content -Path $env:KEPLOY_DOCKER_JOB_LOCK -Value 'x'
        $env:KEPLOY_START_STEP_OUTCOME = 'failure'
        Set-State @(
            (Container 'mine' 'keploy-v3-aaaa' (Ago 2) (Ago 2) $NEVER 'keploy-5e7e50f3'),
            (Container 'sib' 'keploy-v3-bbbb' (Ago 1) (Ago 1) $NEVER 'keploy-0c0ffee0')
        )
        $r = Invoke-Script $removeJob @{ WedgeDir = (New-Dir 'wedge') }
        Report "the project the run step records reaches the teardown, which removes exactly its containers and releases the lock" $r @(
            $problems, (Removed $r @('mine')), (Code $r 0),
            $(if ($vars['KEPLOY_JOB_COMPOSE_PROJECT'] -ne 'keploy-5e7e50f3') { "GITHUB_ENV recorded [$($vars['KEPLOY_JOB_COMPOSE_PROJECT'])]" }),
            $(if (Test-Path -LiteralPath $env:KEPLOY_DOCKER_JOB_LOCK) { "the job's Docker lock was not released" }),
            $(if ($r.Output -match '::warning::') { "warned" })
        )

        # The run step started (and maybe started containers) but recorded
        # nothing: say only that, loudly.
        Clear-JobEnv
        $env:KEPLOY_DOCKER_JOB_LOCK = Join-Path $lockDir 'docker-job-41-1-a.lock'
        Set-Content -Path $env:KEPLOY_DOCKER_JOB_LOCK -Value 'x'
        $env:KEPLOY_START_STEP_OUTCOME = 'failure'
        Set-State @((Container 'mine' 'keploy-v3-aaaa' (Ago 2) (Ago 2) $NEVER 'keploy-5e7e50f3'))
        $r = Invoke-Script $removeJob @{ WedgeDir = (New-Dir 'wedge') }
        Report "a job that took its Docker lock and ran its app step but recorded no project warns, and claims nothing about its containers" $r @(
            (Removed $r @()), (Code $r 0),
            (Says $r '::warning::This job took its Docker lock but recorded no compose project' "the missing-project warning"),
            $(if ($r.Output -match 'started no containers') { "claimed it started no containers" }),
            $(if (Test-Path -LiteralPath $env:KEPLOY_DOCKER_JOB_LOCK) { "the job's Docker lock was not released" })
        )

        Clear-JobEnv
        $env:KEPLOY_DOCKER_JOB_LOCK = Join-Path $lockDir 'docker-job-42-1-a.lock'
        Set-Content -Path $env:KEPLOY_DOCKER_JOB_LOCK -Value 'x'
        $env:KEPLOY_START_STEP_OUTCOME = 'skipped'
        $r = Invoke-Script $removeJob @{ WedgeDir = (New-Dir 'wedge') }
        $r2 = $null
        Clear-JobEnv
        $r2 = Invoke-Script $removeJob @{ WedgeDir = (New-Dir 'wedge') }
        Report "a job whose app step never ran, or that never took its Docker lock, has nothing to warn about" ([pscustomobject]@{ Output = $r.Output + $r2.Output }) @(
            (Code $r 0), (Code $r2 0),
            $(if (($r.Output + $r2.Output) -match '::warning::') { "warned" }),
            $(if (Test-Path -LiteralPath (Join-Path $lockDir 'docker-job-42-1-a.lock')) { "the job's Docker lock was not released" })
        )

        # The run step's script finds the recorder by a path relative to its own.
        $dedup = Join-Path $repoRoot '.github/workflows/test_workflow_scripts/golang/go-dedup'
        $call = @(Select-String -Path (Join-Path $dedup 'golang-docker-windows.ps1') -Pattern "Join-Path \`$PSScriptRoot '([^']*register-job-compose-project\.ps1)'")
        $problems = @()
        if ($call.Count -ne 1) { $problems += "go-dedup's golang-docker-windows.ps1 calls register-job-compose-project.ps1 $($call.Count) time(s), want 1" }
        elseif (-not (Test-Path -LiteralPath (Join-Path $dedup ($call[0].Matches[0].Groups[1].Value -replace '\\', '/')))) { $problems += "its path $($call[0].Matches[0].Groups[1].Value) does not resolve" }
        Report "go-dedup records its compose project through the recorder the teardown reads" ([pscustomobject]@{ Output = '' }) $problems
    } finally {
        foreach ($v in $saved.Keys) { [Environment]::SetEnvironmentVariable($v, $saved[$v]) }
    }

    # ---- cleanup-windows.ps1 ------------------------------------------------

    # $locks maps a lock file name to its age in minutes, or to @(age,
    # content); a docker-prune-*.inprogress name is a marker of that age.
    function New-LockDir($locks) {
        $lockDir = New-Dir 'locks'
        foreach ($name in $locks.Keys) {
            $p = Join-Path $lockDir $name
            $spec = @($locks[$name])
            $content = 'x'
            if ($spec.Count -gt 1) { $content = $spec[1] }
            Set-Content -Path $p -Value $content
            (Get-Item $p).LastWriteTimeUtc = [DateTime]::UtcNow.AddMinutes(-$spec[0])
        }
        $lockDir
    }
    # Whether a docker-prune-*.inprogress marker is down in $dir right now.
    function Test-Marker($dir) { @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath $dir -Filter 'docker-prune-*.inprogress').Count -gt 0 }

    # $runs maps "<run>" or "<run>/<attempt>" to what the Actions API says of
    # it; anything else is unknown ($null), so age alone decides. With
    # -HttpApi, the script's own Get-RunStatusFromApi asks a fake
    # Invoke-RestMethod instead, and $runs maps to a status or an HTTP error
    # code. Each question is recorded as repo#key, suffixed +marker if a
    # prune marker was down while it was asked: it must not be, since that
    # would keep starting jobs waiting on the API.
    # -OnAsk runs on each question (a job taking its lock meanwhile).
    # -KeepOnDelete names a lock whose deletion fails; -RecreateOnDelete one
    # that is written again the moment it is deleted (a re-run of its run).
    function Invoke-Cleanup($locks, $runs = @{}, [switch]$HttpApi, [scriptblock]$OnAsk = $null, [string]$KeepOnDelete = '', [string]$RecreateOnDelete = '') {
        $lockDir = New-LockDir $locks
        # Lists, not arrays: the closures below get their own scope, so they
        # can only add to objects they share with this function.
        $asked = New-Object System.Collections.ArrayList
        $listings = New-Object System.Collections.ArrayList
        $status = {
            param($repo, $run, $attempt)
            $key = $run
            if ($attempt) { $key = "$run/$attempt" }
            # Inline, not Test-Marker: a closure does not see this file's functions.
            $down = @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath $lockDir -Filter 'docker-prune-*.inprogress').Count -gt 0
            [void]$asked.Add("$repo#$key$(if ($down) { '+marker' })")
            if ($OnAsk) { & $OnAsk $lockDir }
            $runs[$key]
        }.GetNewClosure()
        $cleanupArgs = @{ LockDir = $lockDir; WedgeDir = (New-Dir 'wedge'); Repository = 'keploy/keploy'; GetRunStatus = $status }
        # Local to this call, like the fakes below; the scripts find them
        # before the cmdlets. Every look at the locks is recorded with whether
        # a marker was down: the one a prune relies on must come after the
        # marker, or a job that takes its lock in between is pruned under.
        function Get-ChildItem {
            $items = Microsoft.PowerShell.Management\Get-ChildItem @args
            if ("$args" -match '\*\.lock') { [void]$listings.Add($(if (Test-Marker $lockDir) { 'marker' } else { 'bare' })) }
            $items
        }
        function Remove-Item {
            if ($KeepOnDelete -and ("$args" -like "*$KeepOnDelete*")) { return }
            Microsoft.PowerShell.Management\Remove-Item @args
            if ($RecreateOnDelete -and ("$args" -like "*$RecreateOnDelete*")) {
                Set-Content -Path (Join-Path $lockDir $RecreateOnDelete) -Value 'keploy/keploy started-2'
            }
        }
        if ($HttpApi) {
            $cleanupArgs.Remove('GetRunStatus')
            function Invoke-RestMethod($Uri, $Headers, $TimeoutSec, [switch]$UseBasicParsing) {
                $key = $Uri -replace '^https://api\.github\.com/repos/[^/]+/[^/]+/actions/runs/', '' -replace '/attempts/', '/'
                [void]$asked.Add(($Uri -replace '^https://api\.github\.com/repos/([^/]+/[^/]+)/actions/runs/.*$', '$1') + "#$key$(if (Test-Marker $lockDir) { '+marker' })")
                $answer = $runs[$key]
                if ($answer -is [int]) { throw (New-Object FakeHttpException $answer) }
                [pscustomobject]@{ status = $answer }
            }
        }
        $env:FAKE_LOCK_DIR = $lockDir
        try {
            $r = Invoke-Script $cleanup $cleanupArgs
        } finally {
            $env:FAKE_LOCK_DIR = ''
        }
        $r | Add-Member -NotePropertyName Locks -NotePropertyValue @(Microsoft.PowerShell.Management\Get-ChildItem $lockDir | ForEach-Object { $_.Name } | Sort-Object)
        $r | Add-Member -NotePropertyName Asked -NotePropertyValue @($asked | Sort-Object)
        $r | Add-Member -NotePropertyName Listings -NotePropertyValue @($listings)
        $r
    }
    function Pruned($result, [bool]$want) {
        $did = $result.Pruned.Count -gt 0
        if ($did -ne $want) { "pruned=$did, want $want" }
        $bare = @($result.Pruned | Where-Object { $_ -like '*-unannounced' })
        if ($bare.Count) { "pruned with no in-progress marker down: $($bare -join ', ')" }
        if ($did -and (@($result.Listings)[-1] -ne 'marker')) { "pruned on a look at the locks taken before the marker went down (looks: $($result.Listings -join ','))" }
    }
    function Asked($result, [string[]]$want) {
        $got = @($result.Asked) -join ','
        $exp = @($want | Sort-Object) -join ','
        if ($got -ne $exp) { "asked the Actions API about [$got], want [$exp]" }
    }
    function Locks($result, [string[]]$want) {
        $got = @($result.Locks) -join ','
        $exp = @($want | Sort-Object) -join ','
        if ($got -ne $exp) { "locks left [$got], want [$exp]" }
    }

    # The failing job's own moment: its app sits Created while a sibling run's
    # cleanup_windows sweeps. It used to be removed (StartedAt year 1 parsed
    # as ancient) and then force a prune past 44 lock files.
    Set-State @(
        (Container 'app' 'dedup-go-5e7e50f3' (Ago 0.1) $NEVER $NEVER 'keploy-5e7e50f3'),
        (Container 'agent' 'keploy-v3-ee1c' (Ago 0.2) (Ago 0.15) $NEVER 'keploy-5e7e50f3')
    )
    $r = Invoke-Cleanup @{ 'docker-job-1-1-a.lock' = 1; 'prepare-windows-workflow-2.lock' = 20 }
    Report "cleanup keeps a sibling's Created-state app and does not prune under live locks" $r @(
        (Removed $r @()), (Pruned $r $false), (Locks $r @('docker-job-1-1-a.lock', 'prepare-windows-workflow-2.lock')),
        # Live locks found by the first look call the prune off before any
        # marker goes down, so no starting job waits on a prune that will not run.
        $(if (@($r.Listings) -join ',' -ne 'bare') { "looked at the locks [$($r.Listings -join ',')], want one look with no marker down" }),
        (Says $r 'Skipping Docker prune: 2 live lock\(s\)' "the live-locks message")
    )

    Set-State @((Container 'old' 'dedup-go-0badf00d' (Ago 300) (Ago 300) (Ago 200)))
    $r = Invoke-Cleanup @{ 'docker-job-1-1-a.lock' = 5 }
    Report "removing a leftover no longer forces a prune past a live job lock" $r @((Removed $r @('old')), (Pruned $r $false))

    Set-State @()
    $r = Invoke-Cleanup @{ 'docker-job-1-1-a.lock' = ($minAge + 1); 'prepare-windows-workflow-2.lock' = (25 * 60); 'prepare-windows-workflow-3.lock' = (23 * 60) }
    Report "stale locks are deleted, and a run lock under a day old still blocks the prune" $r @(
        (Pruned $r $false), (Locks $r @('prepare-windows-workflow-3.lock'))
    )

    Set-State @()
    $r = Invoke-Cleanup @{ 'docker-job-1-1-a.lock' = ($minAge + 1); 'prepare-windows-workflow-2.lock' = (25 * 60) }
    Report "once every lock is stale the prune runs" $r @((Pruned $r $true), (Locks $r @()))

    # A cancelled run's cleanup_windows can be cancelled at "Set up job"
    # (run 35707841215), so its run lock is never deleted by its owner. Its
    # run being over is what makes it stale, not a day passing.
    Set-State @()
    $r = Invoke-Cleanup @{
        'prepare-windows-workflow-35707841215.lock' = @(90, 'started-1758531582')
        'prepare-windows-workflow-36092000545.lock' = @(20, 'keploy/keploy started-1758772800')
        'docker-job-36091939524-1-a.lock'          = @(10, 'keploy/keploy Prepare Binary and Run Workflows win-runner-3')
    } @{ '35707841215' = 'completed'; '36092000545' = 'in_progress'; '36091939524/1' = 'completed' }
    Report "locks of runs that are over are deleted at once; a live run's lock blocks the prune" $r @(
        (Pruned $r $false), (Locks $r @('prepare-windows-workflow-36092000545.lock')),
        (Asked $r @('keploy/keploy#35707841215', 'keploy/keploy#36092000545', 'keploy/keploy#36091939524/1'))
    )

    Set-State @()
    $r = Invoke-Cleanup @{
        'prepare-windows-workflow-7.lock' = @(30, 'keploy/other started-1')
        'docker-job-8-2-a.lock'           = @(5, 'keploy/other wf win-runner-1')
    } @{ '7' = 'completed'; '8/2' = 'completed' }
    Report "each lock's run is looked up in the repository the lock names, and once all are over the prune runs" $r @(
        (Pruned $r $true), (Locks $r @()), (Asked $r @('keploy/other#7', 'keploy/other#8/2'))
    )

    Set-State @()
    $r = Invoke-Cleanup @{ 'prepare-windows-workflow-9.lock' = 30; 'docker-job-9-1-a.lock' = ($minAge + 5) }
    Report "a lock past its age bound is stale without asking; when the API cannot answer, age decides" $r @(
        (Pruned $r $false), (Locks $r @('prepare-windows-workflow-9.lock')), (Asked $r @('keploy/keploy#9'))
    )

    # The script's own API lookup. A 404 is not "over": a token that cannot
    # see the repository gets one for a live run too.
    Set-State @()
    $r = Invoke-Cleanup @{
        'prepare-windows-workflow-21.lock' = @(30, 'keploy/keploy started-1')
        'prepare-windows-workflow-22.lock' = @(30, 'keploy/private started-1')
        'docker-job-23-1-a.lock'           = @(5, 'keploy/keploy wf win-runner-2')
    } @{ '21' = 'completed'; '22' = 404; '23/1' = 500 } -HttpApi
    Report "the Actions API's completed deletes a lock, and a 404 or other error leaves it to age" $r @(
        (Pruned $r $false), (Locks $r @('docker-job-23-1-a.lock', 'prepare-windows-workflow-22.lock')),
        (Asked $r @('keploy/keploy#21', 'keploy/private#22', 'keploy/keploy#23/1'))
    )

    # The marker goes down only after the API has been asked (every Asked
    # above has no +marker), so the handshake rests on the look at the locks
    # after it: a lock not judged stale before the marker is live.
    Set-State @()
    $r = Invoke-Cleanup @{ 'docker-job-30-1-a.lock' = @(5, 'keploy/keploy wf win-runner-1') } @{ '30/1' = 'completed' } -OnAsk {
        param($dir) Set-Content -Path (Join-Path $dir 'docker-job-31-1-b.lock') -Value 'keploy/keploy wf win-runner-2'
    }
    Report "a job lock taken while the Actions API was being asked calls the prune off" $r @(
        (Pruned $r $false), (Locks $r @('docker-job-31-1-b.lock')), (Asked $r @('keploy/keploy#30/1')),
        (Says $r 'lock\(s\) taken while the others were judged - docker-job-31-1-b\.lock' "the taken-meanwhile message")
    )

    Set-State @()
    $r = Invoke-Cleanup @{ 'docker-job-32-1-a.lock' = @(5, 'keploy/keploy wf win-runner-1') } @{ '32/1' = 'completed' } -KeepOnDelete 'docker-job-32-1-a.lock'
    Report "a lock judged stale that could not be deleted does not call the prune off" $r @(
        (Pruned $r $true), (Locks $r @('docker-job-32-1-a.lock'))
    )

    Set-State @()
    $r = Invoke-Cleanup @{ 'prepare-windows-workflow-33.lock' = @(30, 'keploy/keploy started-1') } @{ '33' = 'completed' } -RecreateOnDelete 'prepare-windows-workflow-33.lock'
    Report "a lock written again under a name judged stale (a re-run) calls the prune off" $r @(
        (Pruned $r $false), (Locks $r @('prepare-windows-workflow-33.lock'))
    )

    # A cleanup killed mid-prune leaves its marker; jobs stop waiting on it
    # after PruneMaxMinutes, and the next cleanup deletes it. A younger one may
    # be another cleanup's, still pruning.
    $pruneMax = Get-Default $cleanup 'PruneMaxMinutes'
    Set-State @()
    $r = Invoke-Cleanup @{ 'docker-prune-dead.inprogress' = ($pruneMax + 1); 'docker-prune-busy.inprogress' = ($pruneMax - 1) }
    Report "a marker past PruneMaxMinutes is deleted, a younger one is kept" $r @(
        (Locks $r @('docker-prune-busy.inprogress'))
    )

    # A start or restart of Docker Desktop (ensure-docker.ps1) is under way:
    # a prune now would fail under it, or remove what it is bringing back.
    # The prune gives way; the next cleanup prunes.
    Set-State @()
    $r = Invoke-Cleanup @{ 'docker-job-34-1-a.lock' = ($minAge + 1); 'docker-prune-restart.inprogress' = @(1, 'a Docker Desktop restart by keploy/keploy run 3 win-runner-2') }
    Report "a start or restart of Docker Desktop under way calls the prune off" $r @(
        (Pruned $r $false), (Locks $r @('docker-prune-restart.inprogress')),
        (Says $r 'Skipping Docker prune: another Docker prune, start or restart is under way - docker-prune-restart\.inprogress' "the under-way message")
    )

    # ---- take-docker-job-lock.ps1, the job's half of the prune handshake ----

    $lockDir = New-Dir 'locks'
    $savedRepo = $env:GITHUB_REPOSITORY
    $env:GITHUB_REPOSITORY = 'keploy/keploy'
    try { $out = @(& $takeLock -LockDir $lockDir -RunId 11 -RunAttempt 3 6>$null) } finally { $env:GITHUB_REPOSITORY = $savedRepo }
    $problems = @()
    if ($out.Count -ne 1 -or -not (Test-Path -LiteralPath "$($out[0])")) { $problems += "printed [$($out -join '|')], want the one lock path" }
    elseif ((Split-Path -Leaf $out[0]) -notmatch '^docker-job-11-3-[0-9a-f]{32}\.lock$') { $problems += "lock named $(Split-Path -Leaf $out[0])" }
    elseif ("$(Get-Content -LiteralPath $out[0] -TotalCount 1)" -notmatch '^keploy/keploy ') { $problems += "lock does not start with the repository" }
    Report "the job lock names its run and attempt and starts with its repository" ([pscustomobject]@{ Output = ($out -join "`n") }) $problems

    # A job that takes its lock while a prune is under way waits for it, and
    # its lock is already down while it waits: written after the wait, a
    # prune that started in between would not see it. Driven by events, not
    # by timing - the job runs in the background, and the marker it waits on
    # is far from expiry and is taken down only once the job has written its
    # lock and said it is waiting. The deadlines only bound a failure.
    $lockDir = New-Dir 'locks'
    $m = Join-Path $lockDir 'docker-prune-0123.inprogress'
    Set-Content -Path $m -Value 'x'
    $bg = Start-Background $takeLock @{ LockDir = $lockDir; RunId = 12; RunAttempt = 1; PollSeconds = 1 }
    $problems = @()
    try {
        $deadline = [DateTime]::UtcNow.AddSeconds(120)
        $held = @(); $said = ''
        while ($true) {
            $held = @(Get-ChildItem -LiteralPath $lockDir -Filter 'docker-job-12-1-*.lock')
            $said = Read-Background $bg
            if (($held.Count -gt 0) -and ($said -match 'is in progress')) { break }
            if (Test-BackgroundDone $bg) { break }
            if ([DateTime]::UtcNow -ge $deadline) { break }
            Start-Sleep -Milliseconds 200
        }
        if (Test-BackgroundDone $bg) {
            $said = Read-Background $bg
            $problems += "take-docker-job-lock.ps1 finished while the prune marker was down"
        } elseif ($held.Count -ne 1) {
            $problems += "no docker-job-12-1-*.lock written before waiting for the prune (found $($held.Count))"
        } elseif ($said -notmatch 'A Docker prune, or a start or restart of Docker Desktop, is in progress') {
            $problems += "did not say it was waiting for the prune"
        } else {
            Remove-Item -LiteralPath $m -Force
            if (-not (Wait-Background $bg 120)) { $problems += "still waiting 120s after the prune marker was taken down" }
            else {
                $said = Read-Background $bg
                $after = @(Get-ChildItem -LiteralPath $lockDir -Filter 'docker-job-12-1-*.lock')
                if ($bg.PS.InvocationStateInfo.State -ne 'Completed') { $problems += "ended $($bg.PS.InvocationStateInfo.State)" }
                if ($said -notmatch 'The Docker prune, start or restart has finished') { $problems += "did not say the prune was over" }
                if (($after.Count -ne 1) -or ($said -notmatch [regex]::Escape($after[0].FullName))) { $problems += "did not print the one lock it holds" }
            }
        }
    } finally {
        Stop-Background $bg
    }
    Report "a job waits for a prune in progress before using Docker, holding its lock" ([pscustomobject]@{ Output = $said }) $problems

    # A marker past PruneMaxMinutes: its cleanup was killed. Waiting on it is
    # announced before the first sleep, so the case ends on whichever comes
    # first, the announcement or the script finishing; the deadline only
    # bounds a failure.
    $lockDir = New-Dir 'locks'
    $m = Join-Path $lockDir 'docker-prune-4567.inprogress'
    Set-Content -Path $m -Value 'x'
    (Get-Item $m).LastWriteTimeUtc = [DateTime]::UtcNow.AddMinutes(-((Get-Default $takeLock 'PruneMaxMinutes') + 1))
    $bg = Start-Background $takeLock @{ LockDir = $lockDir; RunId = 13; RunAttempt = 1; PollSeconds = 60 }
    $problems = @()
    try {
        $deadline = [DateTime]::UtcNow.AddSeconds(120)
        $said = ''
        while ($true) {
            $said = Read-Background $bg
            if ($said -match 'is in progress') { $problems += "waits for it"; break }
            if (Test-BackgroundDone $bg) { break }
            if ([DateTime]::UtcNow -ge $deadline) { $problems += "neither finished nor said it was waiting within 120s"; break }
            Start-Sleep -Milliseconds 200
        }
        if (-not $problems.Count) {
            $said = Read-Background $bg
            if ($bg.PS.InvocationStateInfo.State -ne 'Completed') { $problems += "ended $($bg.PS.InvocationStateInfo.State)" }
            elseif ($said -notmatch 'docker-job-13-1-[0-9a-f]{32}\.lock') { $problems += "did not print its lock" }
        }
    } finally {
        Stop-Background $bg
    }
    Report "a marker left by a killed cleanup is not waited for" ([pscustomobject]@{ Output = $said }) $problems

    # ---- ensure-docker.ps1: precheck-windows' Docker check ------------------

    # Every Docker Desktop action goes to a fake here: the defaults act on the
    # real Docker Desktop of the machine these tests also run on. Each fake
    # action is recorded, suffixed +marker if a prune/restart marker was down
    # at that moment, and Held is how long the marker it saw had been down
    # when the script ended. Stamps is what docker-desktop.started said at
    # each action (THE LAST START in docker-locks.ps1). Starting Desktop brings the fake daemon back
    # unless -StaysDown; with -HangsAfterStart it comes back wedged, and every
    # docker call hangs. -StartedUnwritable puts a directory where
    # docker-desktop.started goes, so that writing it fails. -StampOnCall N:
    # another runner's start or restart lands on the Nth docker call (the fake
    # docker's stampOnCall). -InLockDir runs it in a lock directory an earlier
    # run left, instead of a new one made from $locks. $locks and $runs are as for Invoke-Cleanup; $Extra
    # overrides any parameter. Two checks before giving up rather than the
    # script's five: each docker call here starts a PowerShell process.
    function Invoke-Ensure($locks = @{}, $runs = @{}, [switch]$DesktopDown, [switch]$StaysDown, [switch]$HangsAfterStart, [switch]$StartedUnwritable, [int]$StampOnCall = 0, [string]$InLockDir = '', [scriptblock]$OnAsk = $null, [hashtable]$Extra = @{}) {
        $lockDir = $InLockDir
        if (-not $lockDir) { $lockDir = New-LockDir $locks }
        if ($StampOnCall) {
            $st = Get-State
            $st.stampOnCall = $StampOnCall
            $st.stampFile = Join-Path $lockDir 'docker-desktop.started'
            $st | ConvertTo-Json -Depth 5 | Set-Content -Path $stateFile -Encoding ASCII
        }
        if ($StartedUnwritable) { New-Item -ItemType Directory -Path (Join-Path $lockDir 'docker-desktop.started') | Out-Null }
        $events = New-Object System.Collections.ArrayList
        $markedAt = New-Object System.Collections.ArrayList
        $stamps = New-Object System.Collections.ArrayList
        $stampFile = Join-Path $lockDir 'docker-desktop.started'
        $stateFile = $env:FAKE_DOCKER_STATE
        $running = -not $DesktopDown
        $ensureArgs = @{
            LockDir = $lockDir; Repository = 'keploy/keploy'; Attempts = 2
            BackoffSeconds = 0; PollSeconds = 0; ReadyTimeoutSeconds = 0; StabilizeSeconds = 0
            DesktopExe = 'C:\no\such\Docker Desktop.exe'
            TestDesktopRunning = { $running }.GetNewClosure()
            StopDesktop = {
                $m = @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath $lockDir -Filter 'docker-prune-*.inprogress')
                if ($m.Count) { [void]$markedAt.Add($m[0].LastWriteTimeUtc) }
                [void]$stamps.Add("$(Get-Content -LiteralPath $stampFile -Raw -ErrorAction SilentlyContinue)".Trim())
                [void]$events.Add("stop$(if ($m.Count) { '+marker' })")
            }.GetNewClosure()
            StartDesktop = {
                $m = @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath $lockDir -Filter 'docker-prune-*.inprogress')
                if ($m.Count) { [void]$markedAt.Add($m[0].LastWriteTimeUtc) }
                [void]$stamps.Add("$(Get-Content -LiteralPath $stampFile -Raw -ErrorAction SilentlyContinue)".Trim())
                [void]$events.Add("start$(if ($m.Count) { '+marker' })")
                if ($HangsAfterStart) { Set-Content -LiteralPath "$stateFile.hang" -Value 'hang' }
                if (-not $StaysDown) { Set-Content -LiteralPath "$stateFile.up" -Value 'up' }
            }.GetNewClosure()
            GetRunStatus = {
                param($repo, $run, $attempt)
                $key = $run
                if ($attempt) { $key = "$run/$attempt" }
                $down = @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath $lockDir -Filter 'docker-prune-*.inprogress').Count -gt 0
                [void]$events.Add("ask $repo#$key$(if ($down) { '+marker' })")
                if ($OnAsk) { & $OnAsk $lockDir }
                $runs[$key]
            }.GetNewClosure()
        }
        foreach ($k in $Extra.Keys) { $ensureArgs[$k] = $Extra[$k] }
        $r = Invoke-Script $ensure $ensureArgs
        $held = $null
        if ($markedAt.Count) { $held = ([DateTime]::UtcNow - $markedAt[0]).TotalSeconds }
        Remove-Item -LiteralPath "$stateFile.hang" -Force -ErrorAction SilentlyContinue
        $r | Add-Member -NotePropertyName Locks -NotePropertyValue @(Get-ChildItem $lockDir | Where-Object { $_.Name -ne 'docker-desktop.started' } | ForEach-Object { $_.Name } | Sort-Object)
        $r | Add-Member -NotePropertyName Events -NotePropertyValue @($events)
        $r | Add-Member -NotePropertyName Held -NotePropertyValue $held
        $r | Add-Member -NotePropertyName Stamps -NotePropertyValue @($stamps)
        $r | Add-Member -NotePropertyName LockDir -NotePropertyValue $lockDir
        $r
    }
    # Docker answering (-Up) or not, maybe only after -FailInfo more failed
    # checks while it comes up.
    function Set-Docker([switch]$Up, [int]$FailInfo = 0) {
        if ($FailInfo) {
            $st = Get-State
            $st.failInfo = $FailInfo
            $st | ConvertTo-Json -Depth 5 | Set-Content -Path $stateFile -Encoding ASCII
        }
        if ($Up) { Set-Content -LiteralPath "$stateFile.up" -Value 'up' }
    }
    # What another runner's start or restart of Docker Desktop leaves behind:
    # a new docker-desktop.started, and Docker as Set-Docker leaves it.
    function Write-OtherStart([string]$dir, [switch]$Up, [int]$FailInfo = 0) {
        Set-Content -LiteralPath (Join-Path $dir 'docker-desktop.started') -Value "$([guid]::NewGuid().ToString('N')) another runner's restart"
        Set-Docker -Up:$Up -FailInfo $FailInfo
    }
    function Events($result, [string[]]$want) {
        $got = @($result.Events) -join ','
        $exp = @($want) -join ','
        if ($got -ne $exp) { "did [$got], want [$exp]" }
    }
    # Exit code, Docker Desktop actions, and no error records: a script that
    # throws (the tests' guard below, say) exits 0 and does nothing. Every
    # action must come after its own start or restart wrote
    # docker-desktop.started: another runner's start or restart (the cases
    # below write one saying "another runner") does not count.
    function Ensured($result, [int]$code, [string[]]$events) {
        (Code $result $code); (Events $result $events); (NoErrors $result)
        $what = 'start'
        if (@($events) -match '^stop') { $what = 'restart' }
        foreach ($st in @($result.Stamps)) {
            if ($st -notmatch "^[0-9a-f]{32} a Docker Desktop $what by ") { "acted with docker-desktop.started saying [$st], want this run's $what" }
        }
    }

    Set-State @()
    $r = Invoke-Ensure
    Report "a healthy Docker is left alone" $r @(Ensured $r 0 @())

    # A daemon too busy to answer (four jobs loading images) used to get
    # Docker Desktop force-killed on the first failed `docker info`.
    Set-State @() -FailInfo 2
    $r = Invoke-Ensure -Extra @{ Attempts = (Get-Default $ensure 'Attempts') }
    Report "a Docker that misses a check or two is asked again, not restarted" $r @(Ensured $r 0 @())

    # A wedged Docker Desktop often hangs `docker info` instead of failing it.
    # The script's own CallTimeoutSeconds: the timeout counts the process
    # start, and on the loaded runner a PowerShell fake can take seconds just
    # to start, so a short one here would kill healthy calls too.
    Set-State @() -HangInfo 1
    $callTimeout = Get-Default $ensure 'CallTimeoutSeconds'
    $t0 = [DateTime]::UtcNow
    $r = Invoke-Ensure
    $took = ([DateTime]::UtcNow - $t0).TotalSeconds
    Report "a docker call that hangs is killed, counted as a failed check, and asked again" $r @(
        (Ensured $r 0 @()), (Says $r "did not return within $callTimeout s; killing it" "the killed call"),
        $(if ($took -ge 100) { "took $([int]$took) s: the hung call (120 s) was not cut short" })
    )

    Set-State @() -DaemonDown
    $r = Invoke-Ensure @{
        'docker-job-50-1-a.lock'           = @(5, 'keploy/keploy wf win-runner-2')
        'prepare-windows-workflow-51.lock' = @(20, 'keploy/keploy started-1')
    } @{ '50/1' = 'in_progress'; '51' = 'in_progress' }
    Report "Docker Desktop is not restarted while a job holds a live Docker lock, and the job fails saying why" $r @(
        (Ensured $r 1 @('ask keploy/keploy#50/1')),
        (Says $r '::error::Docker Desktop is running but .* 1 job\(s\) hold a live Docker lock \(docker-job-50-1-a\.lock\), so Docker Desktop was NOT restarted' "the refusal naming the lock"),
        (Says $r "wsl --shutdown" "the remediation"),
        (Locks $r @('docker-job-50-1-a.lock', 'prepare-windows-workflow-51.lock'))
    )

    # A run lock marks a run in flight, not a container: it does not hold the
    # restart off, or a sick daemon would never be restarted on a busy day.
    Set-State @() -DaemonDown
    $r = Invoke-Ensure @{
        'docker-job-60-1-a.lock'           = @(5, 'keploy/keploy wf win-runner-2')
        'docker-job-61-1-b.lock'           = ($minAge + 1)
        'prepare-windows-workflow-62.lock' = @(20, 'keploy/keploy started-1')
    } @{ '60/1' = 'completed'; '62' = 'in_progress' }
    Report "with no live Docker lock, Docker Desktop is restarted under the marker, asking the API before it" $r @(
        (Ensured $r 0 @('ask keploy/keploy#60/1', 'stop+marker', 'start+marker')),
        (Locks $r @('prepare-windows-workflow-62.lock'))
    )

    Set-State @() -DaemonDown
    $r = Invoke-Ensure @{ 'docker-job-70-1-a.lock' = @(5, 'keploy/keploy wf win-runner-2') } @{ '70/1' = 'completed' } -OnAsk {
        param($dir) Set-Content -Path (Join-Path $dir 'docker-job-71-1-b.lock') -Value 'keploy/keploy wf win-runner-3'
    }
    Report "a job lock taken while the API was being asked calls the restart off" $r @(
        (Ensured $r 1 @('ask keploy/keploy#70/1')),
        (Says $r 'took a Docker lock while the others were being judged \(docker-job-71-1-b\.lock\)' "the refusal naming the new lock"),
        (Locks $r @('docker-job-71-1-b.lock'))
    )

    # Nothing runs on a Docker Desktop that is not running, so live locks do
    # not matter; but its start puts a marker down, so that another runner
    # does not restart it while it comes up.
    Set-State @() -DaemonDown
    $r = Invoke-Ensure @{ 'docker-job-80-1-a.lock' = @(5, 'keploy/keploy wf win-runner-2') } @{ '80/1' = 'in_progress' } -DesktopDown
    Report "a Docker Desktop that is not running is started under a marker, whatever the locks" $r @(
        (Ensured $r 0 @('start+marker')), (Locks $r @('docker-job-80-1-a.lock'))
    )

    Set-State @() -DaemonDown
    $r = Invoke-Ensure -StaysDown
    Report "a restart after which Docker never answers fails the job and takes its marker down" $r @(
        (Ensured $r 1 @('stop+marker', 'start+marker')), (Locks $r @()),
        (Says $r '::error::Docker did not become ready' "the not-ready error")
    )

    # Docker Desktop comes back wedged and every call hangs. The marker must
    # still come down within the bound the guard at the end holds under
    # PruneMaxMinutes; process starts get 30 s on top. A 1 s call timeout can
    # also kill a call that was only slow to start, but every call here fails
    # anyway, so that changes nothing.
    Set-State @() -DaemonDown
    $bounded = @{ CallTimeoutSeconds = 1; ReadyTimeoutSeconds = 1 }
    $r = Invoke-Ensure -HangsAfterStart -Extra $bounded
    $bound = 2 * 0 + $bounded.ReadyTimeoutSeconds + 0 + 6 * $bounded.CallTimeoutSeconds
    Report "a Docker that hangs after its restart still gets the restart's marker taken down in time" $r @(
        (Ensured $r 1 @('stop+marker', 'start+marker')), (Locks $r @()),
        (Says $r 'did not return within 1 s; killing it' "the killed call"),
        (Says $r '::error::Docker did not become ready' "the not-ready error"),
        $(if (($null -eq $r.Held) -or ($r.Held -gt $bound + 30)) { "held its marker $([int]$r.Held) s; the bound is $bound s" })
    )

    Set-State @() -DaemonDown
    $r = Invoke-Ensure -Extra @{ DockerExe = 'no-such-docker-cli' }
    Report "without a docker CLI on the PATH, Docker Desktop is neither started nor restarted" $r @(
        (Ensured $r 1 @()), (Says $r "::error::docker CLI not found on PATH for this runner" "the missing-CLI error"), (Locks $r @())
    )

    # `docker ps` is half the check: the original script restarted a Docker
    # that answered `info` but not `ps`, and it still counts as not answering.
    Set-State @() -PsDown
    $r = Invoke-Ensure
    Report "a Docker that answers 'docker info' but not 'docker ps' is asked again, then restarted under the marker" $r @(
        (Ensured $r 0 @('stop+marker', 'start+marker')), (Says $r 'attempt 1 of 2' "the retry"), (Locks $r @())
    )

    Set-State @() -PsDown
    $r = Invoke-Ensure @{ 'docker-job-85-1-a.lock' = @(5, 'keploy/keploy wf win-runner-2') } @{ '85/1' = 'in_progress' }
    Report "a Docker that answers 'docker info' but not 'docker ps' is not restarted while a job holds a live Docker lock" $r @(
        (Ensured $r 1 @('ask keploy/keploy#85/1')),
        (Says $r 'hold a live Docker lock \(docker-job-85-1-a\.lock\), so Docker Desktop was NOT restarted' "the refusal naming the lock"),
        (Locks $r @('docker-job-85-1-a.lock'))
    )

    # Another runner restarts Docker Desktop while this one asks the Actions
    # API: its marker comes and goes before this one's is down, so there is
    # nothing to wait for. Docker is still coming up and fails the next check,
    # but the check this one made before that restart no longer counts: it
    # checks again, with every retry, instead of restarting.
    Set-State @() -DaemonDown
    $r = Invoke-Ensure @{ 'docker-job-72-1-a.lock' = @(5, 'keploy/keploy wf win-runner-2') } @{ '72/1' = 'completed' } -OnAsk {
        param($dir) Write-OtherStart $dir -Up -FailInfo 1
    }
    Report "a restart by another runner just before this one's marker went down is not followed by a second restart" $r @(
        (Ensured $r 0 @('ask keploy/keploy#72/1')),
        (Says $r 'started or restarted by another runner since this one checked Docker' "the out-of-date check"),
        (Says $r 'Docker engine is running and healthy' "the check after it"), (Locks $r @())
    )

    # Another runner's restart lands during this one's check, on its first
    # docker call, and its marker comes and goes before this one looks for
    # markers. Docker, coming up from it, misses that check and one more
    # call, and answers after. docker-desktop.started is read before the check, so the
    # restart shows under this one's marker: it checks again, with every
    # retry, and finds Docker answering. Read after the check, it would miss
    # that restart, act on one more probe, and restart Docker Desktop again.
    Set-State @() -FailInfo 3
    $r = Invoke-Ensure -StampOnCall 1
    Report "a restart by another runner during this one's check is not followed by a second restart" $r @(
        (Ensured $r 0 @()),
        (Says $r "started or restarted by another runner since this one checked Docker(.|\n)*attempt 1 of 2(.|\n)*Docker engine is running and healthy" "the out-of-date check, then a full check"),
        (Locks $r @())
    )

    # A waiter compares docker-desktop.started with what it read before its
    # check: each start or restart must write one no earlier one wrote, or a
    # second restart by the same run on the same runner (a re-run of
    # precheck-windows, say) would look like no restart at all.
    Set-State @() -DaemonDown
    $r1 = Invoke-Ensure
    Set-State @() -DaemonDown
    $r2 = Invoke-Ensure -InLockDir $r1.LockDir
    $s1 = @($r1.Stamps | Select-Object -Unique); $s2 = @($r2.Stamps | Select-Object -Unique)
    Report "each restart writes a docker-desktop.started of its own" $r2 @(
        (Ensured $r1 0 @('stop+marker', 'start+marker')), (Ensured $r2 0 @('stop+marker', 'start+marker')),
        $(if (($s1.Count -ne 1) -or ($s2.Count -ne 1) -or ($s1[0] -eq $s2[0])) { "the restarts left [$($s1 -join '|')] then [$($s2 -join '|')], want one stamp each, different" })
    )

    # Docker can answer again by the time the marker is down with nobody
    # having started or restarted it: a prune or an operation this one waited
    # for found it answering, or it recovered. It is checked once more under
    # the marker, and not restarted.
    Set-State @() -DaemonDown
    $r = Invoke-Ensure @{ 'docker-job-74-1-a.lock' = @(5, 'keploy/keploy wf win-runner-2') } @{ '74/1' = 'completed' } -OnAsk {
        param($dir) Set-Content -LiteralPath "$env:FAKE_DOCKER_STATE.up" -Value 'up'
    }
    Report "a Docker that answers again once the restart's marker is down is not restarted" $r @(
        (Ensured $r 0 @('ask keploy/keploy#74/1')), (Says $r 'Docker answers now' "the check under the marker"), (Locks $r @())
    )

    # Every round after the first follows another runner's operation, and
    # MaxWaitMinutes (0 here) ends them: the job fails with the remediation.
    Set-State @() -DaemonDown
    $r = Invoke-Ensure @{ 'docker-job-73-1-a.lock' = @(5, 'keploy/keploy wf win-runner-2') } @{ '73/1' = 'completed' } -OnAsk {
        param($dir) Write-OtherStart $dir
    } -Extra @{ MaxWaitMinutes = 0 }
    Report "past MaxWaitMinutes it fails the job instead of checking again after another runner's restart" $r @(
        (Ensured $r 1 @('ask keploy/keploy#73/1')),
        (Says $r '::error::Docker does not answer, and 0 min after this step started' "the deadline error"),
        (Says $r "wsl --shutdown" "the remediation"), (Locks $r @())
    )

    # Without docker-desktop.started written first, another runner could
    # restart Docker again while this one's restart comes up: no restart.
    # (A directory in its place makes the write fail.)
    Set-State @() -DaemonDown
    $r = Invoke-Ensure -StartedUnwritable
    Report "a restart that cannot write docker-desktop.started first does not happen" $r @(
        (Ensured $r 1 @()), (Says $r '::error::Docker Desktop was NOT restarted: docker-desktop.started' "the error"), (Locks $r @())
    )

    # Another runner's start or restart is under way. The runs below are in
    # the background, so that the test can play the other runners, and each
    # is a PowerShell process of its own, as each runner's is. (Not a
    # runspace: runspaces of one process share a script's compiled form, and
    # two of them starting the same script at once can break it - "An item
    # with the same key has already been added".) Their Docker Desktop fakes
    # record into a file of their own, a start writes the fake daemon's .up,
    # and they also write, to <events>.marked, when the marker they saw was
    # last written. $getRunStatus is the body of the -GetRunStatus fake,
    # $prelude runs before the script, $extra adds parameters (lines of a
    # hashtable), and $running is the body of -TestDesktopRunning. Time is
    # real here (PollSeconds 1), and
    # the cases move on events, not on timing: the deadlines only bound a
    # failure.
    $ensureDriver = @'
$ErrorActionPreference = 'Continue'
$drvLog = '@@LOG@@'; $drvLockDir = '@@LOCKDIR@@'; $drvEvents = '@@EVENTS@@'; $drvState = '@@STATE@@'
function Get-MarkerSeen {
    $m = @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath $drvLockDir -Filter 'docker-prune-*.inprogress')
    if ($m.Count) { Microsoft.PowerShell.Management\Add-Content -LiteralPath "$drvEvents.marked" -Value $m[0].LastWriteTimeUtc.Ticks; return '+marker' }
    ''
}
@@PRELUDE@@
$drvParams = @{
    LockDir = $drvLockDir; Repository = 'keploy/keploy'; DockerExe = '@@FAKE@@'; Attempts = 2
    BackoffSeconds = 0; PollSeconds = 1; ReadyTimeoutSeconds = 60; StabilizeSeconds = 0; CallTimeoutSeconds = 60
    DesktopExe = 'C:\no\such\Docker Desktop.exe'
    TestDesktopRunning = { @@RUNNING@@ }
    StopDesktop = { Microsoft.PowerShell.Management\Add-Content -LiteralPath $drvEvents -Value "stop$(Get-MarkerSeen)" }
    StartDesktop = {
        Microsoft.PowerShell.Management\Add-Content -LiteralPath $drvEvents -Value "start$(Get-MarkerSeen)"
        Microsoft.PowerShell.Management\Set-Content -LiteralPath "$drvState.up" -Value up
    }
    GetRunStatus = { param($repo, $run, $attempt) @@RUNSTATUS@@ }
    @@EXTRA@@
}
& '@@ENSURE@@' @drvParams *>&1 | ForEach-Object { Microsoft.PowerShell.Management\Add-Content -LiteralPath $drvLog -Value "$_" }
$drvCode = $LASTEXITCODE
Microsoft.PowerShell.Management\Add-Content -LiteralPath $drvLog -Value "exit=$drvCode"
exit $drvCode
'@
    function Start-Ensure([string]$lockDir, [string]$events, [switch]$DesktopDown, [string]$prelude = '', [string]$getRunStatus = '$null', [string]$extra = '', [string]$running = '') {
        $dir = New-Dir 'ensure'
        $driver = Join-Path $dir 'driver.ps1'
        $log = Join-Path $dir 'log'
        Set-Content -LiteralPath $log -Value $null
        if (-not $running) {
            $running = '$true'
            if ($DesktopDown) { $running = "Test-Path -LiteralPath '$env:FAKE_DOCKER_STATE.up'" }
        }
        $text = $ensureDriver.Replace('@@PRELUDE@@', $prelude).Replace('@@RUNNING@@', $running).Replace('@@RUNSTATUS@@', $getRunStatus).Replace('@@EXTRA@@', $extra).
            Replace('@@LOG@@', $log).Replace('@@LOCKDIR@@', $lockDir).Replace('@@EVENTS@@', $events).Replace('@@STATE@@', $env:FAKE_DOCKER_STATE).
            Replace('@@FAKE@@', $fake).Replace('@@ENSURE@@', $ensure)
        Set-Content -LiteralPath $driver -Encoding ASCII -Value $text
        $psi = New-Object System.Diagnostics.ProcessStartInfo
        $psi.FileName = [System.Diagnostics.Process]::GetCurrentProcess().MainModule.FileName
        $psi.Arguments = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$driver`""
        $psi.UseShellExecute = $false
        $psi.CreateNoWindow = $true
        $psi.RedirectStandardOutput = $true
        $psi.RedirectStandardError = $true
        $p = [System.Diagnostics.Process]::Start($psi)
        [pscustomobject]@{ Proc = $p; Log = $log; StdOut = $p.StandardOutput.ReadToEndAsync(); Err = $p.StandardError.ReadToEndAsync() }
    }
    function Get-Events([string]$file) { @(Get-Content -LiteralPath $file -ErrorAction SilentlyContinue | Where-Object { $_ }) }
    function Get-Markers([string]$dir) { @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath $dir -Filter 'docker-prune-*.inprogress' | ForEach-Object { $_.Name } | Sort-Object) }

    # A restart or a prune by another runner has its marker down, Desktop is
    # running and the daemon does not answer yet. Restarting now would kill
    # that restart mid-startup, or the prune. The wait itself is pinned by what
    # happens meanwhile, not by timing: the fake docker logs every call
    # (FAKE_DOCKER_CALLS) and the prelude every look for markers, and the
    # other runner's marker stays down until this one has looked for it
    # three more times without a single docker call. Once it is gone, the
    # check made before the wait no longer counts, whatever the other runner
    # did: a prune writes no docker-desktop.started, and Docker here misses
    # one more call after it. Only a full check again, with every retry,
    # finds it answering; one probe would restart it.
    foreach ($v in @(
            @{ What = 'restart'; Marker = 'a Docker Desktop restart by keploy/keploy run 1 win-runner-2'; End = { param($dir) Write-OtherStart $dir -Up }
                Says = 'Docker engine is running and healthy' },
            @{ What = 'prune'; Marker = 'a Docker prune by keploy/keploy run 1 win-runner-2'; End = { param($dir) Set-Docker -Up -FailInfo 1 }
                Says = 'waiting for it to finish(.|\n)*attempt 1 of 2(.|\n)*Docker engine is running and healthy' })) {
        Set-State @() -DaemonDown
        $lockDir = New-LockDir @{ 'docker-prune-other.inprogress' = @(0, $v.Marker) }
        $evFile = Join-Path (New-Dir 'events') 'events'
        $callsFile = "$evFile.calls"; $listedFile = "$evFile.listed"
        $countPrelude = @'
$env:FAKE_DOCKER_CALLS = '@@CALLS@@'
function Get-ChildItem {
    $items = Microsoft.PowerShell.Management\Get-ChildItem @args
    if ($args -contains 'docker-prune-*.inprogress') { Microsoft.PowerShell.Management\Add-Content -LiteralPath '@@LISTED@@' -Value x }
    $items
}
'@ -replace '@@CALLS@@', $callsFile -replace '@@LISTED@@', $listedFile
        $bg = Start-Ensure $lockDir $evFile -prelude $countPrelude
        $problems = @(); $said = ''
        try {
            $deadline = [DateTime]::UtcNow.AddSeconds(120)
            while ($true) {
                $said = Read-Background $bg
                if ($said -match 'waiting for it to finish, then checking Docker again') { break }
                if ((Test-BackgroundDone $bg) -or (Get-Events $evFile).Count) { $problems += "did not wait for the other runner's $($v.What)"; break }
                if ([DateTime]::UtcNow -ge $deadline) { $problems += "did not say it was waiting within 120s"; break }
                Start-Sleep -Milliseconds 200
            }
            if (-not $problems.Count) {
                $calls0 = (Get-Events $callsFile).Count
                $listed0 = (Get-Events $listedFile).Count
                while ($true) {
                    if ((Get-Events $callsFile).Count -gt $calls0) { $problems += "asked docker while the other runner's marker was down"; break }
                    if ((Get-Events $listedFile).Count -ge $listed0 + 3) { break }
                    if ((Test-BackgroundDone $bg) -or (Get-Events $evFile).Count) { $problems += "stopped waiting while the other runner's marker was down"; break }
                    if ([DateTime]::UtcNow -ge $deadline) { $problems += "did not look for the marker 3 times within 120s"; break }
                    Start-Sleep -Milliseconds 100
                }
            }
            if (-not $problems.Count) {
                # The other runner's restart or prune finishes, and its
                # marker comes down.
                & $v.End $lockDir
                Remove-Item -LiteralPath (Join-Path $lockDir 'docker-prune-other.inprogress') -Force
                if (-not (Wait-Background $bg 120)) { $problems += "still going 120s after the other runner's $($v.What) finished" }
                $said = Read-Background $bg
                if ((Get-BackgroundExit $bg) -ne 0) { $problems += "exit $(Get-BackgroundExit $bg), want 0" }
                if ($said -notmatch $v.Says) { $problems += "missing '$($v.Says)'" }
            }
            $ev = @(Get-Events $evFile) -join ','
            if ($ev) { $problems += "did [$ev], want nothing" }
            $left = Get-Markers $lockDir
            if ($left.Count) { $problems += "left $($left -join ', ')" }
        } finally {
            Stop-Background $bg
        }
        Report "Docker Desktop is not started or restarted while another runner's $($v.What) has its marker down; Docker is checked again after it" ([pscustomobject]@{ Output = $said }) $problems
    }

    # Another runner's restart gets going just after this one looked for
    # markers: its marker comes down while this one judges the locks (the
    # Actions API call). Its name sorts after any GUID's, so this one waits
    # for it under its own marker. When it ends:
    # - having restarted Docker Desktop (a new docker-desktop.started), the
    #   check this one made before no longer counts, since Docker may still
    #   be coming up. It takes its marker back down and checks again, with
    #   every retry: Docker answers, or answers only after another failed
    #   check (one probe there would restart it again), or a job took its
    #   lock meanwhile, or Docker is still down, and then it restarts on the
    #   next round, one that did not wait;
    # - having given way (no start or restart), this one's check still
    #   counts: it restarts under its marker, which is written again first.
    #   The wait can outlast part of the marker's life (here it is made to
    #   look 10 minutes old), and a job stops waiting for a marker
    #   PruneMaxMinutes old.
    foreach ($v in @(
            @{ What = 'restarted Docker Desktop and brought Docker back'; Started = $true; Up = $true; FailInfo = 0; Lock = $false; Want = ''
                Says = @('since this one checked Docker', 'Docker engine is running and healthy') },
            @{ What = 'restarted Docker Desktop, which fails one more check before it answers'; Started = $true; Up = $true; FailInfo = 1; Lock = $false; Want = ''
                Says = @('since this one checked Docker', 'attempt 1 of 2', 'Docker engine is running and healthy') },
            @{ What = 'restarted Docker Desktop, and a job took its lock meanwhile'; Started = $true; Up = $true; FailInfo = 0; Lock = $true; Want = ''
                Says = @('took a Docker lock while this waited', 'Docker engine is running and healthy') },
            @{ What = 'restarted Docker Desktop and left Docker down'; Started = $true; Up = $false; FailInfo = 0; Lock = $false; Want = 'stop+marker,start+marker'
                Says = @('since this one checked Docker(.|\n)*restarting Docker Desktop', 'Docker is ready') },
            @{ What = 'gave way without starting or restarting Docker Desktop'; Started = $false; Up = $false; FailInfo = 0; Lock = $false; Want = 'stop+marker,start+marker'
                Says = @('restarting Docker Desktop', 'Docker is ready'); SaysNot = 'since this one checked Docker' })) {
        Set-State @() -DaemonDown
        $lockDir = New-LockDir @{ 'docker-job-90-1-a.lock' = @(5, 'keploy/keploy wf win-runner-2') }
        $other = Join-Path $lockDir 'docker-prune-zzzz.inprogress'
        $evFile = Join-Path (New-Dir 'events') 'events'
        $bg = Start-Ensure $lockDir $evFile -getRunStatus "Microsoft.PowerShell.Management\Set-Content -LiteralPath '$other' -Value 'a Docker Desktop restart by keploy/keploy run 2 win-runner-3'; 'completed'"
        $problems = @(); $said = ''; $wentAt = $null
        try {
            $deadline = [DateTime]::UtcNow.AddSeconds(120)
            while ($true) {
                $said = Read-Background $bg
                if ($said -match 'waiting for its marker to go') { break }
                if ((Test-BackgroundDone $bg) -or (Get-Events $evFile).Count) { $problems += "did not wait for the other runner's restart"; break }
                if ([DateTime]::UtcNow -ge $deadline) { $problems += "did not say it was waiting within 120s"; break }
                Start-Sleep -Milliseconds 200
            }
            if (-not $problems.Count) {
                $mine = @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath $lockDir -Filter 'docker-prune-*.inprogress' | Where-Object { $_.FullName -ne $other })
                if ($mine.Count -ne 1) { $problems += "found $($mine.Count) marker(s) of its own while it waited" }
                else { $mine[0].LastWriteTimeUtc = [DateTime]::UtcNow.AddMinutes(-10) }
                if ($v.Lock) { Set-Content -LiteralPath (Join-Path $lockDir 'docker-job-91-1-b.lock') -Value 'keploy/keploy wf win-runner-4' }
                if ($v.Started) { Write-OtherStart $lockDir -Up:$v.Up -FailInfo $v.FailInfo }
                $wentAt = [DateTime]::UtcNow
                Remove-Item -LiteralPath $other -Force
                if (-not (Wait-Background $bg 120)) { $problems += "still going 120s after the other runner's restart ended" }
                $said = Read-Background $bg
                if ((Get-BackgroundExit $bg) -ne 0) { $problems += "exit $(Get-BackgroundExit $bg), want 0" }
                foreach ($p in $v.Says) { if ($said -notmatch $p) { $problems += "missing '$p'" } }
                if ($v.SaysNot -and ($said -match $v.SaysNot)) { $problems += "says '$($v.SaysNot)'" }
            }
            $ev = @(Get-Events $evFile) -join ','
            if ($ev -ne $v.Want) { $problems += "did [$ev], want [$($v.Want)]" }
            foreach ($t in @(Get-Events "$evFile.marked")) {
                if ($wentAt -and ([DateTime]::new([long]$t, [DateTimeKind]::Utc) -lt $wentAt.AddSeconds(-5))) { $problems += "restarted under a marker last written before its wait ended" }
            }
            $left = Get-Markers $lockDir
            if ($left.Count) { $problems += "left $($left -join ', ')" }
        } finally {
            Stop-Background $bg
        }
        Report "a restart that waited for another runner's, which $($v.What), goes on correctly" ([pscustomobject]@{ Output = $said }) $problems
    }

    # The waits end at MaxWaitMinutes (0 here: at once), and the job fails
    # with the remediation, taking its own marker back down. In the
    # background, since a wait that did not end would never end: the other
    # runner's marker stays down.
    foreach ($v in @(
            @{ What = "for a marker already down"; Other = 'other'; Via = '' },
            @{ What = "for a marker put down while this one judged the locks, to restart Docker Desktop"; Other = 'zzzz'; Via = 'locks' },
            @{ What = "for a marker put down while this one looked at Docker Desktop, to start it"; Other = 'zzzz'; Via = 'desktop' })) {
        Set-State @() -DaemonDown
        $locks = @{}
        if ($v.Via -eq 'locks') { $locks['docker-job-95-1-a.lock'] = @(5, 'keploy/keploy wf win-runner-2') }
        elseif (-not $v.Via) { $locks["docker-prune-$($v.Other).inprogress"] = @(0, 'a Docker Desktop restart by keploy/keploy run 1 win-runner-2') }
        $lockDir = New-LockDir $locks
        $other = Join-Path $lockDir "docker-prune-$($v.Other).inprogress"
        $putDown = "Microsoft.PowerShell.Management\Set-Content -LiteralPath '$other' -Value 'a Docker Desktop start by keploy/keploy run 2 win-runner-3'"
        $runStatus = '$null'; $running = ''
        if ($v.Via -eq 'locks') { $runStatus = "$putDown; 'completed'" }
        if ($v.Via -eq 'desktop') { $running = "$putDown; `$false" }
        $evFile = Join-Path (New-Dir 'events') 'events'
        $bg = Start-Ensure $lockDir $evFile -getRunStatus $runStatus -extra 'MaxWaitMinutes = 0' -running $running
        $problems = @(); $said = ''
        try {
            if (-not (Wait-Background $bg 120)) { $problems += "still waiting 120s later" }
            $said = Read-Background $bg
            if ((Get-BackgroundExit $bg) -ne 1) { $problems += "exit $(Get-BackgroundExit $bg), want 1" }
            if ($said -notmatch "::error::Docker does not answer, and 0 min after this step started, .*\(docker-prune-$($v.Other)\.inprogress\)") { $problems += "missing the deadline error naming the other marker" }
            if ($said -notmatch 'wsl --shutdown') { $problems += "missing the remediation" }
            $ev = @(Get-Events $evFile) -join ','
            if ($ev) { $problems += "did [$ev], want nothing" }
            $left = (Get-Markers $lockDir) -join ','
            if ($left -ne "docker-prune-$($v.Other).inprogress") { $problems += "markers left [$left], want only the other runner's" }
        } finally {
            Stop-Background $bg
        }
        Report "a wait $($v.What) ends at MaxWaitMinutes and fails the job" ([pscustomobject]@{ Output = $said }) $problems
    }

    # Two runners whose checks fail together put their markers down at the
    # same moment, and each sees the other's. Barriers make that happen every
    # time: neither goes on from its first look for markers until both have
    # looked, nor from writing its marker until both markers are down, nor
    # from its look at the markers then until both have looked.
    # Exactly one of them may start or restart Docker Desktop, and the names
    # decide which: the lower-named waits for the other to give way, and goes
    # on; the other gives way, and then waits for it and finds Docker
    # answering. Each run's log says which part it took, and ends with
    # Docker answering.
    function Get-RacePrelude([string]$barrierDir, [string]$id) {
        @'
$global:listed = $false; $global:marked = $false; $global:raced = $false
function Wait-Barrier([string]$what, [scriptblock]$ready) {
    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    while (-not (& $ready)) {
        if ([DateTime]::UtcNow -ge $deadline) { Write-Host "BARRIER TIMEOUT: $what"; return }
        Start-Sleep -Milliseconds 20
    }
}
function Get-ChildItem {
    $items = Microsoft.PowerShell.Management\Get-ChildItem @args
    if ($global:marked -and (-not $global:raced) -and ($args -contains 'docker-prune-*.inprogress')) {
        # The race's look, made with both markers down: neither acts on it
        # (gives way, and takes its marker back down) until both have looked.
        $global:raced = $true
        Microsoft.PowerShell.Management\Set-Content -LiteralPath (Join-Path '@@BARRIER@@' 'raced-@@ID@@') -Value x
        Wait-Barrier 'both saw both markers' { @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath '@@BARRIER@@' -Filter 'raced-*').Count -ge 2 }
    }
    if ((-not $global:listed) -and ($args -contains 'docker-prune-*.inprogress')) {
        $global:listed = $true
        Microsoft.PowerShell.Management\Set-Content -LiteralPath (Join-Path '@@BARRIER@@' 'listed-@@ID@@') -Value x
        Wait-Barrier 'both looked for markers' { @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath '@@BARRIER@@' -Filter 'listed-*').Count -ge 2 }
    }
    $items
}
function Set-Content {
    Microsoft.PowerShell.Management\Set-Content @args
    if ((-not $global:marked) -and ("$args" -like '*docker-prune-*.inprogress*')) {
        $global:marked = $true
        # Its marker is down. A file that stays, not the markers themselves:
        # the other may see both, give way and take its marker back down
        # between two looks.
        Microsoft.PowerShell.Management\Set-Content -LiteralPath (Join-Path '@@BARRIER@@' 'marked-@@ID@@') -Value x
        Wait-Barrier 'both markers down' { @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath '@@BARRIER@@' -Filter 'marked-*').Count -ge 2 }
    }
}
'@ -replace '@@BARRIER@@', $barrierDir -replace '@@ID@@', $id
    }
    foreach ($race in @(
            @{ What = 'restart'; DesktopDown = $false; Want = 'stop+marker,start+marker' },
            @{ What = 'start'; DesktopDown = $true; Want = 'start+marker' })) {
        Set-State @() -DaemonDown
        $lockDir = New-Dir 'locks'
        $barrierDir = New-Dir 'barrier'
        $evDir = New-Dir 'events'
        $bgs = @(foreach ($id in 'a', 'b') {
                Start-Ensure $lockDir (Join-Path $evDir $id) -DesktopDown:$race.DesktopDown -prelude (Get-RacePrelude $barrierDir $id)
            })
        $problems = @(); $said = ''
        try {
            foreach ($bg in $bgs) { if (-not (Wait-Background $bg 180)) { $problems += "a run was still going after 180s" } }
            $logs = @($bgs | ForEach-Object { Read-Background $_ })
            $said = $logs -join "`n----`n"
            foreach ($bg in $bgs) { if ((Get-BackgroundExit $bg) -ne 0) { $problems += "a run exited $(Get-BackgroundExit $bg), want 0" } }
            if ($said -match 'BARRIER TIMEOUT') { $problems += "the runs never both put their markers down" }
            $waiters = @($logs | Where-Object { $_ -match 'waiting for its marker to go' })
            $yielders = @($logs | Where-Object { $_ -match "leaving the $($race.What) to it" })
            if (($waiters.Count -ne 1) -or ($yielders.Count -ne 1) -or ($waiters[0] -eq $yielders[0])) {
                $problems += "$($waiters.Count) run(s) waited for the other to give way and $($yielders.Count) gave way, want one of each"
            }
            foreach ($log in $logs) {
                $last = @($log -split "`r?`n" | Where-Object { $_.Trim() -and ($_ -notmatch '^exit=') }) | Select-Object -Last 1
                if ($last -notmatch '^(Docker engine is running and healthy|Docker is ready)\.$') { $problems += "a run ended with [$last], not with Docker answering" }
            }
            $ev = @(foreach ($id in 'a', 'b') { Get-Events (Join-Path $evDir $id) })
            if (($ev -join ',') -ne $race.Want) { $problems += "together did [$($ev -join ',')], want [$($race.Want)]" }
            $left = Get-Markers $lockDir
            if ($left.Count) { $problems += "left $($left -join ', ')" }
        } finally {
            foreach ($bg in $bgs) { Stop-Background $bg }
        }
        Report "two runners that put their markers down at the same moment $($race.What) Docker Desktop once" ([pscustomobject]@{ Output = $said }) $problems
    }

    # ---- recover-runner-state.ps1: the step before checkout ----------------

    # Processes and services go to fakes: $procs is what Get-Process lists,
    # $services maps a service name to its status. Did records each process
    # stopped (kill <pid>) and service stopped (stop <name>).
    function Invoke-Recover([object[]]$procs, [hashtable]$services, [string]$ws) {
        $did = New-Object System.Collections.ArrayList
        function Get-Process { [CmdletBinding()] param([string[]]$Name) $procs }
        function Stop-Process {
            [CmdletBinding()] param([Parameter(ValueFromPipeline = $true)]$InputObject, [switch]$Force)
            process { [void]$did.Add("kill $($InputObject.Id)") }
        }
        function Get-Service {
            [CmdletBinding()] param([string]$Name)
            if ($services.ContainsKey($Name)) { [pscustomobject]@{ Name = $Name; Status = $services[$Name] } }
        }
        function Stop-Service { [CmdletBinding()] param([string]$Name, [switch]$Force) [void]$did.Add("stop $Name") }
        $savedWs = $env:GITHUB_WORKSPACE
        $env:GITHUB_WORKSPACE = $ws
        $global:LASTEXITCODE = 0
        try {
            $out = & $recover *>&1 | Out-String
        } finally {
            $env:GITHUB_WORKSPACE = $savedWs
        }
        [pscustomobject]@{ Output = $out; Code = $LASTEXITCODE; Did = @($did) }
    }
    function Proc($id, $name, $path) { [pscustomobject]@{ Id = $id; ProcessName = $name; Path = $path } }
    function Did($result, [string[]]$want) {
        $got = @($result.Did) -join ','
        if ($got -ne (@($want) -join ',')) { "did [$got], want [$(@($want) -join ',')]" }
    }
    $ws = New-Dir 'ws'
    New-Item -ItemType Directory -Path (Join-Path $ws '.git') | Out-Null
    $bin = Join-Path $ws 'bin'
    # A sibling runner's workspace whose path starts with this one's.
    $sibling = Join-Path ($ws + '2') 'bin'

    $r = Invoke-Recover @(
        (Proc 11 'keploy-record' (Join-Path $bin 'keploy-record.exe')),
        (Proc 12 'keploy' (Join-Path $sibling 'keploy.exe')),
        (Proc 13 'go' (Join-Path $bin 'go.exe'))
    ) @{ WinDivert = 'Running'; WinDivert64 = 'Stopped' } $ws
    Report "the pre-checkout recovery stops only this workspace's keploy, and leaves WinDivert to a sibling's" $r @(
        (Did $r @('kill 11')), (Code $r 0),
        (Says $r 'Not resetting the WinDivert driver: 1 other keploy' "the kept-driver message")
    )

    $r = Invoke-Recover @((Proc 21 'keploy' (Join-Path $bin 'keploy.exe'))) @{ WinDivert = 'Stopped'; WinDivert64 = 'Running' } $ws
    Report "with no other keploy running, the recovery resets the WinDivert driver that is loaded" $r @((Did $r @('kill 21', 'stop WinDivert64')), (Code $r 0))

    $r = Invoke-Recover @((Proc 31 'keploy' $null)) @{ WinDivert = 'Running' } $ws
    Report "a keploy whose path cannot be read is another's: not stopped, and WinDivert is kept" $r @((Did $r @()), (Code $r 0))

    $noGit = New-Dir 'ws'
    Set-Content -Path (Join-Path $noGit 'leftover.txt') -Value 'x'
    $r = Invoke-Recover @() @{} $noGit
    $r2 = Invoke-Recover @() @{} $ws
    Report "a workspace with files but no .git is wiped, and one with a .git is kept" ([pscustomobject]@{ Output = $r.Output + $r2.Output }) @(
        $(if (@(Get-ChildItem -LiteralPath $noGit -Force).Count) { "the workspace without a .git was not wiped" }),
        $(if (-not (Test-Path -LiteralPath (Join-Path $ws '.git'))) { "the workspace with a .git was wiped" })
    )

    # It runs before actions/checkout, so no job can call it from the
    # repository: every "Recover stale runner state" step carries its text.
    $canon = (Get-Content -Raw -Path $recover).TrimEnd() -replace "`r`n", "`n"
    $steps = @(); $problems = @()
    foreach ($wf in @(Get-ChildItem -Path (Join-Path $repoRoot '.github/workflows') -File | Where-Object { $_.Extension -in '.yml', '.yaml' })) {
        $lines = [string[]]@(Get-Content -Path $wf.FullName)
        for ($i = 0; $i -lt $lines.Count; $i++) {
            if ($lines[$i] -notmatch '^(\s*)- name: Recover stale runner state\s*$') { continue }
            $stepIndent = $Matches[1].Length
            $where = "$($wf.Name):$($i + 1)"
            $steps += $where
            $runIndent = -1
            for ($j = $i + 1; $j -lt $lines.Count; $j++) {
                if ($lines[$j] -match '^(\s*)run: \|\s*$') { $runIndent = $Matches[1].Length; break }
                if (($lines[$j] -match '^(\s*)-\s') -and ($Matches[1].Length -le $stepIndent)) { break }
            }
            if ($runIndent -lt 0) { $problems += "$where has no run: | block"; continue }
            $body = New-Object System.Collections.ArrayList
            for ($k = $j + 1; $k -lt $lines.Count; $k++) {
                if (($lines[$k].Trim() -ne '') -and (($lines[$k] -replace '^(\s*).*$', '$1').Length -le $runIndent)) { break }
                [void]$body.Add($lines[$k])
            }
            $cut = $runIndent + 2
            $text = (@($body | ForEach-Object { if ($_.Length -ge $cut) { $_.Substring($cut) } else { $_.TrimStart() } }) -join "`n").TrimEnd()
            if ($text -ne $canon) { $problems += "$where differs from recover-runner-state.ps1" }
        }
    }
    if ($steps.Count -lt 4) { $problems += "found $($steps.Count) Recover stale runner state step(s) ($($steps -join ', ')), want the 4 of the self-hosted Windows jobs" }
    Report "every Recover stale runner state step runs recover-runner-state.ps1's text" ([pscustomobject]@{ Output = ($steps -join "`n") }) $problems

    # ---- the guard that keeps these cases off the shared machine ----------

    # Each script, with one of its shared-machine parameters left out and the
    # rest given, must itself stop before it does anything (not leave that to
    # a script it calls). Were it to run, the
    # default would land here instead: USERPROFILE points at an empty
    # directory (the default lock directory and wedge records live under
    # it), and a `docker` function stands in for the real CLI and records
    # every call, as the fake docker does (FAKE_DOCKER_CALLS). Passing '' is
    # leaving it out: the script then falls back to the default. With nothing
    # left out, each runs: the list of what is given is complete.
    function Invoke-Guarded([string]$script, [hashtable]$given, [string]$leftOut, [switch]$Blank) {
        $profileDir = New-Dir 'profile'
        $callsFile = Join-Path (New-Dir 'calls') 'calls'
        $dockerCalls = New-Object System.Collections.ArrayList
        function docker { [void]$dockerCalls.Add("$args") }
        $p = @{}
        foreach ($k in $given.Keys) { if ($k -ne $leftOut) { $p[$k] = $given[$k] } elseif ($Blank) { $p[$k] = '' } }
        $savedProfile = $env:USERPROFILE
        $env:USERPROFILE = $profileDir
        $env:FAKE_DOCKER_CALLS = $callsFile
        $records = New-Object System.Collections.ArrayList
        $global:LASTEXITCODE = 0
        try {
            try { & $script @p *>&1 | ForEach-Object { [void]$records.Add($_) } } catch { [void]$records.Add($_) }
        } finally {
            $env:USERPROFILE = $savedProfile
            $env:FAKE_DOCKER_CALLS = $null
        }
        [pscustomobject]@{
            Output  = ($records | Out-String)
            Refused = @(foreach ($rec in @($records | Where-Object { $_ -is [System.Management.Automation.ErrorRecord] })) {
                    if ("$($rec.Exception.Message)" -match "^$([regex]::Escape((Split-Path -Leaf $script))) run by the tests \(KEPLOY_WINDOWS_SCRIPT_TESTS\) without -(\S.*): the default") { $Matches[1] -split ', -' }
                })
            Calls   = $dockerCalls.Count + (Get-Events $callsFile).Count
            Wrote   = @(Get-ChildItem -LiteralPath $profileDir -Recurse -Force | ForEach-Object { $_.FullName.Substring($profileDir.Length) })
        }
    }
    $sbParams = @(([System.Management.Automation.Language.Parser]::ParseFile($ensure, [ref]$null, [ref]$null)).ParamBlock.Parameters |
        Where-Object { $_.StaticType -eq [scriptblock] } | ForEach-Object { $_.Name.VariablePath.UserPath })
    $ensureGiven = @{
        LockDir = (New-Dir 'locks'); DockerExe = $fake; Repository = 'keploy/keploy'; Attempts = 1; BackoffSeconds = 0
        PollSeconds = 0; ReadyTimeoutSeconds = 0; StabilizeSeconds = 0
        GetRunStatus = { $null }; TestDesktopRunning = { $true }; StopDesktop = { }; StartDesktop = { }
    }
    foreach ($guarded in @(
            @{ Script = $ensure; Names = @('LockDir', 'DockerExe') + $sbParams; Given = $ensureGiven },
            @{ Script = $cleanup; Names = @('LockDir', 'WedgeDir', 'DockerExe')
                Given = @{ LockDir = (New-Dir 'locks'); WedgeDir = (New-Dir 'wedge'); DockerExe = $fake; Repository = 'keploy/keploy'; GetRunStatus = { $null } } },
            @{ Script = $takeLock; Names = @('LockDir'); Given = @{ LockDir = (New-Dir 'locks'); RunId = 7; RunAttempt = 1 } },
            @{ Script = $reaper; Names = @('WedgeDir', 'DockerExe'); Given = @{ WedgeDir = (New-Dir 'wedge'); DockerExe = $fake; SettleSeconds = 0 } },
            @{ Script = $removeJob; Names = @('WedgeDir', 'DockerExe')
                Given = @{ WedgeDir = (New-Dir 'wedge'); DockerExe = $fake; ComposeProject = 'keploy-guard'; JobLock = ''; StartStepOutcome = '' } })) {
        $name = Split-Path -Leaf $guarded.Script
        $problems = @(); $out = ''
        Set-State @()
        $r = Invoke-Guarded $guarded.Script $guarded.Given ''
        $out += $r.Output
        if ($r.Refused.Count) { $problems += "refused to run with everything given (without -$($r.Refused -join ', -'))" }
        foreach ($n in $guarded.Names) {
            foreach ($blank in @($false) + @(if ($guarded.Given[$n] -is [string]) { $true })) {
                $how = "without -$n"
                if ($blank) { $how = "with -$n ''" }
                Set-State @()
                $r = Invoke-Guarded $guarded.Script $guarded.Given $n -Blank:$blank
                $out += $r.Output
                if ($r.Refused -notcontains $n) { $problems += "ran $how" }
                if ($r.Calls) { $problems += "called docker $($r.Calls) time(s) $how" }
                if ($r.Wrote.Count) { $problems += "wrote $($r.Wrote -join ', ') under USERPROFILE $how" }
            }
        }
        Report "under the tests, $name refuses to run with $($guarded.Names -join ', ') left at a default that reaches the shared machine" ([pscustomobject]@{ Output = $out }) $problems
    }

    # ---- the thresholds against every job that can run on these runners ----

    # The thresholds are only safe while no job on this Docker VM can outlive
    # them, and a job's if: always() steps still run after it is cancelled at
    # its timeout-minutes (with none, it inherits GitHub's 360). So every
    # self-hosted Windows job must carry a timeout-minutes at least
    # $timeoutMargin below both MinAgeMinutes defaults, and so must
    # keploy/windows-redirector's build job, which shares the runners (listed
    # by hand: it lives in another repository).
    #
    # Get-Jobs reads a job's runs-on in each form GitHub accepts: labels on the
    # line ([self-hosted, Windows] or one label), a block list under it, or a
    # labels: mapping. An expression is followed to every literal the
    # workflows can feed it: its own quoted literals, and for each inputs.X or
    # matrix.X it reads, every value any workflow gives an X: key (inline, as
    # a block list, or as the default: of an input declaration), following
    # expressions among those. A form it cannot resolve (a runner group, an
    # expression reading anything else) fails the check instead of passing it,
    # since it could land on these runners.
    $timeoutMargin = 5
    function Get-Labels([string]$value) {
        $v = ($value -replace '\s+#.*$', '').Trim().Trim("'").Trim()
        if ($v -match '^\[(.*)\]$') { $v = $Matches[1] }
        @($v -split ',' | ForEach-Object { $_.Trim().Trim('"').Trim("'") } | Where-Object { $_ })
    }
    function Test-WindowsLabels([object[]]$labels) { ($labels -contains 'self-hosted') -and ($labels -contains 'windows') }
    # The non-blank, non-comment lines of the block under key line $i.
    function Get-Block([string[]]$lines, [int]$i) {
        $col = ($lines[$i] -replace '^(\s*(?:-\s+)?).*$', '$1').Length
        for ($j = $i + 1; $j -lt $lines.Count; $j++) {
            if ($lines[$j] -match '^\s*(#.*)?$') { continue }
            $ind = ($lines[$j] -replace '^(\s*).*$', '$1').Length
            if ($ind -lt $col -or ($ind -eq $col -and $lines[$j] -notmatch '^\s*-\s')) { break }
            $lines[$j]
        }
    }
    # Adds to $sets every label list $expr can evaluate to; $false when it
    # reads something other than inputs.X / matrix.X (through fromJSON).
    function Resolve-Expression([string]$expr, $files, [System.Collections.ArrayList]$sets) {
        $seen = @{}
        $todo = New-Object System.Collections.Queue
        $todo.Enqueue($expr)
        while ($todo.Count -gt 0) {
            $e = $todo.Dequeue()
            foreach ($lit in [regex]::Matches($e, "'([^']*)'")) { [void]$sets.Add(@(Get-Labels $lit.Groups[1].Value)) }
            $e = [regex]::Replace($e, "'[^']*'", ' ')
            $names = @([regex]::Matches($e, '\b(?:inputs|matrix)\.([A-Za-z0-9_-]+)') | ForEach-Object { $_.Groups[1].Value })
            $rest = [regex]::Replace($e, '\b(?:inputs|matrix)\.[A-Za-z0-9_-]+', ' ') -replace '\$\{\{|\}\}|\|\||fromJSON\(|\)|\s', ''
            if ($rest) { return $false }
            foreach ($n in $names) {
                if ($seen.ContainsKey($n)) { continue }
                $seen[$n] = $true
                $keyRe = '^\s*(?:-\s+)?' + [regex]::Escape($n) + ':(?:\s+(.*))?$'
                foreach ($lines in $files.Values) {
                    for ($i = 0; $i -lt $lines.Count; $i++) {
                        if ($lines[$i] -notmatch $keyRe) { continue }
                        $v = ("$($Matches[1])" -replace '(^|\s+)#.*$', '').Trim()
                        $values = @()
                        if ($v) { $values = @($v) } else {
                            foreach ($b in @(Get-Block $lines $i)) {
                                if ($b -match '^\s*-\s+(.+)$' -or $b -match '^\s*default:\s*(.+)$') { $values += $Matches[1] }
                            }
                        }
                        foreach ($val in $values) {
                            if ($val -match '\$\{\{') { $todo.Enqueue($val) } else { [void]$sets.Add(@(Get-Labels $val)) }
                        }
                    }
                }
            }
        }
        $true
    }
    # Jobs are the keys one level under a top-level jobs:, at whatever
    # indentation the file uses, and a job's timeout-minutes and runs-on are
    # the keys one level under it. A line at job-key indentation that is not a
    # plain job key (a quoted key, a flow mapping, ...) fails the check, and
    # so does a jobs: line it cannot read: a job the guard does not see is one
    # it silently passes, and its runs-on and timeout-minutes would be
    # credited to the job above it.
    function Get-Jobs([string]$dir) {
        $files = @{}
        foreach ($wf in @(Get-ChildItem -Path $dir -File | Where-Object { $_.Extension -in '.yml', '.yaml' })) {
            $files[$wf.Name] = [string[]]@(Get-Content -Path $wf.FullName)
        }
        foreach ($name in @($files.Keys | Sort-Object)) {
            $lines = $files[$name]; $job = $null; $inJobs = $false; $jobIndent = -1; $keyIndent = -1
            for ($i = 0; $i -lt $lines.Count; $i++) {
                $line = $lines[$i]
                if ($line -match '^jobs:\s*(#.*)?$') { $inJobs = $true; $jobIndent = -1; continue }
                if ($line -match '^[^\s#]') {
                    $inJobs = $false
                    if ($line -match '^jobs\s*:') {
                        if ($job) { $job }
                        $job = $null
                        [pscustomobject]@{ where = "$name`:line$($i + 1)"; windows = $false; timeout = $null; unknown = "a jobs: line it cannot read: $($line.Trim())" }
                    }
                }
                if (-not $inJobs) { continue }
                if ($line -match '^\s*(#.*)?$') { continue }
                $ind = ($line -replace '^(\s*).*$', '$1').Length
                if ($jobIndent -lt 0) { $jobIndent = $ind }
                if ($ind -le $jobIndent) {
                    if ($job) { $job }
                    $keyIndent = -1
                    if (($ind -eq $jobIndent) -and ($line -match '^\s*([A-Za-z_][A-Za-z0-9_-]*):\s*(#.*)?$')) {
                        $job = [pscustomobject]@{ where = "$name`:$($Matches[1])"; windows = $false; timeout = $null; unknown = '' }
                    } else {
                        $job = [pscustomobject]@{ where = "$name`:line$($i + 1)"; windows = $false; timeout = $null; unknown = "a line at job-key indentation that is not a job key: $($line.Trim())" }
                    }
                    continue
                }
                if ($keyIndent -lt 0) { $keyIndent = $ind }
                if ($ind -ne $keyIndent) { continue }
                if ($line -match '^\s*timeout-minutes:\s*(\d+)\s*(#.*)?$') {
                    $job.timeout = [int]$Matches[1]
                } elseif ($line -match '^\s*runs-on:(?:\s+(.*))?$') {
                    $value = ("$($Matches[1])" -replace '(^|\s+)#.*$', '').Trim()
                    $sets = New-Object System.Collections.ArrayList
                    if ($value -match '\$\{\{') {
                        if (-not (Resolve-Expression $value $files $sets)) { $job.unknown = "runs-on: $value" }
                    } elseif ($value) {
                        [void]$sets.Add(@(Get-Labels $value))
                    } else {
                        $labels = @()
                        foreach ($b in @(Get-Block $lines $i)) {
                            if ($b -match '\$\{\{') { $job.unknown = "runs-on: ... $($b.Trim())" }
                            elseif ($b -match '^\s*-\s+(.+)$' -or $b -match '^\s*labels:\s*(\S.*)$') { $labels += Get-Labels $Matches[1] }
                            elseif ($b -notmatch '^\s*labels:\s*$') { $job.unknown = "runs-on: ... $($b.Trim())" }
                        }
                        if (-not $labels.Count -and -not $job.unknown) { $job.unknown = 'runs-on: (no labels)' }
                        [void]$sets.Add($labels)
                    }
                    foreach ($set in $sets) { if (Test-WindowsLabels $set) { $job.windows = $true } }
                }
            }
            if ($job) { $job }
        }
    }
    function Get-TimeoutProblems($jobs, [int]$limit) {
        foreach ($j in $jobs) {
            if ($j.unknown) { "$($j.where) (cannot tell whether it runs on these runners from $($j.unknown))" }
            elseif ($j.windows -and (($null -eq $j.timeout) -or ($j.timeout + $timeoutMargin -gt $limit))) { "$($j.where) (timeout-minutes $($j.timeout))" }
        }
    }

    # The parser itself, over every runs-on form, on a workflows dir of its own.
    $wfDir = New-Dir 'workflows'
    Set-Content -Path (Join-Path $wfDir 'forms.yml') -Value @'
on:
  workflow_call:
    inputs:
      pool:
        type: string
        default: '["self-hosted", "Windows"]'
      runner:
        type: string
        default: 'ubuntu-latest' # a hosted runner
jobs:
  listform:
    runs-on:
      - self-hosted
      - Windows
    steps: []
  grouped:
    runs-on:
      group: win-pool
      labels: X64
  viainput:
    runs-on: ${{ fromJSON(inputs.pool) }}
    timeout-minutes: 42
  linuxinput:
    runs-on: ${{ inputs.runner || 'ubuntu-24.04-arm' }}
  fromvars:
    runs-on: ${{ vars.POOL }}
    timeout-minutes: 10
  flow:
    runs-on: [self-hosted, Windows, X64]
    timeout-minutes: 30
  caller:
    uses: ./.github/workflows/forms.yml
    with:
      runner: ubuntu-latest
  commented: # a trailing comment on the job key
    runs-on: [self-hosted, Windows]
  'quoted':
    runs-on: [self-hosted, Windows]
  after:
    runs-on: ubuntu-latest
    timeout-minutes: 5
'@
    # Jobs indented by four, a comment on jobs:, and timeout-minutes read only
    # at the job's own level (a step's does not count for the job).
    Set-Content -Path (Join-Path $wfDir 'deep.yml') -Value @'
on: push
jobs: # four-space job keys
    deepbare:
        runs-on: [self-hosted, Windows]
        steps:
          - run: echo
            timeout-minutes: 5
    deepok:   # with a comment
        runs-on: [self-hosted, Windows]
        timeout-minutes: 20 # minutes
'@
    Set-Content -Path (Join-Path $wfDir 'flow.yml') -Value @'
on: push
jobs: { flowjob: { runs-on: [self-hosted, Windows] } }
'@
    $got = @(Get-TimeoutProblems @(Get-Jobs $wfDir) 45 | ForEach-Object { ($_ -split ' \(', 2)[0] + $(if ($_ -match 'cannot tell') { '=unresolved' } else { '=timeout' }) }) -join ','
    $quoted = [array]::IndexOf([string[]]@(Get-Content -Path (Join-Path $wfDir 'forms.yml')), "  'quoted':") + 1
    $want = "deep.yml:deepbare=timeout,flow.yml:line2=unresolved,forms.yml:listform=timeout,forms.yml:grouped=unresolved,forms.yml:viainput=timeout,forms.yml:fromvars=unresolved,forms.yml:commented=timeout,forms.yml:line$quoted=unresolved"
    Report "the timeout guard reads list, group, flow and expression runs-on forms, and every job-key form" ([pscustomobject]@{ Output = $got }) @(
        $(if ($got -ne $want) { "flagged [$got], want [$want]" })
    )

    $limit = [Math]::Min([Math]::Min($minAge, (Get-Default $cleanup 'MinAgeMinutes')), (Get-Default $ensure 'MinAgeMinutes'))
    $jobs = @(Get-Jobs (Join-Path $repoRoot '.github/workflows')) +
        @([pscustomobject]@{ where = 'keploy/windows-redirector:build'; windows = $true; timeout = 40; unknown = '' })
    $win = @($jobs | Where-Object { $_.windows })
    $bad = @(Get-TimeoutProblems $jobs $limit)
    if ($win.Count -lt 2) {
        $failures++; Write-Host "FAIL - found no self-hosted Windows jobs; the thresholds are unguarded"
    } elseif ($bad.Count -gt 0) {
        $failures++; Write-Host "FAIL - jobs without a timeout-minutes at least $timeoutMargin below MinAgeMinutes ($limit)`: $($bad -join ', ')"
    } else {
        Write-Host "ok   - MinAgeMinutes ($limit) outlasts all $($win.Count) self-hosted Windows jobs' timeout-minutes by at least $timeoutMargin"
    }

    # A running cleanup's marker must never look abandoned to a job: the
    # cleanup job's timeout keeps it under the bound both scripts use.
    $pruneMax = Get-Default $cleanup 'PruneMaxMinutes'
    $cw = @($jobs | Where-Object { $_.where -eq 'prepare_and_run.yml:cleanup_windows' })
    Report "a live cleanup's or restart's marker is younger than PruneMaxMinutes" ([pscustomobject]@{ Output = '' }) @(
        $(foreach ($other in @($takeLock, $ensure)) {
            if ($pruneMax -ne (Get-Default $other 'PruneMaxMinutes')) { "PruneMaxMinutes differs: cleanup-windows.ps1 $pruneMax, $(Split-Path -Leaf $other) $(Get-Default $other 'PruneMaxMinutes')" }
        }),
        # ensure-docker.ps1's bound on how long a start or restart holds its
        # marker (see its ReadyTimeoutSeconds): every docker call in it is
        # cut off at CallTimeoutSeconds.
        $(if ((2 * (Get-Default $ensure 'PollSeconds')) + (Get-Default $ensure 'ReadyTimeoutSeconds') + (Get-Default $ensure 'StabilizeSeconds') + (6 * (Get-Default $ensure 'CallTimeoutSeconds')) + ($timeoutMargin * 60) -gt ($pruneMax * 60)) {
            "ensure-docker.ps1's start or restart can hold its marker for longer than $timeoutMargin min short of PruneMaxMinutes ($pruneMax)"
        }),
        $(if ($cw.Count -ne 1 -or $null -eq $cw[0].timeout -or ($cw[0].timeout + $timeoutMargin -gt $pruneMax)) {
            "cleanup_windows timeout-minutes [$(@($cw | ForEach-Object { $_.timeout }) -join ',')] is not at least $timeoutMargin below PruneMaxMinutes ($pruneMax)"
        })
    )

    # ensure-docker.ps1 must fail with its remediation before precheck-windows
    # is killed at its timeout-minutes. It stops waiting for other runners at
    # MaxWaitMinutes after it started; past that it may still finish one full
    # check of Docker (every attempt, each of two calls cut off at
    # CallTimeoutSeconds, and the backoff between them) and one start or
    # restart (the marker bound above). The margin is for the job's other
    # steps.
    $e = @{}
    foreach ($p in 'Attempts', 'BackoffSeconds', 'CallTimeoutSeconds', 'PollSeconds', 'ReadyTimeoutSeconds', 'StabilizeSeconds', 'MaxWaitMinutes') { $e[$p] = Get-Default $ensure $p }
    $fullCheck = $e.Attempts * 2 * $e.CallTimeoutSeconds + $e.BackoffSeconds * ([Math]::Pow(2, $e.Attempts - 1) - 1)
    $restart = 2 * $e.PollSeconds + $e.ReadyTimeoutSeconds + $e.StabilizeSeconds + 6 * $e.CallTimeoutSeconds
    $ensureMax = $e.MaxWaitMinutes * 60 + $fullCheck + $restart
    $pc = @($jobs | Where-Object { $_.where -eq 'prepare_and_run.yml:precheck-windows' })
    Report "ensure-docker.ps1 gives up on other runners in time to fail precheck-windows with its remediation" ([pscustomobject]@{ Output = '' }) @(
        $(if ($pc.Count -ne 1 -or $null -eq $pc[0].timeout -or ($ensureMax + $timeoutMargin * 60 -gt $pc[0].timeout * 60)) {
            "ensure-docker.ps1 can run $([int]$ensureMax) s (MaxWaitMinutes $($e.MaxWaitMinutes)), not at least $timeoutMargin min below precheck-windows' timeout-minutes [$(@($pc | ForEach-Object { $_.timeout }) -join ',')]"
        })
    )
} finally {
    $env:KEPLOY_WINDOWS_SCRIPT_TESTS = $savedTestsFlag
    Remove-Item -Recurse -Force -Path $work -ErrorAction SilentlyContinue
}

if ($failures -gt 0) {
    Write-Host "$failures case(s) failed"
    exit 1
}
Write-Host "all reaper and cleanup cases passed"
exit 0

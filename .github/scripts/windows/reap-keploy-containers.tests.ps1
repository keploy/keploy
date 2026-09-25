# Tests for the scripts in this directory that act on the self-hosted Windows
# runners' shared Docker daemon: reap-keploy-containers.ps1,
# cleanup-windows.ps1, take-docker-job-lock.ps1, remove-job-containers.ps1 (with
# register-job-compose-project.ps1) and ensure-docker.ps1. Plain PowerShell, no
# Pester, so it runs unchanged under Windows PowerShell 5.1 (what the
# self-hosted runners use) and PowerShell 7 on Linux (what the ubuntu CI job
# uses):
#
#   pwsh -NoProfile -File .github/scripts/windows/reap-keploy-containers.tests.ps1
#
# The scripts drive a fake docker CLI (-DockerExe) backed by a JSON state file,
# so every case pins the daemon clock and each container's timestamps exactly.
# Nothing here touches the real Docker daemon or Docker Desktop.
$ErrorActionPreference = 'Stop'

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$reaper = Join-Path $here 'reap-keploy-containers.ps1'
$cleanup = Join-Path $here 'cleanup-windows.ps1'
$takeLock = Join-Path $here 'take-docker-job-lock.ps1'
$ensure = Join-Path $here 'ensure-docker.ps1'
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
$state = Get-Content -Raw -Path $env:FAKE_DOCKER_STATE | ConvertFrom-Json
function Save { $state | ConvertTo-Json -Depth 5 | Set-Content -Path $env:FAKE_DOCKER_STATE -Encoding ASCII }
function Find($id) { @($state.containers | Where-Object { $_.id -eq $id }) }
switch ($args[0]) {
    'info' {
        if ($state.daemonDown) { exit 1 }
        # A daemon too busy to answer the next failInfo calls.
        if ($state.failInfo -gt 0) { $state.failInfo--; Save; exit 1 }
        Write-Output $state.systemTime; exit 0
    }
    'ps' {
        if ($state.daemonDown) { exit 1 }
        # A container whose removal is already under way leaves the list after
        # its countdown of `ps` calls, the way the daemon finishes it.
        foreach ($c in @($state.containers)) { if ($c.removing) { $c.removing--; if ($c.removing -eq 0) { $c.gone = $true } } }
        $state.containers = @($state.containers | Where-Object { -not $_.gone }); Save
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
function Set-State($containers, $systemTime = $NOW, [switch]$DaemonDown, [int]$FailInfo = 0) {
    [pscustomobject]@{ systemTime = $systemTime; daemonDown = [bool]$DaemonDown; failInfo = $FailInfo; containers = @($containers); removed = @(); pruned = @() } |
        ConvertTo-Json -Depth 5 | Set-Content -Path $stateFile -Encoding ASCII
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
    $scriptArgs.DockerExe = $fake
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
    $job = Start-Job -ScriptBlock {
        param($script, $dir)
        & $script -LockDir $dir -RunId 12 -RunAttempt 1 -PollSeconds 1 *>&1 | ForEach-Object { "$_" }
    } -ArgumentList $takeLock, $lockDir
    $problems = @()
    try {
        $deadline = [DateTime]::UtcNow.AddSeconds(120)
        $held = @(); $said = ''
        while ($true) {
            $held = @(Get-ChildItem -LiteralPath $lockDir -Filter 'docker-job-12-1-*.lock')
            $said = @(Receive-Job -Job $job -Keep) -join "`n"
            if (($held.Count -gt 0) -and ($said -match 'A Docker prune or restart is in progress')) { break }
            if ("$($job.State)" -in 'Completed', 'Failed', 'Stopped') { break }
            if ([DateTime]::UtcNow -ge $deadline) { break }
            Start-Sleep -Milliseconds 200
        }
        if ("$($job.State)" -in 'Completed', 'Failed', 'Stopped') {
            $problems += "take-docker-job-lock.ps1 finished ($($job.State)) while the prune marker was down"
        } elseif ($held.Count -ne 1) {
            $problems += "no docker-job-12-1-*.lock written before waiting for the prune (found $($held.Count))"
        } elseif ($said -notmatch 'A Docker prune or restart is in progress') {
            $problems += "did not say it was waiting for the prune"
        } else {
            Remove-Item -LiteralPath $m -Force
            if (-not (Wait-Job -Job $job -Timeout 120)) { $problems += "still waiting 120s after the prune marker was taken down" }
            elseif ($job.State -ne 'Completed') { $problems += "ended $($job.State)" }
            else {
                $said = @(Receive-Job -Job $job -Keep) -join "`n"
                $after = @(Get-ChildItem -LiteralPath $lockDir -Filter 'docker-job-12-1-*.lock')
                if ($said -notmatch 'The Docker prune or restart has finished') { $problems += "did not say the prune was over" }
                if (($after.Count -ne 1) -or ($said -notmatch [regex]::Escape($after[0].FullName))) { $problems += "did not print the one lock it holds" }
            }
        }
    } finally {
        Remove-Job -Job $job -Force -ErrorAction SilentlyContinue
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
    $job = Start-Job -ScriptBlock {
        param($script, $dir)
        & $script -LockDir $dir -RunId 13 -RunAttempt 1 -PollSeconds 60 *>&1 | ForEach-Object { "$_" }
    } -ArgumentList $takeLock, $lockDir
    $problems = @()
    try {
        $deadline = [DateTime]::UtcNow.AddSeconds(120)
        $said = ''
        while ($true) {
            $said = @(Receive-Job -Job $job -Keep) -join "`n"
            if ($said -match 'is in progress') { $problems += "waits for it"; break }
            if ("$($job.State)" -in 'Completed', 'Failed', 'Stopped') { break }
            if ([DateTime]::UtcNow -ge $deadline) { $problems += "neither finished nor said it was waiting within 120s"; break }
            Start-Sleep -Milliseconds 200
        }
        if (-not $problems.Count) {
            $said = @(Receive-Job -Job $job -Keep) -join "`n"
            if ("$($job.State)" -ne 'Completed') { $problems += "ended $($job.State)" }
            elseif ($said -notmatch 'docker-job-13-1-[0-9a-f]{32}\.lock') { $problems += "did not print its lock" }
        }
    } finally {
        Remove-Job -Job $job -Force -ErrorAction SilentlyContinue
    }
    Report "a marker left by a killed cleanup is not waited for" ([pscustomobject]@{ Output = $said }) $problems

    # ---- ensure-docker.ps1: precheck-windows' Docker check ------------------

    # Every Docker Desktop action goes to a fake here: the defaults act on the
    # real Docker Desktop of the machine these tests also run on. Each fake
    # action is recorded, suffixed +marker if a prune/restart marker was down
    # at that moment. Starting Desktop brings the fake daemon back unless
    # -StaysDown. $locks and $runs are as for Invoke-Cleanup.
    function Invoke-Ensure($locks = @{}, $runs = @{}, [switch]$DesktopDown, [switch]$StaysDown, [scriptblock]$OnAsk = $null) {
        $lockDir = New-LockDir $locks
        $events = New-Object System.Collections.ArrayList
        $stateFile = $env:FAKE_DOCKER_STATE
        $running = -not $DesktopDown
        $ensureArgs = @{
            LockDir = $lockDir; Repository = 'keploy/keploy'
            BackoffSeconds = 0; PollSeconds = 0; ReadyTimeoutSeconds = 0; StabilizeSeconds = 0
            DesktopExe = 'C:\no\such\Docker Desktop.exe'
            TestDesktopRunning = { $running }.GetNewClosure()
            StopDesktop = {
                $down = @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath $lockDir -Filter 'docker-prune-*.inprogress').Count -gt 0
                [void]$events.Add("stop$(if ($down) { '+marker' })")
            }.GetNewClosure()
            StartDesktop = {
                $down = @(Microsoft.PowerShell.Management\Get-ChildItem -LiteralPath $lockDir -Filter 'docker-prune-*.inprogress').Count -gt 0
                [void]$events.Add("start$(if ($down) { '+marker' })")
                if (-not $StaysDown) {
                    $st = Get-Content -Raw -Path $stateFile | ConvertFrom-Json
                    $st.daemonDown = $false
                    $st | ConvertTo-Json -Depth 5 | Set-Content -Path $stateFile -Encoding ASCII
                }
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
        $r = Invoke-Script $ensure $ensureArgs
        $r | Add-Member -NotePropertyName Locks -NotePropertyValue @(Get-ChildItem $lockDir | ForEach-Object { $_.Name } | Sort-Object)
        $r | Add-Member -NotePropertyName Events -NotePropertyValue @($events)
        $r
    }
    function Events($result, [string[]]$want) {
        $got = @($result.Events) -join ','
        $exp = @($want) -join ','
        if ($got -ne $exp) { "did [$got], want [$exp]" }
    }

    Set-State @()
    $r = Invoke-Ensure
    Report "a healthy Docker is left alone" $r @((Code $r 0), (Events $r @()))

    # A daemon too busy to answer (four jobs loading images) used to get
    # Docker Desktop force-killed on the first failed `docker info`.
    Set-State @() -FailInfo 2
    $r = Invoke-Ensure
    Report "a Docker that misses a check or two is asked again, not restarted" $r @((Code $r 0), (Events $r @()))

    Set-State @() -DaemonDown
    $r = Invoke-Ensure @{
        'docker-job-50-1-a.lock'           = @(5, 'keploy/keploy wf win-runner-2')
        'prepare-windows-workflow-51.lock' = @(20, 'keploy/keploy started-1')
    } @{ '50/1' = 'in_progress'; '51' = 'in_progress' }
    Report "Docker Desktop is not restarted while a job holds a live Docker lock, and the job fails saying why" $r @(
        (Code $r 1), (Events $r @('ask keploy/keploy#50/1')),
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
        (Code $r 0), (Events $r @('ask keploy/keploy#60/1', 'stop+marker', 'start+marker')),
        (Locks $r @('prepare-windows-workflow-62.lock'))
    )

    Set-State @() -DaemonDown
    $r = Invoke-Ensure @{ 'docker-job-70-1-a.lock' = @(5, 'keploy/keploy wf win-runner-2') } @{ '70/1' = 'completed' } -OnAsk {
        param($dir) Set-Content -Path (Join-Path $dir 'docker-job-71-1-b.lock') -Value 'keploy/keploy wf win-runner-3'
    }
    Report "a job lock taken while the API was being asked calls the restart off" $r @(
        (Code $r 1), (Events $r @('ask keploy/keploy#70/1')),
        (Says $r 'took a Docker lock while the others were being judged \(docker-job-71-1-b\.lock\)' "the refusal naming the new lock"),
        (Locks $r @('docker-job-71-1-b.lock'))
    )

    Set-State @() -DaemonDown
    $r = Invoke-Ensure @{ 'docker-job-80-1-a.lock' = @(5, 'keploy/keploy wf win-runner-2') } @{ '80/1' = 'in_progress' } -DesktopDown
    Report "a Docker Desktop that is not running is started, whatever the locks" $r @((Code $r 0), (Events $r @('start')))

    Set-State @() -DaemonDown
    $r = Invoke-Ensure -StaysDown
    Report "a restart after which Docker never answers fails the job and takes its marker down" $r @(
        (Code $r 1), (Events $r @('stop+marker', 'start+marker')), (Locks $r @()),
        (Says $r '::error::Docker did not become ready' "the not-ready error")
    )

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
        $(if ((Get-Default $ensure 'PollSeconds') + (Get-Default $ensure 'ReadyTimeoutSeconds') + (Get-Default $ensure 'StabilizeSeconds') + ($timeoutMargin * 60) -gt ($pruneMax * 60)) {
            "ensure-docker.ps1's restart can hold its marker for longer than $timeoutMargin min short of PruneMaxMinutes ($pruneMax)"
        }),
        $(if ($cw.Count -ne 1 -or $null -eq $cw[0].timeout -or ($cw[0].timeout + $timeoutMargin -gt $pruneMax)) {
            "cleanup_windows timeout-minutes [$(@($cw | ForEach-Object { $_.timeout }) -join ',')] is not at least $timeoutMargin below PruneMaxMinutes ($pruneMax)"
        })
    )
} finally {
    Remove-Item -Recurse -Force -Path $work -ErrorAction SilentlyContinue
}

if ($failures -gt 0) {
    Write-Host "$failures case(s) failed"
    exit 1
}
Write-Host "all reaper and cleanup cases passed"
exit 0

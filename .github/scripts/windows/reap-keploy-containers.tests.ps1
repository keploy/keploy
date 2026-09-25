# Tests for reap-keploy-containers.ps1, cleanup-windows.ps1 and take-docker-job-lock.ps1. Plain
# PowerShell, no Pester, so it runs unchanged under Windows PowerShell 5.1
# (what the self-hosted runners use) and PowerShell 7 on Linux (what the ubuntu
# CI job uses):
#
#   pwsh -NoProfile -File .github/scripts/windows/reap-keploy-containers.tests.ps1
#
# Both scripts drive a fake docker CLI (-DockerExe) backed by a JSON state file,
# so every case pins the daemon clock and each container's timestamps exactly.
$ErrorActionPreference = 'Stop'

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$reaper = Join-Path $here 'reap-keploy-containers.ps1'
$cleanup = Join-Path $here 'cleanup-windows.ps1'
$takeLock = Join-Path $here 'take-docker-job-lock.ps1'
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
        Write-Output $state.systemTime; exit 0
    }
    'ps' {
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
function Set-State($containers, $systemTime = $NOW, [switch]$DaemonDown) {
    [pscustomobject]@{ systemTime = $systemTime; daemonDown = [bool]$DaemonDown; containers = @($containers); removed = @(); pruned = @() } |
        ConvertTo-Json -Depth 5 | Set-Content -Path $stateFile -Encoding ASCII
    $env:FAKE_DOCKER_STATE = $stateFile
}
function Get-State { Get-Content -Raw -Path $stateFile | ConvertFrom-Json }
function New-Dir($name) {
    $d = Join-Path $work ($name + '-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $d | Out-Null
    $d
}
# Runs a script; returns its output, exit code, and what the fake saw.
function Invoke-Script($script, [hashtable]$scriptArgs) {
    $scriptArgs.DockerExe = $fake
    $global:LASTEXITCODE = 0
    $output = & $script @scriptArgs *>&1 | Out-String
    $code = $LASTEXITCODE
    $after = Get-State
    [pscustomobject]@{
        Output  = $output
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
    Report "an unreadable daemon clock reaps nothing by age" $r @((Removed $r @()), (Code $r 0))

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

    # ---- cleanup-windows.ps1 ------------------------------------------------

    # $locks maps a lock file name to its age in minutes, or to @(age,
    # content); a docker-prune-*.inprogress name is a marker of that age.
    # $runs maps "<run>" or "<run>/<attempt>" to what the Actions API says of
    # it; anything else is unknown ($null), so age alone decides. With
    # -HttpApi, the script's own Get-RunStatusFromApi asks a fake
    # Invoke-RestMethod instead, and $runs maps to a status or an HTTP error
    # code.
    function Invoke-Cleanup($locks, $runs = @{}, [switch]$HttpApi) {
        $lockDir = New-Dir 'locks'
        foreach ($name in $locks.Keys) {
            $p = Join-Path $lockDir $name
            $spec = @($locks[$name])
            $content = 'x'
            if ($spec.Count -gt 1) { $content = $spec[1] }
            Set-Content -Path $p -Value $content
            (Get-Item $p).LastWriteTimeUtc = [DateTime]::UtcNow.AddMinutes(-$spec[0])
        }
        # A list, not an array: the closure below gets its own scope, so it can
        # only add to an object it shares with this function.
        $asked = New-Object System.Collections.ArrayList
        $status = {
            param($repo, $run, $attempt)
            $key = $run
            if ($attempt) { $key = "$run/$attempt" }
            $announced = @(Get-ChildItem -LiteralPath $lockDir -Filter 'docker-prune-*.inprogress').Count -gt 0
            [void]$asked.Add("$repo#$key$(if (-not $announced) { '-unannounced' })")
            $runs[$key]
        }.GetNewClosure()
        $cleanupArgs = @{ LockDir = $lockDir; WedgeDir = (New-Dir 'wedge'); Repository = 'keploy/keploy'; GetRunStatus = $status }
        if ($HttpApi) {
            $cleanupArgs.Remove('GetRunStatus')
            # Local to this call; the script finds it before the cmdlet.
            function Invoke-RestMethod($Uri, $Headers, $TimeoutSec, [switch]$UseBasicParsing) {
                $key = $Uri -replace '^https://api\.github\.com/repos/[^/]+/[^/]+/actions/runs/', '' -replace '/attempts/', '/'
                [void]$asked.Add(($Uri -replace '^https://api\.github\.com/repos/([^/]+/[^/]+)/actions/runs/.*$', '$1') + "#$key")
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
        $r | Add-Member -NotePropertyName Locks -NotePropertyValue @(Get-ChildItem $lockDir | ForEach-Object { $_.Name } | Sort-Object)
        $r | Add-Member -NotePropertyName Asked -NotePropertyValue @($asked | Sort-Object)
        $r
    }
    function Pruned($result, [bool]$want) {
        $did = $result.Pruned.Count -gt 0
        if ($did -ne $want) { "pruned=$did, want $want" }
        $bare = @($result.Pruned | Where-Object { $_ -like '*-unannounced' })
        if ($bare.Count) { "pruned with no in-progress marker down: $($bare -join ', ')" }
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
        (Removed $r @()), (Pruned $r $false), (Locks $r @('docker-job-1-1-a.lock', 'prepare-windows-workflow-2.lock'))
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

    # A prune announced 3s short of the marker's lifetime: the job waits for
    # it (here, until the marker ages out at $pruneEnds; a finishing cleanup
    # deletes it). The lock must already be down while it waits - written
    # after the wait, a prune that started in between would not see it - so it
    # must predate the prune's end by most of those 3s.
    $lockDir = New-Dir 'locks'
    $m = Join-Path $lockDir 'docker-prune-0123.inprogress'
    Set-Content -Path $m -Value 'x'
    $pruneEnds = [DateTime]::UtcNow.AddSeconds(3)
    (Get-Item $m).LastWriteTimeUtc = $pruneEnds.AddMinutes(-15)
    $t = [Diagnostics.Stopwatch]::StartNew()
    $out = & $takeLock -LockDir $lockDir -RunId 12 -RunAttempt 1 -PollSeconds 1 *>&1 | Out-String
    $waited = $t.Elapsed.TotalSeconds
    $held = @(Get-ChildItem -LiteralPath $lockDir -Filter 'docker-job-12-1-*.lock')
    Report "a job waits for a prune in progress before using Docker, holding its lock" ([pscustomobject]@{ Output = $out }) @(
        $(if ($waited -lt 2) { "returned after $([Math]::Round($waited, 1))s, before the prune was over" }),
        $(if ($out -notmatch 'A Docker prune is in progress') { "did not say it was waiting" }),
        $(if ($held.Count -ne 1) { "left $($held.Count) docker-job-12-1-*.lock files, want 1" }
          elseif ($held[0].LastWriteTimeUtc -gt $pruneEnds.AddSeconds(-1)) {
              "the lock was not written before waiting for the prune (written $([Math]::Round(($held[0].LastWriteTimeUtc - $pruneEnds).TotalSeconds, 2))s from the prune's end)"
          })
    )

    $lockDir = New-Dir 'locks'
    $m = Join-Path $lockDir 'docker-prune-4567.inprogress'
    Set-Content -Path $m -Value 'x'
    (Get-Item $m).LastWriteTimeUtc = [DateTime]::UtcNow.AddMinutes(-16)
    $t = [Diagnostics.Stopwatch]::StartNew()
    $out = & $takeLock -LockDir $lockDir -RunId 13 -RunAttempt 1 -PollSeconds 1 *>&1 | Out-String
    Report "a marker left by a killed cleanup is not waited for" ([pscustomobject]@{ Output = $out }) @(
        $(if ($t.Elapsed.TotalSeconds -ge 1) { "waited $([Math]::Round($t.Elapsed.TotalSeconds, 1))s" })
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
    function Get-Jobs([string]$dir) {
        $files = @{}
        foreach ($wf in @(Get-ChildItem -Path $dir -File | Where-Object { $_.Extension -in '.yml', '.yaml' })) {
            $files[$wf.Name] = [string[]]@(Get-Content -Path $wf.FullName)
        }
        foreach ($name in @($files.Keys | Sort-Object)) {
            $lines = $files[$name]; $job = $null; $inJobs = $false
            for ($i = 0; $i -lt $lines.Count; $i++) {
                $line = $lines[$i]
                if ($line -match '^jobs:\s*$') { $inJobs = $true; continue }
                if ($line -match '^[^\s#]') { $inJobs = $false }
                if (-not $inJobs) { continue }
                if ($line -match '^  ([A-Za-z0-9_-]+):\s*$') {
                    if ($job) { $job }
                    $job = [pscustomobject]@{ where = "$name`:$($Matches[1])"; windows = $false; timeout = $null; unknown = '' }
                } elseif ($job -and $line -match '^    timeout-minutes:\s*(\d+)') {
                    $job.timeout = [int]$Matches[1]
                } elseif ($job -and $line -match '^    runs-on:(?:\s+(.*))?$') {
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
'@
    $got = @(Get-TimeoutProblems @(Get-Jobs $wfDir) 45 | ForEach-Object { ($_ -split ' ')[0] + $(if ($_ -match 'cannot tell') { '=unresolved' } else { '=timeout' }) }) -join ','
    $want = 'forms.yml:listform=timeout,forms.yml:grouped=unresolved,forms.yml:viainput=timeout,forms.yml:fromvars=unresolved'
    Report "the timeout guard reads list, group, flow and expression runs-on forms" ([pscustomobject]@{ Output = $got }) @(
        $(if ($got -ne $want) { "flagged [$got], want [$want]" })
    )

    $limit = [Math]::Min($minAge, (Get-Default $cleanup 'MinAgeMinutes'))
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
    Report "a live cleanup's prune marker is younger than PruneMaxMinutes" ([pscustomobject]@{ Output = '' }) @(
        $(if ($pruneMax -ne (Get-Default $takeLock 'PruneMaxMinutes')) { "PruneMaxMinutes differs: cleanup-windows.ps1 $pruneMax, take-docker-job-lock.ps1 $(Get-Default $takeLock 'PruneMaxMinutes')" }),
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

# Tests for machine-cache.ps1. Plain PowerShell, no Pester, so it runs
# unchanged under Windows PowerShell 5.1 (Windows CI scripts on
# windows-latest, and prepare_and_run's cleanup_windows on the self-hosted
# machine) and PowerShell 7 on Linux (Windows CI scripts):
#
#   pwsh -NoProfile -File .github/scripts/windows/machine-cache.tests.ps1
#
# Every case uses a -Root of its own under the runner's temp directory
# (RUNNER_TEMP, on the drive the runner and the cache are on), or the
# system's: nothing here touches the machine's real cache.
$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$cache = Join-Path $here 'machine-cache.ps1'
$tempBase = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { [IO.Path]::GetTempPath() }
$work = Join-Path $tempBase ("machine-cache-tests-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null
$failures = 0

function Report([string]$what, [string[]]$problems) {
    if ($problems.Count -eq 0) { Write-Host "ok   - $what" }
    else { $script:failures++; Write-Host "FAIL - $what`: $($problems -join '; ')" }
}

# Run-Cache ARGS: runs machine-cache.ps1 in a PowerShell of its own, as a
# workflow step does; returns its exit code and its output, host and error
# lines too. Windows PowerShell 5.1 (and pwsh before 7.2) turns a native
# command's stderr into errors, which 'Stop' would throw on.
function Run-Cache([string[]]$cacheArgs) {
    $exe = (Get-Process -Id $PID).Path
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $out = & $exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File $cache @cacheArgs 2>&1 | ForEach-Object { "$_" }
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $prev
    }
    [pscustomobject]@{ Code = $code; Out = @($out) }
}

function Get-Sha([string]$path) { (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() }

function New-Case([string]$name, [int]$seed = 1) {
    $d = Join-Path $work $name
    New-Item -ItemType Directory -Path $d | Out-Null
    $src = Join-Path $d 'keploy.exe'
    [IO.File]::WriteAllBytes($src, [byte[]](1..200 | ForEach-Object { ($_ * $seed) % 256 }))
    [pscustomobject]@{ Dir = $d; Root = (Join-Path $d 'root'); Src = $src; Sha = (Get-Sha $src); Dest = (Join-Path $d 'out\keploy.exe') }
}

function Get-Case($c) {
    Run-Cache @('-Get', '-Name', 'build-windows', '-Key', $c.Sha, '-File', 'keploy.exe', '-Destination', $c.Dest, '-Root', $c.Root)
}
function Put-Case($c) {
    Run-Cache @('-Put', '-Name', 'build-windows', '-File', $c.Src, '-Root', $c.Root)
}

try {
    # 1. Nothing there: a miss, and nothing written.
    $c = New-Case 'miss'
    $r = Get-Case $c
    $p = @()
    if ($r.Code -ne 0) { $p += "exit $($r.Code)" }
    if ($r.Out[-1] -ne 'hit=false') { $p += "last line '$($r.Out[-1])', want hit=false" }
    if (Test-Path -LiteralPath $c.Dest) { $p += 'wrote the destination' }
    Report 'a get with nothing cached is a miss (hit=false, exit 0) and writes nothing' $p

    # 2. Put, then get by the file's SHA-256: the same bytes.
    $c = New-Case 'hit'
    $r = Put-Case $c
    $p = @()
    if ($r.Code -ne 0) { $p += "put exit $($r.Code): $($r.Out -join ' | ')" }
    if ($r.Out[-1] -ne "sha256=$($c.Sha)") { $p += "put's last line '$($r.Out[-1])', want sha256=$($c.Sha)" }
    $r = Get-Case $c
    if ($r.Out[-1] -ne 'hit=true') { $p += "last line '$($r.Out[-1])', want hit=true" }
    if (-not (Test-Path -LiteralPath $c.Dest)) { $p += 'no destination' }
    elseif ((Get-Sha $c.Dest) -ne $c.Sha) { $p += 'the copy differs' }
    $left = @(Get-ChildItem -LiteralPath (Join-Path $c.Root 'build-windows') -Force | Where-Object { $_.Name -like '.*' })
    if ($left.Count -gt 0) { $p += "a temporary directory is left: $($left.Name -join ', ')" }
    Report "a put says the file's SHA-256; a get by it copies the same bytes; no temporary directory is left" $p

    # 3. A build of the same commit with other content is another entry: a get
    # by its hash misses until it is put, and the old entry is not served.
    $c2 = New-Case 'rebuilt' 3
    $c2.Root = $c.Root
    $r = Get-Case $c2
    $p = @()
    if ($r.Out[-1] -ne 'hit=false') { $p += "a get of the new build hit before its put: $($r.Out[-1])" }
    $r = Put-Case $c2
    if ($r.Code -ne 0) { $p += "put exit $($r.Code)" }
    $r = Get-Case $c2
    if ($r.Out[-1] -ne 'hit=true' -or (Get-Sha $c2.Dest) -ne $c2.Sha) { $p += "the new build was not served by its hash: $($r.Out[-1])" }
    Report 'an entry is its content: another build of the same commit is another entry, never the old one' $p

    # 4. A second put of the same file: there already, exit 0.
    $r = Put-Case $c
    $p = @()
    if ($r.Code -ne 0) { $p += "exit $($r.Code)" }
    if (-not ($r.Out -match 'already')) { $p += "output: $($r.Out -join ' | ')" }
    Report 'a put of a file already there leaves it, and succeeds' $p

    # 5. A cached file that no longer hashes to its key: a miss, with a
    # warning, and a put replaces it.
    $c = New-Case 'corrupt'
    Put-Case $c | Out-Null
    $cached = Join-Path $c.Root "build-windows\$($c.Sha)\keploy.exe"
    [IO.File]::WriteAllBytes($cached, [byte[]](1..100))
    $r = Get-Case $c
    $p = @()
    if ($r.Out[-1] -ne 'hit=false') { $p += "last line '$($r.Out[-1])', want hit=false" }
    if (-not ($r.Out -match 'does not hash to its key')) { $p += "no warning: $($r.Out -join ' | ')" }
    if (Test-Path -LiteralPath $c.Dest) { $p += 'wrote the destination' }
    $r = Put-Case $c
    if ($r.Code -ne 0) { $p += "the repairing put exited $($r.Code): $($r.Out -join ' | ')" }
    $r = Get-Case $c
    if ($r.Out[-1] -ne 'hit=true') { $p += "after the put: $($r.Out[-1])" }
    Report 'a cached file that does not hash to its key is a miss, said, not copied, and a put replaces it' $p

    # 6. A torn entry (its directory, no file): a miss, and a put replaces it.
    $c = New-Case 'torn'
    New-Item -ItemType Directory -Path (Join-Path $c.Root "build-windows\$($c.Sha)") -Force | Out-Null
    $r = Get-Case $c
    $p = @()
    if ($r.Out[-1] -ne 'hit=false') { $p += "last line '$($r.Out[-1])', want hit=false" }
    $r = Put-Case $c
    if ($r.Code -ne 0) { $p += "put exit $($r.Code): $($r.Out -join ' | ')" }
    $r = Get-Case $c
    if ($r.Out[-1] -ne 'hit=true') { $p += "after the put: $($r.Out[-1])" }
    Report 'an entry without its file is a miss, and a put replaces it' $p

    # 7. Prune: per name, an entry older than -MaxAgeDays goes, and one past
    # the -MaxEntries newest goes once it is older than -KeepHours; a dead
    # put's directory and an older layout's go once -MaxAgeDays old.
    $c = New-Case 'prune'
    $shas = @()
    foreach ($seed in 2, 3, 5, 7, 11) {
        $s = New-Case "prune-$seed" $seed
        Run-Cache @('-Put', '-Name', 'build-windows', '-File', $s.Src, '-Root', $c.Root) | Out-Null
        $shas += $s.Sha
    }
    $base = Join-Path $c.Root 'build-windows'
    (Get-Item -LiteralPath (Join-Path $base $shas[0])).LastWriteTime = (Get-Date).AddDays(-5)
    (Get-Item -LiteralPath (Join-Path $base $shas[1])).LastWriteTime = (Get-Date).AddHours(-3)
    (Get-Item -LiteralPath (Join-Path $base $shas[2])).LastWriteTime = (Get-Date).AddHours(-1)
    (Get-Item -LiteralPath (Join-Path $base $shas[3])).LastWriteTime = (Get-Date).AddMinutes(-20)
    (Get-Item -LiteralPath (Join-Path $base $shas[4])).LastWriteTime = (Get-Date).AddMinutes(-10)
    $dead = New-Item -ItemType Directory -Path (Join-Path $base ".$($shas[1]).tmp-deadbeef") -Force
    $dead.LastWriteTime = (Get-Date).AddDays(-2)
    $oldLayout = New-Item -ItemType Directory -Path (Join-Path $base ('a' * 40)) -Force
    $oldLayout.LastWriteTime = (Get-Date).AddDays(-2)
    $freshOther = New-Item -ItemType Directory -Path (Join-Path $base ('b' * 40)) -Force
    $r = Run-Cache @('-Prune', '-MaxEntries', '2', '-KeepHours', '2', '-MaxAgeDays', '1', '-Root', $c.Root)
    $p = @()
    if ($r.Code -ne 0) { $p += "exit $($r.Code)" }
    if (Test-Path -LiteralPath (Join-Path $base $shas[0])) { $p += 'the 5-day-old entry is still there' }
    if (Test-Path -LiteralPath (Join-Path $base $shas[1])) { $p += 'the fourth newest entry, 3 hours old, is still there' }
    if (-not (Test-Path -LiteralPath (Join-Path $base $shas[2]))) { $p += 'the third newest entry, an hour old, is gone' }
    if (-not (Test-Path -LiteralPath (Join-Path $base $shas[3]))) { $p += 'the second newest entry is gone' }
    if (-not (Test-Path -LiteralPath (Join-Path $base $shas[4]))) { $p += 'the newest entry is gone' }
    if (Test-Path -LiteralPath $dead.FullName) { $p += "a dead put's 2-day-old directory is still there" }
    if (Test-Path -LiteralPath $oldLayout.FullName) { $p += "an older layout's 2-day-old directory is still there" }
    if (-not (Test-Path -LiteralPath $freshOther.FullName)) { $p += "a fresh directory that is not an entry is gone" }
    Report 'a prune removes entries past -MaxAgeDays, and past the -MaxEntries newest once older than -KeepHours, and day-old directories that are not entries' $p

    # 8. A name or key that is not a plain segment, a SHA-256, is refused.
    $c = New-Case 'badargs'
    $p = @()
    foreach ($n in '..', '.', 'a\b', 'a/b') {
        $r = Run-Cache @('-Put', '-Name', $n, '-File', $c.Src, '-Root', $c.Root)
        if ($r.Code -eq 0) { $p += "-Name '$n' was taken" }
    }
    foreach ($k in '..', 'abc123', ('A' * 64), ($c.Sha + 'x')) {
        $r = Run-Cache @('-Get', '-Name', 'build-windows', '-Key', $k, '-File', 'keploy.exe', '-Destination', $c.Dest, '-Root', $c.Root)
        if ($r.Code -eq 0) { $p += "-Key '$k' was taken" }
    }
    if (@(Get-ChildItem -LiteralPath $c.Dir -Force | Where-Object { $_.Name -notin 'keploy.exe' }).Count -gt 0) { $p += 'something was written outside a refused call''s root' }
    Report 'a name that is not one plain path segment, or a key that is not a lowercase SHA-256, is refused' $p

    # 9. Two puts of the same file at once: both succeed, one entry, nothing left.
    $c = New-Case 'race'
    $exe = (Get-Process -Id $PID).Path
    $procs = 1..2 | ForEach-Object {
        # Process.Start holds the process's handle from its creation, so its
        # ExitCode can be read after it exits. Start-Process -PassThru's
        # cannot, under Windows PowerShell 5.1 (.NET Framework), unless its
        # Handle is read before the exit, a race.
        $psi = New-Object Diagnostics.ProcessStartInfo
        $psi.FileName = $exe
        $psi.Arguments = (@('-NoLogo', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $cache, '-Put', '-Name', 'build-windows', '-File', $c.Src, '-Root', $c.Root) | ForEach-Object { '"' + $_ + '"' }) -join ' '
        $psi.UseShellExecute = $false
        $psi.RedirectStandardOutput = $true
        $psi.RedirectStandardError = $true
        $pr = [Diagnostics.Process]::Start($psi)
        [pscustomobject]@{ Proc = $pr; N = $_; Out = $pr.StandardOutput.ReadToEndAsync(); Err = $pr.StandardError.ReadToEndAsync() }
    }
    $procs | ForEach-Object { $_.Proc.WaitForExit() }
    $p = @()
    foreach ($x in $procs) {
        $code = $x.Proc.ExitCode
        $said = "out: $($x.Out.Result.Trim()); err: $($x.Err.Result.Trim())"
        if ($null -eq $code) { $p += "put $($x.N)'s exit code could not be read ($said)" }
        elseif ($code -ne 0) { $p += "put $($x.N) exited $code ($said)" }
    }
    $entries = @(Get-ChildItem -LiteralPath (Join-Path $c.Root 'build-windows') -Force)
    if ($entries.Count -ne 1 -or $entries[0].Name -ne $c.Sha) { $p += "entries: $($entries.Name -join ', ')" }
    $r = Get-Case $c
    if ($r.Out[-1] -ne 'hit=true') { $p += "a get after them: $($r.Out[-1])" }
    Report 'two puts of the same file at once both succeed, and leave one whole entry and nothing else' $p

    # An entry is a saving, never a dependency: a get of one that is gone is
    # a miss, hit=false, exit 0, with nothing left at the destination, and
    # the caller downloads the artifact (.github/actions/windows-binary,
    # which Windows CI scripts runs end to end for both of these).

    # 10. Evicted before the get: put, pruned for its age, then got.
    $c = New-Case 'evicted'
    Put-Case $c | Out-Null
    (Get-Item -LiteralPath (Join-Path $c.Root "build-windows\$($c.Sha)")).LastWriteTime = (Get-Date).AddDays(-2)
    Run-Cache @('-Prune', '-MaxAgeDays', '1', '-Root', $c.Root) | Out-Null
    $r = Get-Case $c
    $p = @()
    if (Test-Path -LiteralPath (Join-Path $c.Root "build-windows\$($c.Sha)")) { $p += 'the prune left the entry' }
    if ($r.Code -ne 0) { $p += "exit $($r.Code): $($r.Out -join ' | ')" }
    if ($r.Out[-1] -ne 'hit=false') { $p += "last line '$($r.Out[-1])', want hit=false" }
    if (Test-Path -LiteralPath $c.Dest) { $p += 'wrote the destination' }
    Report 'an entry evicted before the get is a miss (hit=false, exit 0), and writes nothing' $p

    # 11. Unreadable mid-copy: the entry passes the get's check, and the copy
    # fails. Here another process holds the file shut; a prune or a replacing
    # put that removes it between the check and the copy fails the copy the
    # same way. (One that tears it is case 5's hash mismatch.)
    $c = New-Case 'midcopy'
    Put-Case $c | Out-Null
    $cached = Join-Path $c.Root "build-windows\$($c.Sha)\keploy.exe"
    $held = [IO.File]::Open($cached, 'Open', 'Read', 'None')
    try {
        $r = Get-Case $c
    } finally {
        $held.Dispose()
    }
    $p = @()
    if ($r.Code -ne 0) { $p += "exit $($r.Code): $($r.Out -join ' | ')" }
    if ($r.Out[-1] -ne 'hit=false') { $p += "last line '$($r.Out[-1])', want hit=false" }
    if (-not ($r.Out -match 'could not be read')) { $p += "no warning: $($r.Out -join ' | ')" }
    if (Test-Path -LiteralPath $c.Dest) { $p += 'left something at the destination' }
    $r = Get-Case $c
    if ($r.Out[-1] -ne 'hit=true') { $p += "once the file was free again: $($r.Out[-1])" }
    Report 'an entry that cannot be read once the get has found it is a miss (hit=false, exit 0, said), and leaves nothing at the destination' $p

    # 12. A put of a file whose entry another process holds open (a get
    # copying it): left as it is, said, and the put succeeds.
    $c = New-Case 'held'
    Put-Case $c | Out-Null
    $cached = Join-Path $c.Root "build-windows\$($c.Sha)\keploy.exe"
    $held = [IO.File]::Open($cached, 'Open', 'Read', 'None')
    try {
        $r = Put-Case $c
    } finally {
        $held.Dispose()
    }
    $p = @()
    if ($r.Code -ne 0) { $p += "exit $($r.Code): $($r.Out -join ' | ')" }
    if ($r.Out[-1] -ne "sha256=$($c.Sha)") { $p += "last line '$($r.Out[-1])', want sha256=$($c.Sha)" }
    if (-not ($r.Out -match 'held by another process')) { $p += "not said: $($r.Out -join ' | ')" }
    if ((Get-Sha $cached) -ne $c.Sha) { $p += 'the held entry changed' }
    Report 'a put over an entry another process holds open leaves it, says so, and succeeds' $p
} finally {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}

if ($failures -gt 0) { Write-Host "$failures failure(s)"; exit 1 }
Write-Host 'all machine-cache.ps1 cases pass'
exit 0

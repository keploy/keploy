# Tests for fetch-artifact.ps1, against fetch-artifact.fake-store.py (the
# artifact API and a blob store that serves byte ranges, with faults). Plain
# PowerShell, no Pester, so it runs unchanged under Windows PowerShell 5.1
# (Windows CI scripts on windows-latest) and PowerShell 7 on Linux (Windows CI
# scripts), with Python 3 and curl:
#
#   pwsh -NoProfile -File .github/scripts/windows/fetch-artifact.tests.ps1
$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$fetch = Join-Path $here 'fetch-artifact.ps1'
$storeScript = Join-Path $here 'fetch-artifact.fake-store.py'
$onWindows = $env:OS -eq 'Windows_NT'
$python = if ($onWindows) { 'python' } else { 'python3' }
$tempBase = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { [IO.Path]::GetTempPath() }
$work = Join-Path $tempBase ("fetch-artifact-tests-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null
$failures = 0
$stores = @()

function Report([string]$what, [string[]]$problems) {
    if ($problems.Count -eq 0) { Write-Host "ok   - $what" }
    else { $script:failures++; Write-Host "FAIL - $what`: $($problems -join '; ')" }
}

function Get-Sha([string]$path) { (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() }

# The artifact: a zip (stored, not compressed) holding image.tar, 1 MiB of
# random bytes, as upload-artifact makes one.
Add-Type -AssemblyName System.IO.Compression
Add-Type -AssemblyName System.IO.Compression.FileSystem
$payload = Join-Path $work 'image.tar'
$bytes = New-Object byte[] 1048576
(New-Object Random 7).NextBytes($bytes)
[IO.File]::WriteAllBytes($payload, $bytes)
$zipPath = Join-Path $work 'artifact.zip'
$z = [IO.Compression.ZipFile]::Open($zipPath, 'Create')
try { [void][IO.Compression.ZipFileExtensions]::CreateEntryFromFile($z, $payload, 'image.tar', [IO.Compression.CompressionLevel]::NoCompression) } finally { $z.Dispose() }
$zipSize = (Get-Item -LiteralPath $zipPath).Length
$zipSha = Get-Sha $zipPath
$payloadSha = Get-Sha $payload

# Start-Store NAME FAULTS [DIGEST]: the fake store for one case; its port and log.
function Start-Store([string]$name, [hashtable]$faults, [string]$digest = $zipSha) {
    $d = Join-Path $work $name
    New-Item -ItemType Directory -Path $d | Out-Null
    $cfg = @{ blob = $zipPath; digest = $digest; name = 'docker-image-linux-amd64'; token = 'test-token'; faults = $faults
        port_file = (Join-Path $d 'port'); log_file = (Join-Path $d 'log') }
    $cfgPath = Join-Path $d 'config.json'
    [IO.File]::WriteAllText($cfgPath, (ConvertTo-Json $cfg -Depth 5))
    $psi = New-Object Diagnostics.ProcessStartInfo
    $psi.FileName = $python
    $psi.Arguments = '-I "' + $storeScript + '" "' + $cfgPath + '"'
    $psi.UseShellExecute = $false
    $p = [Diagnostics.Process]::Start($psi)
    $script:stores += $p
    while (-not (Test-Path -LiteralPath $cfg.port_file)) {
        if ($p.HasExited) { throw "the fake store exited ($($p.ExitCode)) before it listened" }
        Start-Sleep -Milliseconds 100
    }
    [pscustomobject]@{ Dir = $d; Port = [int](Get-Content -LiteralPath $cfg.port_file); Log = $cfg.log_file; Dest = (Join-Path $d 'dest') }
}

function Get-StoreLog($s) {
    if (-not (Test-Path -LiteralPath $s.Log)) { return @() }
    @(Get-Content -LiteralPath $s.Log | ForEach-Object { ConvertFrom-Json $_ })
}

# Run-Fetch STORE ARGS: fetch-artifact.ps1 in a PowerShell of its own, as a
# step runs it, against STORE; its exit code and output. Windows PowerShell
# 5.1 (and pwsh before 7.2) turns a native command's stderr into errors, which
# 'Stop' would throw on.
function Run-Fetch($s, [string[]]$extra = @(), [string]$token = 'test-token') {
    $exe = (Get-Process -Id $PID).Path
    $saved = @{}
    $vars = @{ GITHUB_TOKEN = $token; GITHUB_REPOSITORY = 'keploy/keploy'; GITHUB_RUN_ID = '1'; GITHUB_API_URL = "http://127.0.0.1:$($s.Port)" }
    foreach ($k in $vars.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k); [Environment]::SetEnvironmentVariable($k, $vars[$k]) }
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $out = & $exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File $fetch -Name docker-image-linux-amd64 -Destination $s.Dest @extra 2>&1 | ForEach-Object { "$_" }
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $prev
        foreach ($k in $vars.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
    }
    [pscustomobject]@{ Code = $code; Out = @($out) }
}

# Check-Whole R S: R unpacked the artifact whole into S's destination.
function Check-Whole($r, $s) {
    $p = @()
    if ($r.Code -ne 0) { $p += "exit $($r.Code): $($r.Out -join ' | ')" }
    $tar = Join-Path $s.Dest 'image.tar'
    if (-not (Test-Path -LiteralPath $tar)) { $p += 'no image.tar' }
    elseif ((Get-Sha $tar) -ne $payloadSha) { $p += 'image.tar is not the artifact''s' }
    $p
}

# Starts S: the byte each blob request with a 206 began at, per range, in order.
function Get-Blob($s) { @(Get-StoreLog $s | Where-Object { $_.kind -eq 'blob' }) }
function Sum-Sent($reqs) { [long](($reqs | Where-Object { $_.code -eq 206 } | Measure-Object -Property sent -Sum).Sum) }

$part = [long][Math]::Ceiling($zipSize / 4)

try {
    # 1. Four ranges: each asked once, every byte sent once, the files unpacked.
    $s = Start-Store 'whole' @{}
    $r = Run-Fetch $s @('-Parts', '4')
    $p = @(Check-Whole $r $s)
    $reqs = Get-Blob $s
    if ($reqs.Count -ne 4) { $p += "$($reqs.Count) blob requests, want 4" }
    if ((Sum-Sent $reqs) -ne $zipSize) { $p += "$(Sum-Sent $reqs) bytes sent for $zipSize" }
    if (-not ($r.Out -match 'sha256 [0-9a-f]{64}, the artifact''s digest')) { $p += "no digest check said: $($r.Out -join ' | ')" }
    Report 'an artifact comes down as its ranges at once, every byte once, checked against its digest, and unpacked' $p

    # 2. A range that breaks off goes on from its last byte, not from its start.
    $s = Start-Store 'broken' @{ break_once_from = $part }
    $r = Run-Fetch $s @('-Parts', '4')
    $p = @(Check-Whole $r $s)
    $reqs = @(Get-Blob $s | Where-Object { $_.end -eq (2 * $part - 1) })
    if ($reqs.Count -ne 2) { $p += "the broken range was asked $($reqs.Count) time(s), want 2" }
    elseif ($reqs[1].start -ne $reqs[0].start + $reqs[0].sent) { $p += "its second request began at $($reqs[1].start), not at $($reqs[0].start + $reqs[0].sent) where the first stopped" }
    if ((Sum-Sent (Get-Blob $s)) -ne $zipSize) { $p += "$(Sum-Sent (Get-Blob $s)) bytes sent for ${zipSize}: some twice" }
    Report 'a range that breaks off goes on from its last byte, and no byte comes twice' $p

    # 3. The signed URL runs out mid-download (each is good for a third of the
    # file, then cut off and refused): fresh URLs from the API, every range
    # going on from its last byte, the whole never started again.
    $s = Start-Store 'expiring' @{ expire_after_bytes = [long][Math]::Floor($zipSize / 3) }
    $r = Run-Fetch $s @('-Parts', '4')
    $p = @(Check-Whole $r $s)
    $log = Get-StoreLog $s
    $signs = @($log | Where-Object { $_.kind -eq 'sign' }).Count
    $refused = @($log | Where-Object { $_.kind -eq 'blob' -and $_.code -eq 403 }).Count
    if ($signs -lt 3) { $p += "$signs signed URL(s), want 3 or more (each covers a third)" }
    if ($refused -lt 1) { $p += 'no request was refused: the URL never ran out' }
    if ((Sum-Sent (Get-Blob $s)) -ne $zipSize) { $p += "$(Sum-Sent (Get-Blob $s)) bytes sent for ${zipSize}: some came twice" }
    foreach ($e in @(0..3)) {
        $mine = @(Get-Blob $s | Where-Object { $_.end -eq [Math]::Min($zipSize, ($e + 1) * $part) - 1 -and $_.code -eq 206 })
        for ($i = 1; $i -lt $mine.Count; $i++) {
            if ($mine[$i].start -ne $mine[$i - 1].start + $mine[$i - 1].sent) { $p += "range $e restarted at $($mine[$i].start), not where it stopped" }
        }
    }
    Report 'a signed URL that runs out mid-download is replaced from the API, and each range goes on from its last byte' $p

    # 4. A transfer that stalls is given up on (here: under 1 MB/s for 1 s) and
    # goes on from its last byte.
    $s = Start-Store 'stall' @{ stall_once_from = 2 * $part }
    $r = Run-Fetch $s @('-Parts', '4', '-StallSeconds', '1', '-StallBytesPerSecond', '1000000')
    $p = @(Check-Whole $r $s)
    $reqs = @(Get-Blob $s | Where-Object { $_.end -eq (3 * $part - 1) })
    if ($reqs.Count -ne 2) { $p += "the stalled range was asked $($reqs.Count) time(s), want 2" }
    elseif ($reqs[1].start -ne $reqs[0].start + $reqs[0].sent) { $p += "its second request began at $($reqs[1].start), not at $($reqs[0].start + $reqs[0].sent)" }
    Report 'a stalled transfer is given up on by its progress, and goes on from its last byte' $p

    # 5. A download whose bytes are not the digest's is downloaded once more,
    # from a fresh URL, and used only then.
    $s = Start-Store 'corrupt-once' @{ corrupt_tokens = @(1) }
    $r = Run-Fetch $s @('-Parts', '4')
    $p = @(Check-Whole $r $s)
    if (-not ($r.Out -match 'not its digest .*: downloading it once more')) { $p += "no second download said: $($r.Out -join ' | ')" }
    if (@(Get-StoreLog $s | Where-Object { $_.kind -eq 'sign' }).Count -ne 2) { $p += 'the second download did not take a fresh URL' }
    Report 'a download that does not match its digest is downloaded once more, from a fresh URL' $p

    # 6. Twice not the digest: an error naming the artifact, exit 3, nothing
    # unpacked.
    $s = Start-Store 'corrupt-always' @{ corrupt_tokens = 'all' }
    $r = Run-Fetch $s @('-Parts', '4')
    $p = @()
    if ($r.Code -ne 3) { $p += "exit $($r.Code), want 3" }
    if (-not ($r.Out -match '::error::.*docker-image-linux-amd64.*downloaded twice')) { $p += "no error naming the artifact: $($r.Out -join ' | ')" }
    if (Test-Path -LiteralPath (Join-Path $s.Dest 'image.tar')) { $p += 'unpacked bytes that are not the digest''s' }
    Report 'a download twice not its digest fails loudly (exit 3), naming the artifact, and unpacks nothing' $p

    # 7. A store that does not serve ranges: exit 1, for the caller's own
    # download, and said.
    $s = Start-Store 'no-ranges' @{ ignore_range = $true }
    $r = Run-Fetch $s @('-Parts', '4')
    $p = @()
    if ($r.Code -ne 1) { $p += "exit $($r.Code), want 1" }
    if (-not ($r.Out -match 'does not serve ranges')) { $p += "not said: $($r.Out -join ' | ')" }
    Report 'a store that answers a range with the whole file is a miss (exit 1), said' $p

    # 8. A range that never gets a byte: exit 1 after that many attempts in a
    # row, each said.
    $s = Start-Store 'dead-range' @{ fail_always_from = @($part) }
    $r = Run-Fetch $s @('-Parts', '4', '-MaxAttemptsWithoutProgress', '2')
    $p = @()
    if ($r.Code -ne 1) { $p += "exit $($r.Code), want 1" }
    if (-not ($r.Out -match 'got nothing in 2 attempts in a row')) { $p += "not said: $($r.Out -join ' | ')" }
    if (@(Get-Blob $s | Where-Object { $_.start -eq $part }).Count -ne 2) { $p += 'the dead range was not asked exactly twice' }
    Report 'a range that gets nothing in -MaxAttemptsWithoutProgress attempts in a row fails the download (exit 1)' $p

    # 9. No artifact by that name, a token the API refuses, no token at all.
    $p = @()
    $s = Start-Store 'none' @{ no_artifact = $true }
    $r = Run-Fetch $s @()
    if ($r.Code -ne 1 -or -not ($r.Out -match 'no artifact named')) { $p += "no artifact: exit $($r.Code): $($r.Out -join ' | ')" }
    $s = Start-Store 'bad-token' @{}
    $r = Run-Fetch $s @() 'not-the-token'
    if ($r.Code -ne 1 -or -not ($r.Out -match 'HTTP 401')) { $p += "a refused token: exit $($r.Code): $($r.Out -join ' | ')" }
    $r = Run-Fetch $s @() ''
    if ($r.Code -ne 2) { $p += "no token: exit $($r.Code), want 2" }
    Report 'no such artifact, or a refused token, is a miss (exit 1); no token is a wrong call (exit 2)' $p
} finally {
    foreach ($p in $stores) { if (-not $p.HasExited) { try { $p.Kill() } catch { }; $p.WaitForExit() } }
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}

if ($failures -gt 0) { Write-Host "$failures failure(s)"; exit 1 }
Write-Host 'all fetch-artifact.ps1 cases pass'
exit 0

# check-windows-release-start.ps1 -Binary KEPLOY -PgTest PGTEST -Probe PROBE
#
# Starts the windows release binary the way it starts on a machine without
# MinGW, before it is published.
#
# Why: v3.6.216 and the releases before it imported libwinpthread-1.dll, a
# MinGW DLL Windows does not ship. The windows runners have MinGW on their
# PATH, so keploy started on every one of them, and exited before main
# (STATUS_DLL_NOT_FOUND) on a machine without it. Each program here is copied
# alone into a new empty directory and started there with PATH set to
# Windows' own directories only, so the loader can find a DLL only if Windows
# ships it:
#
#   * PROBE (build-windows-release.sh probe: it imports libwinpthread-1.dll,
#     as v3.6.216 did) must be refused with STATUS_DLL_NOT_FOUND. If it
#     starts, this machine has that DLL where Windows looks for system DLLs,
#     so a start here proves nothing, and the check fails;
#   * the release binary's `--version` exits 0 and prints a Keploy version;
#   * PGTEST, the Postgres parser's own tests (libpg_query, the C code that
#     turned cgo on) built by `build-windows-release.sh pgtest`, so linked as
#     the release is, pass: a real parse path.
param(
    [Parameter(Mandatory = $true)][string]$Binary,
    [Parameter(Mandatory = $true)][string]$PgTest,
    [Parameter(Mandatory = $true)][string]$Probe
)
$ErrorActionPreference = 'Stop'

# STATUS_DLL_NOT_FOUND (0xC0000135): the exit code of a process whose
# imports the loader cannot resolve.
$DllNotFound = [int32]-1073741515
$sys = $env:SystemRoot
$cleanPath = "$sys\System32;$sys;$sys\System32\Wbem"

function Start-Clean([string]$Exe, [string[]]$Arguments) {
    $dir = Join-Path ([System.IO.Path]::GetTempPath()) ("keploy-start-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $dir | Out-Null
    try {
        $copy = Join-Path $dir ([System.IO.Path]::GetFileName($Exe))
        if (-not $copy.EndsWith('.exe')) { $copy += '.exe' }
        Copy-Item -LiteralPath $Exe -Destination $copy
        $psi = New-Object System.Diagnostics.ProcessStartInfo
        $psi.FileName = $copy
        foreach ($a in $Arguments) { [void]$psi.ArgumentList.Add($a) }
        $psi.WorkingDirectory = $dir
        $psi.UseShellExecute = $false
        $psi.RedirectStandardOutput = $true
        $psi.RedirectStandardError = $true
        $psi.Environment['PATH'] = $cleanPath
        $p = [System.Diagnostics.Process]::Start($psi)
        $out = $p.StandardOutput.ReadToEndAsync()
        $err = $p.StandardError.ReadToEndAsync()
        if (-not $p.WaitForExit(300000)) {
            $p.Kill($true)
            throw "check-windows-release-start: $Exe did not exit within 300 s"
        }
        $p.WaitForExit()
        return [pscustomobject]@{ Code = [int32]$p.ExitCode; Output = ($out.Result + $err.Result) }
    } finally {
        Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

function Hex([int32]$c) { '0x{0:X8}' -f $c }
function Tail([string]$s, [int]$n) { ($s -split "`r?`n" | Where-Object { $_ -ne '' } | Select-Object -Last $n) -join "`n" }

$failed = @()
Write-Host "check-windows-release-start: PATH for every start: $cleanPath"

$r = Start-Clean $Probe @()
if ($r.Code -ne $DllNotFound) {
    $failed += "the probe, which imports libwinpthread-1.dll, exited $($r.Code) ($(Hex $r.Code)), not STATUS_DLL_NOT_FOUND: this machine finds that DLL with no MinGW on the PATH, so a start here proves nothing"
} else {
    Write-Host "probe (imports libwinpthread-1.dll): refused, STATUS_DLL_NOT_FOUND, as v3.6.216 was"
}

$r = Start-Clean $Binary @('--version')
Write-Host (Tail $r.Output 3)
if ($r.Code -eq $DllNotFound) {
    $failed += "$Binary --version: STATUS_DLL_NOT_FOUND, it imports a DLL Windows does not ship (check-windows-release-binary.sh names it)"
} elseif ($r.Code -ne 0) {
    $failed += "$Binary --version exited $($r.Code) ($(Hex $r.Code))"
} elseif ($r.Output -notmatch 'Keploy ') {
    $failed += "$Binary --version printed no Keploy version"
} else {
    Write-Host "$Binary --version: ok"
}

$r = Start-Clean $PgTest @('-test.count=1')
Write-Host (Tail $r.Output 3)
if ($r.Code -ne 0) {
    $failed += "the Postgres parser tests exited $($r.Code) ($(Hex $r.Code)):`n$(Tail $r.Output 40)"
} else {
    Write-Host "the Postgres parser tests: ok"
}

if ($failed.Count -gt 0) {
    foreach ($f in $failed) { Write-Host "check-windows-release-start: FAIL: $f" }
    exit 1
}
Write-Host "check-windows-release-start: the windows release binary starts, and the Postgres parser passes, with only Windows' own DLLs"

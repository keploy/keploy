# check-windows-release-start.tests.ps1 - check-windows-release-start.ps1
# passes a release-shaped set of programs and fails, naming why, for each way
# one can be wrong: a probe that starts, a binary that needs a DLL Windows does
# not ship, exits non-zero, prints no version or hangs, and a parser test run
# that fails.
#
# Windows only. Builds small C programs with llvm-mingw (KEPLOY_LLVM_MINGW, as
# llvm-mingw-toolchain.sh stages it): each is linked -static, so it needs only
# Windows' DLLs, but the probe, which imports libwinpthread-1.dll as
# v3.6.216 did.
$ErrorActionPreference = 'Stop'

if (-not $env:KEPLOY_LLVM_MINGW) { throw 'KEPLOY_LLVM_MINGW must name the pinned llvm-mingw (.github/scripts/windows/llvm-mingw-toolchain.sh)' }
$check = Join-Path $PSScriptRoot 'check-windows-release-start.ps1'
$clang = Join-Path $env:KEPLOY_LLVM_MINGW 'bin\x86_64-w64-mingw32-clang.exe'
$work = Join-Path ([System.IO.Path]::GetTempPath()) ("keploy-start-tests-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null

function Build([string]$Name, [string]$Source, [string[]]$Flags) {
    $c = Join-Path $work "$Name.c"
    Set-Content -LiteralPath $c -Value $Source -Encoding ascii
    & $clang -O1 -o (Join-Path $work "$Name.exe") $c @Flags
    if ($LASTEXITCODE -ne 0) { throw "building $Name exited $LASTEXITCODE" }
}

$failures = 0
# Row NAME RC WANT ARGS: run the check with ARGS, want exit RC and WANT in
# what it printed.
function Row([string]$Name, [int]$WantRc, [string]$Want, [string[]]$CheckArgs) {
    $out = (& pwsh -NoLogo -NoProfile -File $check @CheckArgs 2>&1 | Out-String)
    $rc = $LASTEXITCODE
    if ($rc -ne $WantRc) {
        Write-Host "FAIL ${Name}: exit $rc, want ${WantRc}:`n$out"; $script:failures++
    } elseif (-not $out.Contains($Want)) {
        Write-Host "FAIL ${Name}: printed:`n$out`nwant '$Want'"; $script:failures++
    } else {
        Write-Host "ok   ${Name}: $Want"
    }
}

try {
    $static = @('-static', '-s')
    Build 'version' "#include <stdio.h>`nint main(void) { puts(`"Keploy 1.2.3`"); return 0; }" $static
    Build 'noversion' "#include <stdio.h>`nint main(void) { puts(`"hello`"); return 0; }" $static
    Build 'pass' "int main(void) { return 0; }" $static
    Build 'exit3' "int main(void) { return 3; }" $static
    Build 'hang' "#include <windows.h>`nint main(void) { Sleep(INFINITE); return 0; }" $static
    # Not -static: the thread library comes from libwinpthread-1.dll.
    Build 'probe' "#include <pthread.h>`n#include <stdio.h>`nint main(void) { printf(`"probe %lu\n`", (unsigned long)pthread_self()); return 0; }" @('-lwinpthread')
    # The release binary's name has no .exe on some paths; the check names
    # its copy .exe so Windows starts it.
    Copy-Item -LiteralPath (Join-Path $work 'version.exe') -Destination (Join-Path $work 'keploy')
    # A probe with libwinpthread-1.dll beside it (llvm-mingw's own copy): the
    # check starts a copy of it alone in an empty directory, so it is still
    # refused.
    $beside = Join-Path $work 'beside'
    New-Item -ItemType Directory -Path $beside | Out-Null
    Copy-Item -LiteralPath (Join-Path $work 'probe.exe') -Destination $beside
    Copy-Item -LiteralPath (Join-Path $env:KEPLOY_LLVM_MINGW 'x86_64-w64-mingw32\bin\libwinpthread-1.dll') -Destination $beside

    $w = { param($n) Join-Path $work $n }
    $good = @('-Binary', (& $w 'version.exe'), '-PgTest', (& $w 'pass.exe'), '-Probe', (& $w 'probe.exe'))

    Row 'all-good' 0 'the windows release binary starts, and the Postgres parser passes' $good
    Row 'dll-beside-probe' 0 'the windows release binary starts' @('-Binary', (& $w 'version.exe'), '-PgTest', (& $w 'pass.exe'), '-Probe', (Join-Path $beside 'probe.exe'))
    Row 'no-exe-suffix' 0 'the windows release binary starts' @('-Binary', (& $w 'keploy'), '-PgTest', (& $w 'pass.exe'), '-Probe', (& $w 'probe.exe'))
    Row 'probe-starts' 1 'the probe, which imports libwinpthread-1.dll, exited 0 (0x00000000), not STATUS_DLL_NOT_FOUND' @('-Binary', (& $w 'version.exe'), '-PgTest', (& $w 'pass.exe'), '-Probe', (& $w 'pass.exe'))
    Row 'binary-needs-dll' 1 'STATUS_DLL_NOT_FOUND, it imports a DLL Windows does not ship' @('-Binary', (& $w 'probe.exe'), '-PgTest', (& $w 'pass.exe'), '-Probe', (& $w 'probe.exe'))
    Row 'binary-exits-3' 1 'exe --version exited 3 (0x00000003)' @('-Binary', (& $w 'exit3.exe'), '-PgTest', (& $w 'pass.exe'), '-Probe', (& $w 'probe.exe'))
    Row 'binary-no-version' 1 'exe --version printed no Keploy version' @('-Binary', (& $w 'noversion.exe'), '-PgTest', (& $w 'pass.exe'), '-Probe', (& $w 'probe.exe'))
    Row 'pgtest-fails' 1 'the Postgres parser tests exited 3 (0x00000003)' @('-Binary', (& $w 'version.exe'), '-PgTest', (& $w 'exit3.exe'), '-Probe', (& $w 'probe.exe'))
    Row 'pgtest-needs-dll' 1 'the Postgres parser tests exited -1073741515 (0xC0000135)' @('-Binary', (& $w 'version.exe'), '-PgTest', (& $w 'probe.exe'), '-Probe', (& $w 'probe.exe'))
    Row 'binary-hangs' 1 'hang.exe --version did not exit within 5 s' @('-Binary', (& $w 'hang.exe'), '-PgTest', (& $w 'pass.exe'), '-Probe', (& $w 'probe.exe'), '-TimeoutSeconds', '5')
    Row 'probe-hangs' 1 'the probe did not exit within 5 s' @('-Binary', (& $w 'version.exe'), '-PgTest', (& $w 'pass.exe'), '-Probe', (& $w 'hang.exe'), '-TimeoutSeconds', '5')
    Row 'pgtest-hangs' 1 'the Postgres parser tests did not exit within 5 s' @('-Binary', (& $w 'version.exe'), '-PgTest', (& $w 'hang.exe'), '-Probe', (& $w 'probe.exe'), '-TimeoutSeconds', '5')

    # Not a row: whether this runner finds libwinpthread-1.dll on its own
    # PATH, which is what makes all-good's refused probe show the PATH was
    # cut (the hosted image has MinGW on it).
    $p = Start-Process -FilePath (& $w 'probe.exe') -NoNewWindow -PassThru
    if ($p.WaitForExit(60000)) {
        Write-Host "note: the probe with this runner's own PATH exited $($p.ExitCode)"
    } else {
        $p.Kill($true)
        Write-Host "note: the probe with this runner's own PATH did not exit within 60 s"
    }
} finally {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}

if ($failures -ne 0) {
    Write-Host "check-windows-release-start.tests: $failures FAILED"
    exit 1
}
Write-Host 'check-windows-release-start.tests: all passed'

# Put a MinGW-w64 gcc on PATH for the native windows/amd64 CGO build.
#
# keploy's Windows binary needs a real cgo link: pkg/agent/hooks/windows
# carries `#cgo windows LDFLAGS: -l:libwindows_redirector.a ...`, so
# `go build` fails at the link stage without gcc. That failure surfaces
# as a cryptic "cannot find -l..." several minutes in, which is why this
# resolves and verifies the toolchain up front instead.
#
# The runner image installs MinGW-w64 to C:\mingw64 and adds
# C:\mingw64\bin to the machine PATH (actions/runner-images,
# images/windows/scripts/build/Install-Mingw64.ps1), so that is the
# candidate that resolves on a current image. The others are defensive,
# covering older image generations and an MSYS2-provided toolchain.
#
# ONE copy, used by both Windows CGO build lanes — prepare_and_run.yml's
# build-windows-amd64 (PR builds) and release.yml's build-windows (the
# binary users download). They previously carried inline copies that had
# genuinely DIVERGED:
#
#   - release.yml probed three paths, appended EVERY match with no
#     `break`, and had no failure branch. Because GITHUB_PATH entries are
#     prepended, appending every match means the LAST match wins — the
#     inverse of the listed preference.
#   - prepare_and_run.yml (self-hosted) probed five paths, took the first
#     hit, fell back to `Get-Command gcc`, and downloaded a portable
#     WinLibs toolchain if nothing was found — all of which existed to
#     cope with unprovisioned fleet machines.
#
# So the two lanes could select different toolchains for the same build.
# The self-hosted-only recovery is dropped deliberately: on a hosted
# image MinGW is provisioned, and a `Get-Command gcc` fallback would be
# actively wrong there because the image also ships Strawberry Perl's gcc
# at C:\Strawberry\c\bin on the machine PATH. Explicitly prepending
# C:\mingw64\bin is what keeps the toolchain deterministic.

$ErrorActionPreference = 'Stop'

$candidates = @(
    'C:\mingw64\bin',
    'C:\ProgramData\mingw64\mingw64\bin',
    'C:\msys64\mingw64\bin'
)

# `break` + `throw` rather than `exit`: both would fail the step (the
# runner invokes `pwsh -command ". '<step>'"` with $ErrorActionPreference
# set to 'stop' and `exit $LASTEXITCODE` appended), so this is a
# readability choice, not a correctness one — one exit path, and the
# script stays usable when dot-sourced or run outside Actions.
$resolved = $null
foreach ($c in $candidates) {
    if (Test-Path (Join-Path $c 'gcc.exe')) {
        $resolved = $c
        # First hit wins, deterministically. Without stopping here, a
        # runner image carrying two toolchains would put both on PATH,
        # and because GITHUB_PATH entries are prepended the LAST match
        # would win — inverting the preference expressed above.
        break
    }
}

if (-not $resolved) {
    # Fail here, naming what was searched, rather than letting the next
    # step die on a bare "gcc: command not found". If a future runner
    # image moves the toolchain, this says exactly what to fix.
    throw ("No MinGW-w64 gcc.exe found. Searched: {0}. " -f ($candidates -join ', ')) +
          'The runner image is expected to ship MinGW-w64 (actions/runner-images ' +
          'Install-Mingw64.ps1). If the image moved it, add the new path to ' +
          '.github/scripts/windows/ensure-mingw.ps1.'
}

# -Encoding utf8 matches every other GITHUB_PATH/GITHUB_ENV write in this
# repo. Without it the encoding depends on the host shell (Windows
# PowerShell 5.1 defaults to ASCII here, pwsh 7 to UTF-8), and this
# script is called from both lanes.
Add-Content -Path $env:GITHUB_PATH -Value $resolved -Encoding utf8
Write-Host "Using MinGW at $resolved"

# Dot-sourced first by every script in this directory that acts on what the
# runners of the shared self-hosted Windows machine have in common: the docker
# CLI and daemon, Docker Desktop, the lock directory
# (%USERPROFILE%\.github-workflow-locks) and the wedge records
# (%USERPROFILE%\.keploy-docker-wedged). Those are each script's defaults.
#
# reap-keploy-containers.tests.ps1 runs on that machine too (precheck-windows),
# and sets KEPLOY_WINDOWS_SCRIPT_TESTS. A case there that left one of those
# parameters at its default would act on the real thing: remove or prune a
# sibling job's containers, restart Docker Desktop under it, put a marker down
# that stalls every job for up to 15 minutes, or delete a live lock it judges
# stale. So under the tests a script stops before doing anything unless each
# of them was passed, with a value.
function Assert-TestOverrides {
    param(
        [string]$Script,
        $Bound,
        [string[]]$Names
    )
    if (-not $env:KEPLOY_WINDOWS_SCRIPT_TESTS) { return }
    $unset = @($Names | Where-Object { -not ($Bound.ContainsKey($_) -and $Bound[$_]) })
    if ($unset.Count -gt 0) {
        throw "$Script run by the tests (KEPLOY_WINDOWS_SCRIPT_TESTS) without -$($unset -join ', -'): the default acts on what every runner of the shared machine uses."
    }
}

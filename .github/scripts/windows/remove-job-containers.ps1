# golang_docker_windows' "Remove this job's containers": the owner's half of the
# container rule in reap-keploy-containers.ps1. It removes every container of
# the compose project this job recorded (register-job-compose-project.ps1), at
# any age, and then releases the job's Docker lock (take-docker-job-lock.ps1).
#
# If a container survives `docker rm -f`, the reaper records it, so the next
# job on any runner fails its pre-job reap at once instead of after the age
# threshold. That pre-job reap is where a wedge fails a job: it retries the
# removal with a fresh settle window and blocks new work. Here a wedge is only
# a warning, because this job's result is already known. A removal that is
# merely slow on a loaded VM would otherwise turn a green job red with advice
# to restart the VM.
#
# With no project recorded, nothing can be removed by project. A job that got
# far enough to start containers (its Docker lock was taken and the step that
# starts them ran) should always have recorded one, so there it is a warning:
# the containers it may have started are left to the age sweep, and a wedge
# they hit goes unnoticed for that long.
[CmdletBinding()]
param(
    [string]$ComposeProject = $env:KEPLOY_JOB_COMPOSE_PROJECT,
    [string]$JobLock = $env:KEPLOY_DOCKER_JOB_LOCK,
    # The outcome of the step that starts the containers (steps.<id>.outcome),
    # when the workflow passes it. 'skipped' means it never ran.
    [string]$StartStepOutcome = $env:KEPLOY_START_STEP_OUTCOME,
    # Only the tests override these.
    [string]$DockerExe = 'docker',
    [string]$WedgeDir = ''
)
$ErrorActionPreference = 'Continue'

$reaper = Join-Path $PSScriptRoot 'reap-keploy-containers.ps1'
$code = 0
if ($ComposeProject -and -not (Test-Path -LiteralPath $reaper)) {
    Write-Host "::warning::$reaper not found; leaving project $ComposeProject to the age sweep."
} elseif ($ComposeProject) {
    $reapArgs = @{ ComposeProject = $ComposeProject; DockerExe = $DockerExe }
    if ($WedgeDir) { $reapArgs.WedgeDir = $WedgeDir }
    & $reaper @reapArgs
    $code = $LASTEXITCODE
} elseif ($JobLock -and ($StartStepOutcome -ne 'skipped')) {
    Write-Host ("::warning::This job took its Docker lock but recorded no compose project (KEPLOY_JOB_COMPOSE_PROJECT " +
        "is not set), so none of its containers can be removed by project. Any container it started is left to the " +
        "age sweep (reap-keploy-containers.ps1 -MinAgeMinutes), and a wedged Docker VM that one of them hit is found " +
        "only then. The step that starts the containers must call register-job-compose-project.ps1 before starting any.")
} elseif ($JobLock) {
    Write-Host "This job recorded no compose project; the step that starts its containers did not run."
} else {
    Write-Host "This job recorded no compose project and never took its Docker lock."
}
if ($JobLock) { Remove-Item -LiteralPath $JobLock -Force -ErrorAction SilentlyContinue }
exit $code

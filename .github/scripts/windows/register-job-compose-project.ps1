# Records the compose project a golang_docker_windows job starts its containers
# under, for the job's "Remove this job's containers" step
# (remove-job-containers.ps1). Call it BEFORE the first container starts.
#
# Every container the job starts, both the application and the keploy agent,
# carries this project's label. So at teardown the job removes exactly its own
# containers, at any age, and a wedged agent is recorded for the very next job
# to find. Without the record, the containers are left to the age sweep and a
# wedge is found only after MinAgeMinutes. remove-job-containers.ps1 warns when
# that happens.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Project
)
$env:COMPOSE_PROJECT_NAME = $Project
$env:KEPLOY_JOB_COMPOSE_PROJECT = $Project
if ($env:GITHUB_ENV) {
    "KEPLOY_JOB_COMPOSE_PROJECT=$Project" | Out-File -FilePath $env:GITHUB_ENV -Encoding utf8 -Append
}
Write-Host "Using COMPOSE_PROJECT_NAME = $Project"

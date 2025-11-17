# Ensure Docker is running on Windows. This script mirrors the inline logic from the workflow.
$ErrorActionPreference = 'Stop'

function Test-Docker {
  # Check if Docker CLI exists
  $dockerCmd = Get-Command docker -ErrorAction SilentlyContinue
  if (-not $dockerCmd) { return $false }

  try {
    # Use Start-Process to avoid NativeCommandError
    $proc = Start-Process -FilePath $dockerCmd.Source -ArgumentList @('info','--format','{{.ServerVersion}}') -NoNewWindow -PassThru -Wait -ErrorAction SilentlyContinue
    if ($proc -and $proc.ExitCode -eq 0) { return $true }
    return $false
  } catch {
    return $false
  }
}

function Test-DockerDesktopProcess {
  $process = Get-Process -Name "Docker Desktop" -ErrorAction SilentlyContinue
  return $process -ne $null
}

if (Test-Docker) {
  Write-Host "✅ Docker engine is already running."
} else {
  if (Test-DockerDesktopProcess) {
    Write-Host "⏳ Docker Desktop process found but engine not ready. Waiting..."
  } else {
    Write-Host "🚀 Starting Docker Desktop..."
    Start-Process "C:\Program Files\Docker\Docker\Docker Desktop.exe" -WindowStyle Hidden
  }

  # Wait for Docker to become ready
  $timeout = (Get-Date).AddSeconds(90)
  while (-not (Test-Docker)) {
    if ((Get-Date) -gt $timeout) {
      Write-Error "❌ Docker did not start within 90 seconds."
      exit 1
    }
    Start-Sleep -Seconds 3
  }

  Write-Host "✅ Docker Desktop is now running and engine is ready."
  Write-Host "Waiting an additional 60 seconds for Docker to stabilize..."
  Start-Sleep -Seconds 60
}

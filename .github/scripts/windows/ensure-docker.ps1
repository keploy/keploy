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

function Test-DockerHealthy {
  try {
    # Try a simple operation to verify Docker is actually healthy
    $result = & docker ps 2>&1
    return $LASTEXITCODE -eq 0
  } catch {
    return $false
  }
}

function Test-DockerDesktopProcess {
  $process = Get-Process -Name "Docker Desktop" -ErrorAction SilentlyContinue
  return $process -ne $null
}

function Restart-DockerDesktop {
  Write-Host "🔄 Restarting Docker Desktop..."
  # Stop Docker Desktop gracefully
  Get-Process -Name "Docker Desktop" -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
  Get-Process -Name "com.docker*" -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
  Start-Sleep -Seconds 5
  
  # Start Docker Desktop
  Start-Process "C:\Program Files\Docker\Docker\Docker Desktop.exe" -WindowStyle Hidden
}

# First check if Docker is running and healthy
if (Test-Docker) {
  if (Test-DockerHealthy) {
    Write-Host "✅ Docker engine is already running and healthy."
    exit 0
  } else {
    Write-Host "⚠️ Docker engine is running but not healthy. Restarting..."
    Restart-DockerDesktop
  }
} else {
  if (Test-DockerDesktopProcess) {
    Write-Host "⏳ Docker Desktop process found but engine not ready. Restarting..."
    Restart-DockerDesktop
  } else {
    Write-Host "🚀 Starting Docker Desktop..."
    Start-Process "C:\Program Files\Docker\Docker\Docker Desktop.exe" -WindowStyle Hidden
  }
}

# Wait for Docker to become ready
$timeout = (Get-Date).AddSeconds(120)
while (-not (Test-Docker) -or -not (Test-DockerHealthy)) {
  if ((Get-Date) -gt $timeout) {
    Write-Error "❌ Docker did not start within 120 seconds."
    exit 1
  }
  Write-Host "⏳ Waiting for Docker to be ready..."
  Start-Sleep -Seconds 5
}

Write-Host "✅ Docker Desktop is now running and engine is ready."
Write-Host "Waiting an additional 30 seconds for Docker to stabilize..."
Start-Sleep -Seconds 30

# Final health check
if (-not (Test-DockerHealthy)) {
  Write-Error "❌ Docker is still not healthy after startup."
  exit 1
}

Write-Host "✅ Docker is fully ready."

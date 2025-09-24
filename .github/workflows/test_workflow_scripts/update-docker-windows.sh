#!/usr/bin/env bash
set -Eeuo pipefail

emit_step_output() {
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    echo "$1=$2" >> "$GITHUB_OUTPUT"
  fi
}

build_linux_image() {
  echo "Linux engine detected; building Linux image from existing Dockerfile..."
  DOCKER_BUILDKIT=1 docker build --ssh default -t ttl.sh/keploy/keploy:1h .
  emit_step_output built true
  emit_step_output image ttl.sh/keploy/keploy:1h
}

build_windows_image() {
  echo "Windows containers engine detected; generating ephemeral Windows Dockerfile (no DinD)..."
  cat > Dockerfile.win <<'EOF'
# escape=`
# --- Build stage (Windows) ---
FROM golang:1.24-windowsservercore-ltsc2022 AS build
SHELL ["powershell", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'"]
WORKDIR C:\app

# Copy mod files and download deps
COPY go.mod go.sum C:\app\
RUN go mod download

# Copy the source and build keploy.exe
COPY . C:\app
# Equivalent build to your Linux stage (minus ldflags you inject in Linux runtime)
RUN go build -tags=viper_bind_struct -o C:\app\keploy.exe .

# --- Runtime stage (Windows) ---
FROM mcr.microsoft.com/windows/nanoserver:ltsc2022
WORKDIR C:\app
COPY --from=build C:\app\keploy.exe C:\app\keploy.exe
# NOTE: No Docker Engine inside this image and no entrypoint.sh (Windows containers don't support the same DinD flow)
ENTRYPOINT ["C:\\app\\keploy.exe"]
EOF

  docker build -f Dockerfile.win -t ttl.sh/keploy/keploy-win:1h .
  emit_step_output built true
  emit_step_output image ttl.sh/keploy/keploy-win:1h
}

main() {
  if ! command -v docker >/dev/null 2>&1; then
    echo "Docker not found; skipping."
    emit_step_output built false
    return 0
  fi

  osType="$(docker info --format '{{.OSType}}' 2>/dev/null || echo unknown)"
  echo "Docker engine OSType: $osType"

  case "$osType" in
    linux)
      build_linux_image
      ;;
    windows)
      build_windows_image
      ;;
    *)
      echo "Unknown Docker engine OSType ($osType); skipping."
      emit_step_output built false
      ;;
  esac
}

main

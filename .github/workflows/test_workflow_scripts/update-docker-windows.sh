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
  echo "Windows containers engine detected; generating ephemeral Windows Dockerfile with SSH mount..."
  cat > Dockerfile.win <<'EOF'
# syntax=docker/dockerfile:1.6
# escape=`
# --- Build stage (Windows) ---
FROM golang:1.24-windowsservercore-ltsc2022 AS build
SHELL ["powershell", "-NoLogo", "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue';"]
WORKDIR C:\app

# Ensure Go uses SSH for private GitHub modules and doesn't prompt for host key
ENV GOPRIVATE=github.com/keploy/*
ENV GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=no"

# Copy mod files first to leverage layer caching
COPY go.mod go.sum C:\app\

# Rewrite https->ssh for GitHub and download deps using the SSH agent provided by BuildKit
RUN git config --global url."ssh://git@github.com/".insteadOf "https://github.com/"
RUN --mount=type=ssh go mod download

# Copy the rest and build (again using SSH in case more private deps are referenced)
COPY . C:\app
RUN --mount=type=ssh go build -tags=viper_bind_struct -o C:\app\keploy.exe .

# --- Runtime stage (Windows) ---
FROM mcr.microsoft.com/windows/nanoserver:ltsc2022
WORKDIR C:\app
COPY --from=build C:\app\keploy.exe C:\app\keploy.exe
ENTRYPOINT ["C:\\app\\keploy.exe"]
EOF

  # MUST use BuildKit and forward the host's SSH agent
  DOCKER_BUILDKIT=1 docker build -f Dockerfile.win --ssh default -t ttl.sh/keploy/keploy-win:1h .
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

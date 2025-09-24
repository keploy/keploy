#!/usr/bin/env bash
set -Eeuo pipefail

emit_step_output() {
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    echo "$1=$2" >> "$GITHUB_OUTPUT"
  fi
}

build_linux_image() {
  echo "Linux engine detected; building Linux image from existing Dockerfile..."
  DOCKER_BUILDKIT=1 docker build -t ttl.sh/keploy/keploy:1h .
  emit_step_output built true
  emit_step_output image ttl.sh/keploy/keploy:1h
}

build_windows_runtime_from_binary() {
  echo "Windows containers engine detected; building runtime-only image from keploy.exe..."

  # try to find keploy.exe anywhere if not at repo root
  if [ ! -f "keploy.exe" ]; then
    shopt -s nullglob globstar
    cand=(**/keploy.exe)
    if [ ${#cand[@]} -gt 0 ]; then
      cp "${cand[0]}" ./keploy.exe
    fi
  fi

  if [ ! -f "keploy.exe" ]; then
    echo "ERROR: keploy.exe not found in workspace. Download the artifact before this step."
    emit_step_output built false
    return 0
  fi

  cat > Dockerfile.win.runtime <<'EOF'
# escape=`
FROM mcr.microsoft.com/windows/nanoserver:ltsc2022
WORKDIR C:\app
COPY keploy.exe C:\app\keploy.exe
ENTRYPOINT ["C:\\app\\keploy.exe"]
EOF

  docker build -f Dockerfile.win.runtime -t ttl.sh/keploy/keploy-win:1h .
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
    linux)   build_linux_image ;;
    windows) build_windows_runtime_from_binary ;;
    *)       echo "Unknown Docker engine OSType ($osType); skipping."
             emit_step_output built false ;;
  esac
}

main

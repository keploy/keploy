#!/bin/bash
set -Eeuo pipefail

DOCKERFILE_PATH="./Dockerfile"

# Create a temp file safely and atomically replace the original
rewrite_in_place() {
  local tmp
  tmp="$(mktemp)"
  cat > "$tmp"
  mv "$tmp" "$DOCKERFILE_PATH"
}

add_race_flag() {
  echo "Ensuring -race in go build lines (idempotent)..."
  awk '
    # If a line starts with RUN go build and does not already contain -race,
    # insert -race right after `go build`
    BEGIN { OFS="" }
    /^[[:space:]]*RUN[[:space:]]+go[[:space:]]+build(\>|[[:space:]])/ {
      if ($0 ~ /(^|\s)-race(\s|$)/) { print; next }
      sub(/RUN[[:space:]]+go[[:space:]]+build/, "RUN go build -race")
      print; next
    }
    { print }
  ' "$DOCKERFILE_PATH" | rewrite_in_place
}

build_docker_image() {
  echo "Building Docker image on Windows (without BuildKit SSH features)..."
  # On Windows, we use regular Docker build without BuildKit features
  # since buildx is not properly configured and SSH mounts aren't needed
  # (private repos are already handled by setup-private-parsers action)
  docker build -t ttl.sh/keploy/keploy:1h .
}

main() {
  add_race_flag
  build_docker_image
}

main

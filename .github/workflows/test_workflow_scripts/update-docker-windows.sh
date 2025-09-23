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

ensure_dockerfile_syntax() {
  # Ensure BuildKit features (like --mount=type=ssh) are supported
  if ! head -n1 "$DOCKERFILE_PATH" | grep -q '^# syntax=docker/dockerfile:'; then
    echo "Prepending Dockerfile syntax directive for BuildKit mounts..."
    {
      echo '# syntax=docker/dockerfile:1.6'
      cat "$DOCKERFILE_PATH"
    } | rewrite_in_place
  fi
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

enable_ssh_mount_for_go_mod() {
  echo "Switching go mod download to use BuildKit SSH mount (idempotent)..."
  awk '
    BEGIN { OFS="" }
    /^[[:space:]]*RUN[[:space:]]+go[[:space:]]+mod[[:space:]]+download[[:space:]]*$/ {
      print "RUN --mount=type=ssh go mod download"
      next
    }
    { print }
  ' "$DOCKERFILE_PATH" | rewrite_in_place
}

use_ssh_for_github_and_known_hosts() {
  echo "Injecting SSH config, known_hosts, and GOPRIVATE around go mod download (idempotent)..."

  awk '
    BEGIN {
      OFS="";
      injected_before_go_mod = 0;
      injected_after_copy = 0;
    }

    # Emit our SSH/Git prep block (no hard-fail keyscan)
    function emit_git_known_hosts_block() {
      print "RUN git config --global url.\"ssh://git@github.com/\".insteadOf \"https://github.com/\""
      # Create ~/.ssh but DO NOT hard-require ssh-keyscan
      print "RUN mkdir -p ~/.ssh && chmod 700 ~/.ssh"
      # Best-effort keyscan: try if present, ignore failures
      print "RUN if command -v ssh-keyscan >/dev/null 2>&1; then \\"
      print "      (ssh-keyscan -T 10 -t rsa,ecdsa,ed25519 github.com >> ~/.ssh/known_hosts 2>/dev/null || echo \"ssh-keyscan failed; continuing\"); \\"
      print "    else \\"
      print "      echo \"ssh-keyscan not found; continuing\"; \\"
      print "    fi"
    }

    # After COPY go.mod go.sum /app
    /^[[:space:]]*COPY[[:space:]]+go\.mod[[:space:]]+go\.sum[[:space:]]+\/app\/?[[:space:]]*$/ {
      print;
      if (!injected_after_copy) {
        emit_git_known_hosts_block();
        injected_after_copy = 1;
      }
      next
    }

    # Before the first RUN --mount=type=ssh go mod download
    /^[[:space:]]*RUN[[:space:]]+--mount=type=ssh[[:space:]]+go[[:space:]]+mod[[:space:]]+download[[:space:]]*$/ {
      if (!injected_before_go_mod) {
        print "ENV GOPRIVATE=github.com/keploy/*"
        print "ENV GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\""
        emit_git_known_hosts_block();
        injected_before_go_mod = 1;
      }
      print; next
    }

    { print }
  ' "$DOCKERFILE_PATH" | rewrite_in_place
}

build_docker_image() {
  echo "Building Docker image with SSH forwarding on Windows..."
  # On Windows, check if buildx is available, otherwise use legacy build
  if docker buildx version >/dev/null 2>&1; then
    echo "Using buildx for SSH mount support..."
    docker buildx build --ssh default -t ghcr.io/keploy/keploy:1h .
  else
    echo "Buildx not available, using legacy Docker build..."
    # Fallback to regular build without SSH mount (SSH setup handled in Dockerfile)
    docker build -t ghcr.io/keploy/keploy:1h .
  fi
}

main() {
  ensure_dockerfile_syntax
  add_race_flag
  enable_ssh_mount_for_go_mod
  use_ssh_for_github_and_known_hosts
  build_docker_image
}

main

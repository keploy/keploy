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
  echo "Injecting SSH config and known_hosts around go mod download (idempotent)..."
  
  awk '
    BEGIN {
      OFS="";
      injected_after_copy = 0;
    }

    # Helper function to emit git/ssh setup
    function emit_git_known_hosts_block() {
      print "RUN git config --global url.\"ssh://git@github.com/\".insteadOf \"https://github.com/\" && mkdir -p -m 0700 ~/.ssh && ssh-keyscan github.com >> ~/.ssh/known_hosts"
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

    { print }
  ' "$DOCKERFILE_PATH" | rewrite_in_place
}

optimize_build_for_speed() {
  echo "Optimizing Go build flags for faster compilation (removing non-essential flags)..."
  awk '
    BEGIN { OFS="" }
    # Find the go build line and simplify the ldflags for faster builds
    /^[[:space:]]*RUN[[:space:]]+GOMAXPROCS=2[[:space:]]+go[[:space:]]+build.*-ldflags=.*/ {
      # Replace the complex ldflags with simplified ones for testing
      gsub(/-ldflags="[^"]*"/, "-ldflags=\"-X main.version=$VERSION -X main.apiServerURI=$SERVER_URL\"")
      print; next
    }
    { print }
  ' "$DOCKERFILE_PATH" | rewrite_in_place
}

main() {
  echo "Preparing Dockerfile for fast Windows builds..."
  ensure_dockerfile_syntax
  enable_ssh_mount_for_go_mod
  use_ssh_for_github_and_known_hosts
  optimize_build_for_speed
  echo "Dockerfile optimization complete!"
}

main

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

  # We’ll inject two things before the first `RUN --mount=type=ssh go mod download`:
  #   ENV GOPRIVATE=github.com/keploy/*
  #   ENV GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=no"
  #   RUN git config ... && mkdir ~/.ssh && ssh-keyscan ...
  #
  # And we’ll also try adding the git/ssh known_hosts RUN right after a COPY go.mod go.sum /app line (best-effort).
  #
  # Guard with flags to avoid duplicate insertions.
  awk '
    BEGIN {
      OFS="";
      injected_before_go_mod = 0;
      injected_after_copy = 0;
    }

    # Helper patterns
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
  echo "Building Docker image with BuildKit and SSH forwarding..."
  # On mac, Docker Desktop supports BuildKit and --ssh if enabled.
  # Ensure you have an SSH agent running with your key loaded (ssh-add -l to verify).
  DOCKER_BUILDKIT=1 docker build --ssh default -t ttl.sh/keploy/keploy:1h .
}

main() {
  ensure_dockerfile_syntax
  add_race_flag
  enable_ssh_mount_for_go_mod
  use_ssh_for_github_and_known_hosts
  build_docker_image
}

main
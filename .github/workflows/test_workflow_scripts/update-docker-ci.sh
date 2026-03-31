#!/bin/bash
set -Eeuo pipefail

# Unified CI Docker image build script.
# Accepts the target platform as the first argument, defaults to linux/amd64 if not specified.
TARGET_PLATFORM=${1:-"linux/amd64"}
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
    BEGIN { OFS="" }
    # Match any RUN line that eventually invokes `go build`, even if env vars precede it
    /^[[:space:]]*RUN[[:space:]]+/ && $0 ~ /[[:space:]]go[[:space:]]+build/ {
      # If -race is already present as a standalone flag, leave the line unchanged.
      if ($0 ~ /(^|[[:space:]])-race([[:space:]]|$)/) { print; next }
      # Insert -race immediately after the first `go build` occurrence.
      sub(/[[:space:]]go[[:space:]]+build/, " go build -race")
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

    function emit_git_known_hosts_block() {
      print "RUN git config --global url.\"ssh://git@github.com/\".insteadOf \"https://github.com/\" && mkdir -p -m 0700 ~/.ssh && ssh-keyscan github.com >> ~/.ssh/known_hosts"
    }

    /^[[:space:]]*COPY[[:space:]]+go\.mod[[:space:]]+go\.sum[[:space:]]+\/app\/?[[:space:]]*$/ {
      print;
      if (!injected_after_copy) {
        emit_git_known_hosts_block();
        injected_after_copy = 1;
      }
      next
    }

    /^[[:space:]]*RUN[[:space:]]+--mount=type=ssh[[:space:]]+go[[:space:]]+mod[[:space:]]+download[[:space:]]*$/ {
      if (!injected_before_go_mod) {
        print "ENV GOPRIVATE=github.com/keploy/*"
        # Using StrictHostKeyChecking=yes as it's more secure, since we already do ssh-keyscan
        print "ENV GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=yes\""
        if (!injected_after_copy) {
          emit_git_known_hosts_block();
        }
        injected_before_go_mod = 1;
      }
      print; next
    }

    { print }
  ' "$DOCKERFILE_PATH" | rewrite_in_place
}

build_docker_image() {
  echo "Building Docker image for ${TARGET_PLATFORM} with BuildKit and SSH forwarding..."
  # Use buildx with --provenance=false to produce a clean single-platform image.
  docker buildx build --platform "${TARGET_PLATFORM}" --ssh default \
    --provenance=false --load -t ttl.sh/keploy/keploy:1h .
}

main() {
  ensure_dockerfile_syntax
  add_race_flag
  enable_ssh_mount_for_go_mod
  use_ssh_for_github_and_known_hosts
  build_docker_image
}

main

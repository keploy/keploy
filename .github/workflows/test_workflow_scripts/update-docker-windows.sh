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

  # Detect Windows base image (handles indentation, multi-stage, or explicit PowerShell shell)
  if grep -qiE '^[[:space:]]*FROM[[:space:]]+[^#]*\b(nanoserver|servercore|windows)\b' "$DOCKERFILE_PATH" \
     || grep -qiE '^[[:space:]]*SHELL[[:space:]]*\[.*powershell' "$DOCKERFILE_PATH"; then
    echo "Windows base detected; skipping --mount=type=ssh rewrite."
    return 0
  fi

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
  echo "Injecting Git SSH config and envs around go mod download (idempotent)..."

  # Detect Windows base image (handles indentation, multi-stage, or explicit PowerShell shell)
  if grep -qiE '^[[:space:]]*FROM[[:space:]]+[^#]*\b(nanoserver|servercore|windows)\b' "$DOCKERFILE_PATH" \
     || grep -qiE '^[[:space:]]*SHELL[[:space:]]*\[.*powershell' "$DOCKERFILE_PATH"; then
    WINDOWS_BASE=1
  else
    WINDOWS_BASE=0
  fi

  if [ "$WINDOWS_BASE" -eq 1 ]; then
    # WINDOWS: emit only safe lines (no mkdir/chmod/ssh-keyscan/&&)
    awk '
      BEGIN { OFS=""; injected_before_go_mod=0; injected_after_copy=0 }

      function emit_win_git_prep() {
        print "RUN git config --global url.\"ssh://git@github.com/\".insteadOf \"https://github.com/\""
        # rely on StrictHostKeyChecking=no so we don t need known_hosts or chmod/mkdir
      }

      /^[[:space:]]*COPY[[:space:]]+go\.mod[[:space:]]+go\.sum[[:space:]]+\/app\/?[[:space:]]*$/ {
        print;
        if (!injected_after_copy) {
          emit_win_git_prep();
          injected_after_copy=1;
        }
        next
      }

      /^[[:space:]]*RUN[[:space:]]+--mount=type=ssh[[:space:]]+go[[:space:]]+mod[[:space:]]+download[[:space:]]*$/ {
        if (!injected_before_go_mod) {
          print "ENV GOPRIVATE=github.com/keploy/*"
          print "ENV GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\""
          emit_win_git_prep();
          injected_before_go_mod=1;
        }
        print; next
      }

      { print }
    ' "$DOCKERFILE_PATH" | rewrite_in_place

  else
    # LINUX: standard POSIX prep; split mkdir/chmod to avoid && issues if ever misdetected
    awk '
      BEGIN { OFS=""; injected_before_go_mod=0; injected_after_copy=0 }

      function emit_nix_git_prep() {
        print "RUN git config --global url.\"ssh://git@github.com/\".insteadOf \"https://github.com/\""
        print "RUN mkdir -p ~/.ssh"
        print "RUN chmod 700 ~/.ssh"
        print "RUN if command -v ssh-keyscan >/dev/null 2>&1; then \\"
        print "      (ssh-keyscan -T 10 -t rsa,ecdsa,ed25519 github.com >> ~/.ssh/known_hosts 2>/dev/null || echo \"ssh-keyscan failed; continuing\"); \\"
        print "    else \\"
        print "      echo \"ssh-keyscan not found; continuing\"; \\"
        print "    fi"
      }

      /^[[:space:]]*COPY[[:space:]]+go\.mod[[:space:]]+go\.sum[[:space:]]+\/app\/?[[:space:]]*$/ {
        print;
        if (!injected_after_copy) {
          emit_nix_git_prep();
          injected_after_copy=1;
        }
        next
      }

      /^[[:space:]]*RUN[[:space:]]+--mount=type=ssh[[:space:]]+go[[:space:]]+mod[[:space:]]+download[[:space:]]*$/ {
        if (!injected_before_go_mod) {
          print "ENV GOPRIVATE=github.com/keploy/*"
          print "ENV GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\""
          emit_nix_git_prep();
          injected_before_go_mod=1;
        }
        print; next
      }

      { print }
    ' "$DOCKERFILE_PATH" | rewrite_in_place
  fi
}

build_docker_image() {
  echo "Building Docker image (will use buildx if available)..."
  if docker buildx version >/dev/null 2>&1; then
    echo "Using buildx for SSH mount support..."
    docker buildx build --ssh default -t ghcr.io/keploy/keploy:1h .
  else
    echo "Buildx not available, using legacy Docker build..."
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

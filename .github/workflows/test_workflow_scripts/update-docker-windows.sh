#!/bin/bash
set -Eeuo pipefail

DOCKERFILE_PATH="./Dockerfile"

# ---------- detect Windows runner ----------
is_windows_runner() {
  if [ "${RUNNER_OS:-}" = "Windows" ]; then return 0; fi
  case "$(uname -s 2>/dev/null || echo)" in
    MINGW*|MSYS*|CYGWIN*|Windows_NT) return 0;;
  esac
  return 1
}

# ---------- atomic replace helper ----------
rewrite_in_place() {
  local tmp
  tmp="$(mktemp)"
  cat > "$tmp"
  mv "$tmp" "$DOCKERFILE_PATH"
}

ensure_dockerfile_syntax() {
  if ! head -n1 "$DOCKERFILE_PATH" | grep -q '^# syntax=docker/dockerfile:'; then
    {
      echo '# syntax=docker/dockerfile:1.6'
      cat "$DOCKERFILE_PATH"
    } | rewrite_in_place
  fi
}

add_race_flag() {
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
  # ALWAYS rewrite to use BuildKit ssh mount (works for Linux; for Windows we rely on buildx + agent)
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
  if is_windows_runner; then
    # WINDOWS: no mkdir/chmod/ssh-keyscan; rely on forwarded SSH key and disable host key checks
    awk '
      BEGIN { OFS=""; injected_before_go_mod=0; injected_after_copy=0 }
      function emit_win_git_prep() {
        # only rewrite keploy org to SSH; keep everything else on HTTPS
        print "RUN git config --global url.\"ssh://git@github.com/keploy/\".insteadOf \"https://github.com/keploy/\""
      }
      /^[[:space:]]*COPY[[:space:]]+go\.mod[[:space:]]+go\.sum[[:space:]]+\/app\/?[[:space:]]*$/ {
        print;
        if (!injected_after_copy) { emit_win_git_prep(); injected_after_copy=1 }
        next
      }
      /^[[:space:]]*RUN[[:space:]]+--mount=type=ssh[[:space:]]+go[[:space:]]+mod[[:space:]]+download[[:space:]]*$/ {
        if (!injected_before_go_mod) {
          print "ENV GOPRIVATE=github.com/keploy/*"
          print "ENV GONOSUMDB=github.com/keploy/*"
          print "ENV GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\""
          emit_win_git_prep();
          injected_before_go_mod=1
        }
        print; next
      }
      { print }
    ' "$DOCKERFILE_PATH" | rewrite_in_place
    return 0
  fi

  # LINUX: safe POSIX prep (no Windows-only ops)
  awk '
    BEGIN { OFS=""; injected_before_go_mod=0; injected_after_copy=0 }
    function emit_nix_git_prep() {
      # only rewrite keploy org to SSH; keep everything else on HTTPS
      print "RUN git config --global url.\"ssh://git@github.com/keploy/\".insteadOf \"https://github.com/keploy/\""
      print "RUN mkdir -p ~/.ssh"
      print "RUN if command -v ssh-keyscan >/dev/null 2>&1; then \\"
      print "      (ssh-keyscan -T 10 -t rsa,ecdsa,ed25519 github.com >> ~/.ssh/known_hosts 2>/dev/null || echo \"ssh-keyscan failed; continuing\"); \\"
      print "    else \\"
      print "      echo \"ssh-keyscan not found; continuing\"; \\"
      print "    fi"
    }
    /^[[:space:]]*COPY[[:space:]]+go\.mod[[:space:]]+go\.sum[[:space:]]+\/app\/?[[:space:]]*$/ {
      print;
      if (!injected_after_copy) { emit_nix_git_prep(); injected_after_copy=1 }
      next
    }
    /^[[:space:]]*RUN[[:space:]]+--mount=type=ssh[[:space:]]+go[[:space:]]+mod[[:space:]]+download[[:space:]]*$/ {
      if (!injected_before_go_mod) {
        print "ENV GOPRIVATE=github.com/keploy/*"
        print "ENV GONOSUMDB=github.com/keploy/*"
        print "ENV GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\""
        emit_nix_git_prep();
        injected_before_go_mod=1
      }
      print; next
    }
    { print }
  ' "$DOCKERFILE_PATH" | rewrite_in_place
}

build_docker_image() {
  echo "Building Docker image with SSH forwarding via buildx…"
  # Ensure an agent is running and has your deploy key; most actions do this already.
  docker buildx version >/dev/null 2>&1 || { echo "buildx is required."; exit 1; }
  # Forward the default SSH agent into BuildKit
  docker buildx build --ssh default -t ghcr.io/keploy/keploy:1h .
}

main() {
  ensure_dockerfile_syntax
  add_race_flag
  enable_ssh_mount_for_go_mod
  use_ssh_for_github_and_known_hosts
  build_docker_image
}

main

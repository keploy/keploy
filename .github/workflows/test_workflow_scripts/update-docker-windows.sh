#!/bin/bash
set -Eeuo pipefail

DOCKERFILE_PATH="./Dockerfile"

# --- helpers ---------------------------------------------------------------

rewrite_in_place() {
  local tmp
  tmp="$(mktemp)"
  cat > "$tmp"
  mv "$tmp" "$DOCKERFILE_PATH"
}

ensure_dockerfile_syntax() {
  # Needed for BuildKit front-end features like --mount
  if ! head -n1 "$DOCKERFILE_PATH" | grep -q '^# syntax=docker/dockerfile:'; then
    {
      echo '# syntax=docker/dockerfile:1.6'
      cat "$DOCKERFILE_PATH"
    } | rewrite_in_place
  fi
}

add_race_flag() {
  # add -race to any RUN go build line if missing
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
  # Turn "RUN go mod download" into BuildKit mount form
  awk '
    BEGIN { OFS="" }
    /^[[:space:]]*RUN[[:space:]]+go[[:space:]]+mod[[:space:]]+download[[:space:]]*$/ {
      print "RUN --mount=type=ssh go mod download"
      next
    }
    { print }
  ' "$DOCKERFILE_PATH" | rewrite_in_place
}

inject_git_settings_minimal() {
  # Minimal + portable:
  # - Only keploy org uses SSH (others stay HTTPS)
  # - Disable proxy/sumdb for keploy/* (private)
  # - Skip known_hosts entirely via StrictHostKeyChecking=no
  # - NO mkdir/chmod/ssh-keyscan/&& anywhere
  awk '
    BEGIN { OFS=""; injected_before_go_mod=0; injected_after_copy=0 }

    function emit_min_git_prep() {
      print "ENV GOPRIVATE=github.com/keploy/*"
      print "ENV GONOSUMDB=github.com/keploy/*"
      print "ENV GIT_SSH_COMMAND=\"ssh -o StrictHostKeyChecking=no\""
      print "RUN git config --global url.\"ssh://git@github.com/keploy/\".insteadOf \"https://github.com/keploy/\""
    }

    /^[[:space:]]*COPY[[:space:]]+go\.mod[[:space:]]+go\.sum[[:space:]]+\/app\/?[[:space:]]*$/ {
      print;
      if (!injected_after_copy) { emit_min_git_prep(); injected_after_copy=1 }
      next
    }

    /^[[:space:]]*RUN[[:space:]]+--mount=type=ssh[[:space:]]+go[[:space:]]+mod[[:space:]]+download[[:space:]]*$/ {
      if (!injected_before_go_mod) {
        emit_min_git_prep();
        injected_before_go_mod=1
      }
      print; next
    }

    { print }
  ' "$DOCKERFILE_PATH" | rewrite_in_place
}

build_docker_image() {
  echo "Building with BuildKit enabled (no buildx dependency)…"
  # Force BuildKit for classic docker build
  export DOCKER_BUILDKIT=1
  export COMPOSE_DOCKER_CLI_BUILD=1
  export BUILDKIT_PROGRESS=plain

  # Forward the runner’s SSH agent into the build so --mount=type=ssh works
  docker build --ssh default -t ghcr.io/keploy/keploy:1h .
}

main() {
  ensure_dockerfile_syntax
  add_race_flag
  enable_ssh_mount_for_go_mod
  inject_git_settings_minimal
  build_docker_image
}

main

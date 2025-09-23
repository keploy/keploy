#!/bin/bash
set -Eeuo pipefail

DOCKERFILE_PATH="./Dockerfile"

# ---- helpers ---------------------------------------------------------------

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
  # Turn: RUN go mod download  ->  RUN --mount=type=ssh go mod download
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
  # No mkdir/chmod/ssh-keyscan. Just:
  # - use SSH only for keploy org
  # - disable host key checking
  # - disable proxy/sumdb for keploy
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
  echo "Building Docker image (trying buildx → BuildKit → legacy)..."

  # 1) Try buildx + SSH agent forwarding
  if docker buildx version >/dev/null 2>&1; then
    if docker buildx build --ssh default -t ghcr.io/keploy/keploy:1h .; then
      return 0
    else
      echo "buildx build failed; falling back to BuildKit docker build…" >&2
    fi
  else
    echo "buildx not present; using BuildKit docker build…" >&2
  fi

  # 2) Try BuildKit-enabled docker build with --ssh
  if DOCKER_BUILDKIT=1 docker build --ssh default -t ghcr.io/keploy/keploy:1h .; then
    return 0
  else
    echo "BuildKit docker build failed; falling back to legacy docker build (no SSH)…" >&2
  fi

  # 3) Last resort (no SSH mount) – this will only work if all deps are public
  docker build -t ghcr.io/keploy/keploy:1h .
}

main() {
  ensure_dockerfile_syntax
  add_race_flag
  enable_ssh_mount_for_go_mod
  inject_git_settings_minimal
  build_docker_image
}

main

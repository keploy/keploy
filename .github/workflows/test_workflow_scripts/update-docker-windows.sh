#!/bin/bash
set -Eeuo pipefail

DOCKERFILE_PATH="./Dockerfile"

# ---------- helpers ----------

rewrite_in_place() {
  local tmp
  tmp="$(mktemp)"
  cat > "$tmp"
  mv "$tmp" "$DOCKERFILE_PATH"
}

ensure_dockerfile_syntax() {
  # needed for --mount
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
  # Only keploy/* over SSH; avoid proxy/sumdb; skip known_hosts & chmod entirely
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

install_buildx_if_missing() {
  if docker buildx version >/dev/null 2>&1; then
    return 0
  fi
  echo "docker buildx not found. Installing locally…"
  local BX_VER="v0.13.1"
  local OS_BIN
  case "$(uname -s 2>/dev/null || echo)" in
    MINGW*|MSYS*|CYGWIN*|Windows_NT) OS_BIN="buildx-${BX_VER}.windows-amd64.exe" ;;
    Linux) OS_BIN="buildx-${BX_VER}.linux-amd64" ;;
    Darwin) OS_BIN="buildx-${BX_VER}.darwin-amd64" ;;
    *) echo "Unsupported OS"; exit 1 ;;
  esac
  local URL="https://github.com/docker/buildx/releases/download/${BX_VER}/${OS_BIN}"
  local PLUGDIR="${HOME}/.docker/cli-plugins"
  mkdir -p "${PLUGDIR}"
  curl -fsSL "${URL}" -o "${PLUGDIR}/docker-buildx"
  chmod +x "${PLUGDIR}/docker-buildx"
  if [[ "$OS_BIN" == *.exe ]]; then mv "${PLUGDIR}/docker-buildx" "${PLUGDIR}/docker-buildx.exe"; fi
  docker buildx version
}

build_image() {
  echo "Building with buildx (docker driver) and SSH agent forwarding…"

  # Ensure the lightweight "docker" driver (no privileged container)
  if ! docker buildx inspect keploybx >/dev/null 2>&1; then
    docker buildx create --name keploybx --driver docker --use >/dev/null
  else
    docker buildx use keploybx >/dev/null
  fi

  # On Docker Desktop/Windows daemon, driver=docker uses built-in BuildKit.
  # Use --load so the image is available to classic 'docker' after build.
  if docker buildx build --ssh default -t ghcr.io/keploy/keploy:1h --load .; then
    return 0
  fi

  echo "buildx (docker driver) failed; trying classic docker build with BuildKit…" >&2
  DOCKER_BUILDKIT=1 docker build --ssh default -t ghcr.io/keploy/keploy:1h .
}

# ---------- main ----------
ensure_dockerfile_syntax
add_race_flag
enable_ssh_mount_for_go_mod
inject_git_settings_minimal
install_buildx_if_missing
build_image

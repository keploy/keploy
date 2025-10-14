#!/bin/bash
set -Eeuo pipefail

DOCKERFILE_PATH="./Dockerfile"

ensure_dockerfile_syntax() {
    # Ensure BuildKit features (like --mount=type=ssh) are supported
    if ! head -n1 "$DOCKERFILE_PATH" | grep -q '^# syntax=docker/dockerfile:'; then
        echo "Prepending Dockerfile syntax directive for BuildKit mounts..."
        tmp="$(mktemp)" && {
          echo '# syntax=docker/dockerfile:1.6'
          cat "$DOCKERFILE_PATH"
        } >"$tmp" && mv "$tmp" "$DOCKERFILE_PATH"
    fi
}

has_ssh_agent() {
    # Return success if an SSH agent socket is available and usable
    if [[ -n "${SSH_AUTH_SOCK:-}" ]]; then
        ssh-add -l >/dev/null 2>&1 || true
        return 0
    fi
    return 1
}

add_race_flag() {
    echo "Adding -race to go build..."
    sed -i \
      's/^(RUN[[:space:]]\+go[[:space:]]\+build)\([[:space:]]\)/\1 -race\2/' \
      "$DOCKERFILE_PATH" 2>/dev/null || true
}

use_ssh_for_github_and_known_hosts() {
    echo "Injecting SSH config and known_hosts before go mod download..."
    # Ensure we rewrite https -> ssh and have known_hosts available
    # Insert just before the go mod download line (after we switch it to --mount=type=ssh)
    sed -i \
      '/^RUN --mount=type=ssh go mod download/i ENV GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=no"\nRUN git config --global url."ssh:\/\/git@github.com\/".insteadOf "https:\/\/github.com\/" \&\& mkdir -p -m 0700 ~\/\.ssh \&\& ssh-keyscan github.com >> ~\/\.ssh\/known_hosts' \
      "$DOCKERFILE_PATH" || true

    # Also try to add after the COPY go.mod go.sum if present (best-effort)
    sed -i \
      '/^COPY[[:space:]]\+go\.mod[[:space:]]\+go\.sum[[:space:]]\+\/app\/?[[:space:]]*$/a RUN git config --global url."ssh:\/\/git@github.com\/".insteadOf "https:\/\/github.com\/" \&\& mkdir -p -m 0700 ~\/\.ssh \&\& ssh-keyscan github.com >> ~\/\.ssh\/known_hosts' \
      "$DOCKERFILE_PATH" || true

    # Ensure GOPRIVATE skips proxy for keploy org
    sed -i \
      '/^RUN --mount=type=ssh go mod download/i ENV GOPRIVATE=github.com\/keploy\/*' \
      "$DOCKERFILE_PATH" || true
}

enable_ssh_mount_for_go_mod() {
    echo "Switching go mod download to use BuildKit SSH mount..."
    sed -i 's/^RUN[[:space:]]\+go[[:space:]]\+mod[[:space:]]\+download[[:space:]]*$/RUN --mount=type=ssh go mod download/' "$DOCKERFILE_PATH" || true
}

build_docker_image_without_ssh() {
    echo "Building Docker image (no SSH forwarding)..."
    cat "$DOCKERFILE_PATH"

    DOCKER_BUILDKIT=1 docker build -t ttl.sh/keploy/keploy:1h .
}

build_docker_image_with_ssh() {
    echo "Building Docker image (with SSH forwarding)..."
    cat "$DOCKERFILE_PATH"

    DOCKER_BUILDKIT=1 docker build --ssh default -t ttl.sh/keploy/keploy:1h .
}

main() {
    ensure_dockerfile_syntax
    add_race_flag || true

    if has_ssh_agent; then
        enable_ssh_mount_for_go_mod
        use_ssh_for_github_and_known_hosts
        build_docker_image_with_ssh
    else
        echo "SSH agent not detected. Skipping SSH-dependent Dockerfile edits and build flags."
        echo "- Will NOT rewrite github URLs to SSH"
        echo "- Will NOT use --mount=type=ssh for go mod download"
        build_docker_image_without_ssh
    fi
}

main

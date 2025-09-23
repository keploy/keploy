#!/bin/bash
set -Eeuo pipefail

DOCKERFILE_PATH="./Dockerfile"

# Choose correct in-place sed flag for macOS (BSD) vs Linux (GNU)
if [[ "$(uname)" == "Darwin" ]]; then
  SED_INPLACE=(-i '')
else
  SED_INPLACE=(-i)
fi

ensure_dockerfile_syntax() {
  # BuildKit mount needs a recent dockerfile syntax directive in many setups
  if ! head -n1 "$DOCKERFILE_PATH" | grep -q '^# syntax=docker/dockerfile:'; then
    echo "Prepending Dockerfile syntax directive for BuildKit mounts..."
    tmp="$(mktemp)"
    {
      echo '# syntax=docker/dockerfile:1.6'
      cat "$DOCKERFILE_PATH"
    } > "$tmp"
    mv "$tmp" "$DOCKERFILE_PATH"
  fi
}

add_race_flag() {
  echo "Adding -race to the go build line..."
  # Insert -race immediately after 'RUN go build' (keeps existing flags/ldflags/output intact)
  # Uses ERE with BSD/GNU compatible -E
  sed -E "${SED_INPLACE[@]}" \
    's/^(RUN[[:space:]]+go[[:space:]]+build)([[:space:]])/\1 -race\2/' \
    "$DOCKERFILE_PATH"
}

inject_ssh_known_hosts() {
  echo "Ensuring SSH/known_hosts setup after the go.mod/go.sum COPY..."
  # Append a RUN line after the COPY go.mod go.sum /app/ line
  # Need BSD-safe newline escaping for 'a\'.
  sed "${SED_INPLACE[@]}" \
    -e '/^COPY[[:space:]]\+go\.mod[[:space:]]\+go\.sum[[:space:]]\/app\/[[:space:]]*$/a\
RUN git config --global url."ssh:\/\/git@github.com\/".insteadOf "https:\/\/github.com\/" \&\& mkdir -p -m 0700 ~/.ssh \&\& ssh-keyscan github.com >> ~/.ssh/known_hosts' \
    "$DOCKERFILE_PATH"
}

use_ssh_mount_for_go_mod_download() {
  echo "Switching go mod download to use SSH mount..."
  sed -E "${SED_INPLACE[@]}" \
    's/^RUN[[:space:]]+go[[:space:]]+mod[[:space:]]+download[[:space:]]*$/RUN --mount=type=ssh go mod download/' \
    "$DOCKERFILE_PATH"
}

build_docker_image() {
  echo "Building Docker image with BuildKit and SSH agent forwarding..."
  DOCKER_BUILDKIT=1 docker build --ssh default -t ttl.sh/keploy/keploy:1h .
}

main() {
  ensure_dockerfile_syntax
  add_race_flag
  inject_ssh_known_hosts
  use_ssh_mount_for_go_mod_download
  build_docker_image
}

main

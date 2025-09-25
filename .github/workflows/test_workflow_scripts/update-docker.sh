#!/bin/bash
set -Eeuo pipefail

DOCKERFILE_PATH="./Dockerfile"


# Function to add the -race flag to the go build command in the Dockerfile
update_dockerfile() {
    echo "Updating Dockerfile to include the -race flag in the go build command..."

    # Use sed to update the Dockerfile
    sed -i 's/RUN go build -tags=viper_bind_struct -ldflags="-X main.dsn=$SENTRY_DSN_DOCKER -X main.version=$VERSION" -o keploy ./RUN go build -race -tags=viper_bind_struct -ldflags="-X main.dsn=$SENTRY_DSN_DOCKER -X main.version=$VERSION" -o keploy ./' "$DOCKERFILE_PATH"
    
    # Configure Git to use SSH and add GitHub's SSH key to known_hosts in a single layer.
    # This prevents the "Host key verification failed" error.
    sed -i '/COPY go.mod go.sum \/app\//a RUN git config --global url."ssh:\/\/git@github.com\/".insteadOf "https:\/\/github.com\/" \&\& mkdir -p -m 0700 ~\/.ssh \&\& ssh-keyscan github.com >> ~\/.ssh\/known_hosts' "$DOCKERFILE_PATH"
    
    # Ensure the go mod download command uses the SSH mount.
    sed -i 's/RUN go mod download/RUN --mount=type=ssh go mod download/' "$DOCKERFILE_PATH"
    
    # Add go mod tidy after COPY . /app
    sed -i '/COPY \. \/app/a RUN --mount=type=ssh go mod tidy' "$DOCKERFILE_PATH"
}

# Function to build the Docker image
build_docker_image() {
    echo "Building Docker image..."
    cat "$DOCKERFILE_PATH"


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

build_docker_image() {
    echo "Building Docker image with BuildKit and SSH forwarding..."
    DOCKER_BUILDKIT=1 docker build --ssh default -t ttl.sh/keploy/keploy:1h .
}

main() {
    ensure_dockerfile_syntax
    add_race_flag || true
    enable_ssh_mount_for_go_mod
    use_ssh_for_github_and_known_hosts
    build_docker_image
}

main

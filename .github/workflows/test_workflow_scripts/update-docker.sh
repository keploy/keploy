#!/bin/bash

# Define the Dockerfile path
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
}

# Function to build the Docker image
build_docker_image() {
    echo "Building Docker image..."
    cat "$DOCKERFILE_PATH"

    # Enable Docker BuildKit and build the image, forwarding the SSH agent
    DOCKER_BUILDKIT=1 docker build --ssh default -t ttl.sh/keploy/keploy:1h .

    if [ $? -eq 0 ]; then
        echo "Docker image built successfully."
    else
        echo "Failed to build Docker image."
        exit 1
    fi
}

# Main function to update the Dockerfile and build the Docker image
main() {
    update_dockerfile
    build_docker_image
}

# Run the main function
main
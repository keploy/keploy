#!/bin/bash

# Define the Dockerfile path
DOCKERFILE_PATH="./Dockerfile"

# Function to add the -race flag to the go build command in the Dockerfile
update_dockerfile() {
    echo "Updating Dockerfile to include the -race flag in the go build command..."

    # Use sed to update the Dockerfile
    sed -i 's/RUN go build -tags=viper_bind_struct -ldflags="-X main.dsn=$SENTRY_DSN_DOCKER -X main.version=$VERSION" -o keploy ./RUN go build -race -tags=viper_bind_struct -ldflags="-X main.dsn=$SENTRY_DSN_DOCKER -X main.version=$VERSION" -o keploy ./' "$DOCKERFILE_PATH"
    
    echo "Dockerfile updated successfully."
}

# Function to build the Docker image
build_docker_image() {
    echo "Building Docker image..."

    # Build the Docker image
    docker image build -t ttl.sh/keploy/keploy:1h .

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

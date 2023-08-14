# === Build Stage ===
FROM golang:1.21 AS build

# Set the working directory
WORKDIR /app

# Copy the Go module files and download dependencies
COPY go.mod go.sum /app/
RUN go mod download

# Copy the contents of the current directory into the build container
COPY . /app

# Build the keploy binary
RUN go build -o keploy .

# === Runtime Stage ===
FROM debian:bookworm-slim

# Update the package lists and install required packages
RUN apt-get update && \
    apt-get install -y ca-certificates curl sudo && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Install Docker CLI (docker-compose)
RUN curl -fsSL https://get.docker.com -o get-docker.sh && \
    sh get-docker.sh && \
    rm get-docker.sh

# Copy the keploy binary and the entrypoint script from the build container
COPY --from=build /app/keploy /app/keploy
COPY --from=build /app/entrypoint.sh /app/entrypoint.sh

# Make the entrypoint.sh file executable
RUN chmod +x /app/entrypoint.sh

# Change working directory (optional depending on your needs)
WORKDIR /files

# Set the entrypoint
ENTRYPOINT ["/app/entrypoint.sh", "/app/keploy"]

# === Build Stage ===
FROM golang:1.22 AS build

# Set the working directory
WORKDIR /app

# Define build arguments for ldflags
ARG SENTRY_DSN_DOCKER
ARG VERSION

# Copy the Go module files and download dependencies
COPY go.mod go.sum /app/
RUN go mod download

# Copy the contents of the current directory into the build container
COPY . /app

# Build the keploy binary
RUN go build -tags=viper_bind_struct -ldflags="-X main.dsn=$SENTRY_DSN_DOCKER -X main.version=$VERSION" -o keploy .

# === Runtime Stage ===
FROM debian:bookworm-slim

ENV KEPLOY_INDOCKER=true

# Update the package lists and install required packages
RUN apt-get update
RUN apt-get install -y ca-certificates curl sudo && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Install Docker engine
RUN curl -fsSL https://get.docker.com -o get-docker.sh && \
    sh get-docker.sh && \
    rm get-docker.sh

# Install docker-compose to PATH
RUN apt install docker-compose -y

# Copy the keploy binary and the entrypoint script from the build container
COPY --from=build /app/keploy /app/keploy
COPY --from=build /app/entrypoint.sh /app/entrypoint.sh

# Make the entrypoint.sh file executable
RUN chmod +x /app/entrypoint.sh

# Set the entrypoint
ENTRYPOINT ["/app/entrypoint.sh", "/app/keploy"]
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
# setting GOMAXPROCS to avoid crashing qemu while building different arch with docker buildx
# ref - https://github.com/golang/go/issues/70329#issuecomment-2559049444
RUN GOMAXPROCS=2 go build -tags=viper_bind_struct -ldflags="-X main.dsn=$SENTRY_DSN_DOCKER -X main.version=$VERSION -X main.apiServerURI=$SERVER_URL -X main.gitHubClientID=$GITTHUB_APP_CLIENT_ID" -o keploy .

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
# Install specific version of Docker Compose plugin (v2.29.1)
RUN mkdir -p /usr/lib/docker/cli-plugins && \
    curl -SL "https://github.com/docker/compose/releases/download/v2.29.1/docker-compose-linux-$(uname -m)" -o /usr/lib/docker/cli-plugins/docker-compose && \
    chmod +x /usr/lib/docker/cli-plugins/docker-compose

# Copy the keploy binary and the entrypoint script from the build container
COPY --from=build /app/keploy /app/keploy
COPY --from=build /app/entrypoint.sh /app/entrypoint.sh

# windows comapatibility
RUN sed -i 's/\r$//' /app/entrypoint.sh

# Make the entrypoint.sh file executable
RUN chmod +x /app/entrypoint.sh

# Set the entrypoint
ENTRYPOINT ["/app/entrypoint.sh", "/app/keploy"]

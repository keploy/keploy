# ## build ui
# #FROM --platform=${BUILDPLATFORM} node:18-bullseye as ui-builder
# #
# ##RUN apt-get update && apt-get install libvips-dev -y
# #
# #RUN npm install -g gatsby-cli
# #
# #RUN git clone https://github.com/keploy/ui
# #
# #WORKDIR /ui
# #
# #RUN npm install --legacy-peer-deps
# #
# #ARG KEPLOY_PATH_PREFIX='/'
# #
# #RUN PATH_PREFIX="$KEPLOY_PATH_PREFIX" gatsby build --prefix-paths

# # build stage
# FROM --platform=${BUILDPLATFORM} golang:alpine as go-builder

# RUN apk add -U --no-cache ca-certificates && apk add build-base

# ENV GO111MODULE=on

# # Build Delve
# RUN go install github.com/go-delve/delve/cmd/dlv@latest

# WORKDIR /app

# COPY go.mod .
# COPY go.sum .

# RUN go mod download

# COPY . .

# #COPY --from=ui-builder /ui/public /app/web/public

# #RUN CGO_ENABLED=0 GOOS=linux go build -o health cmd/health/main.go
# RUN CGO_ENABLED=0 GOOS=linux go build -o keploy cmd/server/main.go

# # final stage
# FROM --platform=${BUILDPLATFORM} alpine
# COPY --from=alpine /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# #COPY --from=builder /app/health /app/
# COPY --from=go-builder /app/keploy /app/
# COPY --from=go-builder /go/bin/dlv /

# EXPOSE 6789
# ENTRYPOINT ["/app/keploy"]
FROM ubuntu:latest

# Update the package lists
RUN apt-get update

# Install required packages
RUN apt-get install -y llvm-14 clang-14 linux-tools-common libbpf-dev ca-certificates wget sudo nano curl

# Install Go 1.19
RUN wget https://golang.org/dl/go1.19.linux-amd64.tar.gz && \
    tar -C /usr/local -xzf go1.19.linux-amd64.tar.gz && \
    rm go1.19.linux-amd64.tar.gz

# Add Go binary path to the environment variable
ENV PATH="/usr/local/go/bin:${PATH}"

# Install Docker CLI (docker-compose)
RUN curl -fsSL https://get.docker.com -o get-docker.sh && \
    sh get-docker.sh && \
    rm get-docker.sh

# Copy the contents of the current directory into the image
COPY . /app

# Set the working directory
WORKDIR /app

# Make the entrypoint.sh file executable
RUN chmod +x /app/entrypoint.sh

# Build the keployV2 binary
RUN go build -o keployV2

# Change working directory
WORKDIR /files

# Set the entrypoint
ENTRYPOINT ["/app/entrypoint.sh", "/app/keployV2"]
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

# Set the working directory
WORKDIR /app

# To cache go modules
COPY go.mod /app/
COPY go.sum /app/

RUN go mod download

# Copy the contents of the current directory into the image
COPY . /app

# Make the entrypoint.sh file executable
RUN chmod +x /app/entrypoint.sh

# Build the keployV2 binary
RUN go build -o keployV2 .

# Change working directory
WORKDIR /files

# Set the entrypoint
ENTRYPOINT ["/app/entrypoint.sh", "/app/keployV2"]
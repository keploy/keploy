# syntax=docker/dockerfile:1.6
#
# Self-contained keploy build — intended for external/user-side
# `docker build .` invocations. CI does NOT use this Dockerfile;
# the release pipeline and PR runs go through Dockerfile.runtime
# (COPY of a prebuilt binary from a preceding GHA build step on a
# native runner with gcc), which is the pattern Session P requires
# (CGo outside Dockerfile wherever the binary is built in CI).
#
# This Dockerfile keeps the in-container `go build` path because
# external users still need a single-command "build from source"
# entry point. libpg_query / pg_query_go v6.2.2 requires CGo, so
# the builder image MUST carry a working gcc — golang:1.27 (Debian
# trixie base) already ships gcc, so no extra apt install is
# needed. CGO_ENABLED=1 is set explicitly so a mis-configured
# builder (someone passing CGO_ENABLED=0 as a docker build-arg)
# fails loud rather than producing a non-cgo binary that crashes
# the moment the classifier tries to pg_query.Parse.
#
# === Build Stage ===
FROM golang:1.27 AS build

# Set the working directory
WORKDIR /app

# Define build arguments for ldflags
ARG SENTRY_DSN_DOCKER
ARG VERSION
ARG SERVER_URL
ARG GITHUB_APP_CLIENT_ID

# pg_query_go links libpg_query via CGo. golang:1.27 (trixie)
# ships gcc; we set CGO_ENABLED=1 explicitly so ARG overrides can't
# accidentally disable it. GOMAXPROCS=2 stays to avoid crashing qemu
# under buildx multi-arch builds.
ENV CGO_ENABLED=1

# Copy the Go module files and download dependencies
COPY go.mod go.sum /app/
RUN go mod download

# Copy the contents of the current directory into the build container
COPY . /app

# Build the keploy binary
# setting GOMAXPROCS to avoid crashing qemu while building different arch with docker buildx
# ref - https://github.com/golang/go/issues/70329#issuecomment-2559049444
RUN GOMAXPROCS=2 go build -tags=viper_bind_struct -ldflags="-X main.dsn=$SENTRY_DSN_DOCKER -X main.version=$VERSION -X main.apiServerURI=$SERVER_URL -X main.gitHubClientID=$GITHUB_APP_CLIENT_ID" -o keploy .

# === Runtime Stage ===
FROM debian:trixie-slim

ENV KEPLOY_INDOCKER=true

# Update the package lists and install required packages.
# One RUN, not two: a separate `apt-get update` layer bakes its ~22MB of
# /var/lib/apt/lists into the image - the later `rm -rf` only masks it
# from the upper layer, it cannot reclaim it - and a cached update layer
# can feed `install` from stale indices. Merging cuts ~17MB off the
# pulled image and ~22MB off it unpacked.
# --no-install-recommends skips five packages nothing here uses
# (bash-completion, krb5-locales, libldap-common, libsasl2-modules,
# publicsuffix), a further ~3.6MB unpacked. Keep ca-certificates in the
# explicit list: it is only a Recommends of libcurl4t64, so dropping it
# would silently remove every trusted root.
# apt-get clean is unnecessary: the base image ships
# /etc/apt/apt.conf.d/docker-clean, which already discards the .deb cache
# after every install.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        sudo && \
    rm -rf /var/lib/apt/lists/*

# Copy the keploy binary and the entrypoint script from the build container
COPY --from=build /app/keploy /app/keploy
COPY --from=build /app/entrypoint.sh /app/entrypoint.sh

# windows comapatibility and make the entrypoint.sh file executable
RUN sed -i 's/\r$//' /app/entrypoint.sh && \
    chmod +x /app/entrypoint.sh

# Set the entrypoint
ENTRYPOINT ["/app/entrypoint.sh", "/app/keploy", "agent","--is-docker"]

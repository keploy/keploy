# Dockerfile — the concurrent-agents CA e2e, one runtime image per distro:
#
#   --target debian12     update-ca-certificates  (Debian 12)
#   --target debian13     update-ca-certificates  (Debian 13)
#   --target ubuntu2204   update-ca-certificates  (Ubuntu 22.04)
#   --target fedora44     p11-kit layout: bundles, directory-hash, and the hash
#                         links in /etc/pki/tls/certs, with no /etc/pki/tls/cert.pem
#   --target rhel8        p11-kit layout: bundles only
#   --target rhel9        p11-kit layout: bundles and directory-hash (with its
#                         two compatibility links)
#   --target rhel10       p11-kit layout: as Fedora 44, plus /etc/pki/tls/cert.pem
#   --target al2023       p11-kit layout: as RHEL 9, extracted with "p11-kit
#                         extract"; /etc/ssl/certs is a symlink to directory-hash
#
# Debian's tool and the p11-kit layout rebuild the store in different ways, and
# the three RHEL generations and Fedora each lay the p11-kit outputs out
# differently, so the scenario runs against each. Each image holds the distro's
# own ca-certificates package, curl and openssl as the TLS clients, and the
# harness; see harness/main.go for what it does and run.sh for how it is run.
#
# The Docker Hub images default to their ECR Public mirrors, like
# tests/e2e/ca-bundle-merge, so CI is not subject to Docker Hub's
# unauthenticated pull limits; the RHEL ones are Red Hat's freely
# redistributable UBI images, and Amazon Linux's is Amazon's own ECR Public one. Override the ARGs to use images already cached
# locally (run.sh forwards every *_IMAGE variable it is given).

ARG GOLANG_IMAGE=public.ecr.aws/docker/library/golang:1.27-bookworm
ARG DEBIAN12_IMAGE=public.ecr.aws/docker/library/debian:bookworm-slim
ARG DEBIAN13_IMAGE=public.ecr.aws/docker/library/debian:trixie-slim
ARG UBUNTU2204_IMAGE=public.ecr.aws/docker/library/ubuntu:22.04
ARG FEDORA44_IMAGE=public.ecr.aws/docker/library/fedora:44
ARG RHEL8_IMAGE=registry.access.redhat.com/ubi8/ubi-minimal
ARG RHEL9_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal
ARG RHEL10_IMAGE=registry.access.redhat.com/ubi10/ubi-minimal
ARG AL2023_IMAGE=public.ecr.aws/amazonlinux/amazonlinux:2023

FROM ${GOLANG_IMAGE} AS build
# BuildKit sets these to the platform being built for; the harness must match
# the runtime stage's architecture.
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
# Only what the harness compiles: the module files, the tls package under test
# with the packages it imports, and the harness itself.
COPY go.mod go.sum /src/
COPY pkg /src/pkg
COPY utils /src/utils
COPY tests/e2e/ca-concurrent-agents/harness /src/tests/e2e/ca-concurrent-agents/harness
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/harness ./tests/e2e/ca-concurrent-agents/harness

FROM ${DEBIAN12_IMAGE} AS debian12
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl openssl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/harness /usr/local/bin/harness
ENTRYPOINT ["/usr/local/bin/harness", "scenario"]

FROM ${DEBIAN13_IMAGE} AS debian13
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl openssl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/harness /usr/local/bin/harness
ENTRYPOINT ["/usr/local/bin/harness", "scenario"]

FROM ${UBUNTU2204_IMAGE} AS ubuntu2204
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl openssl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/harness /usr/local/bin/harness
ENTRYPOINT ["/usr/local/bin/harness", "scenario"]

FROM ${FEDORA44_IMAGE} AS fedora44
RUN dnf install -y ca-certificates curl openssl && dnf clean all
COPY --from=build /out/harness /usr/local/bin/harness
ENTRYPOINT ["/usr/local/bin/harness", "scenario"]

FROM ${RHEL8_IMAGE} AS rhel8
RUN microdnf install -y ca-certificates openssl && microdnf clean all
COPY --from=build /out/harness /usr/local/bin/harness
ENTRYPOINT ["/usr/local/bin/harness", "scenario"]

FROM ${RHEL9_IMAGE} AS rhel9
RUN microdnf install -y ca-certificates openssl && microdnf clean all
COPY --from=build /out/harness /usr/local/bin/harness
ENTRYPOINT ["/usr/local/bin/harness", "scenario"]

FROM ${RHEL10_IMAGE} AS rhel10
RUN microdnf install -y ca-certificates openssl && microdnf clean all
COPY --from=build /out/harness /usr/local/bin/harness
ENTRYPOINT ["/usr/local/bin/harness", "scenario"]

FROM ${AL2023_IMAGE} AS al2023
RUN dnf install -y ca-certificates openssl && dnf clean all
COPY --from=build /out/harness /usr/local/bin/harness
ENTRYPOINT ["/usr/local/bin/harness", "scenario"]

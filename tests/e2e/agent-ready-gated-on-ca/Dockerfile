# Multi-stage build of the e2e harness for
# tests/e2e/agent-ready-gated-on-ca. The harness mounts the real
# pkg/agent/routes handlers but sidesteps the full agent (eBPF,
# proxy, memoryguard) so the container doesn't need privileged mode.
#
# Images are pulled from Docker Hub directly. Tags are pinned —
# `:latest` would make the e2e non-reproducible across CI runs.
# (We previously routed through ECR Public as a Hub mirror to dodge
# Hub's anon rate limits, but ECR Public itself started returning
# HTTP 429 on shared GHA runners — see PR #4101 CI flake.)
FROM golang:1.26-bookworm AS build
WORKDIR /src

# Copy the full keploy module so the harness build picks up the
# in-tree handler code under test — not a published version.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/e2e-harness \
      ./tests/e2e/agent-ready-gated-on-ca/harness

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/e2e-harness /usr/local/bin/e2e-harness
EXPOSE 8086
ENTRYPOINT ["/usr/local/bin/e2e-harness"]

# Build a stand-in for keploy-agent that uses the real cbmap.Publish().
# This proves the cbmap package integrates correctly end-to-end inside a
# container, against a real libpq client.

FROM golang:1.26 AS build
WORKDIR /src/keploy

# Build context is the keploy repo root. Copy the whole tree so the
# cbmap package and its (transitive) dependencies are available to
# link against. CI typically builds the real keploy-agent binary;
# this test just needs the cbmap import to resolve.
COPY . /src/keploy

WORKDIR /src/mitm
RUN cp /src/keploy/tests/e2e/scram-channel-binding/mitm/main.go .

RUN go mod init scram-cb-mitm && \
    printf 'replace go.keploy.io/server/v3 => /src/keploy\n' >> go.mod && \
    go mod edit -go=1.26 && \
    go mod tidy && \
    CGO_ENABLED=0 go build -o /out/mitm .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/mitm /mitm
ENTRYPOINT ["/mitm"]

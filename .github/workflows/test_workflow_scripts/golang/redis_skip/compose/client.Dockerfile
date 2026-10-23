# The compose cell's keploy mock test command: the lane's client (../client),
# built static (CGO_ENABLED=0) by ../golang-compose-linux.sh into the build
# context. No base image to pull.
FROM scratch
COPY redis-client /redis-client
ENTRYPOINT ["/redis-client"]

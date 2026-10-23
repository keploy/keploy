# The compose cell's app image: the lane's app (../app), built static
# (CGO_ENABLED=0) by ../golang-compose-linux.sh into the build context. No
# base image to pull.
FROM scratch
COPY redis-app /redis-app
ENTRYPOINT ["/redis-app"]

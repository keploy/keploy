# start_mongo: the MongoDB the sample-app lanes record against.
#
# Sourced via:
#
#   source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/mongo-ci.sh"
#
# Usage: [MONGO_NAME=<name>] [MONGO_IMAGE=<image>] start_mongo [docker run options...]
#   e.g. start_mongo --net keploy-network
#
# Starts the container MONGO_NAME (default mongoDb) from MONGO_IMAGE (default
# mongo) on port 27017, and returns once it answers a ping, or fails, saying
# why.
#
# The container is NOT started with --rm. With --rm a mongod that exits while
# it starts takes its container, and with it its exit code and its logs, away
# with it: a lane then only ever saw "No such container: mongoDb" until its
# wait ran out, and nothing to say why mongod had exited (the macOS lane,
# golang-docker-macos.sh, learned the same). A container that stops while this
# waits is reported at once, with its state and its logs. start_mongo removes
# an earlier one before it starts; the scripts that need it gone later stop and
# remove it themselves, and one left at the end of a run is gone with CI's
# throwaway runner.
#
# docker_pull_retry exits the shell when the pull fails for good.

if ! command -v docker_pull_retry >/dev/null 2>&1; then
  # shellcheck source=docker-build-retry.sh
  source "$(dirname "${BASH_SOURCE[0]}")/docker-build-retry.sh"
fi

start_mongo() {
  local name="${MONGO_NAME:-mongoDb}" image="${MONGO_IMAGE:-mongo}"
  local deadline=$((SECONDS + 60)) probe="" rc=0 state
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker_pull_retry "$image"
  docker run -d --name "$name" -p 27017:27017 "$@" "$image" >/dev/null || return 1
  while [ "$SECONDS" -lt "$deadline" ]; do
    # The probe's own words and exit are kept: while mongod starts they are the
    # only account of what the probe met (ECONNREFUSED, a missing mongosh, a
    # hang). mongosh's own timeouts end it within seconds, and socketTimeoutMS
    # bounds a connection that stalls once open (the driver's default is no
    # bound), so a probe does not outlive its turn inside the container;
    # timeout is the backstop for a stuck docker exec.
    rc=0
    probe=$(timeout 10 docker exec "$name" mongosh --quiet \
      'mongodb://127.0.0.1:27017/?connectTimeoutMS=5000&socketTimeoutMS=5000' \
      --eval 'db.adminCommand({ ping: 1 }).ok' 2>&1) || rc=$?
    if [ "$(printf '%s\n' "$probe" | tail -n 1)" = 1 ]; then
      return 0
    fi
    state=$(docker inspect -f '{{.State.Status}} exit={{.State.ExitCode}} oomkilled={{.State.OOMKilled}} error="{{.State.Error}}"' "$name" 2>&1) || true
    state=$(printf '%s' "$state" | tr -s '\n' ' ' | sed 's/^ *//; s/ *$//')
    # Only a container that stopped, or is gone, is a verdict; a daemon that
    # did not answer this inspect is asked again.
    case $state in
      exited* | dead* | removing* | *"o such object"* | *"o such container"*)
        echo "::error::MongoDB ($name) stopped while it started: ${state}. Its logs follow."
        docker logs --tail 200 "$name" 2>&1 || true
        return 1 ;;
    esac
    sleep 1
  done
  local how="exit $rc"
  if [ "$rc" = 124 ]; then how="killed after 10s"; fi
  echo "::error::MongoDB ($name) did not answer a ping within 60 seconds. The last probe ($how) said: $(printf '%s' "$probe" | tail -n 3 | tr '\n' ' ')"
  docker logs --tail 200 "$name" 2>&1 || true
  return 1
}

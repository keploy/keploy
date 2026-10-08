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

# mongo_ping <container>: ask the MongoDB in <container> for a ping, with
# mongosh through `docker exec`. It succeeds only when the ping answered 1,
# and leaves mongosh's output in MONGO_PING_OUTPUT and its exit status in
# MONGO_PING_RC. Every lane's MongoDB readiness probe is this one, with one
# bound on every runner:
#
# - mongosh's own timeouts end it within seconds inside the container, and
#   socketTimeoutMS bounds a connection that stalls once open (the driver's
#   default is no bound).
# - The docker exec itself is killed after 10 s, and then exits 124 as GNU
#   timeout's would. perl does the timing because macOS has no GNU timeout,
#   and perl ships with the Linux and macOS runners. It sends SIGKILL, not
#   SIGALRM: the docker CLI is a Go program, and Go ignores a SIGALRM it did
#   not ask for.
#
# HOME=/tmp: the mongo image sets HOME=/data/db, and docker exec runs mongosh
# as root, so mongosh would write its state (.mongodb/mongosh: am-unknown.json,
# its .lock, config and log files, all owned by root) into the database
# directory. While mongod starts, the image's entrypoint runs
# `find -L /data/configdb /data/db \! -user mongodb -exec chown mongodb '{}' +`
# under set -e. A file the probe creates and then removes in between fails
# find or chown, the entrypoint exits 1, and mongod never starts ("chown:
# cannot access '/data/db/.mongodb/mongosh/am-unknown.json.lock'").
mongo_ping() {
  local name=$1
  MONGO_PING_RC=0
  MONGO_PING_OUTPUT=$(perl -e '
      my $secs = shift;
      defined(my $pid = fork) or die "fork: $!\n";
      if (!$pid) { exec @ARGV or die "exec $ARGV[0]: $!\n" }
      $SIG{ALRM} = sub { kill "KILL", $pid; waitpid $pid, 0; exit 124 };
      alarm $secs;
      waitpid $pid, 0;
      alarm 0;
      exit(($? & 127) ? 128 + ($? & 127) : $? >> 8);
    ' 10 docker exec -e HOME=/tmp "$name" mongosh --quiet \
    'mongodb://127.0.0.1:27017/?connectTimeoutMS=5000&socketTimeoutMS=5000' \
    --eval 'db.adminCommand({ ping: 1 }).ok' 2>&1) || MONGO_PING_RC=$?
  [ "$(printf '%s\n' "$MONGO_PING_OUTPUT" | tail -n 1)" = 1 ]
}

# mongo_ping_said: one line on how the last mongo_ping ended and what it said,
# for a wait that gives up.
mongo_ping_said() {
  local how="exit ${MONGO_PING_RC:-none}"
  if [ "${MONGO_PING_RC:-}" = 124 ]; then how="killed after 10s"; fi
  printf 'The last probe (%s) said: %s' "$how" \
    "$(printf '%s' "${MONGO_PING_OUTPUT:-}" | tail -n 3 | tr '\n' ' ')"
}

start_mongo() {
  local name="${MONGO_NAME:-mongoDb}" image="${MONGO_IMAGE:-mongo}"
  local deadline=$((SECONDS + 60)) state
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker_pull_retry "$image"
  docker run -d --name "$name" -p 27017:27017 "$@" "$image" >/dev/null || return 1
  while [ "$SECONDS" -lt "$deadline" ]; do
    # The probe's own words and exit are kept: while mongod starts they are
    # the only account of what the probe met (ECONNREFUSED, a missing
    # mongosh, a hang).
    if mongo_ping "$name"; then
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
  echo "::error::MongoDB ($name) did not answer a ping within 60 seconds. $(mongo_ping_said)"
  docker logs --tail 200 "$name" 2>&1 || true
  return 1
}

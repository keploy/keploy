#!/usr/bin/env bash
# E2E: an agent that cannot start fails `keploy mock record|replay` at once,
# with the exit code that names why, and the test command never runs.
#
# The failure this guards is a false green. A CI step that ran keploy where its
# agent could not start (an unprivileged job container, no tracefs) exited 0
# with the tests never run, or waited out the agent's whole ready budget
# first. The reason crosses two process boundaries on the way out: the
# agent's own exit status, then the CLI's. Unit tests can only model those, so
# this runs the real binary in the real environments.
#
# Every case runs the binary as root in a throwaway container with no network,
# so nothing it loads or starts outlives the case:
#   unprivileged   a default container: the kernel refuses eBPF          -> 3
#   no-tracefs     --privileged, but neither debugfs nor tracefs mounted -> 6
# No network is not a reason: a native agent is reached over loopback, and
# mock-offline-linux.sh runs it with nothing but loopback. An agent started
# with --is-docker still needs a non-loopback address, because its hooks send
# the connections of the applications it serves to its container's own one.
#   agent-no-ipv4  `keploy agent --is-docker` in record and test mode, as
#                  the agent container runs it: --privileged, tracefs
#                  mounted, no network                                 -> 6
#                  The agent runs alone, with no test command; docker-6
#                  checks how the CLI passes an agent container's 6 on.
# The docker-* cases run keploy's docker mode with a stand-in `docker` whose
# agent container exits with AGENT_EXIT, as `docker run` does with its
# container's status. Docker mode lowers kernel.perf_event_paranoid, which is
# not namespaced: a --privileged container writing it writes the host's. So
# every docker case that can reach it gets a file of its own mounted over it
# (PIN_PARANOID), which also pins the level it starts from:
#   docker-unprivileged    a default container, as a CI job's is: keploy
#                          lacks the capabilities docker mode needs, and
#                          says so before it changes anything            -> 3
#   docker-sysctl-refused  those capabilities, but /proc/sys read-only (as
#                          in any container that is not --privileged) at
#                          level 4, which keploy has to lower            -> 3
#   docker-sysctl-relaxed  the same, at level 2: keploy has nothing to
#                          write and gets on with starting its agent     -> AGENT_EXIT (6)
#   docker-N, --privileged 3 and 6 are the agent's reasons, 0 an agent
#                          that stopped before it was ready              -> N, N, 1
# (A working environment is mock-linux.sh's job, on the runner itself.)
#
# Env: RECORD_BIN (the keploy binary under test). E2E_IMAGE overrides the
# image, which defaults to the runner's own distribution so the binary finds
# the libc it was built against.
set -uo pipefail

BIN="${RECORD_BIN:-keploy}"
BIN="$(command -v "$BIN" || echo "$BIN")"
# shellcheck disable=SC1091
IMAGE="${E2E_IMAGE:-$(. /etc/os-release && echo "${ID}:${VERSION_ID}")}"
WORK="$(mktemp -d)"
FAIL=0

fail() {
  echo "FAIL: $*"
  FAIL=1
}

if [ -z "${E2E_IMAGE:-}" ]; then
  docker pull -q "$IMAGE" >/dev/null || { echo "FAIL: could not pull $IMAGE"; exit 1; }
fi
cp "$BIN" "$WORK/keploy" || { echo "FAIL: no keploy binary at $BIN"; exit 1; }

# Runs inside each container: $1 is the case. Leaves, per command, its exit
# code and log, and a marker if the test command (or, in docker mode, the
# app's container) ever ran.
cat > "$WORK/inside.sh" <<'SH'
#!/bin/sh
CASE=$1
D=/work/$CASE
mkdir -p "$D/home" "$D/app" "$D/bin"
trap 'chown -R "$HOSTUID" "$D"' EXIT
export HOME="$D/home"
cd "$D/app" || exit 1
CMD="sh -c 'touch $D/ran'"
EXTRA=""
case "$CASE" in
  agent-no-ipv4)
    mount -t tracefs nodev /sys/kernel/tracing || exit 1
    for sub in record replay; do
      mode=record
      [ "$sub" = replay ] && mode=test
      timeout -s INT -k 30 120 /work/keploy agent --is-docker --mode "$mode" --client-pid 1 > "$D/$sub.log" 2>&1
      echo $? > "$D/$sub.exit"
    done
    exit 0
    ;;
  docker-*)
    if [ -n "${PIN_PARANOID:-}" ]; then
      echo "$PIN_PARANOID" > "$D/perf_event_paranoid"
      mount --bind "$D/perf_event_paranoid" /proc/sys/kernel/perf_event_paranoid || exit 1
      if [ -n "${PIN_READ_ONLY:-}" ]; then
        mount -o remount,bind,ro /proc/sys/kernel/perf_event_paranoid || exit 1
      fi
    fi
    # The agent's alias is `sudo docker container run ...`; the app's command
    # is `docker run ...`, and only runs once the agent is up.
    cat > "$D/bin/docker" <<EOD
#!/bin/sh
case "\$1 \${2:-}" in
  "container run") exit ${AGENT_EXIT:?} ;;
  run*) touch $D/ran; exit 0 ;;
esac
exit 0
EOD
    printf '#!/bin/sh\nexec "$@"\n' > "$D/bin/sudo"
    chmod +x "$D/bin/docker" "$D/bin/sudo"
    export PATH="$D/bin:$PATH"
    CMD="docker run --rm --name app alpine true"
    EXTRA="--container-name app"
    ;;
esac
for sub in record replay; do
  # shellcheck disable=SC2086
  timeout -s INT 120 /work/keploy mock "$sub" -c "$CMD" $EXTRA --disable-tele > "$D/$sub.log" 2>&1
  echo $? > "$D/$sub.exit"
done
SH
chmod +x "$WORK/inside.sh"

# check <case> <record/replay exit code> <what the log has to say> <docker flags...>
# The runs are `keploy mock record` and `keploy mock replay`, except for
# agent-no-ipv4, whose are the agent's own record and test modes.
check() {
  local name=$1 want=$2 says=$3
  shift 3
  echo "--- $name: expecting exit $want from its record and replay runs ---"
  docker run --rm --network none "$@" -e HOSTUID="$(id -u):$(id -g)" \
    -v "$WORK:/work" "$IMAGE" /work/inside.sh "$name" || fail "$name: the container itself failed"
  for sub in record replay; do
    local got
    got="$(cat "$WORK/$name/$sub.exit" 2>/dev/null)"
    if [ "$got" != "$want" ]; then
      fail "$name: the $sub run exited ${got:-nothing}, want $want"
      tail -n 30 "$WORK/$name/$sub.log" 2>/dev/null
    fi
    grep -aqF "$says" "$WORK/$name/$sub.log" 2>/dev/null ||
      fail "$name: the $sub run's log does not say \"$says\""
  done
  [ -e "$WORK/$name/ran" ] && fail "$name: the test command ran, with no agent to record or replay it"
}

check unprivileged 3 "keploy does not have the kernel privileges it needs"
check no-tracefs 6 "neither debugfs nor tracefs are mounted" --privileged
check agent-no-ipv4 6 "could not find a non-loopback IP for the container" --privileged
# Mounting over the knob takes CAP_SYS_ADMIN, and a mount AppArmor allows.
CAPS=(--cap-add BPF --cap-add PERFMON --cap-add NET_ADMIN --cap-add SYS_RESOURCE --cap-add SYS_PTRACE
  --cap-add SYS_ADMIN --security-opt apparmor=unconfined)
check docker-unprivileged 3 "keploy does not have the kernel privileges it needs: missing required capabilities" \
  -e AGENT_EXIT=0
check docker-sysctl-refused 3 "keploy does not have the kernel privileges it needs: open /proc/sys/kernel/perf_event_paranoid: read-only file system" \
  "${CAPS[@]}" -e PIN_PARANOID=4 -e PIN_READ_ONLY=1 -e AGENT_EXIT=0
check docker-sysctl-relaxed 6 "this environment lacks something keploy needs" \
  "${CAPS[@]}" -e PIN_PARANOID=2 -e PIN_READ_ONLY=1 -e AGENT_EXIT=6
check docker-3 3 "keploy does not have the kernel privileges it needs" --privileged -e PIN_PARANOID=4 -e AGENT_EXIT=3
check docker-6 6 "this environment lacks something keploy needs" --privileged -e PIN_PARANOID=4 -e AGENT_EXIT=6
check docker-0 1 "the keploy agent exited before it became ready" --privileged -e PIN_PARANOID=4 -e AGENT_EXIT=0

rm -rf "$WORK"
if [ "$FAIL" -ne 0 ]; then
  echo "agent-cannot-start e2e FAILED"
  exit 1
fi
echo "agent-cannot-start e2e passed"

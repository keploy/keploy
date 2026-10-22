#!/usr/bin/env bash
# e2e, under the proven switch: keploy's stop of a `docker run` app at a test
# set's end waits for the app's container to stop (the daemon's wait, an
# event), not for a number of seconds, and kills only when the container made
# no progress on its way out for the stall bound of keploy's own runnable
# time (P101-b-k).
#
#   0. Docker is starved of CPU throughout: the daemon, containerd and its
#      shims at 5% of a CPU, and the app's container at --cpus 0.05. The app's
#      shutdown works 400 ms of CPU (SHUTDOWN_CPU), some 8 s at that share,
#      longer than the 5 s grace keploy's stop used to give a docker run app,
#      which then killed it.
#   1. keploy record's stop of it waits for the container to stop: no kill,
#      and the recording is not marked cut (recordStopKilled).
#   2. Replayed RUNS times (20 by default): every set's end waits for the
#      container to stop, every set passes and keploy test exits 0; no stop
#      kills.
#   3. Docker given its CPU back, an app that ignores SIGINT: its container
#      never stops, and makes no progress, so keploy kills it once the stall
#      bound has passed; the set is not judged, keploy test exits non-zero,
#      and the report names the stall.
#
# Env: RECORD_BIN, REPLAY_BIN (the keploy binary under test), RUNS, LANE_WORK
# (the work dir; a new temporary one by default). The agent
# image the binary names must be loaded (.github/actions/download-image).
# Port 8080 (the app).
set -uo pipefail
# The proven switch, stated for every keploy this lane runs: the steps that
# say KEPLOY_PROVEN=1 run under it, and every other one with it off
# (KEPLOY_PROVEN=0, the open-source build's default, and keploy enterprise's
# on Windows and macOS), as this lane always ran them. Unset, keploy
# enterprise runs the proven model on Linux.
export KEPLOY_PROVEN=0

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
RUNS="${RUNS:-20}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8080
NAME="ff-setend-$$"
IMAGE="ff-app-setend:$$"
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

# LANE_WORK: where the run keeps its logs and reports (CI uploads it).
WORK="${LANE_WORK:-$(mktemp -d)}"
mkdir -p "$WORK/img"
echo "work dir: $WORK"
(cd "$SRC" && CGO_ENABLED=0 go build -o "$WORK/img/ff-app" ./app) || { echo "::error::build failed"; exit 1; }
printf 'FROM scratch\nCOPY ff-app /ff-app\nENTRYPOINT ["/ff-app"]\n' > "$WORK/img/Dockerfile"
sudo docker build -q -t "$IMAGE" "$WORK/img" >/dev/null || { echo "::error::the app's image did not build"; exit 1; }
cd "$WORK" || exit 1

# starve_docker: the Docker daemon, containerd and its shims (what stops and
# removes a container) get 5% of a CPU: on a host with systemd through their
# units' CPUQuota, else (Docker in a container of its own) through a cgroup v2
# of their own. unstarve_docker gives it back.
STARVED=""
STARVE_CG=/sys/fs/cgroup/keploy-lane-starved-docker
starve_docker() {
    if systemctl is-active --quiet docker.service 2>/dev/null; then
        STARVED="docker.service"
        systemctl is-active --quiet containerd.service 2>/dev/null && STARVED="$STARVED containerd.service"
        for u in $STARVED; do sudo systemctl set-property --runtime "$u" CPUQuota=5% || return 1; done
        return 0
    fi
    [ -f /sys/fs/cgroup/cgroup.controllers ] || return 1
    local p cg
    sudo mkdir -p "$STARVE_CG" && echo "5000 100000" | sudo tee "$STARVE_CG/cpu.max" >/dev/null || return 1
    : >"$WORK/starved.pids"
    for p in $(pgrep -x dockerd) $(pgrep -x containerd) $(pgrep containerd-shim); do
        cg="$(sed -n 's/^0:://p' "/proc/$p/cgroup" 2>/dev/null)"
        [ -n "$cg" ] || continue
        echo "$p" | sudo tee "$STARVE_CG/cgroup.procs" >/dev/null && echo "$p $cg" >>"$WORK/starved.pids"
    done
    [ -s "$WORK/starved.pids" ] && STARVED=cgroup
}
unstarve_docker() {
    local p cg u
    case "$STARVED" in
    "") ;;
    cgroup)
        # Each back where it was; a shim started since goes where the
        # daemon was.
        while read -r p cg; do echo "$p" | sudo tee "/sys/fs/cgroup$cg/cgroup.procs" >/dev/null 2>&1; done <"$WORK/starved.pids"
        cg="$(awk 'NR == 1 {print $2}' "$WORK/starved.pids")"
        for p in $(cat "$STARVE_CG/cgroup.procs" 2>/dev/null); do
            echo "$p" | sudo tee "/sys/fs/cgroup$cg/cgroup.procs" >/dev/null 2>&1
        done
        sudo rmdir "$STARVE_CG" 2>/dev/null
        ;;
    *) for u in $STARVED; do sudo systemctl set-property --runtime "$u" CPUQuota= 2>/dev/null; done ;;
    esac
    STARVED=""
}

cleanup() {
    unstarve_docker
    sudo docker rm -f "$NAME" >/dev/null 2>&1
    sudo docker rmi -f "$IMAGE" >/dev/null 2>&1
    return 0
}
trap cleanup EXIT

test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
wait_tests() {
    until [ "$(test_files)" -ge "$1" ]; do
        kill -0 "$2" 2>/dev/null || return 1
        sleep 0.2
    done
}
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
stop_pid() {
    local sudo_pid="$1" child
    child="$(pgrep -P "$sudo_pid" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$sudo_pid" 2>/dev/null
}
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -V | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }
set_status() { grep -m1 '^status:' "$(REPORT)" 2>/dev/null | awk '{print $2}'; }

STARVED_APP="--cpus 0.05 -e SHUTDOWN_CPU=400ms"
starve_docker || { echo "::error::could not starve the Docker daemon of CPU"; exit 1; }
echo "starved: $STARVED"

echo "== 1. recording, Docker starved =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "docker run --rm --name $NAME $STARVED_APP -p $APP:$APP $IMAGE" --container-name "$NAME" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
# The port is the agent's (keploy moves -p to it), so it accepts before the
# app runs: drive the app until it answers.
until curl -sf -o /dev/null "http://127.0.0.1:$APP/c-ping"; do
    kill -0 "$REC_PID" 2>/dev/null || { echo "::error::the app did not start under keploy record"; tail -40 record.log; exit 1; }
    sleep 0.5
done
wait_tests 1 "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want 1"
stop_pid "$REC_PID"
own
grep -q "shutdown worked" record.log || fail "1: the app's shutdown did not run to its end under keploy record"
grep -q "so keploy killed it" record.log && fail "1: keploy record's stop killed the starved app: $(grep -m1 'so keploy killed it' record.log)"
grep -q "recordStopKilled" keploy/test-set-0/config.yaml 2>/dev/null && fail "1: the recording is marked cut: $(grep recordStopKilled keploy/test-set-0/config.yaml)"

# replay NAME [docker run options...]: keploy test of the recording, the app
# run as docker run with the options; RC is its exit code.
replay() {
    local name=$1 before
    shift
    before="$(newest_run)"
    sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "docker run --rm --name $NAME $* -p $APP:$APP $IMAGE" --container-name "$NAME" \
        --delay 2 --generateGithubActions=false --disable-tele >"$name.log" 2>&1 </dev/null
    RC=$?
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$name: keploy test wrote no report (exit $RC)"
        tail -30 "$name.log"
    fi
}

echo "== 2. replayed $RUNS times, Docker starved =="
for i in $(seq 1 "$RUNS"); do
    replay "starved-$i" $STARVED_APP
    if [ "$RC" -ne 0 ] || [ "$(set_status)" != PASSED ] || grep -q "so keploy killed it" "starved-$i.log"; then
        fail "2, run $i: the set must pass, no stop killing (exit $RC, status $(set_status)): $(grep -m1 '^failure_reason:' "$(REPORT)")"
    fi
done
unstarve_docker

echo "== 3. an app that ignores SIGINT =="
replay ignores -e IGNORE_INT=1
if [ "$RC" -eq 0 ] || [ "$(set_status)" != INTERNAL_ERR ]; then
    fail "3: the set must not be judged, keploy test non-zero (exit $RC, status $(set_status))"
fi
grep -m1 '^failure_reason:' "$(REPORT)" | grep -q "made no progress on its way out" ||
    fail "3: the report does not name the stall: $(grep -m1 '^failure_reason:' "$(REPORT)")"

if [ "$FAIL" -ne 0 ]; then
    echo "::error::$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"

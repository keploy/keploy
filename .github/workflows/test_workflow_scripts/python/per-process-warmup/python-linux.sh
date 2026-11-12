#!/bin/bash
# Guard: a per-process warm-up is served to whichever test reaches a cold
# process at replay.
#
# Each app runs two worker processes on one port, and each worker makes a
# read on its first request and then keeps the answer:
#   - jwks:  app.py fetches the JWKS it verifies tokens with (HTTP GET);
#   - mysql: app_mysql.py reads the users table's columns (SHOW FULL FIELDS),
#            as an ORM without a schema cache does.
# At record the read is made by the first request each worker served; at
# replay the kernel may hand a different test to a cold worker. Owned by the
# test that recorded it alone, the read then misses for that other test and
# the test fails (a per-process warm-up recorded as one test's mock).
#
# keploy record now sees which process made each call. Two or more processes
# making the same read once each, on their first request, with one answer, is
# a warm-up: record adds its mocks to the set's worker scope (mappings.yaml
# `worker:`, reason per-process-warmup) and keeps them in their tests' lists,
# so a keploy that does not read the scope replays the set as before. keploy
# test serves them to every test.
#
# Each leg records with --sync (a recording without it writes no
# mappings.yaml, and record adds warm-ups only to a set that has one), sending
# requests until both workers have made the read. It checks the worker scope,
# stops the dependency so replay can only answer from mocks, and replays until
# a run has had a cold worker reached by another test than at record (the app
# logs which request made each read): every run must pass, and at least one
# must be such a run, or the leg proved nothing.

# shellcheck source=../../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../sudo_env.sh" || exit 1

set -uo pipefail
set +e

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

APP_PORT=8090
JWKS_PORT=9100
MYSQL_PORT=3317
MYSQL_CONTAINER=keploy-warmup-mysql
# A run reaches a cold worker by another test than at record about two times
# in three; ten runs leave a leg without one about once in 60,000.
MAX_REPLAYS=${MAX_REPLAYS:-10}
MAX_RECORD_REQUESTS=40
REPLAY_BIN=${REPLAY_BIN:-$RECORD_BIN}

jwks_pid=""
cleanup() {
    echo "cleanup..."
    sudo pkill -9 keploy 2>/dev/null || true
    if [ -n "$jwks_pid" ]; then
        kill -9 "$jwks_pid" 2>/dev/null || true
    fi
    docker rm -f "$MYSQL_CONTAINER" >/dev/null 2>&1 || true
    sleep 1
}
trap cleanup EXIT

cd "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/python/per-process-warmup" || exit 1
rm -rf jwks/ mysql/ jwks-control/ mysql-control/ keploy.yml ./*_logs*.txt

echo "=== install python deps ==="
python3 -m pip install --quiet pymysql || exit 1

# A dependency is reached at the host's own address, not loopback, so the
# call leaves the app as any dependency call does.
HOST_IP=$(hostname -I | awk '{print $1}')
TOKEN=$(python3 make_token.py)

# readers prints, sorted, one per line, the request paths that made the leg's
# read in log $2 ($1 the app's line for it).
readers() {
    grep -a -oE "$1 pid [0-9]+ path /s[0-9]+" "$2" | awk '{print $NF}' | sort -u
}

# cold_by_another says whether a replay's readers ($2) hold a request path no
# reader at record ($1) was: a cold worker was reached by another test than at
# record. Fewer readers (a worker never reached) is not that.
cold_by_another() {
    [ -n "$(comm -13 <(echo "$1") <(echo "$2"))" ]
}

# replay_until_cold replays the set at path $2 (keploy --path) of leg $1
# until a run reaches a cold worker by another test than at record, at most
# MAX_REPLAYS runs and at least $3; $4 is the app's command, $5 its line for
# the read, $6 the readers at record, the app's environment after them. It
# prints, per run, "<status> <cold-by-another: yes|no> <exit code>", the status
# read from the report the run wrote (NONE when it wrote none, RACE on a data
# race), and returns 1 when a port stays busy.
replay_until_cold() {
    local leg=$1 path=$2 min=$3 app=$4 line=$5 recorded=$6
    shift 6
    local env=("$@") r=0 exercised=0 log run_dir status readers other code before
    while [ "$r" -lt "$MAX_REPLAYS" ] && { [ "$exercised" -eq 0 ] || [ "$r" -lt "$min" ]; }; do
        r=$((r + 1))
        port_free >&2 || return 1
        log="${path}_replay_logs_${r}.txt"
        before=$(find "${path}/keploy/reports" -maxdepth 1 -name 'test-run-*' 2>/dev/null | sort)
        sudo env PATH="$PATH" "${SUDO_ENV[@]}" "${env[@]}" "$REPLAY_BIN" test \
            -c "$app" --path "./${path}" --delay 5 --disable-ansi \
            > "$log" 2>&1
        code=$?
        # The report of this run: the one run directory it added.
        run_dir=$(comm -13 <(echo "$before") <(find "${path}/keploy/reports" -maxdepth 1 -name 'test-run-*' 2>/dev/null | sort) | head -1)
        status=NONE
        if grep -q 'WARNING: DATA RACE' "$log"; then
            echo "::error::${leg}: data race detected during a replay of ${path}" >&2
            status=RACE
        elif [ -n "$run_dir" ]; then
            status=$(awk '/^status:/{print $2; exit}' "${run_dir}/test-set-0-report.yaml" 2>/dev/null)
            status=${status:-NONE}
        fi
        readers=$(readers "$line" "$log")
        other=no
        if cold_by_another "$recorded" "$readers"; then
            other=yes
            exercised=$((exercised + 1))
        fi
        echo "${status} ${other} ${code}"
    done
}

# port_free waits until nothing listens on the app's port: with SO_REUSEPORT a
# worker left from the previous run would share it unnoticed.
port_free() {
    for _ in $(seq 1 30); do
        ss -tln | awk '{print $4}' | grep -q ":${APP_PORT}$" || return 0
        sleep 1
    done
    echo "::error::port ${APP_PORT} is still in use"
    ss -tlnp | grep ":${APP_PORT}" || true
    return 1
}

# drive waits for the app, sends authorized requests one after another (the
# kernel spreads them over the two workers) until both workers have made the
# read ($1, the app's line for it, in log $2) and at least ten were sent,
# then stops keploy.
drive() {
    local up=0
    for _ in $(seq 1 60); do
        if ss -tln | awk '{print $4}' | grep -q ":${APP_PORT}$"; then
            up=1
            break
        fi
        sleep 1
    done
    if [ "$up" -ne 1 ]; then
        echo "::error::the app did not listen on ${APP_PORT} within 60s"
        sudo pkill -INT keploy 2>/dev/null || true
        return 1
    fi
    sleep 3
    local failed=0 i=0 code
    while [ "$i" -lt "$MAX_RECORD_REQUESTS" ]; do
        i=$((i + 1))
        code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
            -H "Authorization: Bearer ${TOKEN}" "http://127.0.0.1:${APP_PORT}/s${i}")
        echo "record request s${i}: ${code}"
        [ "$code" = "200" ] || failed=1
        sleep 0.3
        if [ "$i" -ge 10 ] && [ "$(grep -a -c "$1 pid" "$2")" -ge 2 ]; then
            break
        fi
    done
    sleep 5
    sudo pkill -INT keploy 2>/dev/null || true
    return "$failed"
}

# run_leg records and replays one app. $1 the leg's name (its keploy path and
# log prefix), $2 the app's command, $3 the app's line for its read, $4 what
# the read's mocks hold, $5 the command that stops its dependency, $6 the app's
# line when a request failed for want of the read; the app's environment is
# the variables after them ($7...).
run_leg() {
    local leg=$1 app=$2 line=$3 holds=$4 stop_dep=$5 missed=$6
    shift 6
    local env=("$@")
    local rec_log="${leg}_record_logs.txt"

    echo "=== ${leg}: record (--sync) ==="
    port_free || return 1
    drive "$line" "$rec_log" &
    local driver_pid=$!
    sudo env PATH="$PATH" "${SUDO_ENV[@]}" "${env[@]}" "$RECORD_BIN" record \
        -c "$app" --path "./${leg}" --sync --disable-ansi \
        > "$rec_log" 2>&1
    local record_status=$?
    echo "${leg}: record exit status ${record_status}"
    if [ "$record_status" -ne 0 ]; then
        echo "::error::${leg}: keploy record exited with ${record_status}"
        tail -100 "$rec_log"
        return 1
    fi
    if ! wait "$driver_pid"; then
        echo "::error::${leg}: a request failed while recording"
        tail -100 "$rec_log"
        return 1
    fi
    if grep -q 'WARNING: DATA RACE' "$rec_log"; then
        echo "::error::${leg}: data race detected during record"
        tail -200 "$rec_log"
        return 1
    fi
    local made
    made=$(grep -a -c "$line pid" "$rec_log")
    if [ "$made" -lt 2 ]; then
        echo "::error::${leg}: only ${made} worker(s) made the read in ${MAX_RECORD_REQUESTS} requests at record; the leg needs both"
        tail -100 "$rec_log"
        return 1
    fi
    local recorded_readers
    recorded_readers=$(readers "$line" "$rec_log")
    echo "${leg}: record: read made by $(echo "$recorded_readers" | tr '\n' ' ')"

    echo "=== ${leg}: check the worker scope ==="
    local set="${leg}/keploy/test-set-0"
    if [ ! -s "${set}/mappings.yaml" ]; then
        echo "::error::${leg}: no mappings.yaml was recorded"
        ls -la "$set" 2>/dev/null
        return 1
    fi
    # Each mock of the read must be in the worker scope, marked, and still
    # in the list of the test that recorded it.
    if ! python3 - "${set}/mappings.yaml" "${set}/mocks.yaml" "$holds" <<'EOF'
import re, sys
mappings, mocks, holds = open(sys.argv[1]).read(), open(sys.argv[2]).read(), sys.argv[3]
reads = [re.search(r"^name: (\S+)", d, re.M).group(1)
         for d in mocks.split("\n---") if holds in d and re.search(r"^name: ", d, re.M)]
def section(name):
    """The lines of a top-level section of mappings.yaml, to the next one."""
    m = re.search(r"^" + name + r":\n((?:[ -].*\n?)*)", mappings, re.M)
    return m.group(1) if m else ""
worker_part, tests_part = section("worker"), section("tests")
worker = re.findall(r"- name: (\S+)", worker_part)
marked = re.findall(r"- name: (\S+)\n(?:\s+\w+: .*\n)*?\s+reason: per-process-warmup\b", worker_part)
print("mocks of the read:", reads, "worker scope:", worker, "marked:", marked)
# Exactly the read's mocks are marked: no other call taken for a warm-up.
ok = len(reads) >= 2 and sorted(marked) == sorted(reads)
for n in reads:
    if not re.search(r"- name: " + re.escape(n) + r"\n", tests_part):
        print("not in any test's list:", n)
        ok = False
sys.exit(0 if ok else 1)
EOF
    then
        echo "::error::${leg}: the read's mocks are not in the worker scope as per-process warm-ups"
        cat "${set}/mappings.yaml"
        grep -E 'warm-up' "$rec_log"
        return 1
    fi

    echo "=== ${leg}: stop the dependency: replay answers from mocks only ==="
    eval "$stop_dep"

    # The negative control: the same set with its warm-ups taken out of the
    # worker scope, as a keploy that does not add them records it. A run that
    # reaches a cold worker by another test than at record must fail on it,
    # or the leg's passes below prove nothing.
    local control="${leg}-control"
    rm -rf "./${control}" && cp -r "./${leg}" "./${control}" && rm -rf "./${control}/keploy/reports"
    python3 - "${control}/keploy/test-set-0/mappings.yaml" <<'PYEOF'
import re, sys
p = sys.argv[1]
s = open(p).read()
# The worker section alone, to the next top-level key.
open(p, "w").write(re.sub(r"^worker:\n(?:[ -].*\n?)*", "", s, flags=re.M))
PYEOF
    local results failed=0 exercised=0 runs=0 status other code
    results=$(replay_until_cold "$leg" "$control" 1 "$app" "$line" "$recorded_readers" "${env[@]}") || return 1
    while read -r status other code; do
        runs=$((runs + 1))
        echo "${leg}: control replay ${runs}: ${status}, exit ${code} (cold worker reached by another test than at record: ${other})"
        if [ "$other" = yes ]; then
            exercised=$((exercised + 1))
            # Failed on the read: a report that says so, and the app's word
            # that a request went without it.
            if [ "$status" != FAILED ] || ! grep -aq "$missed" "${control}_replay_logs_${runs}.txt"; then
                echo "::error::${leg}: a control run that reached a cold worker by another test than at record did not fail on the read (status ${status}): the leg cannot tell the fix from its absence"
                grep -aE "$line|$missed|Total test" "${control}_replay_logs_${runs}.txt" | tail -20
                failed=1
            fi
        fi
    done <<< "$results"
    if [ "$exercised" -eq 0 ]; then
        echo "::error::${leg}: in ${runs} control replays no cold worker was reached by another test than at record; the leg proved nothing"
        return 1
    fi

    # The set as recorded: every run must pass, and one must reach a cold
    # worker by another test than at record.
    runs=0
    exercised=0
    results=$(replay_until_cold "$leg" "$leg" 2 "$app" "$line" "$recorded_readers" "${env[@]}") || return 1
    while read -r status other code; do
        runs=$((runs + 1))
        echo "${leg}: replay ${runs}: ${status}, exit ${code} (cold worker reached by another test than at record: ${other})"
        [ "$other" = yes ] && exercised=$((exercised + 1))
        if [ "$status" != PASSED ] || [ "$code" -ne 0 ]; then
            echo "::error::${leg}: replay ${runs} did not pass: a test that reached a cold worker missed the read"
            grep -aE "$line|Total test|mock mismatch" "${leg}_replay_logs_${runs}.txt" | tail -30
            failed=1
        fi
    done <<< "$results"
    if [ "$failed" -ne 0 ]; then
        return 1
    fi
    if [ "$exercised" -eq 0 ]; then
        echo "::error::${leg}: in ${runs} replays no cold worker was reached by another test than at record; the leg proved nothing"
        return 1
    fi
    echo "${leg}: PASS: ${runs} replays passed, ${exercised} with a cold worker reached by another test than at record; the control failed such a run"
}

echo "=== start the JWKS upstream ==="
python3 jwks.py "$JWKS_PORT" > jwks_logs.txt 2>&1 &
jwks_pid=$!
for _ in $(seq 1 20); do
    ss -tln | awk '{print $4}' | grep -q ":${JWKS_PORT}$" && break
    sleep 1
done
run_leg jwks "python3 app.py ${APP_PORT} 2" "jwks attempt" "jwks.json" \
    'kill -9 "$jwks_pid" 2>/dev/null; jwks_pid=""' "jwks fetch failed" \
    JWKS_URL="http://${HOST_IP}:${JWKS_PORT}/.well-known/jwks.json" || exit 1

echo "=== start MySQL (throwaway) ==="
docker rm -f "$MYSQL_CONTAINER" >/dev/null 2>&1
docker run -d --name "$MYSQL_CONTAINER" -p "${MYSQL_PORT}:3306" \
    -e MYSQL_ROOT_PASSWORD=repro -e MYSQL_DATABASE=app mysql:8 >/dev/null || exit 1
ready=0
for _ in $(seq 1 90); do
    # Over TCP: the image's entrypoint first runs a server with networking
    # off to initialise, then restarts it; only the real server answers here.
    if docker exec "$MYSQL_CONTAINER" mysqladmin ping -h127.0.0.1 --protocol=tcp -uroot -prepro --silent >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 2
done
if [ "$ready" -ne 1 ]; then
    echo "::error::MySQL did not become ready"
    docker logs "$MYSQL_CONTAINER" 2>&1 | tail -30
    exit 1
fi
docker exec "$MYSQL_CONTAINER" mysql -uroot -prepro app -e \
    "CREATE TABLE users (id INT PRIMARY KEY, name VARCHAR(50)); INSERT INTO users VALUES (1,'a'),(2,'b'),(3,'c'),(4,'d'),(5,'e'),(6,'f'),(7,'g'),(8,'h'),(9,'i'),(10,'j');" 2>/dev/null || exit 1
run_leg mysql "python3 app_mysql.py ${APP_PORT} 2" "schema attempt" "SHOW FULL FIELDS" \
    'docker rm -f "$MYSQL_CONTAINER" >/dev/null 2>&1' "request failed" \
    DB_HOST="$HOST_IP" DB_PORT="$MYSQL_PORT" || exit 1

echo "PASS: per-process warm-ups were served to whichever test reached a cold worker (jwks and mysql)"

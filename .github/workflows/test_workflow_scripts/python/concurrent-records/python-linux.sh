#!/bin/bash
# Two `keploy record` runs started together on one host, on the default
# ports. Each CLI checks its ports free before its agent binds them, and both
# could find them free; the second agent then failed to bind. Now the agent's
# bind is the check: an agent whose port is taken when it binds it binds
# another, says so naming the holder, and tells its hooks before it is ready.
#
# On every request each app resolves a DNS name of its own run's and calls an
# upstream on a path of its own run's (app.py). In every round each run must
# record with no bind failure, its app must get its own upstream answers, and
# its mocks must hold its own run's DNS names and upstream paths and none of
# the other run's: hooks still sending an app to a port its agent left, or to
# the other run's, would lose its names or put them in the other run's mocks.
#
# Rounds:
# - "together": the runs as reported. Whether the CLI's check or the agent's
#   bind finds the other run's port taken depends on how their starts
#   interleave; either way both runs must record. The moves are printed.
# - "dns-held": the same, with a process holding UDP 26789 only. The CLI
#   checks the port over TCP and finds it free, so each agent finds it taken
#   at its bind, every time: both must move their DNS servers, naming that
#   process, and their apps' DNS must still reach their own moved servers.

# keploy runs under sudo, which hands it only the variables its command line
# names (sudo-rs, Ubuntu 26.04's sudo, ignores -E): SUDO_ENV names the
# switches a workflow sets for keploy, each only while it is set.
# shellcheck source=../../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../sudo_env.sh" || exit 1

set -uo pipefail
set +e

TOGETHER_ROUNDS=${TOGETHER_ROUNDS:-2}
UPSTREAM_PORT=9092
dir="$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/python/concurrent-records"
cd "$dir" || exit 1

upstream_pid=""
holder_pid=""
record_pids=()
cleanup() {
    # timeout(1) stops its keploy (SIGINT, then SIGKILL 30s on) when it
    # ends; a run still going here is stopped through its timeout.
    for p in "${record_pids[@]}"; do
        sudo kill -INT "$p" 2>/dev/null
    done
    [ -n "$holder_pid" ] && kill "$holder_pid" 2>/dev/null
    [ -n "$upstream_pid" ] && kill "$upstream_pid" 2>/dev/null
}
trap cleanup EXIT

python3 upstream.py "$UPSTREAM_PORT" > upstream_logs.txt 2>&1 &
upstream_pid=$!
up=0
for _ in $(seq 1 20); do
    if curl -fsS -m 2 -o /dev/null "http://127.0.0.1:$UPSTREAM_PORT/ready"; then
        up=1
        break
    fi
    sleep 1
done
[ "$up" = 1 ] || { echo "::error::the upstream did not start"; cat upstream_logs.txt; exit 1; }

# count PATTERN FILE...: how many lines of the files match (0 for no file).
count() {
    local pattern=$1 n
    shift
    n=$(cat "$@" /dev/null 2>/dev/null | grep -a -c -E -- "$pattern")
    echo "${n:-0}"
}

fail=0
# round NAME: starts runs A and B together, drives each app, and checks what
# each recorded.
round() {
    local name=$1 run tag app_port ready got k p
    echo "=== round $name: two records started together ==="
    sudo rm -rf A B # the last round's, which root-run keploy wrote
    mkdir -p A B
    record_pids=()
    for run in A B; do
        if [ "$run" = A ]; then tag=runa app_port=8081; else tag=runb app_port=8082; fi
        (cd "$run" && exec timeout -s INT -k 30 120 sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$RECORD_BIN" record \
            -c "python3 $dir/app.py $tag $app_port 127.0.0.1 $UPSTREAM_PORT" \
            --path "$dir/$run" --record-timer 40s --generateGithubActions=false \
            > "$dir/record_logs_${run}_$name.txt" 2>&1) &
        record_pids+=($!)
    done

    ready=0
    for _ in $(seq 1 60); do
        if curl -fsS -m 5 -o /dev/null http://127.0.0.1:8081/ready && curl -fsS -m 5 -o /dev/null http://127.0.0.1:8082/ready; then
            ready=1
            break
        fi
        sleep 0.5
    done
    if [ "$ready" = 1 ]; then
        for k in 1 2 3; do
            for run in A B; do
                if [ "$run" = A ]; then tag=runa app_port=8081; else tag=runb app_port=8082; fi
                got=$(curl -fsS -m 15 "http://127.0.0.1:$app_port/q$k")
                if [ "$got" != "up:/${tag}up/xq$k" ]; then
                    echo "::error::round $name run $run: /q$k answered '$got', want 'up:/${tag}up/xq$k'"
                    fail=1
                fi
            done
        done
    else
        echo "::error::round $name: the two apps did not both answer under keploy record"
        fail=1
    fi
    for p in "${record_pids[@]}"; do wait "$p"; done
    record_pids=()

    local logs=("record_logs_A_$name.txt" "record_logs_B_$name.txt") log
    for log in "${logs[@]}"; do
        if [ ! -s "$log" ]; then
            echo "::error::round $name: keploy record wrote no log to $log"
            fail=1
        fi
    done
    echo "round $name: the agents' moves:"
    cat "${logs[@]}" | grep -a -o -E "the (proxy|DNS server)'s port [0-9]+ is taken by [^;]*; the (proxy|DNS server) listens on port [0-9]+ instead" | sed 's/^/  /'

    local own other mocks tests binds own_dns other_dns own_up other_up
    for run in A B; do
        if [ "$run" = A ]; then own=runa other=runb; else own=runb other=runa; fi
        mapfile -t mocks < <(find "$run/keploy" -name mocks.yaml 2>/dev/null)
        tests=$(find "$run/keploy" -path '*/tests/*.yaml' 2>/dev/null | wc -l)
        binds=$(count 'address already in use' "record_logs_${run}_$name.txt")
        own_dns=$(cat "${mocks[@]}" /dev/null | grep -a -o -E "${own}dnsxq[1-3]\." | sort -u | wc -l)
        other_dns=$(count "${other}dns" "${mocks[@]}")
        own_up=$(cat "${mocks[@]}" /dev/null | grep -a -o -E "/${own}up/xq[1-3]" | sort -u | wc -l)
        other_up=$(count "/${other}up/" "${mocks[@]}")
        echo "round $name run $run: tests $tests, bind failures $binds, own DNS names $own_dns/3, other run's DNS lines $other_dns, own upstream paths $own_up/3, other run's upstream lines $other_up"
        if [ "$binds" != 0 ] || [ "$tests" -lt 3 ] || [ "$own_dns" != 3 ] || [ "$other_dns" != 0 ] || [ "$own_up" != 3 ] || [ "$other_up" != 0 ]; then
            echo "::error::round $name run $run did not record its own app's traffic alone"
            grep -a -i -E 'error|taken|listens on port' "record_logs_${run}_$name.txt" | tail -20
            fail=1
        fi
    done
}

for r in $(seq 1 "$TOGETHER_ROUNDS"); do
    round "together-$r"
done

python3 -c '
import socket, sys, time
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("0.0.0.0", 26789))
print("held", flush=True)
time.sleep(3600)
' > holder_logs.txt 2>&1 &
holder_pid=$!
held=0
for _ in $(seq 1 20); do
    if grep -q held holder_logs.txt; then
        held=1
        break
    fi
    sleep 0.5
done
if [ "$held" != 1 ]; then
    echo "::error::nothing could hold UDP 26789"
    cat holder_logs.txt
    exit 1
fi
round dns-held
moves=$(count "the DNS server's port 26789 is taken by pid $holder_pid \\([^)]*\\); the DNS server listens on port [0-9]+ instead" record_logs_A_dns-held.txt record_logs_B_dns-held.txt)
if [ "$moves" != 2 ]; then
    echo "::error::with UDP 26789 held by pid $holder_pid, want both agents to move their DNS servers naming it; $moves did"
    fail=1
fi
kill "$holder_pid" 2>/dev/null
holder_pid=""

if [ "$fail" != 0 ]; then
    echo "::error::two records started together did not each record their own app"
    exit 1
fi
echo "PASS: two records started together each recorded their own app's traffic, moving off a taken port where their agent found one"

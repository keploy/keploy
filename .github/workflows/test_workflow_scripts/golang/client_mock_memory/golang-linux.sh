#!/bin/bash
# keploy test does not hold a test set's mocks in memory. It reads the set's
# mocks.yaml into a small entry per mock, and reads each mock's payload back
# from the file only to send it to the agent. A keploy that loads the set
# whole holds about twice the file at the read's peak: on recordings of a few
# GB that ran CI runners, and the api-server's prod lane, out of memory
# (Killed, exit 137).
#
# This lane records http-pokeapi, adds INFLATE_MB mocks of 1 MiB each to every
# test set's mocks.yaml (calls to a host the app never reaches, a quarter of
# them session mocks, the rest per-test), and replays. Every set must pass,
# the agent must be sent every mock of the largest set (so the lane cannot
# pass by not loading them), each read back from the file as it was sent, and
# the keploy test process's peak RSS (VmHWM) must stay under
# CLI_PEAK_LIMIT_MB. A keploy that loads the set whole reads none back, and
# peaks above the largest mocks.yaml.

source "$(dirname "${BASH_SOURCE[0]}")/../../go-retry.sh"

echo "$RECORD_BIN"
echo "$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

INFLATE_MB="${INFLATE_MB:-600}"
CLI_PEAK_LIMIT_MB="${CLI_PEAK_LIMIT_MB:-300}"

rm -f ./keploy.yml
rm -rf keploy/

go_retry build -o http-pokeapi || exit 1
echo "go binary built"

cat > ./keploy.yml <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"body": {"updated_at":[]}}
KEPLOY_CFG

send_request() {
    sleep 6
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -s -X GET http://localhost:8080/api/locations >/dev/null; then
            app_started=true
        fi
        sleep 3
    done
    echo "App started"

    response=$(curl -s -X GET http://localhost:8080/api/locations)
    location=$(echo "$response" | jq -r ".location[1]")
    curl -s -X GET "http://localhost:8080/api/locations/$location" >/dev/null
    curl -s -X GET http://localhost:8080/api/greet
    curl -s -X GET "http://localhost:8080/api/greet?format=html"

    # Wait for Keploy to record the tcs and mocks.
    sleep 7
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing Keploy"
    sudo kill $pid
}

send_request &
"$RECORD_BIN" record -c "./http-pokeapi" --generateGithubActions=false 2>&1 | tee record_logs.txt
wait
if grep "ERROR" record_logs.txt; then
    echo "::error::Error found in the recording"
    exit 1
fi

shopt -s nullglob
mock_files=( ./keploy/test-set-*/mocks.yaml )
shopt -u nullglob
if [ ${#mock_files[@]} -eq 0 ]; then
    echo "::error::the recording wrote no mocks.yaml"
    exit 1
fi

# Appends n mocks of 1 MiB each to a mocks.yaml: GETs of a host the app never
# calls, so no test is served one, every fourth a session (config) mock and
# the others per-test, recorded before the set's first test.
inflate() {
    python3 - "$1" "$2" <<'PY'
import sys
path, n = sys.argv[1], int(sys.argv[2])
body = "x" * (1 << 20)
with open(path, "rb") as f:
    last = b"\n"
    if f.seek(0, 2) > 0:
        f.seek(-1, 2)
        last = f.read(1)
with open(path, "a") as f:
    if last != b"\n":
        f.write("\n")
    for i in range(n):
        kind = "config" if i % 4 == 0 else "mocks"
        f.write(f"""---
version: api.keploy.io/v1beta1
kind: Http
name: inflate-{i}
spec:
    metadata:
        name: Http
        type: {kind}
    req:
        method: GET
        proto_major: 1
        proto_minor: 1
        url: http://inflate.keploy.invalid/blob/{i}
        header:
            Accept: '*/*'
        body: ""
        timestamp: 2026-01-01T00:00:00Z
    resp:
        status_code: 200
        header:
            Content-Type: text/plain
        body: {body}
        status_message: OK
        proto_major: 1
        proto_minor: 1
        timestamp: 2026-01-01T00:00:00Z
    objects: []
    created: 1767225600
    reqTimestampMock: 2026-01-01T00:00:00Z
    resTimestampMock: 2026-01-01T00:00:00.001Z
""")
PY
}

largest_mb=0
for f in "${mock_files[@]}"; do
    inflate "$f" "$INFLATE_MB"
    mb=$(( $(stat -c %s "$f") / 1048576 ))
    echo "$f: ${mb} MiB"
    [ "$mb" -gt "$largest_mb" ] && largest_mb=$mb
done

# The replay, its keploy test process sampled for its peak RSS until it ends.
"$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --generateGithubActions=false > test_logs.txt 2>&1 &
test_pid=$!
peak_kb=0
while kill -0 "$test_pid" 2>/dev/null; do
    hwm=$(awk '/^VmHWM:/ {print $2}' "/proc/$test_pid/status" 2>/dev/null)
    if [ -n "$hwm" ] && [ "$hwm" -gt "$peak_kb" ]; then
        peak_kb=$hwm
    fi
    sleep 0.5
done
wait "$test_pid"
test_exit=$?
cat test_logs.txt
peak_mb=$(( peak_kb / 1024 ))
echo "keploy test exited $test_exit; its peak RSS was ${peak_mb} MiB, the largest mocks.yaml ${largest_mb} MiB"

if grep "ERROR" test_logs.txt; then
    echo "::error::Error found in the replay"
    exit 1
fi
if grep "WARNING: DATA RACE" test_logs.txt; then
    echo "::error::Race condition detected in the replay"
    exit 1
fi

shopt -s nullglob
reports=( ./keploy/reports/test-run-0/test-set-*-report.yaml )
shopt -u nullglob
if [ ${#reports[@]} -ne ${#mock_files[@]} ]; then
    echo "::error::the replay wrote ${#reports[@]} test-set reports for ${#mock_files[@]} test sets"
    exit 1
fi
for report_file in "${reports[@]}"; do
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "$(basename "$report_file"): $test_status"
    if [ "$test_status" != "PASSED" ]; then
        echo "::error::$(basename "$report_file") is $test_status with ${INFLATE_MB} unused 1 MiB mocks in its mocks.yaml"
        exit 1
    fi
done

# The agent was sent every mock of the largest set, inflated ones included,
# each read back from the set's mocks.yaml as it was sent: the CLI says, for
# each set it stores, how many mocks it stored and how many of them it read
# back (payloadsReadBack; a set read whole has none).
read -r stored read_back < <(python3 - test_logs.txt <<'PY'
import json, re, sys
best = (0, 0)
for line in open(sys.argv[1], errors="replace"):
    line = re.sub(r"\x1b\[[0-9;]*m", "", line)
    at = line.find("stored the mocks on the agent")
    if at < 0 or "{" not in line[at:]:
        continue
    try:
        f = json.loads(line[line.index("{", at):])
    except ValueError:
        continue
    best = max(best, (f.get("mocks", 0), f.get("payloadsReadBack", 0)))
print(*best)
PY
)
if [ "${stored:-0}" -lt "$INFLATE_MB" ]; then
    echo "::error::the agent was sent at most ${stored:-no} mocks of a set; the inflated sets hold more than ${INFLATE_MB}, so the replay did not load them and the peak below proves nothing"
    exit 1
fi
if [ "${read_back:-0}" -ne "$stored" ]; then
    echo "::error::keploy test read ${read_back:-no} of the ${stored} mocks it sent the agent back from the set's mocks.yaml; it reads every one back as it sends it, holding a small entry per mock until then"
    exit 1
fi

if [ "$peak_mb" -ge "$CLI_PEAK_LIMIT_MB" ]; then
    echo "::error::keploy test peaked at ${peak_mb} MiB of RSS replaying test sets whose largest mocks.yaml is ${largest_mb} MiB; the limit is ${CLI_PEAK_LIMIT_MB} MiB. keploy test holds a small entry per mock and reads each mock's payload back from the file only to send it; a peak near or above the file's size means it held the set in memory."
    exit 1
fi
echo "✓ keploy test peaked at ${peak_mb} MiB replaying ${largest_mb} MiB of mocks (limit ${CLI_PEAK_LIMIT_MB} MiB); every set passed"
exit 0

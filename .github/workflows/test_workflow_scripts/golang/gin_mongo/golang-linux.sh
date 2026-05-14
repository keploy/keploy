#!/bin/bash

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

dump_gin_mongo_ci_diagnostics() {
    echo "===== docker ps -a ====="
    docker ps -a || true

    echo "===== mongoDb logs (last 200 lines) ====="
    if docker ps -a --format '{{.Names}}' | grep -qx 'mongoDb'; then
        docker logs --tail 200 mongoDb || true
    else
        echo "mongoDb container not found"
    fi

    echo "===== candidate Keploy/app log files under $(pwd) ====="
    if find . -type f \( -iname '*keploy*.log' -o -iname '*app*.log' -o -name '*.txt' \) -print | sed 's|^\./||' | grep -q '.'; then
        find . -type f \( -iname '*keploy*.log' -o -iname '*app*.log' -o -name '*.txt' \) -print | sed 's|^\./||'
    else
        echo "No Keploy/app log files matching '*keploy*.log', '*app*.log', or '*.txt' were found in the workspace"
    fi
}

# Start mongo before starting keploy.
docker rm -f mongoDb >/dev/null 2>&1 || true
docker run --rm -d -p27017:27017 --name mongoDb mongo
trap 'docker rm -f mongoDb >/dev/null 2>&1 || true' EXIT

mongo_ready=false
for mongo_attempt in {1..30}; do
    if docker exec mongoDb mongosh --quiet --eval 'db.adminCommand({ ping: 1 }).ok' | grep -q '^1$'; then
        mongo_ready=true
        break
    fi
    sleep 2
done

if [ "$mongo_ready" != true ]; then
    echo "::error::MongoDB did not become ready within 60 seconds. Next steps: verify the Docker daemon is available, check for image pull or container startup errors, and inspect the mongoDb logs printed below."
    dump_gin_mongo_ci_diagnostics
    exit 1
fi

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

# Generate the keploy-config file.
sudo "$RECORD_BIN" config --generate

# Update the global noise to ts.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

sed -i 's/ports: 0/ports: 27017/' "$config_file"

# Remove any preexisting keploy tests and mocks.
rm -rf keploy/

# Build the binary.
go build -cover -coverpkg=./... -o ginApp

stop_recording(){
    local kp_pid="${1:-}"
    local rec_pid

    rec_pid="$(pgrep -n -f "$(basename "${RECORD_BIN:-keploy}") record" || true)"
    echo "$rec_pid Keploy PID"
    echo "Killing keploy"
    if [ -n "$rec_pid" ]; then
        sudo kill -INT "$rec_pid" 2>/dev/null || true
        # Wait for keploy to flush and exit (up to 30s).
        for stop_attempt in {1..30}; do
            kill -0 "$rec_pid" 2>/dev/null || break
            sleep 1
        done
        # SIGINT-then-SIGKILL escalation. On keploy/keploy#4077 run
        # 24631193918 a single gin-mongo record iteration hung the
        # entire job for 140+ minutes because keploy didn't respond
        # to SIGINT and `wait` at the bottom of the loop blocked
        # forever on its tee'd stdout. Dumping goroutines before
        # SIGKILL gives us the actual RCA on the next hang instead
        # of a blank 6-hour timeout. SIGQUIT is Go-runtime-friendly
        # and prints all goroutine stacks to stderr, which the
        # tee above captures into ${app_name}.txt.
        if kill -0 "$rec_pid" 2>/dev/null; then
            echo "::error::keploy did not exit within 30s of SIGINT (pid $rec_pid). Dumping goroutines via SIGQUIT, then escalating to SIGKILL."
            sudo kill -QUIT "$rec_pid" 2>/dev/null || true
            sleep 3
            sudo kill -9 "$rec_pid" 2>/dev/null || true
            # Brief pause so the tee pipe has a chance to drain
            # before the outer `wait` returns and the loop moves on.
            sleep 2
            return 1
        fi
    elif [ -n "$kp_pid" ]; then
        sudo kill -INT "$kp_pid" 2>/dev/null || true
    else
        echo "No keploy process found to kill."
    fi
}

send_request(){
    local kp_pid="$1"

    # Bound the readiness loop so a never-starting app (keploy
    # stuck, mongo stuck, docker stuck, anything) fails the
    # iteration in 5 minutes instead of hanging the whole job
    # for hours — see run 24631193918 where this loop was the
    # suspected entry point into a 140-minute gin-mongo hang.
    local deadline=$(( $(date +%s) + 300 ))
    app_started=false
    while [ "$app_started" = false ]; do
        if [ "$(date +%s)" -gt "$deadline" ]; then
            echo "::error::gin-mongo app did not respond on http://localhost:8080 within 5 minutes. Next steps: review 'docker ps -a' and the mongoDb logs printed below, then inspect any Keploy/app log files listed for this workspace."
            dump_gin_mongo_ci_diagnostics
            stop_recording "$kp_pid"
            return 1
        fi
        if curl --silent --fail --max-time 5 --output /dev/null --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }'; then
            app_started=true
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done
    echo "App started"
    # Start making curl calls to record the testcases and mocks.
    curl --fail --show-error --max-time 30 --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://google.com"
    }' || { stop_recording "$kp_pid"; return 1; }

    curl --fail --show-error --max-time 30 --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }' || { stop_recording "$kp_pid"; return 1; }

    curl --fail --show-error --max-time 30 -X GET http://localhost:8080/CJBKJd92 || { stop_recording "$kp_pid"; return 1; }

    # Test email verification endpoint
    curl --fail --show-error --max-time 30 --request GET \
      --url 'http://localhost:8080/verify-email?email=test@gmail.com' \
      --header 'Accept: application/json' || { stop_recording "$kp_pid"; return 1; }

    curl --fail --show-error --max-time 30 --request GET \
      --url 'http://localhost:8080/verify-email?email=admin@yahoo.com' \
      --header 'Accept: application/json' || { stop_recording "$kp_pid"; return 1; }

    # Wait for keploy to record the tcs and mocks.
    sleep 10
    stop_recording "$kp_pid" || return 1
}

has_unexpected_errors() {
    local log_file="$1"
    grep "ERROR" "$log_file" | grep -Ev "failed to read from connection.*use of closed network connection"
}


for record_iteration in {1..2}; do
    app_name="ginMongo_${record_iteration}"
    "$RECORD_BIN" record -c "./ginApp" 2>&1 | tee "${app_name}.txt" &
    
    KEPLOY_PID=$!

    # Drive traffic and stop keploy (will fail the pipeline if health never comes up).
    if ! send_request "$KEPLOY_PID"; then
        echo "::error::Failed to drive gin-mongo traffic for record iteration ${record_iteration}"
        cat "${app_name}.txt" || true
        exit 1
    fi

    if unexpected_errors="$(has_unexpected_errors "${app_name}.txt")"; then
        echo "Error found in pipeline..."
        printf '%s\n' "$unexpected_errors"
        cat "${app_name}.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "${app_name}.txt"; then
      echo "Race condition detected in recording, stopping pipeline..."
      cat "${app_name}.txt"
      exit 1
    fi
    sleep 5
    wait
    echo "Recorded test case and mocks for iteration ${record_iteration}"
done

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    # Additional record pass with --storage-format json. The new test-sets
    # land alongside the yaml ones in the same keploy/ tree
    # (FindLastIndexAny picks the next free index across both extensions),
    # so the subsequent default-format replay exercises the read-side
    # auto-detect path over a directory that mixes yaml and json fixtures.
    for record_iteration in {1..2}; do
        app_name="ginMongo_json_${record_iteration}"
        "$RECORD_BIN" record --storage-format json -c "./ginApp" 2>&1 | tee "${app_name}.txt" &

        KEPLOY_PID=$!

        if ! send_request "$KEPLOY_PID"; then
            echo "::error::Failed to drive gin-mongo traffic for json record iteration ${record_iteration}"
            cat "${app_name}.txt" || true
            exit 1
        fi

        if unexpected_errors="$(has_unexpected_errors "${app_name}.txt")"; then
            echo "Error found in pipeline..."
            printf '%s\n' "$unexpected_errors"
            cat "${app_name}.txt"
            exit 1
        fi
        if grep "WARNING: DATA RACE" "${app_name}.txt"; then
          echo "Race condition detected in json recording, stopping pipeline..."
          cat "${app_name}.txt"
          exit 1
        fi
        sleep 5
        wait
        echo "Recorded json test case and mocks for iteration ${record_iteration}"
    done
else
    echo "Skipping --storage-format json record pass: at least one binary is the released keploy (no json support)."
fi

# Keep MongoDB running during test replay. Keploy will serve mocks for
# matched requests; unmatched requests fall through to the real database
# which returns the same data recorded earlier, preventing flaky failures
# caused by non-deterministic mock matching across test sets.

# Start the gin-mongo app in test mode.
"$REPLAY_BIN" test -c "./ginApp" --delay 7   2>&1 | tee test_logs.txt
replay_status=${PIPESTATUS[0]}

cat test_logs.txt || true

if [ "$replay_status" -ne 0 ]; then
  echo "::error::Keploy replay failed with exit code ${replay_status}"
  cat test_logs.txt
  exit "$replay_status"
fi

# ✅ Extract and validate coverage percentage from log
coverage_line=$(grep -Eo "Total Coverage Percentage:[[:space:]]+[0-9]+(\.[0-9]+)?%" "test_logs.txt" | tail -n1 || true)

if [[ -z "$coverage_line" ]]; then
  echo "::error::No coverage percentage found in test_logs.txt"
  cat test_logs.txt
  exit 1
fi

coverage_percent=$(echo "$coverage_line" | grep -Eo "[0-9]+(\.[0-9]+)?" || echo "0")
echo "📊 Extracted coverage: ${coverage_percent}%"

# Compare coverage with threshold (50%)
if (( $(echo "$coverage_percent < 50" | bc -l) )); then
  echo "::error::Coverage below threshold (50%). Found: ${coverage_percent}%"
  cat test_logs.txt
  exit 1
else
  echo "✅ Coverage meets threshold (>= 50%)"
fi

if unexpected_errors="$(has_unexpected_errors "test_logs.txt")"; then
    echo "Error found in pipeline..."
    printf '%s\n' "$unexpected_errors"
    cat "test_logs.txt"
    exit 1
fi

if grep "WARNING: DATA RACE" "test_logs.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs.txt"
    exit 1
fi

all_passed=true


# Default-format (yaml) report scan. Glob picks up reports for every
# test-set in test-run-0 — including the json-recorded ones, since the
# default replay above wrote yaml reports for the entire directory.
shopt -s nullglob
yaml_reports=( ./keploy/reports/test-run-0/test-set-*-report.yaml )
shopt -u nullglob
if [ ${#yaml_reports[@]} -eq 0 ]; then
    echo "::error::No yaml test-set reports found under ./keploy/reports/test-run-0/"
    cat "test_logs.txt"
    exit 1
fi
for report_file in "${yaml_reports[@]}"; do
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "yaml report $(basename "$report_file"): $test_status"
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "$(basename "$report_file") did not pass."
        break
    fi
done

if json_pass_supported; then
    # Second replay pass with --storage-format json — exercises the json
    # write path AND, because the keploy/ tree holds both yaml- and
    # json-recorded test-sets, validates that a json-mode replay reads
    # the yaml fixtures via the read-side auto-detect path.
    "$REPLAY_BIN" test --storage-format json -c "./ginApp" --delay 7 2>&1 | tee test_logs_json.txt
    replay_status_json=${PIPESTATUS[0]}

    if [ "$replay_status_json" -ne 0 ]; then
        echo "::error::Keploy json replay failed with exit code ${replay_status_json}"
        cat test_logs_json.txt
        exit "$replay_status_json"
    fi

    if unexpected_errors="$(has_unexpected_errors "test_logs_json.txt")"; then
        echo "Error found in pipeline (json replay)..."
        printf '%s\n' "$unexpected_errors"
        cat "test_logs_json.txt"
        exit 1
    fi

    if grep "WARNING: DATA RACE" "test_logs_json.txt"; then
        echo "Race condition detected in json test, stopping pipeline..."
        cat "test_logs_json.txt"
        exit 1
    fi

    if ! json_scan_reports; then
        all_passed=false
        cat "test_logs_json.txt"
    fi
fi

# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    if json_pass_supported; then
        echo "All tests passed (yaml + json)"
    else
        echo "All tests passed (yaml only — json pass skipped for compat-matrix cell)"
    fi
    exit 0
else
    cat "test_logs.txt"
    cat "test_logs_json.txt" 2>/dev/null || true
    exit 1
fi

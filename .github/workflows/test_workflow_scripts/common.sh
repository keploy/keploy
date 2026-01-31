#!/usr/bin/env bash
# Common helper functions for Keploy E2E tests

set -Eeuo pipefail

# Logging
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

log_info()  { echo "[INFO] $*"; }
log_warn()  { echo "[WARN] $*"; echo "::warning::$*"; }
log_error() { echo "[ERROR] $*"; echo "::error::$*"; }
log_pass()  { echo "[PASS] $*"; }
log_fail()  { echo "[FAIL] $*"; }

# Wait for HTTP endpoint
wait_for_http() {
    local url="$1"
    local timeout="${2:-60}"
    
    for i in $(seq 1 "$timeout"); do
        if curl -s "$url" > /dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# Wait for TCP port
wait_for_port() {
    local host="${1:-localhost}"
    local port="$2"
    local timeout="${3:-60}"
    
    for i in $(seq 1 "$timeout"); do
        if nc -z "$host" "$port" 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# Check log file for errors
check_for_errors() {
    local logfile="$1"
    
    if [[ ! -f "$logfile" ]]; then
        return 0
    fi
    
    if grep -q "WARNING: DATA RACE" "$logfile"; then
        log_error "Data race detected in $logfile"
        return 1
    fi
    
    if grep "ERROR" "$logfile" | grep -q "Keploy:"; then
        log_error "Keploy error found in $logfile"
        return 1
    fi
    
    return 0
}

# Validate test reports
validate_test_reports() {
    if [[ ! -d "./keploy/reports" ]]; then
        log_error "No reports directory found"
        return 1
    fi
    
    local run_dir
    run_dir=$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)
    
    if [[ -z "$run_dir" ]]; then
        log_error "No test run directory found"
        return 1
    fi
    
    local all_passed=true
    for report in "$run_dir"/test-set-*-report.yaml; do
        [[ -f "$report" ]] || continue
        local status
        status=$(grep '^status:' "$report" | head -n1 | awk '{print $2}')
        
        if [[ "$status" != "PASSED" ]]; then
            log_fail "$(basename "$report"): $status"
            all_passed=false
        else
            log_pass "$(basename "$report"): PASSED"
        fi
    done
    
    [[ "$all_passed" == "true" ]]
}

# Extract coverage from log
validate_coverage() {
    local logfile="$1"
    local min="${2:-0}"
    
    local coverage
    coverage=$(grep -Eo "Total Coverage Percentage:[[:space:]]+[0-9]+(\.[0-9]+)?%" "$logfile" 2>/dev/null | tail -n1 | grep -Eo "[0-9]+(\.[0-9]+)?" || echo "0")
    
    log_info "Coverage: ${coverage}%"
    
    if (( $(echo "$coverage < $min" | bc -l) )); then
        log_warn "Coverage below ${min}%"
        return 1
    fi
    return 0
}

# Kill keploy process
kill_keploy() {
    local pid
    pid=$(pgrep -n -f 'keploy' || true)
    if [[ -n "$pid" ]]; then
        sudo kill -INT "$pid" 2>/dev/null || true
        sleep 2
    fi
}

# Initialize test environment
init_test_env() {
    if [[ -z "${RECORD_BIN:-}" ]] || [[ -z "${REPLAY_BIN:-}" ]]; then
        log_error "RECORD_BIN and REPLAY_BIN must be set"
        exit 1
    fi
    
    # Setup fake installation ID
    sudo mkdir -p ~/.keploy
    echo "ObjectID('123456789')" | sudo tee ~/.keploy/installation-id.yaml > /dev/null
}

# Clean keploy artifacts
clean_keploy_artifacts() {
    rm -rf keploy/ keploy.yml *.txt *.log 2>/dev/null || true
}

# Generate keploy config
generate_keploy_config() {
    local noise="${1:-}"
    
    [[ -f "./keploy.yml" ]] && rm ./keploy.yml
    sudo "$RECORD_BIN" config --generate
    
    if [[ -n "$noise" ]] && [[ -f "./keploy.yml" ]]; then
        sed -i "s/global: {}/global: $noise/" ./keploy.yml
    fi
}

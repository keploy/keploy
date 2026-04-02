#!/bin/bash

# Performance Test Runner with Multi-Run Validation
# Runs performance tests 3 times and fails only if 2+ runs show regression

# Note: NOT using 'set -e' because we need to handle test failures gracefully

# Configuration
NUM_RUNS=${NUM_RUNS:-3}
REQUIRED_FAILURES=${REQUIRED_FAILURES:-2}
RESULTS_DIR="perf-results"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)

# Thresholds for regression detection (milliseconds)
# These are defaults - the workflow typically overrides these via environment variables
P50_THRESHOLD=${P50_THRESHOLD:-5}      # Default: 5ms (workflow can override)
P90_THRESHOLD=${P90_THRESHOLD:-15}     # Default: 15ms (workflow can override)
P99_THRESHOLD=${P99_THRESHOLD:-70}     # Default: 70ms (workflow can override)
RPS_THRESHOLD=${RPS_THRESHOLD:-100}
ERROR_RATE_THRESHOLD=${ERROR_RATE_THRESHOLD:-1.0}
CHECK_KEPLOY=${CHECK_KEPLOY:-true}
KEPLOY_PID_FILE="keploy.pid"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "========================================="
echo "Keploy Performance Test Runner"
echo "========================================="
echo "Configuration:"
echo "  Number of runs: $NUM_RUNS"
echo "  Required failures to fail pipeline: $REQUIRED_FAILURES"
echo "  Thresholds:"
echo "    P50 < ${P50_THRESHOLD}ms"
echo "    P90 < ${P90_THRESHOLD}ms"
echo "    P99 < ${P99_THRESHOLD}ms"
echo "    RPS >= ${RPS_THRESHOLD}"
echo "    Error Rate < ${ERROR_RATE_THRESHOLD}%"
echo "========================================="
echo ""

# Create results directory
mkdir -p "$RESULTS_DIR/$TIMESTAMP"

# Array to track results
declare -a run_results
declare -a p50_values
declare -a p90_values
declare -a p99_values
declare -a rps_values
declare -a error_rate_values

# Function to extract metrics from k6 output
extract_metrics() {
    local output_file=$1
    
    # Extract P50, P90, P99 from http_req_duration line (including units)
    local p50=$(grep "http_req_duration" "$output_file" | grep -oP 'med=\K[0-9.]+[µm]?s' | head -1)
    local p90=$(grep "http_req_duration" "$output_file" | grep -oP 'p\(90\)=\K[0-9.]+[µm]?s' | head -1)
    local p99=$(grep "http_req_duration" "$output_file" | grep -oP 'p\(99\)=\K[0-9.]+[µm]?s' | head -1)
    
    # Extract RPS (use the final summary http_reqs line)
    local rps=$(grep "http_reqs" "$output_file" | grep -oP ':\s+\d+\s+\K[0-9.]+(?=/s)' | tail -1)
    
    # Extract Error Rate (http_req_failed percentage)
    local error_rate=$(grep "http_req_failed" "$output_file" | grep -oP ':\s+\K[0-9.]+(?=%)' | head -1)
    
    echo "$p50|$p90|$p99|$rps|$error_rate"
}

# Function to convert time units to milliseconds
convert_to_ms() {
    local value=$1
    
    # Check if value contains 'ms', 'µs', 'us', 's', etc.
    # Order matters: check µs/us before generic 's' to avoid false matches
    if [[ $value == *"ms"* ]]; then
        echo "$value" | sed 's/ms//'
    elif [[ $value == *"µs"* ]] || [[ $value == *"us"* ]]; then
        local num=$(echo "$value" | sed 's/[µu]s//')
        echo "$(echo "scale=3; $num / 1000" | bc -l)"
    elif [[ $value == *"s"* ]]; then
        local num=$(echo "$value" | sed 's/s//')
        echo "$(echo "scale=3; $num * 1000" | bc -l)"
    else
        echo "$value"
    fi
}

# Function to check if a run passed thresholds
check_thresholds() {
    local metrics=$1
    local run_num=$2
    
    IFS='|' read -r p50 p90 p99 rps error_rate <<< "$metrics"
    
    # Validate that all metrics were extracted successfully
    if [ -z "$p50" ] || [ -z "$p90" ] || [ -z "$p99" ] || [ -z "$rps" ] || [ -z "$error_rate" ]; then
        echo -e "${RED}  ✗ ERROR: Failed to extract metrics from k6 output${NC}"
        echo -e "${RED}    P50: '$p50', P90: '$p90', P99: '$p99', RPS: '$rps', Error Rate: '$error_rate'${NC}"
        echo -e "${RED}    This indicates a parsing failure or unexpected k6 output format${NC}"
        return 1
    fi
    
    # Convert to milliseconds if needed
    p50=$(convert_to_ms "$p50")
    p90=$(convert_to_ms "$p90")
    p99=$(convert_to_ms "$p99")
    
    # Validate converted values are numeric
    if ! [[ "$p50" =~ ^[0-9]+\.?[0-9]*$ ]] || ! [[ "$p90" =~ ^[0-9]+\.?[0-9]*$ ]] || \
       ! [[ "$p99" =~ ^[0-9]+\.?[0-9]*$ ]] || ! [[ "$rps" =~ ^[0-9]+\.?[0-9]*$ ]] || \
       ! [[ "$error_rate" =~ ^[0-9]+\.?[0-9]*$ ]]; then
        echo -e "${RED}  ✗ ERROR: Invalid numeric values after conversion${NC}"
        echo -e "${RED}    P50: '$p50', P90: '$p90', P99: '$p99', RPS: '$rps', Error Rate: '$error_rate'${NC}"
        return 1
    fi
    
    # Check each threshold
    local passed=true
    
    if (( $(echo "$p50 >= $P50_THRESHOLD" | bc -l) )); then
        echo -e "${RED}  ✗ P50 regression: ${p50}ms >= ${P50_THRESHOLD}ms${NC}"
        passed=false
    else
        echo -e "${GREEN}  ✓ P50 passed: ${p50}ms < ${P50_THRESHOLD}ms${NC}"
    fi
    
    if (( $(echo "$p90 >= $P90_THRESHOLD" | bc -l) )); then
        echo -e "${RED}  ✗ P90 regression: ${p90}ms >= ${P90_THRESHOLD}ms${NC}"
        passed=false
    else
        echo -e "${GREEN}  ✓ P90 passed: ${p90}ms < ${P90_THRESHOLD}ms${NC}"
    fi
    
    if (( $(echo "$p99 >= $P99_THRESHOLD" | bc -l) )); then
        echo -e "${RED}  ✗ P99 regression: ${p99}ms >= ${P99_THRESHOLD}ms${NC}"
        passed=false
    else
        echo -e "${GREEN}  ✓ P99 passed: ${p99}ms < ${P99_THRESHOLD}ms${NC}"
    fi
    
    if (( $(echo "$rps < $RPS_THRESHOLD" | bc -l) )); then
        # Check if it's within tolerance (1% below threshold)
        local tolerance=$(echo "$RPS_THRESHOLD * 0.99" | bc -l)
        if (( $(echo "$rps >= $tolerance" | bc -l) )); then
            # Within 1% tolerance, consider it passed
            echo -e "${GREEN}  ✓ RPS passed: ${rps} >= ${RPS_THRESHOLD} (within tolerance)${NC}"
        else
            # Significantly below threshold
            echo -e "${RED}  ✗ RPS regression: ${rps} < ${RPS_THRESHOLD}${NC}"
            passed=false
        fi
    else
        echo -e "${GREEN}  ✓ RPS passed: ${rps} >= ${RPS_THRESHOLD}${NC}"
    fi
    
    if (( $(echo "$error_rate >= $ERROR_RATE_THRESHOLD" | bc -l) )); then
        echo -e "${RED}  ✗ Error rate regression: ${error_rate}% >= ${ERROR_RATE_THRESHOLD}%${NC}"
        passed=false
    else
        echo -e "${GREEN}  ✓ Error rate passed: ${error_rate}% < ${ERROR_RATE_THRESHOLD}%${NC}"
    fi
    
    if [ "$passed" = true ]; then
        return 0
    else
        return 1
    fi
}

# Run performance tests
failed_runs=0

# Check if k6 is available
if ! command -v k6 &> /dev/null; then
    echo -e "${RED}❌ ERROR: k6 is not installed or not in PATH${NC}"
    echo ""
    echo "Please install k6:"
    echo "  - macOS: brew install k6"
    echo "  - Ubuntu/Debian: See https://k6.io/docs/get-started/installation/"
    echo "  - GitHub Actions: Add 'uses: grafana/setup-k6-action@v1' to workflow"
    echo ""
    exit 1
fi

echo ""
echo "========================================="
echo "Running Application Warm-up (Results discarded)"
echo "========================================="
echo "Executing initial load test to warm up JVM and lazy-loaded components..."
k6 run load-test.js > /dev/null 2>&1
echo "Warm-up completed successfully. System is now fast."
echo "Waiting 5 seconds before starting official recorded runs..."
sleep 5

for i in $(seq 1 $NUM_RUNS); do
    echo ""
    echo "========================================="
    echo "Run $i of $NUM_RUNS"
    echo "========================================="
    
    output_file="$RESULTS_DIR/$TIMESTAMP/run-${i}-output.log"
    
    # Clear the output file for this run
    > "$output_file"
    
    # Write run header to the file
    echo "=========================================" >> "$output_file"
    echo "Run $i of $NUM_RUNS" >> "$output_file"
    echo "=========================================" >> "$output_file"
    
    # Check if Keploy is still alive before starting the test
    if [ "$CHECK_KEPLOY" = true ]; then
        if [ -f "$KEPLOY_PID_FILE" ]; then
            kp_pid=$(cat "$KEPLOY_PID_FILE")
            if ! ps -p "$kp_pid" > /dev/null; then
                # Check one more time with a small wait in case it just started
                sleep 2
                if ! ps -p "$kp_pid" > /dev/null; then
                    echo -e "${RED}❌ ERROR: Keploy process (PID: $kp_pid) is NOT running. Ensure Keploy is started and reachable, then re-run these performance tests.${NC}" | tee -a "$output_file"
                    run_results[$i]="FAIL"
                    p50_values[$i]="N/A"
                    p90_values[$i]="N/A"
                    p99_values[$i]="N/A"
                    rps_values[$i]="N/A"
                    error_rate_values[$i]="N/A"
                    ((failed_runs++))
                    continue
                fi
            fi
        else
            echo -e "${RED}❌ ERROR: Keploy PID file '${KEPLOY_PID_FILE}' not found. Ensure Keploy is started and writing this file before running tests, or set CHECK_KEPLOY=false to skip Keploy validation.${NC}" | tee -a "$output_file"
            run_results[$i]="FAIL"
            p50_values[$i]="N/A"
            p90_values[$i]="N/A"
            p99_values[$i]="N/A"
            rps_values[$i]="N/A"
            error_rate_values[$i]="N/A"
            ((failed_runs++))
            continue
        fi
    fi
    
    # Run k6 test (capture exit code to ensure thresholds/errors aren't ignored)
    k6 run load-test.js 2>&1 | tee -a "$output_file"
    k6_status=${PIPESTATUS[0]}
    
    echo "" | tee -a "$output_file"
    echo "Checking thresholds for Run $i..." | tee -a "$output_file"
    
    # Extract metrics
    metrics=$(extract_metrics "$output_file")
    
    # Parse and store metrics in parent shell
    IFS='|' read -r p50_raw p90_raw p99_raw rps_raw error_rate_raw <<< "$metrics"
    p50_values[$i]=$(convert_to_ms "$p50_raw")
    p90_values[$i]=$(convert_to_ms "$p90_raw")
    p99_values[$i]=$(convert_to_ms "$p99_raw")
    rps_values[$i]=$rps_raw
    error_rate_values[$i]=$error_rate_raw
    
    # Check thresholds and capture output
    threshold_output=$(check_thresholds "$metrics" $i)
    threshold_result=$?
    
    # Display and log the output
    echo "$threshold_output" | tee -a "$output_file"
    
    if [ $threshold_result -eq 0 ] && [ $k6_status -eq 0 ]; then
        echo "Run $i: PASSED" >> "$output_file"
        echo -e "${GREEN}Run $i: PASSED${NC}"
        run_results[$i]="PASS"
    else
        if [ $k6_status -ne 0 ]; then
            echo "k6 exited with non-zero status: $k6_status" | tee -a "$output_file"
        fi
        echo "Run $i: FAILED (regression or k6 error detected)" >> "$output_file"
        echo -e "${RED}Run $i: FAILED (regression or k6 error detected)${NC}"
        run_results[$i]="FAIL"
        ((failed_runs++))
    fi
    
    # Sleep between runs to allow system to stabilize
    if [ $i -lt $NUM_RUNS ]; then
        echo ""
        echo "Waiting 5 seconds before next run..."
        sleep 5
    fi
done

# Generate summary report
echo ""
echo "========================================="
echo "SUMMARY REPORT"
echo "========================================="
echo ""
echo "Individual Run Results:"
for i in $(seq 1 $NUM_RUNS); do
    if [ "${run_results[$i]}" = "PASS" ]; then
        echo -e "  Run $i: ${GREEN}PASSED${NC}"
    else
        echo -e "  Run $i: ${RED}FAILED${NC}"
    fi
    p50_display="${p50_values[$i]}"; [[ "$p50_display" != "N/A" ]] && p50_display+="ms"
    p90_display="${p90_values[$i]}"; [[ "$p90_display" != "N/A" ]] && p90_display+="ms"
    p99_display="${p99_values[$i]}"; [[ "$p99_display" != "N/A" ]] && p99_display+="ms"
    rps_display="${rps_values[$i]}"
    err_display="${error_rate_values[$i]}"; [[ "$err_display" != "N/A" ]] && err_display+="%"
    echo "    P50: $p50_display"
    echo "    P90: $p90_display"
    echo "    P99: $p99_display"
    echo "    RPS: $rps_display"
    echo "    Error Rate: $err_display"
done

echo ""
echo "Aggregate Statistics:"

# Calculate averages over successful runs only (skip "N/A" placeholders from skipped runs)
avg_p50=$(printf '%s\n' "${p50_values[@]}" | awk '$1 != "N/A" {sum+=$1; n++} END {if (n>0) print sum/n; else print "N/A"}')
avg_p90=$(printf '%s\n' "${p90_values[@]}" | awk '$1 != "N/A" {sum+=$1; n++} END {if (n>0) print sum/n; else print "N/A"}')
avg_p99=$(printf '%s\n' "${p99_values[@]}" | awk '$1 != "N/A" {sum+=$1; n++} END {if (n>0) print sum/n; else print "N/A"}')
avg_rps=$(printf '%s\n' "${rps_values[@]}" | awk '$1 != "N/A" {sum+=$1; n++} END {if (n>0) print sum/n; else print "N/A"}')
avg_error_rate=$(printf '%s\n' "${error_rate_values[@]}" | awk '$1 != "N/A" {sum+=$1; n++} END {if (n>0) print sum/n; else print "N/A"}')

avg_p50_display="$avg_p50"; [[ "$avg_p50_display" != "N/A" ]] && avg_p50_display+="ms"
avg_p90_display="$avg_p90"; [[ "$avg_p90_display" != "N/A" ]] && avg_p90_display+="ms"
avg_p99_display="$avg_p99"; [[ "$avg_p99_display" != "N/A" ]] && avg_p99_display+="ms"
avg_err_display="$avg_error_rate"; [[ "$avg_err_display" != "N/A" ]] && avg_err_display+="%"
echo "  Average P50: $avg_p50_display"
echo "  Average P90: $avg_p90_display"
echo "  Average P99: $avg_p99_display"
echo "  Average RPS: ${avg_rps}"
echo "  Average Error Rate: $avg_err_display"

echo ""
echo "========================================="
echo "FINAL RESULT"
echo "========================================="
echo "Failed runs: $failed_runs / $NUM_RUNS"
echo "Required failures to fail pipeline: $REQUIRED_FAILURES"
echo ""

# Verify that test cases were recorded (if checking Keploy)
if [ "$CHECK_KEPLOY" = true ] && [ "$failed_runs" -lt "$REQUIRED_FAILURES" ]; then
    echo "========================================="
    echo "VERIFYING RECORDED DATA"
    echo "========================================="
    
    # Use -d to check if directory exists first
    if [ ! -d "keploy-tests" ]; then
        echo -e "${RED}❌ ERROR: 'keploy-tests' directory not found!${NC}"
        echo "No test cases were recorded."
        exit 1
    fi
    
    NUM_TESTS=$(find keploy-tests -name "*.yaml" | wc -l)
    if [ "$NUM_TESTS" -eq 0 ]; then
        echo -e "${RED}❌ ERROR: No test cases recorded in 'keploy-tests' directory!${NC}"
        echo "The performance numbers might be invalid (plain app instead of Keploy recording path)."
        exit 1
    else
        echo -e "${GREEN}✅ Verified: $NUM_TESTS test cases recorded in 'keploy-tests'${NC}"
    fi
    echo "========================================="
    echo ""
fi

if [ $failed_runs -ge $REQUIRED_FAILURES ]; then
    echo -e "${RED}❌ PIPELINE FAILED${NC}"
    echo "Performance regression detected in $failed_runs out of $NUM_RUNS runs."
    echo "This exceeds the threshold of $REQUIRED_FAILURES failures."
    echo ""
    echo "Results saved to: $RESULTS_DIR/$TIMESTAMP/"
    exit 1
else
    echo -e "${GREEN}✅ PIPELINE PASSED${NC}"
    echo "Only $failed_runs out of $NUM_RUNS runs failed."
    echo "This is below the threshold of $REQUIRED_FAILURES failures."
    echo ""
    echo "Results saved to: $RESULTS_DIR/$TIMESTAMP/"
    exit 0
fi

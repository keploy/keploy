#!/bin/bash

# Performance Test Runner with Multi-Run Validation
# Runs performance tests 3 times and fails only if 2+ runs show regression

set -e

# Configuration
NUM_RUNS=${NUM_RUNS:-3}
REQUIRED_FAILURES=${REQUIRED_FAILURES:-2}
RESULTS_DIR="perf-results"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)

# Thresholds for regression detection (milliseconds)
P50_THRESHOLD=${P50_THRESHOLD:-50}
P90_THRESHOLD=${P90_THRESHOLD:-100}
P99_THRESHOLD=${P99_THRESHOLD:-500}
ERROR_RATE_THRESHOLD=${ERROR_RATE_THRESHOLD:-0.01}  # 1%
RPS_THRESHOLD=${RPS_THRESHOLD:-100}

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
echo "    Error Rate < $(echo "$ERROR_RATE_THRESHOLD * 100" | bc)%"
echo "    RPS >= ${RPS_THRESHOLD}"
echo "========================================="
echo ""

# Create results directory
mkdir -p "$RESULTS_DIR/$TIMESTAMP"

# Array to track results
declare -a run_results
declare -a p50_values
declare -a p90_values
declare -a p99_values
declare -a error_rates
declare -a rps_values

# Function to extract metrics from k6 output
extract_metrics() {
    local output_file=$1
    
    # Extract P50, P90, P99 from http_req_duration line
    local p50=$(grep "http_req_duration" "$output_file" | grep -oP 'med=\K[0-9.]+' | head -1)
    local p90=$(grep "http_req_duration" "$output_file" | grep -oP 'p\(90\)=\K[0-9.]+' | head -1)
    local p99=$(grep "http_req_duration" "$output_file" | grep -oP 'p\(99\)=\K[0-9.]+' | head -1)
    
    # Extract error rate
    local error_rate=$(grep "http_req_failed" "$output_file" | grep -oP 'rate=\K[0-9.]+' | head -1)
    
    # Extract RPS
    local rps=$(grep "http_reqs" "$output_file" | grep -oP ':\s+\d+\s+\K[0-9.]+(?=/s)' | head -1)
    
    echo "$p50|$p90|$p99|$error_rate|$rps"
}

# Function to convert time units to milliseconds
convert_to_ms() {
    local value=$1
    
    # Check if value contains 'ms', 's', 'µs', etc.
    if [[ $value == *"ms"* ]]; then
        echo "$value" | sed 's/ms//'
    elif [[ $value == *"s"* ]]; then
        local num=$(echo "$value" | sed 's/s//')
        echo "$(echo "$num * 1000" | bc)"
    elif [[ $value == *"µs"* ]] || [[ $value == *"us"* ]]; then
        local num=$(echo "$value" | sed 's/[µu]s//')
        echo "$(echo "$num / 1000" | bc -l)"
    else
        echo "$value"
    fi
}

# Function to check if a run passed thresholds
check_thresholds() {
    local metrics=$1
    local run_num=$2
    
    IFS='|' read -r p50 p90 p99 error_rate rps <<< "$metrics"
    
    # Convert to milliseconds if needed
    p50=$(convert_to_ms "$p50")
    p90=$(convert_to_ms "$p90")
    p99=$(convert_to_ms "$p99")
    
    # Store values for summary
    p50_values[$run_num]=$p50
    p90_values[$run_num]=$p90
    p99_values[$run_num]=$p99
    error_rates[$run_num]=$error_rate
    rps_values[$run_num]=$rps
    
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
    
    if (( $(echo "$error_rate >= $ERROR_RATE_THRESHOLD" | bc -l) )); then
        echo -e "${RED}  ✗ Error rate regression: $(echo "$error_rate * 100" | bc)% >= $(echo "$ERROR_RATE_THRESHOLD * 100" | bc)%${NC}"
        passed=false
    else
        echo -e "${GREEN}  ✓ Error rate passed: $(echo "$error_rate * 100" | bc)% < $(echo "$ERROR_RATE_THRESHOLD * 100" | bc)%${NC}"
    fi
    
    if (( $(echo "$rps < $RPS_THRESHOLD" | bc -l) )); then
        echo -e "${RED}  ✗ RPS regression: ${rps} < ${RPS_THRESHOLD}${NC}"
        passed=false
    else
        echo -e "${GREEN}  ✓ RPS passed: ${rps} >= ${RPS_THRESHOLD}${NC}"
    fi
    
    if [ "$passed" = true ]; then
        return 0
    else
        return 1
    fi
}

# Run performance tests
failed_runs=0

for i in $(seq 1 $NUM_RUNS); do
    echo ""
    echo "========================================="
    echo "Run $i of $NUM_RUNS"
    echo "========================================="
    
    output_file="$RESULTS_DIR/$TIMESTAMP/run-${i}-output.log"
    
    # Run k6 test
    if k6 run load-test.js 2>&1 | tee "$output_file"; then
        echo ""
        echo "Checking thresholds for Run $i..."
        
        # Extract metrics
        metrics=$(extract_metrics "$output_file")
        
        if check_thresholds "$metrics" $i; then
            echo -e "${GREEN}Run $i: PASSED${NC}"
            run_results[$i]="PASS"
        else
            echo -e "${RED}Run $i: FAILED (regression detected)${NC}"
            run_results[$i]="FAIL"
            ((failed_runs++))
        fi
    else
        echo -e "${RED}Run $i: FAILED (test execution error)${NC}"
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
    echo "    P50: ${p50_values[$i]}ms"
    echo "    P90: ${p90_values[$i]}ms"
    echo "    P99: ${p99_values[$i]}ms"
    echo "    Error Rate: $(echo "${error_rates[$i]} * 100" | bc)%"
    echo "    RPS: ${rps_values[$i]}"
done

echo ""
echo "Aggregate Statistics:"

# Calculate averages
avg_p50=$(printf '%s\n' "${p50_values[@]}" | awk '{sum+=$1} END {if (NR>0) print sum/NR; else print 0}')
avg_p90=$(printf '%s\n' "${p90_values[@]}" | awk '{sum+=$1} END {if (NR>0) print sum/NR; else print 0}')
avg_p99=$(printf '%s\n' "${p99_values[@]}" | awk '{sum+=$1} END {if (NR>0) print sum/NR; else print 0}')
avg_error=$(printf '%s\n' "${error_rates[@]}" | awk '{sum+=$1} END {if (NR>0) print sum/NR; else print 0}')
avg_rps=$(printf '%s\n' "${rps_values[@]}" | awk '{sum+=$1} END {if (NR>0) print sum/NR; else print 0}')

echo "  Average P50: ${avg_p50}ms"
echo "  Average P90: ${avg_p90}ms"
echo "  Average P99: ${avg_p99}ms"
echo "  Average Error Rate: $(echo "$avg_error * 100" | bc)%"
echo "  Average RPS: ${avg_rps}"

echo ""
echo "========================================="
echo "FINAL RESULT"
echo "========================================="
echo "Failed runs: $failed_runs / $NUM_RUNS"
echo "Required failures to fail pipeline: $REQUIRED_FAILURES"
echo ""

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

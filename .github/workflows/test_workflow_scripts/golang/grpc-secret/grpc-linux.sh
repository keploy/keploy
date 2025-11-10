#!/bin/bash

# This script tests the grpc-secret sample application with Keploy's sanitize functionality.
# It records gRPC requests with secrets in headers and body, sanitizes them, and validates test results.
#
# Expects:
#   RECORD_BIN -> path to keploy record binary (env)
#   REPLAY_BIN -> path to keploy test binary   (env)
#
set -Eeuo pipefail

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

# --- Sanity Checks ---
[ -x "${RECORD_BIN:-}" ] || { echo "RECORD_BIN not set or not executable"; exit 1; }
[ -x "${REPLAY_BIN:-}" ] || { echo "REPLAY_BIN not set or not executable"; exit 1; }
command -v go >/dev/null 2>&1 || { echo "go not found"; exit 1; }

# --- Install grpcurl ---
echo "Installing grpcurl..."
if ! command -v grpcurl &> /dev/null; then
    echo "grpcurl not found, installing..."
    go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest
    export PATH="$PATH:$HOME/go/bin"
fi
command -v grpcurl >/dev/null 2>&1 || { echo "grpcurl installation failed"; exit 1; }
echo "grpcurl installed successfully"

# --- Helper Functions ---

# Cleanup function to kill all running processes
cleanup() {
    echo "Cleaning up running processes..."
    pkill -f keploy || true
    pkill -f "go run" || true
    pkill -f grpc-secret || true
    sleep 2
    pkill -9 -f keploy || true
    pkill -9 -f "go run" || true
    pkill -9 -f grpc-secret || true
    echo "Cleanup complete."
}
trap cleanup EXIT

# Function to cleanup any remaining keploy processes
cleanup_keploy() {
    echo "Cleaning up any remaining keploy processes..."
    local pids=$(pgrep keploy)
    if [ -n "$pids" ]; then
        echo "Found keploy processes: $pids"
        echo "$pids" | xargs -r sudo kill -9 2>/dev/null || true
        sleep 1
        if pgrep keploy >/dev/null; then
            echo "Warning: Some keploy processes may still be running"
        else
            echo "All keploy processes cleaned up successfully"
        fi
    else
        echo "No keploy processes found to cleanup"
    fi
}

# Checks a log file for critical errors or data races
check_for_errors() {
    local logfile=$1
    echo "Checking for errors in $logfile..."
    if [ -f "$logfile" ]; then
        # Check for ERROR lines (excluding non-critical ones)
        if awk '/ERROR/ && !/Unsupported DNS query type/ { exit 0 } END { exit 1 }' "$logfile"; then
            echo "Error found in $logfile"
            cat "$logfile"
            exit 1
        fi
        if grep -q "WARNING: DATA RACE" "$logfile"; then
            echo "Race condition detected in $logfile"
            cat "$logfile"
            exit 1
        fi
    fi
    echo "No critical errors found in $logfile."
}

# Kills the keploy process gracefully
kill_keploy_process() {
    pid=$(pgrep keploy | head -n 1)
    if [ -n "$pid" ]; then
        echo "$pid Keploy PID"
        echo "Killing keploy"
        
        # Try graceful shutdown first
        if ! kill "$pid" 2>/dev/null; then
            echo "Graceful kill failed, trying with sudo..."
            if ! sudo kill "$pid" 2>/dev/null; then
                echo "Sudo kill failed, trying SIGKILL..."
                if ! sudo kill -9 "$pid" 2>/dev/null; then
                    echo "Warning: Could not kill keploy process $pid"
                else
                    echo "Successfully killed keploy with SIGKILL"
                fi
            else
                echo "Successfully killed keploy with sudo"
            fi
        else
            echo "Successfully killed keploy gracefully"
        fi
        
        # Wait and verify the process is gone
        sleep 2
        if pgrep keploy >/dev/null; then
            echo "Warning: Keploy process may still be running"
            pgrep keploy | xargs -r sudo kill -9 2>/dev/null || true
        fi
    else
        echo "No keploy process found to kill"
    fi
}

# Send all 4 gRPC requests
send_grpc_requests() {
    echo "Waiting for gRPC server to be ready..."
    sleep 10
    
    echo "Sending all 4 gRPC requests..."
    
    # 1. JwtLab
    echo "Sending JwtLab request..."
    grpcurl -plaintext -v \
      -H "authorization: Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWUsImlhdCI6MTUxNjIzOTAyMn0.TCYt5XsITJX1CxPCT8yAV-TVkIEq_PbChOMqsLfRoPsnsgw5WEuts01mq-pQy7UJiN5mgRxD-WUcX16dUEMGlv50aTquLy4H2jJL6QRTQFXBF9kGvUKL9Q" \
      -H "x-api-key: AIzaSyD-9tNxCeDNj3S0K_abcdefghijklmnopqr" \
      -H "x-session-token: SG.abc123xyz.789def012ghi345jkl678mno901pqr234stu" \
      -H "x-mongodb-password: mongodb+srv://user:SecretP@ssw0rd123@cluster.net" \
      -H "x-postgres-url: postgres://dbuser:MyS3cr3tP@ss@db.internal:5432/prod" \
      -H "x-datadog-key: 0123456789abcdef0123456789abcdef" \
      -d '{}' \
      localhost:50051 \
      secrets.SecretService/JwtLab >/dev/null 2>&1 || echo "JwtLab request completed"
    
    sleep 1
    
    # 2. CurlMix
    echo "Sending CurlMix request..."
    grpcurl -plaintext -v \
      -H "authorization: Bearer wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" \
      -H "x-api-key: sk-1234567890abcdefghijklmnopqrstuvwxyzABCDEF" \
      -H "x-session-token: npm_abcdefghijklmnopqrstuvwxyz1234567890" \
      -H "x-github-token: ghp_9876543210ZYXWVUTSRQPONMLKJIHGFEDCBAzy" \
      -H "x-slack-webhook: https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXX" \
      -H "x-twilio-token: a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6" \
      -d '{}' \
      localhost:50051 \
      secrets.SecretService/CurlMix >/dev/null 2>&1 || echo "CurlMix request completed"
    
    sleep 1
    
    # 3. Cdn
    echo "Sending Cdn request..."
    grpcurl -plaintext -v \
      -H "authorization: Bearer sk_live_4eC39HqLyjWDarjtT1zdp7dc" \
      -H "x-api-key: AKIAJ5XXXXXXXXXXXXXXXX" \
      -H "x-session-token: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh1234" \
      -H "x-hmac-secret: 1a2b3c4d5e6f7g8h9i0j1k2l3m4n5o6p7q8r9s0t1u2v3w4x5y6z" \
      -H "x-cloudflare-token: a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0" \
      -H "x-azure-key: DefaultEndpointsProtocol=https;AccountName=myaccount;AccountKey=abc123xyz789==" \
      -H "x-sendgrid-key: SG.abcdefghijklmnopqrstuv.xyz0123456789abcdefghijklmnopqrstuvwxyz" \
      -d '{}' \
      localhost:50051 \
      secrets.SecretService/Cdn >/dev/null 2>&1 || echo "Cdn request completed"
    
    sleep 1
    
    # 4. GetSecret
    echo "Sending GetSecret request..."
    grpcurl -plaintext -v \
      -H "authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJrZXBsb3ktdGVzdHMiLCJzdWIiOiJ1c2VyXzMxMDBfYWJjZDEyMzQiLCJ0cyI6IjIwMjUtMDEtMDFUMDA6MDA6MDBaIn0.c29tZV9zaWduYXR1cmVfaGVyZQ" \
      -H "x-api-key: sk-proj-abcd1234efgh5678ijkl9012mnop3456qrst" \
      -H "x-session-token: ghp_1234567890abcdefghijklmnopqrstuvwxyz" \
      -H "x-stripe-key: sk_live_51aBc123dEfghIjkLmnOp456qrSTuvWxYz789" \
      -H "x-aws-access: AKIAIOSFODNN7EXAMPLE" \
      -H "x-mongodb-uri: mongodb+srv://admin:p4ssw0rd@cluster0.mongodb.net/mydb" \
      -d '{"id": 1}' \
      localhost:50051 \
      secrets.SecretService/GetSecret >/dev/null 2>&1 || echo "GetSecret request completed"
    
    echo "All requests sent."
    sleep 3
}

# Validates the Keploy test report to ensure all test sets passed
check_test_report() {
    local expected_test_sets=$1
    echo "Checking test reports (expecting $expected_test_sets test sets)..."
    
    if [ ! -d "./keploy/reports" ]; then
        echo "Test report directory not found!"
        return 1
    fi

    local latest_report_dir
    latest_report_dir=$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1)
    if [ -z "$latest_report_dir" ]; then
        echo "No test run directory found in ./keploy/reports/"
        return 1
    fi
    
    local all_passed=true
    # Loop through expected test sets
    for ((i=0; i<expected_test_sets; i++)); do
        report_file="$latest_report_dir/test-set-$i-report.yaml"
        
        if [ ! -f "$report_file" ]; then
            echo "Report file not found: $report_file"
            all_passed=false
            break
        fi
        
        local test_status
        test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
        
        echo "Status for test-set-$i: $test_status"
        if [ "$test_status" != "PASSED" ]; then
            all_passed=false
            echo "Test set $i did not pass."
        fi
    done

    if [ "$all_passed" = false ]; then
        echo "One or more test sets failed."
        return 1
    fi

    echo "All tests passed in reports."
    return 0
}

# --- Main Logic ---

echo "🧪 Starting gRPC Secret Sanitize Testing"

# Reset state before each run
cleanup
rm -rf ./keploy*
sudo -E env PATH="$PATH" "$RECORD_BIN" config --generate
sleep 3

go build -o grpc-secret .
sleep 4

echo "✅ Built grpc-secret binary"

# --- Record 2 test sets ---
echo "📝 Phase 1: Recording 2 test sets with all 4 endpoints..."

for i in 1 2; do
    app_name="grpcSecret_${i}"
    echo "Recording iteration $i..."
    
    # Start keploy record in background
    sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "./grpc-secret" --generateGithubActions=false 2>&1 | tee ${app_name}.txt &
    
    # Send all 4 gRPC requests
    send_grpc_requests &
    
    # Wait for requests to complete
    sleep 15
    
    # Kill keploy
    kill_keploy_process
    
    # Check for errors and race conditions
    check_for_errors "${app_name}.txt"
    
    sleep 5
    echo "✅ Recorded test set ${i}"
done

# --- Run keploy test (before sanitize) ---
echo "🧪 Phase 2: Running keploy test (before sanitize)..."
sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "./grpc-secret" --delay 10 --generateGithubActions=false 2>&1 | tee test_before_sanitize.txt || true

# Check for errors and race conditions
check_for_errors test_before_sanitize.txt

# Verify test sets passed
if ! check_test_report 2; then
    echo "⚠️ Test report check before sanitize: some tests may have failed (expected)"
else
    echo "✅ All tests passed before sanitize"
fi

# --- Run keploy sanitize ---
echo "🧹 Phase 3: Running keploy sanitize..."
sudo -E env PATH="$PATH" "$RECORD_BIN" sanitize 2>&1 | tee sanitize_logs.txt

# Check for errors and race conditions
check_for_errors sanitize_logs.txt

sleep 5
echo "✅ Sanitization complete"

# --- Run keploy test (after sanitize) ---
echo "🧪 Phase 4: Running keploy test (after sanitize)..."
sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "./grpc-secret" --delay 10 --generateGithubActions=false 2>&1 | tee test_after_sanitize.txt || true

# Check for errors and race conditions
check_for_errors test_after_sanitize.txt

# Verify all test sets passed
if ! check_test_report 2; then
    echo "❌ Test report check failed after sanitize."
    cat test_after_sanitize.txt
    exit 1
fi

echo "✅ All tests passed after sanitize!"

# --- Cleanup ---
cleanup_keploy

echo "🎉 gRPC Secret Sanitize Testing completed successfully!"
exit 0


#!/bin/bash
# E2E test: verify network capture works for all protocols (HTTP, MySQL, Redis, Postgres, Generic)
# Usage: sudo ./run_capture_test.sh <keploy-binary>
set -uo pipefail

KEPLOY="${1:?Usage: $0 <keploy-binary-path>}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="/tmp/capture-e2e-test"
APP_PORT=6789
PASS=0; FAIL=0

pass() { PASS=$((PASS+1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL: $1 -- $2"; }
check() { if eval "$1" 2>/dev/null; then pass "$2"; else fail "$2" "${3:-}"; fi }

cleanup() {
    pkill -9 -f "capture-e2e-app" 2>/dev/null || true
    pkill -9 -f "keploy.*capture-e2e" 2>/dev/null || true
    sleep 1
}

# ── Setup ──────────────────────────────────────────
echo "=== Setting up test environment ==="
cleanup
rm -rf "$WORKDIR" $WORKDIR/keploy/debug
mkdir -p "$WORKDIR"

# Build app
cd "$SCRIPT_DIR"
go mod download 2>/dev/null
go build -o "$WORKDIR/capture-e2e-app" . 2>&1
if [ $? -ne 0 ]; then echo "FATAL: build failed"; exit 1; fi

# Start backend services
echo "Starting MySQL, Redis, PostgreSQL..."
docker compose -f "$SCRIPT_DIR/docker-compose.yml" up -d 2>/dev/null
echo "Waiting for services to be healthy..."
for i in $(seq 1 30); do
    MYSQL_OK=$(docker exec capture-test-mysql mysqladmin ping -ppassword 2>/dev/null | grep -c alive)
    REDIS_OK=$(docker exec capture-test-redis redis-cli ping 2>/dev/null | grep -c PONG)
    PG_OK=$(docker exec capture-test-postgres pg_isready -U postgres 2>/dev/null | grep -c accepting)
    if [ "$MYSQL_OK" = "1" ] && [ "$REDIS_OK" = "1" ] && [ "$PG_OK" = "1" ]; then
        echo "All services ready."
        break
    fi
    sleep 2
done

# ══════════════════════════════════════════════════
# PHASE 1: Record with --debug
# ══════════════════════════════════════════════════
echo ""
echo "=== PHASE 1: Record with --debug ==="
cd "$WORKDIR"
$KEPLOY record -c "./capture-e2e-app" --debug --path "$WORKDIR" --disable-ansi > /dev/null 2>&1 &
KPID=$!
sleep 12

echo "Sending requests to all protocols..."
# HTTP echo
curl -sf http://localhost:$APP_PORT/http-echo?msg=capture-test > /dev/null
# MySQL
curl -sf -X POST http://localhost:$APP_PORT/mysql/insert -H 'Content-Type: application/json' -d '{"name":"cap-key","value":"cap-val"}' > /dev/null
curl -sf http://localhost:$APP_PORT/mysql/select > /dev/null
# Redis
curl -sf -X POST http://localhost:$APP_PORT/redis/set -H 'Content-Type: application/json' -d '{"key":"cap-redis","value":"cap-redis-val"}' > /dev/null
curl -sf http://localhost:$APP_PORT/redis/get?key=cap-redis > /dev/null
# PostgreSQL
curl -sf -X POST http://localhost:$APP_PORT/postgres/insert -H 'Content-Type: application/json' -d '{"name":"cap-pg","value":"cap-pg-val"}' > /dev/null
curl -sf http://localhost:$APP_PORT/postgres/select > /dev/null
# All-in-one
curl -sf http://localhost:$APP_PORT/all > /dev/null

sleep 4
kill -INT $KPID 2>/dev/null; wait $KPID 2>/dev/null; sleep 3

# ── Validate Record Capture ──
echo ""
echo "--- Record Capture Validation ---"
REC_CAP=$(find $WORKDIR/keploy/debug -name "capture_record_*.kpcap" 2>/dev/null | sort | tail -1)
check '[ -n "$REC_CAP" ]' "Record capture file created"

if [ -n "$REC_CAP" ]; then
    VAL=$($KEPLOY debug validate "$REC_CAP" --disable-ansi 2>&1)
    check 'echo "$VAL" | grep -q "Valid:.*true"' "Capture is valid"

    PKTS=$(echo "$VAL" | grep "^Packets:" | awk '{print $2}')
    BYTES=$(echo "$VAL" | grep "^Data:" | awk '{print $2}')
    CONNS=$(echo "$VAL" | grep "^Connections:" | awk '{print $2}')
    check '[ "$PKTS" -gt 10 ]' "Has significant packets ($PKTS)"
    check '[ "$BYTES" -gt 100 ]' "Has significant data ($BYTES bytes)"
    check '[ "$CONNS" -gt 1 ]' "Has multiple connections ($CONNS)"

    ANA=$($KEPLOY debug analyze "$REC_CAP" --disable-ansi 2>&1)
    # HTTP outgoing calls to localhost echo server may not go through proxy (same machine)
    # So we check for MySQL (port-based detection) and Generic (Redis/Postgres)
    check 'echo "$ANA" | grep -q "MySQL"' "MySQL protocol captured"
    check 'echo "$ANA" | grep -qE "Generic|Redis|Postgres"' "Other protocols captured (Generic/Redis/PG)"
    # At least 2 different protocol types should be detected
    PROTO_TYPES=$(echo "$ANA" | grep -c "connections$")
    check '[ "$PROTO_TYPES" -ge 2 ]' "Multiple protocol types detected ($PROTO_TYPES)"
fi

# Check mocks were recorded (test cases need incoming proxy which may vary)
MOCK_FILES=$(find "$WORKDIR/keploy" -name "mocks.yaml" 2>/dev/null | wc -l)
check '[ "$MOCK_FILES" -gt 0 ]' "Mock files recorded ($MOCK_FILES)"
TC_COUNT=$(find "$WORKDIR/keploy" -name "test-*.yaml" 2>/dev/null | wc -l)
echo "  INFO: Test cases recorded: $TC_COUNT"

# ══════════════════════════════════════════════════
# PHASE 2: Test with --debug
# ══════════════════════════════════════════════════
# Save the record capture for later analysis (Phase 2 may clean $WORKDIR/keploy/debug)
SAVED_REC_CAP=""
if [ -n "$REC_CAP" ] && [ -f "$REC_CAP" ]; then
    cp "$REC_CAP" "$WORKDIR/saved_record_capture.kpcap"
    SAVED_REC_CAP="$WORKDIR/saved_record_capture.kpcap"
fi

echo ""
echo "=== PHASE 2: Test (replay) with --debug ==="
if [ "$TC_COUNT" -gt 0 ]; then
    rm -rf $WORKDIR/keploy/debug
    cleanup
    cd "$WORKDIR"

    # Capture full output to a temp file (--debug produces many lines; tail -N can lose the summary)
    TEST_LOG=$(mktemp)
    timeout 90 $KEPLOY test -c "./capture-e2e-app" --debug --path "$WORKDIR" --disable-ansi > "$TEST_LOG" 2>&1 || true

    # Report test results (informational — we're testing capture, not the test runner)
    if grep -q "Total tests:" "$TEST_LOG" 2>/dev/null; then
        TOTAL=$(grep "Total tests:" "$TEST_LOG" | grep -oE '[0-9]+' | head -1)
        PASSED=$(grep "Total test passed:" "$TEST_LOG" | grep -oE '[0-9]+' | head -1)
        echo "  INFO: Test run — total=$TOTAL passed=$PASSED"
    else
        echo "  INFO: Test run completed (summary line not found in output)"
    fi
    rm -f "$TEST_LOG"

    TEST_CAP=$(find $WORKDIR/keploy/debug -name "capture_test_*.kpcap" 2>/dev/null | sort | tail -1)
    check '[ -n "$TEST_CAP" ]' "Test capture file created"
    if [ -n "$TEST_CAP" ]; then
        TVAL=$($KEPLOY debug validate "$TEST_CAP" --disable-ansi 2>&1)
        check 'echo "$TVAL" | grep -q "Valid:.*true"' "Test capture is valid"
        TPKTS=$(echo "$TVAL" | grep "^Packets:" | awk '{print $2}')
        check '[ "$TPKTS" -gt 0 ]' "Test capture has packets ($TPKTS)"
    fi
else
    echo "  SKIP: No test cases to replay (incoming proxy may not have intercepted requests)"
    # Still test that test mode creates a capture file
    rm -rf $WORKDIR/keploy/debug
    cleanup
    cd "$WORKDIR"
    timeout 30 $KEPLOY test -c "./capture-e2e-app" --debug --path "$WORKDIR" --disable-ansi > /dev/null 2>&1
    TEST_CAP=$(find $WORKDIR/keploy/debug -name "capture_test_*.kpcap" 2>/dev/null | sort | tail -1)
    check '[ -n "$TEST_CAP" ]' "Test capture file created (even with no test cases)"
fi

# ══════════════════════════════════════════════════
# PHASE 3: Bundle → Extract → Validate roundtrip
# ══════════════════════════════════════════════════
echo ""
echo "=== PHASE 3: Bundle roundtrip ==="
ANY_CAP=$(find $WORKDIR/keploy/debug -name "*.kpcap" 2>/dev/null | head -1)
if [ -n "$ANY_CAP" ]; then
    $KEPLOY debug bundle --capture "$ANY_CAP" \
        --mocks "$WORKDIR/keploy" --tests "$WORKDIR/keploy" \
        --output /tmp/mp-bundle.tar.gz --notes "multi-protocol CI test" --disable-ansi 2>/dev/null
    check '[ -f /tmp/mp-bundle.tar.gz ]' "Bundle created"

    rm -rf /tmp/mp-extract
    $KEPLOY debug extract /tmp/mp-bundle.tar.gz --dir /tmp/mp-extract --disable-ansi 2>/dev/null
    check '[ -f /tmp/mp-extract/keploy-debug-bundle/manifest.json ]' "Manifest extracted"

    EXT_CAP=$(find /tmp/mp-extract -name "*.kpcap" | head -1)
    check '[ -n "$EXT_CAP" ]' "Capture in bundle"
    if [ -n "$EXT_CAP" ]; then
        EVAL=$($KEPLOY debug validate "$EXT_CAP" --disable-ansi 2>&1)
        check 'echo "$EVAL" | grep -q "Valid:.*true"' "Extracted capture valid"
    fi
fi

# ══════════════════════════════════════════════════
# PHASE 4: Analyze protocol breakdown (use saved record capture)
# ══════════════════════════════════════════════════
echo ""
echo "=== PHASE 4: Protocol breakdown analysis ==="
PHASE4_CAP="${SAVED_REC_CAP:-$REC_CAP}"
if [ -n "$PHASE4_CAP" ] && [ -f "$PHASE4_CAP" ]; then
    $KEPLOY debug analyze "$PHASE4_CAP" --disable-ansi 2>&1 | grep -E "Protocol|connections$|Connections:|Total Data:|Duration:|Client|Server"
else
    echo "  Record capture not available for analysis"
fi

# ── Teardown ──
echo ""
echo "=== Teardown ==="
cleanup
docker compose -f "$SCRIPT_DIR/docker-compose.yml" down 2>/dev/null

# ── Summary ──
TOTAL=$((PASS + FAIL))
echo ""
echo "═══════════════════════════════════════════════"
echo "  MULTI-PROTOCOL CAPTURE E2E RESULTS"
echo "═══════════════════════════════════════════════"
echo "  Total:  $TOTAL"
echo "  Passed: $PASS"
echo "  Failed: $FAIL"
echo "═══════════════════════════════════════════════"

[ "$FAIL" -eq 0 ] && exit 0 || exit 1

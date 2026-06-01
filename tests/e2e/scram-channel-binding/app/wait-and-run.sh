#!/usr/bin/env sh
# Drive the e2e assertions for the scram-channel-binding test.
#
# Phase 1 — WITHOUT shim. Connecting through the MITM with
#           SCRAM-SHA-256-PLUS must FAIL with "SCRAM channel binding
#           check failed". A success here means we're not actually
#           exercising channel binding (test regression) — exit 10.
#
# Phase 2 — WITH shim. Same conninfo, but LD_PRELOAD points at
#           cbshim.so. Must SUCCEED. A failure here means the
#           cbmap.txt / shim wiring is broken — exit 11.
#
# Phase 3 — WITH shim, channel_binding=require. Forces -PLUS (no
#           fallback to plain SCRAM possible). Must SUCCEED. Proves
#           the shim is actually feeding a valid -PLUS proof, not
#           hiding behind a downgrade.

set -u

CONNINFO_BASE="host=mitm port=5432 dbname=testdb user=postgres password=secret123 sslmode=require"

echo ""
echo "============================================================"
echo "Phase 1: WITHOUT shim — expect FATAL channel binding"
echo "============================================================"
unset LD_PRELOAD
if /usr/local/bin/client "$CONNINFO_BASE channel_binding=require" 2>&1; then
    echo "FAIL: phase 1 succeeded but should have failed (channel binding not in effect?)"
    exit 10
fi
echo "PASS: phase 1 failed as expected"

echo ""
echo "============================================================"
echo "Phase 2: WITH shim, channel_binding=prefer — expect success"
echo "============================================================"
export LD_PRELOAD=/usr/local/lib/cbshim.so
export CBSHIM_HASHMAP=/keploy-tls/cbmap.txt
export CBSHIM_DEBUG=1
if ! /usr/local/bin/client "$CONNINFO_BASE channel_binding=prefer" 2>&1; then
    echo "FAIL: phase 2 (shim+prefer) failed — shim or cbmap not wired"
    echo "--- cbmap.txt ---"
    cat /keploy-tls/cbmap.txt 2>&1
    exit 11
fi
echo "PASS: phase 2 succeeded with shim"

echo ""
echo "============================================================"
echo "Phase 3: WITH shim, channel_binding=require — expect success"
echo "============================================================"
if ! /usr/local/bin/client "$CONNINFO_BASE channel_binding=require" 2>&1; then
    echo "FAIL: phase 3 (shim+require) failed — -PLUS proof rejected"
    exit 12
fi
echo "PASS: phase 3 succeeded with shim under require"

echo ""
echo "============================================================"
echo "ALL PHASES PASSED"
echo "============================================================"
exit 0

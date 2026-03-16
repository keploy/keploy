#!/bin/bash
set -e

# Validate RPS threshold from k6 output
echo "📊 Validating RPS threshold..."

if [ ! -f "k6-output.log" ]; then
  echo "❌ ERROR: k6-output.log not found"
  exit 1
fi

# Extract actual RPS from k6 console output
ACTUAL_RPS=$(grep -oP 'http_reqs.*?:\s+\d+\s+\K[\d.]+(?=/s)' k6-output.log | tail -1)

if [ -z "$ACTUAL_RPS" ]; then
  echo "❌ FAIL: Could not extract RPS from k6 output"
  exit 1
fi

echo "Actual RPS: $ACTUAL_RPS"

# Extract and display percentile metrics
echo ""
echo "📊 Performance Metrics:"
grep -E "http_req_duration|http_req_waiting" k6-output.log | head -2

# Validate RPS >= 100
if (( $(echo "$ACTUAL_RPS >= 100" | bc -l) )); then
  echo ""
  echo "✅ PASS: Achieved $ACTUAL_RPS RPS (target: 100)"
  exit 0
else
  echo ""
  echo "❌ FAIL: Only achieved $ACTUAL_RPS RPS (target: 100)"
  exit 1
fi

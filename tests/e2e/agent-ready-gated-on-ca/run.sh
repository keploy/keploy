#!/usr/bin/env bash
# End-to-end regression guard for the /agent/ready gate on CA readiness.
#
# What this asserts:
#   1. Immediately on startup, POST /agent/ready returns 503 (CA not
#      yet written) and the agent stdout contains the "refusing"
#      warning with ca_path=/tmp/keploy-tls/ca.crt.
#   2. After the harness flips the CAReady signal, subsequent POST
#      /agent/ready return 200 and /tmp/agent.ready exists inside the
#      agent container.
#
# Exit codes:
#   0 — both assertions hold.
#   nonzero — regression (or infrastructure error; see stderr).

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

COMPOSE="${COMPOSE:-docker compose}"
PROJECT="agent-ready-gate-e2e"

cleanup() {
  $COMPOSE -p "$PROJECT" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo ">> building stack"
$COMPOSE -p "$PROJECT" build --quiet

echo ">> starting stack"
$COMPOSE -p "$PROJECT" up -d

# Wait long enough for: boot + ~3s of pre-CAReady 503s + CAReady + ~5s of 200s.
echo ">> waiting for client to finish 30 poll iterations"
for _ in $(seq 1 60); do
  if $COMPOSE -p "$PROJECT" exec -T client sh -c 'grep -q "^DONE$" /shared/status.log 2>/dev/null'; then
    break
  fi
  sleep 1
done

LOG=$(mktemp)
$COMPOSE -p "$PROJECT" exec -T client sh -c 'cat /shared/status.log' > "$LOG"

echo ">> status log:"
cat "$LOG"

# First recorded status must be 503 (CA not ready yet).
first_code=$(awk 'NR==1{print $2}' "$LOG")
if [[ "$first_code" != "503" ]]; then
  echo "FAIL: expected first status=503, got '$first_code'" >&2
  exit 2
fi

# Somewhere in the log there must be a 200 (CA ready after delay).
if ! awk '{print $2}' "$LOG" | grep -qx "200"; then
  echo "FAIL: no 200 observed after CAReady delay" >&2
  exit 3
fi

# After the first 200, every subsequent status must stay 200.
if ! awk '
  $2=="200" { seen=1; next }
  seen && $2!="200" && $2!="DONE" { exit 1 }
' "$LOG"; then
  echo "FAIL: observed non-200 status after the CAReady transition" >&2
  exit 4
fi

# Agent stdout must contain the "refusing" warning (zap JSON output).
AGENT_LOG=$(mktemp)
$COMPOSE -p "$PROJECT" logs agent > "$AGENT_LOG" 2>&1 || true
if ! grep -q "CA bundle is written; refusing" "$AGENT_LOG"; then
  echo "FAIL: agent log missing 'refusing' warning" >&2
  echo "--- agent log ---" >&2
  cat "$AGENT_LOG" >&2
  exit 5
fi

# /tmp/agent.ready must exist inside the agent container by now.
if ! $COMPOSE -p "$PROJECT" exec -T agent sh -c 'test -f /tmp/agent.ready'; then
  echo "FAIL: /tmp/agent.ready not present in agent container" >&2
  exit 6
fi

echo "PASS: /agent/ready correctly gated on CA readiness"
exit 0

#!/usr/bin/env bash
# End-to-end regression guard for the /agent/ready gate on CA readiness.
#
# What this asserts:
#   1. Immediately on startup, POST /agent/ready returns 503 (CA not
#      yet installed) and the agent stdout contains the "refusing"
#      warning (message is mode-agnostic — the install path lives in
#      the upstream SetupCA log line, not in this handler).
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

# Pre-pull the `client` service base image with retries. ECR Public is
# the Docker Hub mirror we use (see docker-compose.yml), but shared-IP
# CI runners (GitHub Actions, Woodpecker) occasionally hit its
# unauthenticated rate limit and `docker compose up` has no native
# retry — it fails fast with "toomanyrequests: Rate exceeded". The pull
# is idempotent (cached on subsequent runs), so retrying for up to ~1m
# converts a transient infrastructure flake into a successful run
# without masking a genuine registry outage.
CLIENT_IMAGE=$(awk '/^  client:/,/^  [a-z]/' docker-compose.yml | awk '/^[[:space:]]*image:/ {print $2; exit}')
echo ">> pre-pulling client image ($CLIENT_IMAGE) with retries"
pull_ok=0
for attempt in 1 2 3 4 5 6; do
  if docker pull "$CLIENT_IMAGE"; then
    pull_ok=1
    break
  fi
  # Exponential-ish backoff (2,4,8,16,16,16s) — first three cover
  # brief ratelimit windows, the remaining hold steady rather than
  # stretching the test's wall clock indefinitely.
  delay=$((attempt*attempt*2))
  if [ "$delay" -gt 16 ]; then delay=16; fi
  echo ">> pull attempt $attempt failed; retrying in ${delay}s" >&2
  sleep "$delay"
done
if [ "$pull_ok" -ne 1 ]; then
  echo "FAIL: unable to pull $CLIENT_IMAGE after 6 attempts — registry may be down" >&2
  exit 7
fi

echo ">> starting stack"
$COMPOSE -p "$PROJECT" up -d

# Wait long enough for: boot + ~3s of pre-CAReady 503s + CAReady + ~5s of 200s.
#
# The client container writes a literal "DONE" sentinel line to
# /shared/status.log after its 30-iteration poll finishes. We wait up
# to 60s for that sentinel; if it never appears, the client is wedged
# (e.g. agent container crashed, volume mount busted, apk-add curl
# failed) and continuing would just produce a confusing parse error
# downstream. Fail loudly here with the client+agent logs so the
# regression is easy to diagnose.
echo ">> waiting for client to finish 30 poll iterations"
saw_done=0
for _ in $(seq 1 60); do
  if $COMPOSE -p "$PROJECT" exec -T client sh -c 'grep -q "^DONE$" /shared/status.log 2>/dev/null'; then
    saw_done=1
    break
  fi
  sleep 1
done

if [[ "$saw_done" -ne 1 ]]; then
  echo "FAIL: timed out after 60s waiting for client DONE sentinel" >&2
  echo "--- status.log (partial) ---" >&2
  $COMPOSE -p "$PROJECT" exec -T client sh -c 'cat /shared/status.log 2>/dev/null || echo "<unavailable>"' >&2 || true
  echo "--- client logs ---" >&2
  $COMPOSE -p "$PROJECT" logs client >&2 || true
  echo "--- agent logs ---" >&2
  $COMPOSE -p "$PROJECT" logs agent >&2 || true
  exit 10
fi

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
# Lines without an HTTP status in $2 (e.g. the trailing "DONE" sentinel
# or any blank line) are skipped.
if ! awk '
  NF < 2 { next }
  $1 == "DONE" { next }
  $2 !~ /^[0-9]+$/ { next }
  $2 == "200" { seen = 1; next }
  seen { exit 1 }
' "$LOG"; then
  echo "FAIL: observed non-200 status after the CAReady transition" >&2
  exit 4
fi

# Agent stdout must contain the "refusing" warning (zap JSON output).
AGENT_LOG=$(mktemp)
$COMPOSE -p "$PROJECT" logs agent > "$AGENT_LOG" 2>&1 || true
if ! grep -q "CA bundle is installed; refusing" "$AGENT_LOG"; then
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

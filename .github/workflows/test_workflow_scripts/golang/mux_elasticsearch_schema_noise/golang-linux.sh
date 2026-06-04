#!/usr/bin/env bash
#
# Schema-based request-body noise detection — end-to-end CI test using the real
# samples-go/mux-elasticsearch app.
#
# The app stamps a server-side `created_at` on every document it indexes, so the
# OUTGOING Elasticsearch request body (POST /documents/_doc) carries a volatile
# field that drifts between a keploy recording and each replay. Three scenarios:
#
#   Phase A — control (flag off): replay writes NO req_body_noise.
#   Phase B — detect+persist (--schema-noise-detection): the body.created_at
#             drift is detected and persisted as req_body_noise on the ES mock.
#   Phase C — strict (test.schemaNoiseStrict): a drift on a NON-noise field
#             (content, induced by tampering the recorded mock) is rejected.
#
# Runs inside samples-go/mux-elasticsearch. Expects: $KEPLOY_BIN (named
# "keploy"), an Elasticsearch reachable at 127.0.0.1:9200, passwordless sudo, Go.
# GitHub runs the `run:` step as `bash -e` and this file is sourced into it.
# This script does its own error handling (a FAILURES counter + explicit exit)
# and deliberately runs commands that return non-zero by design (pkill with no
# match, grep with no match, startup curl retries), so disable errexit here.
set +e
set -o pipefail

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

KEPLOY_BIN="${KEPLOY_BIN:-/usr/local/bin/keploy}"
APP_BIN=mux-elasticsearch
APP_DIR="$PWD"
SND_PATH="$APP_DIR/keploy-snd"          # dedicated path; leaves committed keploy/ untouched
export ELASTICSEARCH_URL="http://127.0.0.1:9200"
# Enable the app's schema-noise demo mode (server-stamped created_at + no
# keep-alive on the ES client). Off by default so the app's committed
# recordings stay valid; this pipeline opts in.
export STAMP_CREATED_AT=1

FAILURES=0
step()  { echo "== $* =="; }
pass()  { echo "  PASS: $*"; }
fail()  { echo "  FAIL: $*"; FAILURES=$((FAILURES + 1)); }
strip_ansi() { sed -E 's/\x1b\[[0-9;]*m//g'; }
mock_file() { find "$SND_PATH" -name mocks.yaml 2>/dev/null | head -1; }

cleanup() {
  sudo pkill -x keploy 2>/dev/null
  sudo pkill -x "$APP_BIN" 2>/dev/null
  return 0
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Build app + wait for Elasticsearch
# ---------------------------------------------------------------------------
step "Building mux-elasticsearch"
build_go_app() {
  local attempt=1
  while [ "$attempt" -le 4 ]; do
    if GOPROXY="proxy.golang.org,direct" go build -o "$APP_BIN" .; then return 0; fi
    echo "go build attempt ${attempt} failed; retrying…"; sleep $((attempt * 5)); attempt=$((attempt + 1))
  done
  echo "::error::go build for $APP_BIN failed"; return 1
}
build_go_app || exit 1

step "Waiting for Elasticsearch at $ELASTICSEARCH_URL"
for i in $(seq 1 30); do
  if curl -fs "$ELASTICSEARCH_URL/_cluster/health" >/dev/null 2>&1; then echo "  ES ready"; break; fi
  sleep 4
done
curl -fs "$ELASTICSEARCH_URL/_cluster/health" >/dev/null 2>&1 || { echo "::error::Elasticsearch not reachable"; exit 1; }

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
record_fresh() {
  cleanup
  sudo rm -rf "$SND_PATH"
  sudo -E env "PATH=$PATH" "ELASTICSEARCH_URL=$ELASTICSEARCH_URL" \
    "$KEPLOY_BIN" record -c "./$APP_BIN" --path "$SND_PATH" >"$APP_DIR/record.log" 2>&1 &
  local rec_pid=$!
  local i
  for i in $(seq 1 60); do curl -fs http://localhost:8000/ >/dev/null 2>&1 && break; sleep 1; done
  sleep 2
  curl -s -X POST http://localhost:8000/documents \
    -H 'Content-Type: application/json' \
    -d '{"title":"hello","content":"world"}' >/dev/null
  sleep 6
  sudo kill -INT "$rec_pid" 2>/dev/null
  for i in $(seq 1 25); do sudo kill -0 "$rec_pid" 2>/dev/null || break; sleep 1; done
  cleanup
  if [ -z "$(mock_file)" ]; then
    echo "  no mock recorded; record.log tail:"; tail -20 "$APP_DIR/record.log" | strip_ansi
    fail "recording produced no ES mock"; return 1
  fi
}

# replay <logfile> <extra keploy test args...>
replay() {
  local log="$1"; shift
  cleanup
  sudo -E env "PATH=$PATH" "ELASTICSEARCH_URL=$ELASTICSEARCH_URL" \
    "$KEPLOY_BIN" test -c "./$APP_BIN" --path "$SND_PATH" --delay 10 "$@" >"$log" 2>&1
  local rc=$?
  cleanup
  return $rc
}

checkout_passed() {  # post-documents testcase passed?
  grep -oE '"testcase id": "[^"]*document[^"]*".*"passed": "[^"]+"' "$1" \
    | strip_ansi | grep -q '"passed": "true"'
}
mock_has_created_at_noise() { grep -A5 'req_body_noise' "$(mock_file)" 2>/dev/null | grep -q 'body.created_at'; }
mock_has_any_noise()        { grep -q 'req_body_noise' "$(mock_file)" 2>/dev/null; }

# ---------------------------------------------------------------------------
# Phase A — control
# ---------------------------------------------------------------------------
step "Phase A — control (no --schema-noise-detection)"
record_fresh
mock_has_any_noise && fail "fresh recording already has req_body_noise" \
                   || pass "fresh recording has no req_body_noise"
replay "$APP_DIR/test_control.log" --remove-unused-mocks
checkout_passed "$APP_DIR/test_control.log" && pass "replay passed with flag off" \
                                            || fail "replay should pass with flag off"
mock_has_any_noise && fail "flag off must NOT write req_body_noise" \
                   || pass "flag off wrote no req_body_noise (inert when disabled)"

# ---------------------------------------------------------------------------
# Phase B — detect + persist
# ---------------------------------------------------------------------------
step "Phase B — detect + persist (--schema-noise-detection)"
record_fresh
replay "$APP_DIR/test_detect.log" --schema-noise-detection --remove-unused-mocks
checkout_passed "$APP_DIR/test_detect.log" && pass "replay passed with detection on" \
                                           || fail "replay should pass with detection on"
if mock_has_created_at_noise; then
  pass "ES mock persisted req_body_noise: body.created_at"
  grep -B1 -A2 'req_body_noise' "$(mock_file)" | sed 's/^/    /'
else
  fail "expected req_body_noise: body.created_at in the ES mock"
  echo "  --- mocks.yaml ---"; cat "$(mock_file)" 2>/dev/null | sed 's/^/    /'
fi

# ---------------------------------------------------------------------------
# Phase C — strict field-specificity
# Reuses Phase B's noise-bearing mock. Only created_at drifts naturally (and it
# is now noise), so to exercise rejection we tamper a NON-noise field (content)
# in the recorded ES request body: on replay the app re-sends "world", which
# now differs from the mock -> strict matching must reject it.
# ---------------------------------------------------------------------------
step "Phase C — strict matching (test.schemaNoiseStrict: true)"
sed -i 's/"content":"world"/"content":"TAMPERED"/' "$(mock_file)"
STRICT_CFG="$APP_DIR/.strict-config"; mkdir -p "$STRICT_CFG"
cat >"$STRICT_CFG/keploy.yml" <<'EOF'
path: ""
appName: mux-elasticsearch
test:
    schemaNoiseStrict: true
EOF
replay "$APP_DIR/test_strict.log" --schema-noise-detection --config-path "$STRICT_CFG" --debug
if grep 'strict req-body match rejected mock' "$APP_DIR/test_strict.log" | grep -q 'body.content'; then
  pass "strict matching rejected the mock on non-noise drift (body.content)"
else
  fail "expected strict rejection on body.content"
  grep -i 'strict\|reject\|drift' "$APP_DIR/test_strict.log" | strip_ansi | tail -10 | sed 's/^/    /'
fi

# ---------------------------------------------------------------------------
echo
if [ "$FAILURES" -eq 0 ]; then
  echo "ALL CHECKS PASSED — schema-noise detection verified on mux-elasticsearch."
  exit 0
fi
echo "$FAILURES CHECK(S) FAILED."
exit 1

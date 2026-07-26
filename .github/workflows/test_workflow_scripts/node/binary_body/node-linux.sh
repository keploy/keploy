#!/usr/bin/env bash
# Record-replay test for a Node.js app that serves HTTP responses with
# non-UTF-8 bytes (application/zip, application/octet-stream). Before the
# body_base64 fix, recording this sample failed with
# "yaml: cannot marshal invalid UTF-8 data as !!str" as reported in
# keploy/enterprise#1902.

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== keploy tree =="
  find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort || true
  for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; cat "$f"; }; done
  exit "$rc"
}
trap die ERR

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

section "Prepare workspace"
npm install --silent
rm -rf keploy/
rm -f keploy.yml
echo "Record: $RECORD_BIN"; sudo "$RECORD_BIN" --version
echo "Replay: $REPLAY_BIN"; sudo "$REPLAY_BIN" --version
sudo "$RECORD_BIN" config --generate
# ETag header is content-hash based; Date is a wall-clock — noise both so
# replay matches even if Express regenerates the headers.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"header": {"Date": [], "ETag": []}}/' "$config_file"
endsec

send_requests() {
  # Wait for the app to come up (server.js listens on :3000 by default).
  local tries=0
  until curl -sf http://localhost:3000/health >/dev/null; do
    tries=$((tries + 1))
    if [[ $tries -ge 30 ]]; then
      echo "::error::App did not become healthy within 30s"
      return 1
    fi
    sleep 1
  done
  echo "App up after ${tries}s"

  # Hit the two endpoints whose payloads contain non-UTF-8 bytes. These are
  # what used to blow up keploy's yaml encoder.
  curl -sS -o /tmp/zip.out     -w "zip %{http_code} %{size_download}\n"     http://localhost:3000/zip
  curl -sS -o /tmp/octets.out  -w "oct %{http_code} %{size_download}\n"     http://localhost:3000/octets

  # Give the recorder a moment to flush testcase + mock YAML.
  sleep 3

  # Stop keploy gracefully so it finalises the report on disk.
  pid=$(pgrep -x keploy || true)
  if [[ -n "$pid" ]]; then
    echo "Stopping keploy (pid=$pid)"
    sudo kill "$pid" || true
  fi
}

# -------- Record phase --------
section "Record"
send_requests &
# Without the fix this terminates early with
#   "error while inserting mock into db, hence stopping keploy"
sudo -E "$RECORD_BIN" record -c 'node server.js' &> record.txt || true
cat record.txt

if grep -q "cannot marshal invalid UTF-8 data as !!str" record.txt; then
  echo "::error::Recorder hit the binary-body regression (keploy/enterprise#1902)"
  exit 1
fi
if grep -q "error while inserting mock into db, hence stopping keploy" record.txt; then
  echo "::error::Recorder stopped on mock insert — binary body not serialised"
  exit 1
fi
if grep -q "WARNING: DATA RACE" record.txt; then
  echo "::error::Data race detected during record"
  exit 1
fi

# Sanity: a test set must have been written.
if ! ls ./keploy/test-set-*/tests/*.yaml >/dev/null 2>&1; then
  echo "::error::No test cases were captured"
  find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort || true
  exit 1
fi

# Sanity: the zip response body must round-trip through YAML. On the fixed
# code path it lands under `body_base64:` (base64 of the raw zip bytes).
if ! grep -R "body_base64:" ./keploy/test-set-*/tests/ >/dev/null 2>&1; then
  echo "::error::Expected body_base64 to be present for the application/zip test case"
  find ./keploy/test-set-*/tests -maxdepth 2 -type f -print 2>/dev/null | sort || true
  exit 1
fi
endsec

# -------- Replay phase --------
section "Replay"
sudo -E "$REPLAY_BIN" test -c 'node server.js' --delay 10 &> replay.txt || true
cat replay.txt

if grep -q "ERROR" replay.txt; then
  # Surface offending lines first so the failure is easy to read in CI logs.
  grep "ERROR" replay.txt | head -20
  echo "::error::Error in replay output"
  exit 1
fi
if grep -q "WARNING: DATA RACE" replay.txt; then
  echo "::error::Data race detected during replay"
  exit 1
fi

report=$(ls -1 ./keploy/reports/test-run-0/*-report.yaml 2>/dev/null | head -n 1 || true)
if [[ -z "$report" ]]; then
  echo "::error::No replay report produced"
  find ./keploy/reports -maxdepth 3 -type f 2>/dev/null || true
  exit 1
fi
status=$(grep -m1 '^status:' "$report" | awk '{print $2}')
echo "Replay status: $status"
if [[ "$status" != "PASSED" ]]; then
  echo "::error::Replay did not pass"
  cat "$report"
  exit 1
fi
endsec

echo "Binary body record+replay succeeded."

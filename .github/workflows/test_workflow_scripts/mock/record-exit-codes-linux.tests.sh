#!/usr/bin/env bash
# Tests for record-exit-codes-linux.sh, with a fake keploy in place of the
# binary. Needs password-free sudo, as a runner has. Run by
# sudo-env-scripts.yml, or locally:
#
#   bash .github/workflows/test_workflow_scripts/mock/record-exit-codes-linux.tests.sh
#
# a keploy record that hangs in every case
#   Each case's hang guard has to end it, and the lane has to fail every case
#   as hung and exit 1: app-killed too, where the guard's SIGKILL gives the
#   very exit code (137) and the fake the very log line ("signal: killed")
#   that case wants, so only the guard's line in the log tells the hang.
set -u
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d)"
# The fake runs as root under sudo, and the guard's SIGKILL ends sudo, which
# cannot relay it: end each fake by the pid it wrote.
cleanup() {
  local pids
  pids=$(cat "$work/pids" 2>/dev/null)
  # shellcheck disable=SC2086
  [ -z "$pids" ] || sudo kill -KILL $pids 2>/dev/null
  sudo rm -rf "$work"
}
trap cleanup EXIT

cat >"$work/keploy" <<'FAKE'
#!/usr/bin/env bash
# A keploy record that never ends, having printed the line app-killed wants.
echo $$ >>"$(dirname "$0")/pids"
case "$*" in *"kill -KILL"*) echo "signal: killed" ;; esac
exec sleep 600
FAKE
chmod +x "$work/keploy"

fails=0
RECORD_BIN="$work/keploy" RECORD_HANG_SECONDS=2 bash "$here/record-exit-codes-linux.sh" >"$work/out" 2>&1 </dev/null
rc=$?
bad=0
if [ "$rc" -ne 1 ]; then
  echo "FAIL hang: the lane exited $rc, want 1"
  bad=1
fi
for c in app-exits-0 app-exits-42 app-terminated app-interrupted app-killed record-timer sigint; do
  if ! grep -qxF "FAIL: $c: keploy record hung, and the hang guard killed it" "$work/out"; then
    echo "FAIL hang: the lane did not fail $c as hung"
    bad=1
  fi
done
if [ "$bad" -ne 0 ]; then
  echo "--- the lane's output:"
  cat "$work/out"
  echo "---"
  fails=$((fails + 1))
else
  echo "ok: a keploy record that hangs fails every case as hung, app-killed's 137 too"
fi

if [ "$fails" -ne 0 ]; then
  echo "$fails case(s) failed"
  exit 1
fi
echo "all cases passed"

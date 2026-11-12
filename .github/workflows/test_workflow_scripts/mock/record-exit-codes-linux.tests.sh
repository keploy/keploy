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
#   The fake runs as root under sudo, which cannot relay the guard's SIGKILL:
#   the guard ends it through sudo -n kill, and no fake may run after the lane.
set -u
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
# running PID: whether PID runs, by its state in /proc (root's too).
running() { local s; { read -r s <"/proc/$1/stat"; } 2>/dev/null || return 1; s=${s##*) }; [ "${s%% *}" != Z ]; }

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
# Each case's fake ended with its case (2 s for root's to be reaped); one
# left is ended here, so the failed row holds nothing after it.
left=""
for ((i = 0; i < 10; i++)); do
  left=""
  for p in $(cat "$work/pids" 2>/dev/null); do running "$p" && left+=" $p"; done
  [ -n "$left" ] || break
  sleep 0.2
done
if [ "$(wc -l <"$work/pids" 2>/dev/null)" != 7 ] || [ -n "$left" ]; then
  echo "FAIL hang: the guard's KILL left keploy running as root:${left:- none}; $(wc -l <"$work/pids" 2>/dev/null) of 7 fakes started"
  # shellcheck disable=SC2086
  [ -z "$left" ] || sudo -n kill -s KILL $left 2>/dev/null
  bad=1
fi
if [ "$bad" -ne 0 ]; then
  echo "--- the lane's output:"
  cat "$work/out"
  echo "---"
  fails=$((fails + 1))
else
  echo "ok: a keploy record that hangs fails every case as hung, app-killed's 137 too, and none runs after"
fi

if [ "$fails" -ne 0 ]; then
  echo "$fails case(s) failed"
  exit 1
fi
echo "all cases passed"

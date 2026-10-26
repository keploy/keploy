#!/usr/bin/env bash
# capture.sh: writes this directory's pip-*.log, go-compile-error.log,
# py-compile-error.log, py-attribute-error.log,
# py-syntax-error-names-timeout-class.log and the two buildkit-* logs made
# from them, from real runs of pip, go and python. Run by hand, where
# python3 (3.10 or later, with venv), go (1.22 or later) and the network to
# install pip and fetch one wheel are at hand; it writes nothing outside a
# temporary directory and this one (no pip or Go cache, no Go telemetry), and
# stops what it starts:
#
#   bash .github/workflows/test_workflow_scripts/docker-build-retry.tests.d/capture.sh
#
# pip runs against pip-stubs.py, so every failure is the one asked for, and
# against 192.0.2.1 (TEST-NET-1) for a connect timeout: that needs a network
# that drops packets to it, as most do. One that refuses them gives
# NewConnectionError instead, and pip-*connect-timeout.log then no longer
# shows a timeout. The other logs here are not written by this script:
# buildkit-pip-read-timeout-macos.log is a CI job's own (see
# docker-build-retry.tests.sh), and the buildkit-*go*, buildkit-docker-hub-*,
# buildkit-two-failed-steps-* and buildkit-failed-step-number-prefix logs are
# written by hand.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/docker-build-retry-capture.XXXXXX")"
stubs=
stop_stubs() {
  if [ -n "$stubs" ]; then
    kill "$stubs" 2>/dev/null || true
    wait "$stubs" 2>/dev/null || true
    stubs=
  fi
}
# Every background job, not only $stubs: a stub started a moment before the
# script is stopped may not be in $stubs yet.
trap 'stop_stubs; kill $(jobs -p) 2>/dev/null || true; rm -rf "$work"' EXIT

# No pip configuration of the machine it runs on reaches a log. --isolated
# (on every pip below) drops the PIP_* variables and the user's pip.conf, but
# not the global ones (/etc/pip.conf, /etc/xdg/pip/pip.conf,
# /Library/Application Support/pip/pip.conf) nor the file PIP_CONFIG_FILE
# names; with PIP_CONFIG_FILE set to /dev/null pip reads no file at all. An
# extra index there would put its host in every log, and a retries setting
# would change the logs meant to show pip's default retries.
export PIP_CONFIG_FILE=/dev/null

# The pips the logs come from: 25.0.1 prints a timed-out download's
# traceback, and 24.2 and 25.0.1 take a download cut short for a whole one;
# 25.1.1 says incomplete-download instead, and 26.0.1 resumes the download
# first.
for v in 24.2 25.0.1 25.1.1 26.0.1; do
  python3 -m venv "$work/pip-$v"
  "$work/pip-$v/bin/python" -m pip install -q --isolated --no-cache-dir --disable-pip-version-check "pip==$v"
done
"$work/pip-26.0.1/bin/pip" download -q --isolated --no-cache-dir --disable-pip-version-check --no-deps -d "$work/wheel" flask==3.1.2
wheel="$work/wheel/flask-3.1.2-py3-none-any.whl"

# start_stubs [flaky failures]: a fresh set of stubs, whose /flaky/ index
# leaves that many first requests unanswered (default 1).
start_stubs() {
  stop_stubs
  rm -f "$work/ports"
  FLAKY_FAILS="${1:-1}" python3 "$here/pip-stubs.py" "$wheel" "$work/ports" &
  stubs=$!
  for _ in $(seq 1 100); do
    [ -f "$work/ports" ] && break
    sleep 0.1
  done
  [ -f "$work/ports" ] || { echo "pip-stubs.py never wrote its ports" >&2; exit 1; }
  # shellcheck disable=SC1091
  . "$work/ports"
}

# pip_log <log> <pip version> <pip install args...>: pip's output, as it is,
# with pip's default retries and a 3 s timeout, and no configuration of the
# machine's (above). No proxy either: pip takes http_proxy and the like from
# the environment whatever else it ignores, and through a proxy a connect
# timeout to 192.0.2.1 is a ProxyError. (The installs above may need one.)
pip_log() {
  local log="$1" v="$2"
  shift 2
  no_proxy='*' NO_PROXY='*' "$work/pip-$v/bin/pip" install --isolated --no-cache-dir --disable-pip-version-check \
    --timeout 3 --trusted-host 192.0.2.1 --target "$work/target" "$@" >"$here/$log" 2>&1 || true
  rm -rf "$work/target"
  echo "wrote $log"
}

start_stubs
pip_log pip-index-read-timeout.log 25.0.1 --index-url "http://127.0.0.1:$IDX_HANG/simple/" flask==3.1.2
pip_log pip-index-connect-timeout.log 24.2 --index-url http://192.0.2.1/simple/ flask==3.1.2
pip_log pip-wheel-connect-timeout.log 25.0.1 --no-deps --index-url "http://127.0.0.1:$IDX/simple/" flask==3.1.2
pip_log pip-wheel-connect-timeout-no-retries.log 25.0.1 --retries 0 --no-deps --index-url "http://127.0.0.1:$IDX/simple/" flask==3.1.2
pip_log pip-wheel-hang-retries-spent.log 25.0.1 --no-deps --index-url "http://127.0.0.1:$IDX/hang/" flask==3.1.2
pip_log pip-wheel-hang-no-retries.log 25.0.1 --retries 0 --no-deps --index-url "http://127.0.0.1:$IDX/hang/" flask==3.1.2
pip_log pip-wheel-read-timeout-traceback.log 25.0.1 --no-deps --index-url "http://127.0.0.1:$IDX/stall/" flask==3.1.2
pip_log pip-wheel-read-timeout-incomplete-download.log 25.1.1 --no-deps --index-url "http://127.0.0.1:$IDX/stall/" flask==3.1.2
pip_log pip-wheel-read-timeout-resumed-incomplete-download.log 26.0.1 --no-deps --index-url "http://127.0.0.1:$IDX/stall/" flask==3.1.2
pip_log pip-wheel-cut-short-incomplete-download.log 25.1.1 --no-deps --index-url "http://127.0.0.1:$IDX/cut/" flask==3.1.2
pip_log pip-wheel-cut-short-resumed-incomplete-download.log 26.0.1 --no-deps --index-url "http://127.0.0.1:$IDX/cut/" flask==3.1.2
pip_log pip-wheel-cut-short-invalid-wheel.log 25.0.1 --no-deps --index-url "http://127.0.0.1:$IDX/cut/" flask==3.1.2
pip_log pip-wheel-cut-short-invalid-wheel-pip24.log 24.2 --no-deps --index-url "http://127.0.0.1:$IDX/cut/" flask==3.1.2
pip_log pip-missing-package.log 26.0.1 --index-url "http://127.0.0.1:$IDX/simple/" nosuchpkg==0.0.0
pip_log pip-recovered-reset-then-missing-package.log 25.0.1 --no-deps --index-url "http://127.0.0.1:$IDX/reset/" flask==3.1.2 nosuchpkg==0.0.0
start_stubs
pip_log pip-recovered-retry-then-missing-package.log 25.0.1 --no-deps --index-url "http://127.0.0.1:$IDX/flaky/" flask==3.1.2 nosuchpkg==0.0.0
# Five unanswered requests, so the one pip gets through on is its last try.
start_stubs 5
pip_log pip-recovered-on-last-try.log 25.0.1 --no-deps --index-url "http://127.0.0.1:$IDX/flaky/" flask==3.1.2
stop_stubs

# Compile errors. Go's is the one a match on "unexpected EOF" once retried.
mkdir -p "$work/go" "$work/py"
printf 'package main\n\nimport "fmt"\n\nfunc main() {\n\tfmt.Println("x")\n' >"$work/go/main.go"
printf 'module app\n\ngo 1.22\n' >"$work/go/go.mod"
# HOME and XDG_CONFIG_HOME: Go 1.23 and later keep telemetry counters there.
(cd "$work/go" && HOME="$work/home" XDG_CONFIG_HOME="$work/config" GOCACHE="$work/gocache" \
  GOTOOLCHAIN=local CGO_ENABLED=0 go build -o /dev/null . >"$here/go-compile-error.log" 2>&1) || true
printf 'def main():\n    print("x"\n' >"$work/py/app.py"
(cd "$work/py" && python3 -m py_compile app.py >"$here/py-compile-error.log" 2>&1) || true
# Failures that name a timeout class without being a timeout: a misspelt
# attribute, and a syntax error, whose message quotes the line it failed on.
(cd "$work/py" && "$work/pip-25.0.1/bin/python" -c 'from pip._vendor.urllib3 import exceptions; exceptions.ReadTimeoutErorr' >"$here/py-attribute-error.log" 2>&1) || true
printf 'import urllib3\n\n\ndef fetch(url):\n    try:\n        return urllib3.request("GET", url)\n    except urllib3.exceptions.ReadTimeoutError: as err:\n        raise\n' >"$work/py/fetch.py"
(cd "$work/py" && python3 -m py_compile fetch.py >"$here/py-syntax-error-names-timeout-class.log" 2>&1) || true
echo "wrote go-compile-error.log py-compile-error.log py-attribute-error.log py-syntax-error-names-timeout-class.log"

# The two BuildKit logs: steps #0-#7 of the CI job's log (which uses the
# python:3.9 image), then the captured output as BuildKit frames a step's.
frame() { sed -n '1,27p' "$here/buildkit-pip-read-timeout-macos.log" | sed "s#\[\([1-3]\)/4\]#[\1/$1]#"; }
step() { sed "s/^/#$1 $2 /" "$3"; }
summary() {
  echo '------'
  echo " > [$1] RUN $2:"
  sed "s/^/$3 /" "$4"
  echo '------'
}
{
  frame 4
  echo '#8 [4/4] RUN pip3 install -r requirements.txt'
  step 8 1.196 "$here/pip-missing-package.log"
  echo '#8 ERROR: process "/bin/sh -c pip3 install -r requirements.txt" did not complete successfully: exit code: 1'
  summary 4/4 'pip3 install -r requirements.txt' 1.196 "$here/pip-missing-package.log"
  echo 'ERROR: failed to build: failed to solve: process "/bin/sh -c pip3 install -r requirements.txt" did not complete successfully: exit code: 1'
} >"$here/buildkit-pip-missing-package.log"
{
  frame 5
  echo '#8 [4/5] RUN pip3 install -r requirements.txt'
  step 8 18.104 "$here/pip-recovered-on-last-try.log"
  echo '#8 DONE 18.9s'
  echo
  echo '#9 [5/5] RUN python -m py_compile app.py'
  step 9 0.311 "$here/py-compile-error.log"
  echo '#9 ERROR: process "/bin/sh -c python -m py_compile app.py" did not complete successfully: exit code: 1'
  summary 5/5 'python -m py_compile app.py' 0.311 "$here/py-compile-error.log"
  echo 'ERROR: failed to build: failed to solve: process "/bin/sh -c python -m py_compile app.py" did not complete successfully: exit code: 1'
} >"$here/buildkit-recovered-pip-retry-then-syntax-error.log"
echo "wrote buildkit-pip-missing-package.log buildkit-recovered-pip-retry-then-syntax-error.log"

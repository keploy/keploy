#!/usr/bin/env bash
# Tests for fetch.sh and go-retry.sh: the lanes' shared fetch rule.
#
#   bash .github/workflows/test_workflow_scripts/fetch-test.sh
#
# No network: go, python3, git, docker and (for the backoff cases) sleep are
# stubs on PATH that record how they were called and fail as a case says.
# Checked: each subcommand retries what it should and still fails what
# persists; Go goes through GOPROXY="https://proxy.golang.org|direct" unless the
# caller chose one; a failure the pipe reports as the module origin's ("invalid
# version: git ls-remote") is retried, a compile error is not; a clone starts
# every attempt from an empty directory and never prints the credentials in its
# URL; pip gets its idle timeout and resume setting; the backoff is attempt x
# 10 s; and docker-build-retry.sh retries a build or pull that failed on a
# fetch's transport on the same schedule, and nothing else.
set -Eeuo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
FETCH="$HERE/fetch.sh"

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "  PASS  $1"; }
bad() {
  fail=$((fail + 1))
  echo "  FAIL  $1"
  [[ -n "${2:-}" ]] && echo "          $2" || true
}

# stub DIR NAME FAILURES [MESSAGE]: an executable NAME that records each call
# (arguments, cwd and the env this test cares about) to DIR/NAME.calls, and
# fails its first FAILURES calls printing MESSAGE to stderr.
stub() {
  local dir=$1 name=$2 failures=$3 msg=${4:-transient}
  cat >"$dir/$name" <<STUB
#!/usr/bin/env bash
n=\$(( \$(wc -l 2>/dev/null <"$dir/$name.calls" || echo 0) + 1 ))
printf '%s|cwd=%s|GOPROXY=%s|PIP_TIMEOUT=%s|PIP_RESUME_RETRIES=%s|LSL=%s|LST=%s\n' \
  "\$*" "\$PWD" "\${GOPROXY-<unset>}" "\${PIP_TIMEOUT-}" "\${PIP_RESUME_RETRIES-}" \
  "\${GIT_HTTP_LOW_SPEED_LIMIT-}" "\${GIT_HTTP_LOW_SPEED_TIME-}" >>"$dir/$name.calls"
if [ "\$n" -le $failures ]; then echo "$msg" >&2; exit 1; fi
exit 0
STUB
  chmod +x "$dir/$name"
}

# run DIR ARGS...: fetch.sh with the stubs first on PATH and no backoff; sets
# rc and leaves the output in DIR/out.
run() {
  local dir=$1
  shift
  rc=0
  (cd "$dir" && PATH="$dir:$PATH" FETCH_BASE_DELAY=0 GO_RETRY_BACKOFF=0 bash "$FETCH" "$@") >"$dir/out" 2>&1 || rc=$?
}
calls() { if [ -f "$1" ]; then wc -l <"$1" | tr -d ' '; else echo 0; fi; }

RESET='go: example.com/m@v1.0.0: read "https://proxy.golang.org/example.com/m/@v/v1.0.0.zip": stream error: stream ID 7; INTERNAL_ERROR; received from peer'
MASKED='go: example.com/m@v1.0.0: invalid version: git ls-remote -q --end-of-options https://example.com/m: exit status 128'
COMPILE='./main.go:3:1: syntax error: non-declaration statement outside function body'

echo "fetch.sh modules / go (through go_retry):"
t=$(mktemp -d)
stub "$t" go 1 "$RESET"
mkdir -p "$t/mod"
(unset GOPROXY; run "$t" modules "$t/mod"; echo "$rc" >"$t/rc")
rc=$(cat "$t/rc")
if [[ $rc == 0 && $(calls "$t/go.calls") == 2 ]] && grep -q "^mod download|cwd=$t/mod|GOPROXY=https://proxy.golang.org|direct|" "$t/go.calls"; then
  ok "a reset stream is retried, in DIR, through GOPROXY=https://proxy.golang.org|direct"
else bad "modules" "rc=$rc calls=$(calls "$t/go.calls"): $(cat "$t/go.calls" "$t/out" 2>&1)"; fi
rm -rf "$t"

t=$(mktemp -d)
stub "$t" go 2 "$MASKED"
(unset GOPROXY; run "$t" go mod tidy; echo "$rc" >"$t/rc")
rc=$(cat "$t/rc")
if [[ $rc == 0 && $(calls "$t/go.calls") == 3 ]]; then
  ok "an origin error the pipe reports in place of the proxy's (invalid version: git ls-remote) is retried"
else bad "masked" "rc=$rc calls=$(calls "$t/go.calls"): $(cat "$t/out")"; fi
rm -rf "$t"

t=$(mktemp -d)
stub "$t" go 9 "$COMPILE"
run "$t" go build ./...
if [[ $rc != 0 && $(calls "$t/go.calls") == 1 ]] && grep -q "does not look transient" "$t/out"; then
  ok "a compile error is not retried"
else bad "compile" "rc=$rc calls=$(calls "$t/go.calls"): $(cat "$t/out")"; fi
rm -rf "$t"

t=$(mktemp -d)
stub "$t" go 99 "$RESET"
run "$t" modules
if [[ $rc != 0 && $(calls "$t/go.calls") == 5 ]] && grep -q "failed after 5 attempts" "$t/out"; then
  ok "a persistent transient failure fails loudly after exactly 5 attempts"
else bad "persistent" "rc=$rc calls=$(calls "$t/go.calls"): $(cat "$t/out")"; fi
rm -rf "$t"

t=$(mktemp -d)
stub "$t" go 0
GOPROXY=off run "$t" modules
if [[ $rc == 0 ]] && grep -q "|GOPROXY=off|" "$t/go.calls"; then ok "a GOPROXY the caller set is kept"
else bad "caller GOPROXY" "rc=$rc: $(cat "$t/go.calls" "$t/out")"; fi
rm -rf "$t"

echo "fetch.sh pip:"
t=$(mktemp -d)
stub "$t" python3 1 "ReadTimeoutError: HTTPSConnectionPool(host='files.pythonhosted.org', port=443): Read timed out."
run "$t" pip -r requirements.txt
if [[ $rc == 0 && $(calls "$t/python3.calls") == 2 ]] &&
  grep -q "^-m pip install -r requirements.txt|.*|PIP_TIMEOUT=30|PIP_RESUME_RETRIES=5|" "$t/python3.calls"; then
  ok "python3 -m pip install ARGS, retried, with a 30 s idle timeout and resume"
else bad "pip" "rc=$rc: $(cat "$t/python3.calls" "$t/out" 2>&1)"; fi
rm -rf "$t"

t=$(mktemp -d)
stub "$t" mypython 0
FETCH_PYTHON=mypython run "$t" pip flask
if [[ $rc == 0 ]] && grep -q "^-m pip install flask|" "$t/mypython.calls"; then ok "FETCH_PYTHON picks the interpreter (a venv's python)"
else bad "FETCH_PYTHON" "rc=$rc: $(cat "$t/out")"; fi
rm -rf "$t"

echo "fetch.sh clone:"
t=$(mktemp -d)
cat >"$t/git" <<STUB
#!/usr/bin/env bash
echo "\$*|LSL=\${GIT_HTTP_LOW_SPEED_LIMIT-}|LST=\${GIT_HTTP_LOW_SPEED_TIME-}|dest_existed=\$([ -e "$t/dest" ] && echo yes || echo no)" >>"$t/git.calls"
case "\$1" in
  clone)
    if [ ! -e "$t/first" ]; then : >"$t/first"; mkdir -p "$t/dest"; : >"$t/dest/partial"; exit 128; fi
    mkdir -p "$t/dest" ;;
  -C) [ "\$3" = rev-parse ] && echo 0123abcd ;;
esac
exit 0
STUB
chmod +x "$t/git"
run "$t" clone "https://s3cr3t-token@github.com/keploy/x.git" "$t/dest"
if [[ $rc == 0 && $(grep -c '^clone ' "$t/git.calls") == 2 ]] &&
  grep '^clone ' "$t/git.calls" | sed -n 2p | grep -q "dest_existed=no" &&
  grep -q "|LSL=1000|LST=30|" "$t/git.calls" && [[ ! -e "$t/dest/partial" ]]; then
  ok "each attempt starts from an empty DIR; shallow; a transfer under 1000 B/s for 30 s is a stall"
else bad "clone" "rc=$rc: $(cat "$t/git.calls" "$t/out")"; fi
if ! grep -q "s3cr3t-token" "$t/out" && grep -q "https://github.com/keploy/x.git" "$t/out"; then
  ok "the credentials in a clone URL are never printed"
else bad "clone token" "$(cat "$t/out")"; fi
rm -rf "$t"

echo "the backoff:"
# Under bash, sleep is not a builtin, so the stub on PATH sees the real waits.
for sub in modules pip; do
  t=$(mktemp -d)
  stub "$t" go 3 "$RESET"
  stub "$t" python3 3 "ReadTimeoutError"
  printf '#!/usr/bin/env bash\nprintf "%%s " "$1" >>"%s/sleeps"\n' "$t" >"$t/sleep"
  chmod +x "$t/sleep"
  rc=0
  args=(modules)
  [ "$sub" = pip ] && args=(pip x)
  (cd "$t" && PATH="$t:$PATH" bash "$FETCH" "${args[@]}") >"$t/out" 2>&1 || rc=$?
  if [[ $rc == 0 && "$(cat "$t/sleeps" 2>/dev/null)" == "10 20 30 " ]]; then ok "$sub waits attempt x 10 s between attempts (10 20 30), none after the last"
  else bad "backoff $sub" "rc=$rc sleeps=[$(cat "$t/sleeps" 2>/dev/null)]: $(cat "$t/out")"; fi
  rm -rf "$t"
done

echo "docker-build-retry.sh (a fetch inside an image build, an image pull):"
# A docker that fails its first FAILURES builds or pulls printing MESSAGE, and
# has no image locally; sleep records the waits. docker_build_retry exits the
# shell it gives up in, so each case runs in a subshell.
# dbr DIR FAILURES MESSAGE FUNCTION ARGS...: sets rc, leaves DIR/out.
dbr() {
  local dir=$1 failures=$2 msg=$3
  shift 3
  cat >"$dir/docker" <<STUB
#!/usr/bin/env bash
[ "\$1" = image ] && exit 1
echo "\$*" >>"$dir/docker.calls"
n=\$(wc -l <"$dir/docker.calls")
if [ "\$n" -le $failures ]; then cat "$dir/msg"; exit 1; fi
exit 0
STUB
  printf '%s\n' "$msg" >"$dir/msg"
  printf '#!/usr/bin/env bash\nprintf "%%s " "$1" >>"%s/sleeps"\n' "$dir" >"$dir/sleep"
  chmod +x "$dir/docker" "$dir/sleep"
  rc=0
  (cd "$dir" && PATH="$dir:$PATH" && source "$HERE/docker-build-retry.sh" && "$@") >"$dir/out" 2>&1 || rc=$?
}
for c in \
  "2|0|3|10 20 |$RESET|the keploy#4791 reset stream" \
  "1|0|2|10 |  ERROR: Exception: ... pip._vendor.urllib3.exceptions.ReadTimeoutError: HTTPSConnectionPool(host='files.pythonhosted.org', port=443): Read timed out.|a pip read that timed out" \
  "1|0|2|10 |go: reading https://proxy.golang.org/example.com/m/@v/list: 502 Bad Gateway|a proxy 5xx" \
  "9|1|5|10 20 30 40 |$RESET|a reset that never clears, failed after 5 attempts" \
  "9|1|1||$COMPILE|a compile error, on the first attempt" \
  "9|1|1||./main.go:9:1: syntax error: unexpected EOF, expected }|a bare unexpected EOF (a syntax error), on the first attempt" \
  "9|1|1||ERROR: Could not find a version that satisfies the requirement nosuchpkg==9.9|a missing pip package, on the first attempt"; do
  IFS='|' read -r failures want_rc want_calls want_sleeps msg what <<<"$c"
  t=$(mktemp -d)
  dbr "$t" "$failures" "$msg" docker_build_retry docker build .
  if [[ $rc == "$want_rc" && $(calls "$t/docker.calls") == "$want_calls" && "$(cat "$t/sleeps" 2>/dev/null)" == "$want_sleeps" ]]; then
    ok "a build: $what (waits [${want_sleeps% }])"
  else bad "build: $what" "rc=$rc calls=$(calls "$t/docker.calls") sleeps=[$(cat "$t/sleeps" 2>/dev/null)]: $(cat "$t/out")"; fi
  rm -rf "$t"
done
t=$(mktemp -d)
dbr "$t" 2 "Error response from daemon: Get \"https://registry-1.docker.io/v2/\": net/http: TLS handshake timeout" docker_pull_retry alpine:3.20
if [[ $rc == 0 && $(calls "$t/docker.calls") == 3 && "$(cat "$t/sleeps" 2>/dev/null)" == "10 20 " ]]; then
  ok "an image pull that timed out is retried on the same schedule (waits [10 20])"
else bad "pull" "rc=$rc calls=$(calls "$t/docker.calls") sleeps=[$(cat "$t/sleeps" 2>/dev/null)]: $(cat "$t/out")"; fi
rm -rf "$t"

echo "usage:"
for args in "" "clone onlyurl" "modules a b" "go" "pip" "bogus"; do
  rc=0
  # shellcheck disable=SC2086 # the argument list is the case
  bash "$FETCH" $args >/dev/null 2>&1 || rc=$?
  if [[ $rc == 2 ]]; then ok "'fetch.sh $args' is a usage error (2)"; else bad "usage '$args'" "rc=$rc"; fi
done

echo
echo "  $pass passed, $fail failed"
[[ $fail == 0 ]]

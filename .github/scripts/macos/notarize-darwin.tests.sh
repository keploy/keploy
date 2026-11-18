#!/usr/bin/env bash
# Tests for notarize-darwin.sh. Run by linux-ci-scripts.yml, or locally (it
# needs bash, tar, sed, awk and sha256sum or shasum, and leaves nothing
# behind):
#
#   bash .github/scripts/macos/notarize-darwin.tests.sh
#
# Stand-ins replace codesign, ditto and xcrun: codesign and xcrun's
# notarytool answer each call with the next line of a queue the case writes
# (the last line again once the queue is used up; past 20 calls a failure
# that is not tried again, so a loop without its bound ends), and record
# their arguments; ditto zips with tar (on macOS the real ditto runs). A
# stand-in sleep records each pause and returns at once, so the cases can
# check the backoff; only the bound cases sleep for real, 1 s pauses against a
# bound of 2 s. A run past 30 s is stopped and fails. About ten seconds in all.
#
# The answers are the ones Apple's tools give: the 500 is the poll that ended
# v3.7.9's release, word for word.
#
# The workflow cases read release.yml: build-darwin downloads the state,
# resumes or builds, signs, submits, keeps the state as an artifact BEFORE it
# waits, waits, then packages; every step a resumed attempt must skip says
# so, and none that it needs does.
set -u
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$here/notarize-darwin.sh"
release_yml="$here/../../workflows/release.yml"
tmp=$(mktemp -d)
main=$$
# Only this shell removes $tmp. A child it forks (a run, a watchdog) that a
# signal ends before bash has dropped the inherited EXIT trap runs the trap
# too, and $$ is this shell's pid in the child as well; the pid sh reports
# as its parent is the shell running the trap.
cleanup() {
  # shellcheck disable=SC2016 # sh expands $PPID
  if [ "$(exec sh -c 'echo "$PPID"')" = "$main" ]; then
    rm -rf "$tmp"
  fi
}
trap cleanup EXIT
fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() {
  printf 'FAIL %s: %s\n' "$1" "$2"
  fails=$((fails + 1))
}

ID=6362ffd4-c23f-42c5-9a83-50b83f46f408
OTHER=0b5c1e6a-1111-4c2d-8e3f-123456789abc
PW='s3cret-app-password'
ERR500='Error: HTTP status code: 500. Internal server error. Error communicating with authentication service. Please try again at a later time.'
ERRNET='Error: The network connection was lost.'
ERR401='Error: HTTP status code: 401. Invalid credentials. Username or password is incorrect. Use the app-specific password generated at appleid.apple.com. Ensure that all authentication arguments are correct.'
TSERR='dist/keploy: The timestamp service is not available.'

mkdir -p "$tmp/bin"
cat >"$tmp/bin/xcrun" <<'EOF'
#!/usr/bin/env bash
# xcrun notarytool SUB ...: answers from $FAKE/q.SUB, one line per call,
# "RC<TAB>TEXT" (TEXT's \n are newlines); stdout on 0, stderr otherwise.
[ "$1" = notarytool ] || exit 64
sub=$2
printf '%s\n' "$*" >>"$FAKE/calls"
n=$(cat "$FAKE/n.$sub" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" >"$FAKE/n.$sub"
if [ "$n" -gt 20 ]; then
  echo "stand-in: notarytool $sub called $n times" >&2
  exit 1
fi
line=$(sed -n "${n}p" "$FAKE/q.$sub")
[ -n "$line" ] || line=$(tail -n 1 "$FAKE/q.$sub")
rc=${line%%	*}
text=$(printf '%b' "${line#*	}")
if [ "$rc" = 0 ]; then printf '%s\n' "$text"; else printf '%s\n' "$text" >&2; fi
exit "$rc"
EOF
cat >"$tmp/bin/codesign" <<'EOF'
#!/usr/bin/env bash
printf 'codesign %s\n' "$*" >>"$FAKE/calls"
n=$(cat "$FAKE/n.codesign" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" >"$FAKE/n.codesign"
if [ "$n" -gt 20 ]; then
  echo "stand-in: codesign called $n times" >&2
  exit 1
fi
line=$(sed -n "${n}p" "$FAKE/q.codesign")
[ -n "$line" ] || line=$(tail -n 1 "$FAKE/q.codesign")
rc=${line%%	*}
printf '%s\n' "${line#*	}" >&2
exit "$rc"
EOF
cat >"$tmp/bin/ditto" <<'EOF'
#!/usr/bin/env bash
# As macOS's ditto: a file zipped alone is stored under its own name.
[ $# -eq 4 ] || exit 64
case "$1 $2" in
"-c -k") tar -C "$(dirname "$3")" -cf "$4" "$(basename "$3")" ;;
"-x -k") tar -xf "$3" -C "$4" ;;
*) exit 64 ;;
esac
EOF
# Dittos that fail one way each: zipping, unzipping, or unzipping into two
# files or into a directory.
mkdir -p "$tmp/ditto"
printf '#!/usr/bin/env bash\nexit 1\n' >"$tmp/ditto/zip-fails"
# fake_unzip: a ditto that only unzips, by the script on stdin.
fake_unzip() {
  # shellcheck disable=SC2016 # the stand-in's own text
  printf '#!/usr/bin/env bash\n[ "$1" = -x ] || exit 64\n'
  cat
}
fake_unzip >"$tmp/ditto/unzip-fails" <<'EOF'
exit 1
EOF
fake_unzip >"$tmp/ditto/unzip-two" <<'EOF'
echo a >"$4/keploy"
echo b >"$4/extra"
EOF
fake_unzip >"$tmp/ditto/unzip-dir" <<'EOF'
mkdir "$4/keploy"
EOF
# sleep, first on the script's PATH: records each pause in $FAKE/sleeps and
# returns at once. The bound cases set REALSLEEP=1.
mkdir -p "$tmp/fakesleep" "$tmp/sha-not-hex" "$tmp/sha-short" "$tmp/no-mktemp"
printf '#!/usr/bin/env bash\nexit 1\n' >"$tmp/no-mktemp/mktemp"
cat >"$tmp/fakesleep/sleep" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$1" >>"$FAKE/sleeps"
EOF
# SHA-256 tools that print 64 characters that are not hex, or a short hex.
for t in shasum sha256sum; do
  printf '#!/usr/bin/env bash\necho "%s  x"\n' "$(printf 'g%.0s' $(seq 64))" >"$tmp/sha-not-hex/$t"
  printf '#!/usr/bin/env bash\necho "abc123  x"\n' >"$tmp/sha-short/$t"
done
chmod +x "$tmp/bin/"* "$tmp/ditto/"* "$tmp/fakesleep/"* "$tmp/sha-not-hex/"* "$tmp/sha-short/"* "$tmp/no-mktemp/"*
# On macOS the real ditto zips and unzips.
ditto_bin="$tmp/bin/ditto"
if [ "$(uname)" = Darwin ] && [ -x /usr/bin/ditto ]; then
  ditto_bin=/usr/bin/ditto
fi

sha_of() {
  if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1"; else sha256sum "$1"; fi | awk '{print $1}'
}

# fresh NAME: a new case directory $c with a binary $c/dist/keploy and empty
# queues; FAKE points at it.
fresh() {
  c="$tmp/$1"
  mkdir -p "$c/dist"
  printf 'keploy darwin binary %s\n' "$1" >"$c/dist/keploy"
  export FAKE="$c"
  : >"$c/calls"
  : >"$c/sleeps"
}
# q SUB LINE...: the answers of notarytool SUB (or codesign).
q() {
  local sub=$1
  shift
  printf '%s\n' "$@" >"$FAKE/q.$sub"
}
ok() { printf '0\t%s' "$1"; }
er() { printf '1\t%s' "$1"; }
info() { ok "{\"createdDate\":\"2026-11-17T20:04:18.092Z\",\"id\":\"$ID\",\"name\":\"keploy.zip\",\"status\":\"$1\"}"; }
# applog STATUS SHA [JOB]: notarytool log's JSON, pretty-printed as Apple's is.
applog() {
  ok "{\\n  \"logFormatVersion\": 1,\\n  \"jobId\": \"${3:-$ID}\",\\n  \"status\": \"$1\",\\n  \"statusSummary\": \"Ready for distribution\",\\n  \"archiveFilename\": \"keploy.zip\",\\n  \"sha256\": \"$2\",\\n  \"ticketContents\": [{\"path\": \"keploy.zip/keploy\", \"digestAlgorithm\": \"SHA-256\", \"cdhash\": \"abc\", \"arch\": \"arm64\"}],\\n  \"issues\": null\\n}"
}

# run ARGS...: notarize-darwin.sh with the stand-ins and pauses that are
# recorded, not slept; its exit in $code, its stdout in $c/out, stderr in
# $c/err. A run past 30 s is stopped and fails its case, so a wait that never
# ends is a failure, not a hang. Variables change one input: POLL, BACKOFF,
# WAITS and ATTEMPTS; IDENTITY, APPLEID, TEAMID and PASSWORD (empty for
# unset); DITTO; RUNID (empty for no GITHUB_RUN_ID); XPATH (put first on the
# PATH); REALSLEEP=1 (sleep for real).
run() {
  local path=$PATH pid dog
  [ -n "${REALSLEEP:-}" ] || path="$tmp/fakesleep:$path"
  [ -z "${XPATH:-}" ] || path="$XPATH:$path"
  rm -f "$c/hung"
  env PATH="$path" NOTARIZE_XCRUN="$tmp/bin/xcrun" \
    NOTARIZE_CODESIGN="$tmp/bin/codesign" NOTARIZE_DITTO="${DITTO:-$ditto_bin}" \
    NOTARIZE_IDENTITY="${IDENTITY-Developer ID Application: Test}" \
    NOTARIZE_APPLE_ID="${APPLEID-dev@example.com}" NOTARIZE_TEAM_ID="${TEAMID-TEAM123}" \
    NOTARIZE_PASSWORD="${PASSWORD-$PW}" NOTARIZE_POLL_S="${POLL:-0}" \
    NOTARIZE_BACKOFF_S="${BACKOFF:-0}" NOTARIZE_WAIT_S="${WAITS:-60}" \
    NOTARIZE_ATTEMPTS="${ATTEMPTS:-3}" GITHUB_RUN_ID="${RUNID-44164655348}" \
    bash "$script" "$@" >"$c/out" 2>"$c/err" &
  pid=$!
  # The watchdog gets a process group of its own (set -m), and stopping it
  # stops that whole group: killing only its shell would leave the sleep it
  # is in running on its own after the tests end.
  set -m
  watchdog "$pid" </dev/null >/dev/null 2>&1 &
  dog=$!
  set +m
  wait "$pid"
  code=$?
  kill -- -"$dog" 2>/dev/null
  wait "$dog" 2>/dev/null
  if [ -e "$c/hung" ]; then
    fail "${c##*/}" "notarize-darwin.sh $1 ran past 30 s and was stopped"
  fi
}
# watchdog PID: stops PID if it runs past 30 s, and says so in $c/hung.
watchdog() {
  local i=0
  while [ "$i" -lt 30 ] && kill -0 "$1" 2>/dev/null; do
    sleep 1
    i=$((i + 1))
  done
  if kill -0 "$1" 2>/dev/null; then
    : >"$c/hung"
    kill "$1"
  fi
}
sleeps() { tr '\n' ' ' <"$c/sleeps" | sed 's/ $//'; }
expect_sleeps() {
  if [ "$(sleeps)" = "$2" ]; then pass "$1: paused $2"; else fail "$1" "paused '$(sleeps)', want '$2'"; fi
}
calls() { grep -c -- "$1" "$c/calls"; }
expect_code() {
  if [ "$code" -eq "$2" ]; then pass "$1: exit $2"; else fail "$1" "exit $code, want $2; stderr: $(cat "$c/err")"; fi
}
expect_err() {
  if grep -qF -- "$2" "$c/err"; then pass "$1: says '$2'"; else fail "$1" "stderr lacks '$2': $(cat "$c/err")"; fi
}
expect_calls() {
  local n
  n=$(calls "$2")
  if [ "$n" -eq "$3" ]; then pass "$1: $3 call(s) of '$2'"; else fail "$1" "$n call(s) of '$2', want $3"; fi
}
no_secret() {
  if grep -qF -- "$PW" "$c/out" "$c/err"; then fail "$1" "the password was printed"; else pass "$1: the password is not printed"; fi
}
# submitted NAME: a case whose state holds a submission of its binary.
submitted() {
  fresh "$1"
  q submit "$(ok "{\"id\":\"$ID\",\"message\":\"Successfully uploaded file\",\"path\":\"$c/state/keploy.zip\"}")"
  run submit "$c/dist/keploy" "$c/state"
  [ "$code" -eq 0 ] || fail "$1" "setup submit exited $code: $(cat "$c/err")"
  cp "$c/dist/keploy" "$c/signed"
  rm -f "$c/dist/keploy"
  sum=$(sha_of "$c/state/keploy.zip")
  : >"$c/calls"
  : >"$c/sleeps"
}

# --- sign ---
fresh sign-ok
q codesign "$(printf '0\tdist/keploy: signed Mach-O thin (arm64)')"
run sign "$c/dist/keploy"
expect_code sign-ok 0
expect_calls sign-ok "codesign -s Developer ID Application: Test -f -v --timestamp --options runtime $c/dist/keploy" 1

fresh sign-timestamp-retried
q codesign "$(printf '1\t%s' "$TSERR")" "$(printf '1\t%s' "$TSERR")" "$(printf '0\tsigned')"
run sign "$c/dist/keploy"
expect_code sign-timestamp-retried 0
expect_calls sign-timestamp-retried "codesign -s" 3
expect_err sign-timestamp-retried "try 1 of 3"

fresh sign-timestamp-every-try
q codesign "$(printf '1\t%s' "$TSERR")"
run sign "$c/dist/keploy"
expect_code sign-timestamp-every-try 5
expect_calls sign-timestamp-every-try "codesign -s" 3
expect_sleeps sign-timestamp-every-try "0 0"
expect_err sign-timestamp-every-try "timestamp.apple.com"
expect_err sign-timestamp-every-try "Rerun only the failed build-darwin job"

fresh sign-no-identity
q codesign "$(printf '1\tDeveloper ID Application: Test: no identity found')"
run sign "$c/dist/keploy"
expect_code sign-no-identity 1
expect_calls sign-no-identity "codesign -s" 1
expect_err sign-no-identity "failed; nothing was submitted to Apple."

fresh sign-identity-unset
IDENTITY='' run sign "$c/dist/keploy"
expect_code sign-identity-unset 2
expect_err sign-identity-unset "NOTARIZE_IDENTITY is not set"
expect_calls sign-identity-unset "codesign" 0

fresh sign-args
q codesign "$(printf '0\tsigned')"
run sign "$c/dist/keploy" "$c/dist/keploy"
expect_code sign-args 2
expect_calls sign-args "codesign" 0

# The pauses between tries double from NOTARIZE_BACKOFF_S up to 300 s.
fresh sign-backoff
q codesign "$(printf '1\t%s' "$TSERR")"
ATTEMPTS=7 BACKOFF=15 run sign "$c/dist/keploy"
expect_code sign-backoff 5
expect_calls sign-backoff "codesign -s" 7
expect_sleeps sign-backoff "15 30 60 120 240 300"

fresh sign-no-binary
q codesign "$(printf '0\tsigned')"
run sign "$c/dist/missing"
expect_code sign-no-binary 2
expect_calls sign-no-binary "codesign" 0

# --- submit ---
fresh submit-ok
q submit "$(ok "{\"id\":\"$ID\",\"message\":\"Successfully uploaded file\",\"path\":\"x\"}")"
run submit "$c/dist/keploy" "$c/state"
expect_code submit-ok 0
if [ "$(cat "$c/state/submission-id" 2>/dev/null)" = "$ID" ]; then pass "submit-ok: the id is kept"; else fail submit-ok "submission-id is '$(cat "$c/state/submission-id" 2>/dev/null)'"; fi
if [ "$(sha_of "$c/state/keploy.zip")" = "$(cat "$c/state/keploy.zip.sha256")" ]; then pass "submit-ok: the zip's SHA-256 is kept"; else fail submit-ok "keploy.zip.sha256 does not match the zip"; fi
expect_calls submit-ok "notarytool submit $c/state/keploy.zip --no-wait --apple-id dev@example.com --password $PW --team-id TEAM123 --output-format json" 1
expect_err submit-ok "submission $ID"
no_secret submit-ok

fresh submit-500-retried
q submit "$(er "$ERR500")" "$(ok "{\"id\":\"$ID\",\"message\":\"Successfully uploaded file\"}")"
run submit "$c/dist/keploy" "$c/state"
expect_code submit-500-retried 0
expect_calls submit-500-retried "notarytool submit" 2
if [ "$(cat "$c/state/submission-id" 2>/dev/null)" = "$ID" ]; then pass "submit-500-retried: the id is kept"; else fail submit-500-retried "no id kept"; fi

fresh submit-500-every-try
q submit "$(er "$ERR500")"
run submit "$c/dist/keploy" "$c/state"
expect_code submit-500-every-try 5
expect_calls submit-500-every-try "notarytool submit" 3
expect_err submit-500-every-try "failed on every try; nothing is waiting on Apple."
if [ -e "$c/state/submission-id" ]; then fail submit-500-every-try "an id was kept"; else pass "submit-500-every-try: no id kept"; fi

fresh submit-401
q submit "$(er "$ERR401")"
run submit "$c/dist/keploy" "$c/state"
expect_code submit-401 1
expect_calls submit-401 "notarytool submit" 1
expect_err submit-401 "refused $c/state/keploy.zip; nothing is waiting on Apple."

fresh submit-429-retried
q submit "$(er 'Error: HTTP status code: 429. Too many requests.')" "$(ok "{\"id\":\"$ID\"}")"
run submit "$c/dist/keploy" "$c/state"
expect_code submit-429-retried 0
expect_calls submit-429-retried "notarytool submit" 2

fresh submit-no-id
q submit "$(ok '{"message":"Successfully uploaded file"}')"
run submit "$c/dist/keploy" "$c/state"
expect_code submit-no-id 1
if [ -e "$c/state/submission-id" ]; then fail submit-no-id "an id was kept"; else pass "submit-no-id: no id kept"; fi

fresh submit-bad-id
q submit "$(ok '{"id":"6362ffd4-c23f-42c5-9a83-50b83f46f40g"}')"
run submit "$c/dist/keploy" "$c/state"
expect_code submit-bad-id 1
expect_err submit-bad-id "gave no submission id"

fresh submit-short-id
q submit "$(ok '{"id":"6362ffd4-c23f"}')"
run submit "$c/dist/keploy" "$c/state"
expect_code submit-short-id 1
expect_err submit-short-id "gave no submission id"

fresh submit-id-unkept
mkdir -p "$c/state/submission-id.tmp"
q submit "$(ok "{\"id\":\"$ID\"}")"
run submit "$c/dist/keploy" "$c/state"
expect_code submit-id-unkept 1
expect_err submit-id-unkept "cannot keep submission $ID in $c/state"
if [ -e "$c/state/submission-id" ]; then fail submit-id-unkept "an id was kept"; else pass "submit-id-unkept: no id kept"; fi

fresh submit-args
run submit "$c/dist/keploy"
expect_code submit-args 2

fresh submit-no-binary
q submit "$(ok "{\"id\":\"$ID\"}")"
run submit "$c/dist/missing" "$c/state"
expect_code submit-no-binary 2
expect_err submit-no-binary "submit: no binary at $c/dist/missing"
expect_calls submit-no-binary "notarytool" 0

fresh submit-state-unmakeable
: >"$c/file"
q submit "$(ok "{\"id\":\"$ID\"}")"
run submit "$c/dist/keploy" "$c/file/state"
expect_code submit-state-unmakeable 1
expect_err submit-state-unmakeable "submit: cannot make $c/file/state"
expect_calls submit-state-unmakeable "notarytool" 0

fresh submit-zip-fails
q submit "$(ok "{\"id\":\"$ID\"}")"
DITTO="$tmp/ditto/zip-fails" run submit "$c/dist/keploy" "$c/state"
expect_code submit-zip-fails 1
expect_err submit-zip-fails "zipping $c/dist/keploy with ditto failed"
expect_calls submit-zip-fails "notarytool" 0

for s in not-hex short; do
  fresh "submit-sha-$s"
  q submit "$(ok "{\"id\":\"$ID\"}")"
  XPATH="$tmp/sha-$s" run submit "$c/dist/keploy" "$c/state"
  expect_code "submit-sha-$s" 1
  expect_err "submit-sha-$s" "no SHA-256 for $c/state/keploy.zip"
  expect_calls "submit-sha-$s" "notarytool" 0
done

fresh submit-twice
mkdir -p "$c/state" && echo "$OTHER" >"$c/state/submission-id"
q submit "$(ok "{\"id\":\"$ID\"}")"
run submit "$c/dist/keploy" "$c/state"
expect_code submit-twice 1
expect_calls submit-twice "notarytool submit" 0

for v in NOTARIZE_APPLE_ID NOTARIZE_TEAM_ID NOTARIZE_PASSWORD; do
  fresh "submit-no-$v"
  q submit "$(ok "{\"id\":\"$ID\"}")"
  case $v in
  NOTARIZE_APPLE_ID) APPLEID='' run submit "$c/dist/keploy" "$c/state" ;;
  NOTARIZE_TEAM_ID) TEAMID='' run submit "$c/dist/keploy" "$c/state" ;;
  NOTARIZE_PASSWORD) PASSWORD='' run submit "$c/dist/keploy" "$c/state" ;;
  esac
  expect_code "submit-no-$v" 2
  expect_err "submit-no-$v" "submit: $v is not set"
  expect_calls "submit-no-$v" "notarytool" 0
done

# Which failures are tried again. Each text that is tried again matches one
# of the script's transient patterns and no other, so a pattern dropped fails
# its own row; a 4xx other than 429 is final even when it says "try again".
n=0
while IFS='|' read -r want text; do
  n=$((n + 1))
  fresh "class-$n"
  q submit "$(er "$text")" "$(ok "{\"id\":\"$ID\"}")"
  run submit "$c/dist/keploy" "$c/state"
  if [ "$want" = again ]; then
    if [ "$code" -eq 0 ] && [ "$(calls "notarytool submit")" -eq 2 ]; then pass "class: tried again: $text"; else fail "class-$n" "'$text' was not tried again (exit $code)"; fi
  elif [ "$code" -eq 1 ] && [ "$(calls "notarytool submit")" -eq 1 ]; then
    pass "class: final: $text"
  else
    fail "class-$n" "'$text' was not final (exit $code)"
  fi
done <<'ROWS'
again|Error: HTTP status code: 503.
again|Error: HTTP status code: 429.
again|Error: Internal Server Error
again|Error: Service Unavailable
again|Error: Bad Gateway
again|Error: Gateway Timeout
again|Error: Gateway Time-out
again|Error: Please Try Again later.
again|Error: Error communicating with the notary service.
again|Error: The request timed out.
again|Error: The network connection was lost.
again|Error: The Internet connection appears to be offline.
again|Error: A server with the specified hostname could not be found.
again|Error: Could not connect to the server.
again|Error: Connection reset by peer
again|Error: The connection was reset.
again|Error: Error Domain=NSURLErrorDomain Code=-1001
again|dist/keploy: The timestamp service is not available.
final|Error: HTTP status code: 400. Bad request. Please try again.
final|Error: HTTP status code: 410. Gone. Please try again.
final|Error: HTTP status code: 420. Please try again.
final|Error: HTTP status code: 428. Please try again.
final|Error: HTTP status code: 430. Please try again.
final|Error: HTTP status code: 499. Please try again.
final|error: http status code: 403. please try again.
final|Error: The file is not a valid archive.
ROWS

# --- resume ---
submitted resume-yes
run resume "$c/state"
expect_code resume-yes 0
if [ "$(cat "$c/out")" = "resume=true" ]; then pass "resume-yes: resume=true"; else fail resume-yes "printed '$(cat "$c/out")'"; fi
expect_err resume-yes "resuming submission $ID"

fresh resume-args
run resume
expect_code resume-args 2

fresh resume-none
run resume "$c/state"
expect_code resume-none 0
if [ "$(cat "$c/out")" = "resume=false" ]; then pass "resume-none: resume=false"; else fail resume-none "printed '$(cat "$c/out")'"; fi
expect_err resume-none "no earlier attempt of this run submitted the binary"

submitted resume-bad-id
echo "not-an-id" >"$c/state/submission-id"
run resume "$c/state"
if [ "$(cat "$c/out")" = "resume=false" ] && [ ! -e "$c/state/submission-id" ]; then pass "resume-bad-id: resume=false, state cleared"; else fail resume-bad-id "printed '$(cat "$c/out")'"; fi
expect_err resume-bad-id "submission id is not one"

submitted resume-no-zip
rm "$c/state/keploy.zip"
run resume "$c/state"
if [ "$(cat "$c/out")" = "resume=false" ] && [ ! -e "$c/state/submission-id" ]; then pass "resume-no-zip: resume=false, state cleared"; else fail resume-no-zip "printed '$(cat "$c/out")'"; fi
expect_err resume-no-zip "the earlier attempt's zip is missing"

submitted resume-no-sum
rm "$c/state/keploy.zip.sha256"
run resume "$c/state"
if [ "$(cat "$c/out")" = "resume=false" ]; then pass "resume-no-sum: resume=false"; else fail resume-no-sum "printed '$(cat "$c/out")'"; fi
expect_err resume-no-sum "the earlier attempt's zip is missing"

submitted resume-no-zip-no-sum
rm "$c/state/keploy.zip" "$c/state/keploy.zip.sha256"
run resume "$c/state"
if [ "$(cat "$c/out")" = "resume=false" ] && [ ! -e "$c/state/submission-id" ]; then pass "resume-no-zip-no-sum: resume=false, state cleared"; else fail resume-no-zip-no-sum "printed '$(cat "$c/out")'"; fi
expect_err resume-no-zip-no-sum "the earlier attempt's zip is missing"

submitted resume-zip-changed
echo extra >>"$c/state/keploy.zip"
run resume "$c/state"
if [ "$(cat "$c/out")" = "resume=false" ] && [ ! -e "$c/state/keploy.zip" ]; then pass "resume-zip-changed: resume=false, state cleared"; else fail resume-zip-changed "printed '$(cat "$c/out")'"; fi
expect_err resume-zip-changed "does not match the SHA-256 kept with it"

# --- wait ---
submitted wait-accepted
q info "$(info 'In Progress')" "$(info 'In Progress')" "$(info Accepted)"
q log "$(applog Accepted "$sum")"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-accepted 0
if cmp -s "$c/dist/keploy" "$c/signed"; then pass "wait-accepted: BIN is the accepted binary"; else fail wait-accepted "BIN is not the submitted binary"; fi
if [ -x "$c/dist/keploy" ]; then pass "wait-accepted: BIN is executable"; else fail wait-accepted "BIN is not executable"; fi
expect_calls wait-accepted "notarytool info $ID " 3
expect_err wait-accepted "submission $ID: Apple's status: In Progress (poll 1"
no_secret wait-accepted

# v3.7.9's poll: one 500 between two In Progress, then Accepted.
submitted wait-500-resumed
q info "$(info 'In Progress')" "$(er "$ERR500")" "$(er "$ERRNET")" "$(info 'In Progress')" "$(info Accepted)"
q log "$(applog Accepted "$sum")"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-500-resumed 0
expect_calls wait-500-resumed "notarytool info $ID " 5
expect_calls wait-500-resumed "notarytool info" 5
expect_calls wait-500-resumed "notarytool submit" 0
expect_err wait-500-resumed "asking Apple's notary service failed (poll 2, exit 1)"
expect_err wait-500-resumed "Error communicating with authentication service"

# After a failed poll the pauses double from NOTARIZE_BACKOFF_S; an answer
# from Apple starts them over, and In Progress waits NOTARIZE_POLL_S.
submitted wait-backoff
q info "$(er "$ERR500")" "$(er "$ERR500")" "$(info 'In Progress')" "$(er "$ERRNET")" "$(info Accepted)"
q log "$(applog Accepted "$sum")"
BACKOFF=15 POLL=30 WAITS=5400 run wait "$c/state" "$c/dist/keploy"
expect_code wait-backoff 0
expect_sleeps wait-backoff "15 30 30 15"

# No pause runs past the bound.
submitted wait-pause-capped
q info "$(info 'In Progress')" "$(info Accepted)"
q log "$(applog Accepted "$sum")"
POLL=1000 WAITS=100 run wait "$c/state" "$c/dist/keploy"
expect_code wait-pause-capped 0
p=$(sleeps)
if [ "$p" = 100 ] || [ "$p" = 99 ]; then pass "wait-pause-capped: paused ${p}s of the 100 s bound"; else fail wait-pause-capped "paused '$p', want 99 or 100"; fi

submitted wait-args
run wait "$c/state"
expect_code wait-args 2
expect_calls wait-args "notarytool" 0

submitted wait-no-password
q info "$(info Accepted)"
PASSWORD='' run wait "$c/state" "$c/dist/keploy"
expect_code wait-no-password 2
expect_err wait-no-password "wait: NOTARIZE_PASSWORD is not set"
expect_calls wait-no-password "notarytool" 0

submitted wait-bad-id
echo 6362ffd4-c23f >"$c/state/submission-id"
q info "$(info Accepted)"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-bad-id 1
expect_err wait-bad-id "'6362ffd4-c23f' in $c/state is not a submission id"
expect_calls wait-bad-id "notarytool" 0

for r in sum zip-and-sum; do
  submitted "wait-no-$r"
  rm "$c/state/keploy.zip.sha256"
  [ "$r" = sum ] || rm "$c/state/keploy.zip"
  q info "$(info Accepted)"
  q log "$(applog Accepted "$sum")"
  run wait "$c/state" "$c/dist/keploy"
  expect_code "wait-no-$r" 1
  expect_err "wait-no-$r" "is missing or is not the zip sent as submission $ID"
  expect_calls "wait-no-$r" "notarytool" 0
done

submitted wait-invalid
q info "$(info 'In Progress')" "$(info Invalid)"
q log "$(ok '{"status": "Invalid", "issues": [{"message": "The signature does not include a secure timestamp."}]}')"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-invalid 1
expect_err wait-invalid "answered Invalid for submission $ID"
expect_err wait-invalid "The signature does not include a secure timestamp."
if [ -e "$c/dist/keploy" ]; then fail wait-invalid "BIN was written"; else pass "wait-invalid: BIN not written"; fi

submitted wait-rejected
q info "$(info Rejected)"
q log "$(ok '{"status": "Rejected"}')"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-rejected 1
expect_err wait-rejected "answered Rejected for submission $ID"
if [ -e "$c/dist/keploy" ]; then fail wait-rejected "BIN was written"; else pass "wait-rejected: BIN not written"; fi

submitted wait-unknown-status
q info "$(info 'Accepted-ish')"
q log "$(applog Accepted "$sum")"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-unknown-status 1
if [ -e "$c/dist/keploy" ]; then fail wait-unknown-status "BIN was written"; else pass "wait-unknown-status: BIN not written"; fi

submitted wait-no-status
q info "$(ok "{\"id\":\"$ID\"}")"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-no-status 1

submitted wait-401
q info "$(info 'In Progress')" "$(er "$ERR401")"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-401 1
expect_calls wait-401 "notarytool info" 2
expect_err wait-401 "a failure another try cannot fix; the binary is not released."

submitted wait-bound-in-progress
q info "$(info 'In Progress')"
REALSLEEP=1 POLL=1 WAITS=2 run wait "$c/state" "$c/dist/keploy"
expect_code wait-bound-in-progress 5
expect_err wait-bound-in-progress "Apple's notary service has not accepted submission $ID"
expect_err wait-bound-in-progress "its last status: In Progress"
expect_err wait-bound-in-progress "gh run rerun 44164655348 --failed): it resumes submission $ID"
if [ -e "$c/dist/keploy" ]; then fail wait-bound-in-progress "BIN was written"; else pass "wait-bound-in-progress: BIN not written"; fi
n=$(calls "notarytool info")
if [ "$n" -ge 2 ] && [ "$n" -le 4 ]; then pass "wait-bound-in-progress: polled $n times in 2 s"; else fail wait-bound-in-progress "polled $n times in 2 s"; fi
# A rerun resumes the same submission: no submit, Accepted.
q info "$(info Accepted)"
q log "$(applog Accepted "$sum")"
: >"$c/calls"
run resume "$c/state"
if [ "$(cat "$c/out")" = "resume=true" ]; then pass "wait-bound-in-progress: the rerun resumes"; else fail wait-bound-in-progress "rerun printed '$(cat "$c/out")'"; fi
run wait "$c/state" "$c/dist/keploy"
expect_code wait-bound-in-progress-rerun 0
expect_calls wait-bound-in-progress-rerun "notarytool info $ID " 1
expect_calls wait-bound-in-progress-rerun "notarytool submit" 0
if cmp -s "$c/dist/keploy" "$c/signed"; then pass "wait-bound-in-progress-rerun: BIN is the accepted binary"; else fail wait-bound-in-progress-rerun "BIN differs"; fi

submitted wait-bound-errors
q info "$(er "$ERR500")"
REALSLEEP=1 BACKOFF=1 WAITS=2 run wait "$c/state" "$c/dist/keploy"
expect_code wait-bound-errors 5
expect_err wait-bound-errors "its last status: none yet; its last error: $ERR500"

submitted wait-zip-changed
echo extra >>"$c/state/keploy.zip"
q info "$(info Accepted)"
q log "$(applog Accepted "$sum")"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-zip-changed 1
expect_calls wait-zip-changed "notarytool" 0
expect_err wait-zip-changed "is missing or is not the zip sent as submission $ID"

submitted wait-log-other-sha
q info "$(info Accepted)"
q log "$(applog Accepted 0000000000000000000000000000000000000000000000000000000000000000)"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-log-other-sha 1
expect_err wait-log-other-sha "names an archive with SHA-256"
if [ -e "$c/dist/keploy" ]; then fail wait-log-other-sha "BIN was written"; else pass "wait-log-other-sha: BIN not written"; fi

submitted wait-log-other-job
q info "$(info Accepted)"
q log "$(applog Accepted "$sum" "$OTHER")"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-log-other-job 1
expect_err wait-log-other-job "the log it gave is of '$OTHER'"
if [ -e "$c/dist/keploy" ]; then fail wait-log-other-job "BIN was written"; else pass "wait-log-other-job: BIN not written"; fi

submitted wait-log-not-accepted
q info "$(info Accepted)"
q log "$(applog Invalid "$sum")"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-log-not-accepted 1
expect_err wait-log-not-accepted "its log says 'Invalid'"
if [ -e "$c/dist/keploy" ]; then fail wait-log-not-accepted "BIN was written"; else pass "wait-log-not-accepted: BIN not written"; fi

submitted wait-log-401
q info "$(info Accepted)"
q log "$(er "$ERR401")"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-log-401 1
expect_calls wait-log-401 "notarytool log" 1
expect_err wait-log-401 "but its log could not be read; the binary is not released."

submitted wait-unzip-fails
q info "$(info Accepted)"
q log "$(applog Accepted "$sum")"
DITTO="$tmp/ditto/unzip-fails" run wait "$c/state" "$c/dist/keploy"
expect_code wait-unzip-fails 1
expect_err wait-unzip-fails "unzipping $c/state/keploy.zip with ditto failed"

for r in two dir; do
  submitted "wait-unzip-$r"
  q info "$(info Accepted)"
  q log "$(applog Accepted "$sum")"
  DITTO="$tmp/ditto/unzip-$r" run wait "$c/state" "$c/dist/keploy"
  expect_code "wait-unzip-$r" 1
  expect_err "wait-unzip-$r" "$c/state/keploy.zip does not hold exactly one file"
  if [ -e "$c/dist/keploy" ]; then fail "wait-unzip-$r" "BIN was written"; else pass "wait-unzip-$r: BIN not written"; fi
done

submitted wait-bin-unwritable
: >"$c/file"
q info "$(info Accepted)"
q log "$(applog Accepted "$sum")"
run wait "$c/state" "$c/file/keploy"
expect_code wait-bin-unwritable 1
expect_err wait-bin-unwritable "cannot write $c/file/keploy"

submitted wait-no-tmp
q info "$(info Accepted)"
q log "$(applog Accepted "$sum")"
XPATH="$tmp/no-mktemp" run wait "$c/state" "$c/dist/keploy"
expect_code wait-no-tmp 1
expect_err wait-no-tmp "no temporary directory"

submitted wait-log-500-retried
q info "$(info Accepted)"
q log "$(er "$ERR500")" "$(applog Accepted "$sum")"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-log-500-retried 0
expect_calls wait-log-500-retried "notarytool log $ID " 2

submitted wait-log-500-every-try
q info "$(info Accepted)"
q log "$(er "$ERR500")"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-log-500-every-try 5
expect_err wait-log-500-every-try "gh run rerun 44164655348 --failed): it resumes submission $ID"

# Outside a GitHub run the hint names no run.
submitted wait-hint-no-run
q info "$(info Accepted)"
q log "$(er "$ERR500")"
RUNID='' run wait "$c/state" "$c/dist/keploy"
expect_code wait-hint-no-run 5
expect_err wait-hint-no-run "Rerun only the failed build-darwin job: it resumes submission $ID"
if grep -q 'gh run rerun' "$c/err"; then fail wait-hint-no-run "named a run: $(cat "$c/err")"; else pass "wait-hint-no-run: names no run"; fi

submitted wait-no-state
rm "$c/state/submission-id"
q info "$(info Accepted)"
run wait "$c/state" "$c/dist/keploy"
expect_code wait-no-state 2

# --- usage ---
fresh usage
run
expect_code usage-none 2
run frobnicate
expect_code usage-unknown 2
for v in POLL:NOTARIZE_POLL_S WAITS:NOTARIZE_WAIT_S ATTEMPTS:NOTARIZE_ATTEMPTS BACKOFF:NOTARIZE_BACKOFF_S; do
  case ${v%%:*} in
  POLL) POLL=x run resume "$c/state" ;;
  WAITS) WAITS=x run resume "$c/state" ;;
  ATTEMPTS) ATTEMPTS=x run resume "$c/state" ;;
  BACKOFF) BACKOFF=x run resume "$c/state" ;;
  esac
  expect_code "usage-${v#*:}" 2
  expect_err "usage-${v#*:}" "${v#*:} must be a whole number, got 'x'"
done
ATTEMPTS=0 run resume "$c/state"
expect_code usage-zero-attempts 2
expect_err usage-zero-attempts "NOTARIZE_ATTEMPTS must be 1 or more"
ATTEMPTS=1 run resume "$c/state"
expect_code usage-one-attempt 0

# --- release.yml's build-darwin job ---
job=$(awk '/^  build-darwin:/{on=1; print; next} on && /^  [a-z][a-z-]*:/{exit} on' "$release_yml")
steps=$(printf '%s\n' "$job" | grep -E '^      - (name|uses):' | sed -E 's/^      - (name|uses): //')
want_order="Download an earlier attempt's notarization state
Resume an earlier attempt's notarization?
Build darwin/arm64 (native clang)
Code-sign
Submit to Apple's notary service
Keep the submission for a rerun
Wait for Apple's notary service
Package keploy_darwin_arm64.tar.gz
Upload darwin release archive"
got_order=$(printf '%s\n' "$steps" | grep -Fx -f <(printf '%s\n' "$want_order"))
if [ "$got_order" = "$want_order" ]; then pass "release.yml: build-darwin's steps are in order"; else fail release.yml "steps in order: $got_order"; fi
# step NAME: the lines of that step.
step() { printf '%s\n' "$job" | awk -v n="      - name: $1" '$0==n{on=1; print; next} on && /^      - /{exit} on'; }
for s in "Add Private Parsers" "Import Code-Signing Certificates" "Resolve release version" \
  "Build darwin/arm64 (native clang)" "Code-sign" "Submit to Apple's notary service" "Keep the submission for a rerun"; do
  if step "$s" | grep -qFx "        if: steps.notary.outputs.resume != 'true'"; then pass "release.yml: '$s' is skipped on resume"; else fail release.yml "'$s' is not skipped on resume"; fi
done
for s in "Wait for Apple's notary service" "Package keploy_darwin_arm64.tar.gz" "Upload darwin release archive" "Resume an earlier attempt's notarization?"; do
  if step "$s" | grep -q 'if:'; then fail release.yml "'$s' has an if:"; else pass "release.yml: '$s' runs on resume"; fi
done
if step "Download an earlier attempt's notarization state" | grep -qFx '          pattern: darwin-notary-state' &&
  step "Keep the submission for a rerun" | grep -qFx '          name: darwin-notary-state' &&
  step "Keep the submission for a rerun" | grep -qFx '          overwrite: true'; then
  pass "release.yml: the state kept is the state downloaded"
else
  fail release.yml "the state artifact's names or overwrite differ"
fi
# The upload keeps NOTARY_STATE's files at the artifact's top; the download
# puts them back in NOTARY_STATE itself (merge-multiple), not in a folder
# named after the artifact, where resume would find no state.
# shellcheck disable=SC2016 # the step's own text, $ and all
if step "Download an earlier attempt's notarization state" | grep -qFx '          path: ${{ env.NOTARY_STATE }}' &&
  step "Download an earlier attempt's notarization state" | grep -qFx "          merge-multiple: 'true'" &&
  step "Keep the submission for a rerun" | grep -qFx '          path: ${{ env.NOTARY_STATE }}'; then
  pass "release.yml: the state downloaded lands in NOTARY_STATE, where it was kept from"
else
  fail release.yml "the state is not kept from and downloaded into NOTARY_STATE itself (path, merge-multiple)"
fi
# shellcheck disable=SC2016 # the step's own text, $ and all
if step "Resume an earlier attempt's notarization?" | grep -qF 'notarize-darwin.sh resume "$NOTARY_STATE" >> "$GITHUB_OUTPUT"' &&
  step "Resume an earlier attempt's notarization?" | grep -qFx '        id: notary'; then
  pass "release.yml: resume writes steps.notary's output"
else
  fail release.yml "resume does not write steps.notary.outputs"
fi
wait_s=$(step "Wait for Apple's notary service" | sed -n "s/^ *NOTARIZE_WAIT_S: '\([0-9]*\)'$/\1/p")
limit=$(printf '%s\n' "$job" | sed -n 's/^    timeout-minutes: \([0-9]*\)$/\1/p')
if [ -n "$wait_s" ] && [ -n "$limit" ] && [ $((wait_s + 15 * 60)) -le $((limit * 60)) ]; then
  pass "release.yml: the wait (${wait_s}s) ends 15 min or more before the job's ${limit} min"
else
  fail release.yml "the wait '$wait_s' s and the job's '$limit' min leave under 15 min"
fi
if printf '%s\n' "$job" | grep -v '^ *#' | grep -qw gon; then
  fail release.yml "build-darwin still runs gon"
else
  pass "release.yml: build-darwin runs no gon"
fi

if [ "$fails" -ne 0 ]; then
  echo "$fails case(s) failed"
  exit 1
fi
echo "all cases passed"

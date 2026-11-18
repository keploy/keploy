#!/bin/bash
# notarize-darwin.sh: sign keploy's darwin release binary and have Apple's
# notary service accept it, in steps that release.yml's build-darwin job can
# rerun without building, signing or submitting again.
#
# Why not gon. gon signed, zipped, submitted and polled in one call, and gave
# up on the first poll that failed. v3.7.9's release (run 44164655348): gon
# submitted request 6362ffd4-c23f-42c5-9a83-50b83f46f408, Apple answered
# "In Progress" twice, then the third poll got
#
#   Error: HTTP status code: 500. Internal server error. Error communicating
#   with authentication service. Please try again at a later time.
#
# and the job failed. Nothing kept the request id, so the rerun built, signed
# and submitted again. Apple's notary, authentication and timestamp services
# fail like this now and then: one failed call is not Apple's answer.
#
# Each part is its own command here:
#
#   sign BIN              codesign BIN with gon's flags (hardened runtime and
#                         a secure timestamp). A failure of Apple's timestamp
#                         service or of the network is tried again with
#                         backoff; any other failure (no identity, a locked
#                         keychain) ends it at once.
#   submit BIN STATE      zip BIN alone with ditto, keep the zip and its
#                         SHA-256 in the directory STATE, send the zip to
#                         Apple without waiting, and keep the submission id
#                         in STATE. build-darwin uploads STATE as the run's
#                         darwin-notary-state artifact before it waits.
#   resume STATE          print resume=true when STATE (an earlier attempt's
#                         darwin-notary-state) holds a submission id and the
#                         zip it was made from, else clear STATE and print
#                         resume=false.
#   wait STATE BIN        ask Apple about THAT submission until it answers
#                         Accepted, Invalid or Rejected, or NOTARIZE_WAIT_S
#                         passes. A poll that fails on Apple's side (HTTP 5xx
#                         or 429, "try again later") or on the network is
#                         logged and asked again, about the same id, with
#                         backoff; every poll logs the id and Apple's status.
#                         On Accepted it checks that Apple's log is of this
#                         submission and names this zip's SHA-256, then
#                         writes the binary in the zip to BIN.
#
# A rerun of build-darwin after wait ended without Apple's answer (exit 5)
# downloads that artifact, resume prints true, and the job skips the build,
# the signing and the submission and waits on the same submission.
#
# Fail closed: only Apple's "Accepted" for this exact zip writes BIN. Invalid
# or Rejected prints Apple's log and exits 1; a status that is not one of
# Apple's, or a log of another archive, exits 1 too, and never counts as
# accepted. Credentials reach notarytool in its arguments, as gon passed them,
# and are never printed.
#
# Exit codes (release.yml branches on none of them; keep them stable):
#   0  signed; submitted; resume answered; or Apple accepted this zip
#   1  a final failure: a refusal another try cannot fix, Apple's Invalid or
#      Rejected, an answer that is not one of Apple's, no submission id
#   2  usage error or missing input
#   5  Apple's services or the network failed on every try, or the wait's
#      bound passed without an answer; after a submission its id is kept, and
#      a rerun of the job resumes it
#
# Inputs (environment):
#   NOTARIZE_IDENTITY     sign: the codesign identity
#   NOTARIZE_APPLE_ID, NOTARIZE_TEAM_ID, NOTARIZE_PASSWORD
#                         submit and wait: the Apple ID, its team and its
#                         app-specific password
#   NOTARIZE_WAIT_S       wait's bound, in seconds                     (5400)
#   NOTARIZE_POLL_S       seconds between polls while in progress        (30)
#   NOTARIZE_ATTEMPTS     tries of one codesign, submit or log call       (5)
#   NOTARIZE_BACKOFF_S    the first wait after a failed call, doubling up
#                         to 300                                         (15)
#   NOTARIZE_XCRUN, NOTARIZE_CODESIGN, NOTARIZE_DITTO
#                         the tools                  (xcrun, codesign, ditto)
#
# Tests: notarize-darwin.tests.sh, run by linux-ci-scripts.yml.
set -u

me=notarize-darwin

log() { printf '%s: %s\n' "$me" "$*" >&2; }

die() {
  local code=$1
  shift
  printf '%s: %s\n' "$me" "$*" >&2
  exit "$code"
}

usage() {
  die 2 "usage: $me sign BIN | submit BIN STATE | resume STATE | wait STATE BIN"
}

whole() {
  case $2 in
  '' | *[!0-9]*) die 2 "$1 must be a whole number, got '$2'" ;;
  esac
}

XCRUN=${NOTARIZE_XCRUN:-xcrun}
CODESIGN=${NOTARIZE_CODESIGN:-codesign}
DITTO=${NOTARIZE_DITTO:-ditto}
WAIT_S=${NOTARIZE_WAIT_S:-5400}
POLL_S=${NOTARIZE_POLL_S:-30}
ATTEMPTS=${NOTARIZE_ATTEMPTS:-5}
BACKOFF_S=${NOTARIZE_BACKOFF_S:-15}
whole NOTARIZE_WAIT_S "$WAIT_S"
whole NOTARIZE_POLL_S "$POLL_S"
whole NOTARIZE_ATTEMPTS "$ATTEMPTS"
whole NOTARIZE_BACKOFF_S "$BACKOFF_S"
[ "$ATTEMPTS" -ge 1 ] || die 2 "NOTARIZE_ATTEMPTS must be 1 or more"

# A failure another try can fix: Apple's service erring or busy (5xx, 429,
# "try again"), the network, or codesign's timestamp service out of reach.
# Any other 4xx (credentials, a submission that is not the team's) is final,
# whatever else its text says.
TRANSIENT='HTTP status code: (5[0-9][0-9]|429)|internal server error|service unavailable|bad gateway|gateway time-?out|try again|error communicating with|timed out|network connection was lost|not connected to the internet|could not connect|cannot connect|connection (was )?reset|NSURLErrorDomain|timestamp service is not available'
FINAL='HTTP status code: 4([0-1][0-9]|2[0-8]|[3-9][0-9])'

transient() {
  if printf '%s\n' "$1" | grep -Eiq -- "$FINAL"; then
    return 1
  fi
  printf '%s\n' "$1" | grep -Eiq -- "$TRANSIENT"
}

first_line() { printf '%s\n' "$1" | sed -n '/[^[:space:]]/{p;q;}'; }

next_delay() {
  local d=$(($1 * 2))
  if [ "$d" -gt 300 ]; then
    d=300
  fi
  echo "$d"
}

# call CMD...: runs CMD, its output (stdout and stderr) in $out, its exit in
# $rc. It never prints CMD, whose arguments may hold the password.
call() {
  out=$("$@" 2>&1)
  rc=$?
}

# try_hard WHAT CMD...: runs CMD up to NOTARIZE_ATTEMPTS times while its
# failure is transient. Returns 0, 1 (a final failure) or 5 (transient on
# every try); the last output is in $out.
try_hard() {
  local what=$1 n=1 delay=$BACKOFF_S
  shift
  while :; do
    call "$@"
    if [ "$rc" -eq 0 ]; then
      return 0
    fi
    if ! transient "$out"; then
      log "$what failed (exit $rc), a failure another try cannot fix:"
      printf '%s\n' "$out" >&2
      return 1
    fi
    if [ "$n" -ge "$ATTEMPTS" ]; then
      log "$what failed on all $ATTEMPTS tries, on Apple's side or the network; the last error:"
      printf '%s\n' "$out" >&2
      return 5
    fi
    log "$what failed (try $n of $ATTEMPTS, exit $rc), trying again in ${delay}s: $(first_line "$out")"
    sleep "$delay"
    n=$((n + 1))
    delay=$(next_delay "$delay")
  done
}

rerun_hint() {
  if [ -n "${GITHUB_RUN_ID:-}" ]; then
    printf 'Rerun only the failed build-darwin job ("Re-run failed jobs", or: gh run rerun %s --failed): it resumes submission %s and does not build, sign or submit again.' "$GITHUB_RUN_ID" "$1"
  else
    printf 'Rerun only the failed build-darwin job: it resumes submission %s and does not build, sign or submit again.' "$1"
  fi
}

valid_id() {
  case $1 in
  *[!0-9a-fA-F-]*) return 1 ;;
  ????????-????-????-????-????????????) return 0 ;;
  esac
  return 1
}

# field KEY TEXT: the string value of "KEY" in notarytool's JSON, each value
# found on its own line.
field() {
  printf '%s\n' "$2" | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p"
}

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1"
  else
    sha256sum "$1"
  fi | awk '{print $1}'
}

need_creds() {
  [ -n "${NOTARIZE_APPLE_ID:-}" ] || die 2 "$1: NOTARIZE_APPLE_ID is not set"
  [ -n "${NOTARIZE_TEAM_ID:-}" ] || die 2 "$1: NOTARIZE_TEAM_ID is not set"
  [ -n "${NOTARIZE_PASSWORD:-}" ] || die 2 "$1: NOTARIZE_PASSWORD is not set"
}

notary() {
  local sub=$1
  shift
  "$XCRUN" notarytool "$sub" "$@" --apple-id "$NOTARIZE_APPLE_ID" \
    --password "$NOTARIZE_PASSWORD" --team-id "$NOTARIZE_TEAM_ID" \
    --output-format json
}

cmd_sign() {
  [ $# -eq 1 ] || usage
  local bin=$1 code
  [ -f "$bin" ] || die 2 "sign: no binary at $bin"
  [ -n "${NOTARIZE_IDENTITY:-}" ] || die 2 "sign: NOTARIZE_IDENTITY is not set"
  try_hard "signing $bin (codesign asks Apple's timestamp service, timestamp.apple.com)" \
    "$CODESIGN" -s "$NOTARIZE_IDENTITY" -f -v --timestamp --options runtime "$bin"
  code=$?
  case $code in
  0) log "signed $bin" ;;
  1) die 1 "signing $bin failed; nothing was submitted to Apple." ;;
  *) die 5 "signing $bin: Apple's timestamp service (timestamp.apple.com) or the network failed on every try; nothing was submitted to Apple. Rerun only the failed build-darwin job." ;;
  esac
}

cmd_submit() {
  [ $# -eq 2 ] || usage
  local bin=$1 state=$2 sum id code
  [ -f "$bin" ] || die 2 "submit: no binary at $bin"
  need_creds submit
  if [ -e "$state/submission-id" ]; then
    die 1 "submit: $state already holds submission $(cat "$state/submission-id"); wait on it instead of sending Apple a second one."
  fi
  mkdir -p "$state" || die 1 "submit: cannot make $state"
  rm -f "$state/keploy.zip" "$state/keploy.zip.sha256"
  "$DITTO" -c -k "$bin" "$state/keploy.zip" ||
    die 1 "submit: zipping $bin with ditto failed"
  sum=$(sha256_of "$state/keploy.zip")
  case $sum in
  *[!0-9a-f]* | '') die 1 "submit: no SHA-256 for $state/keploy.zip" ;;
  esac
  [ ${#sum} -eq 64 ] || die 1 "submit: no SHA-256 for $state/keploy.zip"
  printf '%s\n' "$sum" >"$state/keploy.zip.sha256"
  try_hard "submitting $state/keploy.zip to Apple's notary service" \
    notary submit "$state/keploy.zip" --no-wait
  code=$?
  case $code in
  0) ;;
  1) die 1 "submit: Apple's notary service refused $state/keploy.zip; nothing is waiting on Apple." ;;
  *) die 5 "submit: Apple's notary service or the network failed on every try; nothing is waiting on Apple. Rerun only the failed build-darwin job." ;;
  esac
  id=$(field id "$out" | sed -n 1p)
  valid_id "$id" || die 1 "submit: Apple's notary service gave no submission id; its answer: $out"
  if ! { printf '%s\n' "$id" >"$state/submission-id.tmp" &&
    mv "$state/submission-id.tmp" "$state/submission-id"; }; then
    die 1 "submit: cannot keep submission $id in $state"
  fi
  log "submitted $state/keploy.zip (SHA-256 $sum) to Apple's notary service: submission $id"
}

cmd_resume() {
  [ $# -eq 1 ] || usage
  local state=$1 why=''
  if [ ! -e "$state/submission-id" ]; then
    why="no earlier attempt of this run submitted the binary"
  elif ! valid_id "$(cat "$state/submission-id")"; then
    why="the earlier attempt's submission id is not one"
  elif [ ! -f "$state/keploy.zip" ] || [ ! -f "$state/keploy.zip.sha256" ]; then
    why="the earlier attempt's zip is missing"
  elif [ "$(sha256_of "$state/keploy.zip")" != "$(cat "$state/keploy.zip.sha256")" ]; then
    why="the earlier attempt's zip does not match the SHA-256 kept with it"
  fi
  if [ -n "$why" ]; then
    rm -f "$state/submission-id" "$state/keploy.zip" "$state/keploy.zip.sha256"
    log "building, signing and submitting: $why"
    echo "resume=false"
    return 0
  fi
  log "resuming submission $(cat "$state/submission-id") of an earlier attempt of this run: no build, signing or submission this time"
  echo "resume=true"
}

cmd_wait() {
  [ $# -eq 2 ] || usage
  local state=$1 bin=$2 id want got start polls=0 delay=$BACKOFF_S
  local status last='none yet' lasterr='' pause left code tmp
  need_creds wait
  [ -f "$state/submission-id" ] || die 2 "wait: no submission id in $state"
  id=$(cat "$state/submission-id")
  valid_id "$id" || die 1 "wait: '$id' in $state is not a submission id"
  want=$(cat "$state/keploy.zip.sha256" 2>/dev/null)
  if [ ! -f "$state/keploy.zip" ] || [ -z "$want" ] ||
    [ "$(sha256_of "$state/keploy.zip")" != "$want" ]; then
    die 1 "wait: $state/keploy.zip is missing or is not the zip sent as submission $id"
  fi
  start=$SECONDS
  log "waiting for Apple's notary service to answer submission $id (at most ${WAIT_S}s)"
  while :; do
    polls=$((polls + 1))
    call notary info "$id"
    if [ "$rc" -eq 0 ]; then
      status=$(field status "$out" | sed -n 1p)
      log "submission $id: Apple's status: ${status:-none} (poll $polls, $((SECONDS - start))s waited)"
      case $status in
      Accepted) break ;;
      'In Progress')
        last=$status
        delay=$BACKOFF_S
        pause=$POLL_S
        ;;
      Invalid | Rejected)
        log "Apple's notary log for submission $id:"
        call notary log "$id"
        printf '%s\n' "$out" >&2
        die 1 "Apple's notary service answered $status for submission $id: the binary is not notarized and is not released. Fix what Apple's log names and release again; a rerun of this job asks about the same submission and gets the same answer."
        ;;
      *)
        die 1 "Apple's notary service answered '$status' for submission $id, which is not one of its statuses (In Progress, Accepted, Invalid, Rejected); the binary is not released. Its answer: $out"
        ;;
      esac
    elif transient "$out"; then
      lasterr=$(first_line "$out")
      log "submission $id: asking Apple's notary service failed (poll $polls, exit $rc), asking again in ${delay}s: $lasterr"
      pause=$delay
      delay=$(next_delay "$delay")
    else
      die 1 "asking Apple's notary service about submission $id failed (exit $rc), a failure another try cannot fix; the binary is not released. Its answer: $out"
    fi
    left=$((WAIT_S - (SECONDS - start)))
    if [ "$left" -le 0 ]; then
      die 5 "Apple's notary service has not accepted submission $id after $((SECONDS - start))s (the bound is ${WAIT_S}s; its last status: $last${lasterr:+; its last error: $lasterr}). The binary is not released. $(rerun_hint "$id")"
    fi
    if [ "$pause" -gt "$left" ]; then
      pause=$left
    fi
    sleep "$pause"
  done

  try_hard "fetching Apple's notary log for submission $id" notary log "$id"
  code=$?
  case $code in
  0) ;;
  1) die 1 "Apple accepted submission $id, but its log could not be read; the binary is not released." ;;
  *) die 5 "Apple accepted submission $id, but its log could not be read on any try; the binary is not released. $(rerun_hint "$id")" ;;
  esac
  got=$(field jobId "$out" | sort -u)
  [ "$got" = "$id" ] || die 1 "Apple accepted submission $id, but the log it gave is of '$got'; the binary is not released."
  got=$(field status "$out" | sort -u)
  [ "$got" = Accepted ] || die 1 "Apple accepted submission $id, but its log says '$got'; the binary is not released."
  got=$(field sha256 "$out" | sort -u)
  [ "$got" = "$want" ] || die 1 "Apple accepted submission $id, but its log names an archive with SHA-256 '$got', not $state/keploy.zip ($want); the binary is not released."

  tmp=$(mktemp -d) || die 1 "no temporary directory"
  if ! "$DITTO" -x -k "$state/keploy.zip" "$tmp"; then
    rm -rf "$tmp"
    die 1 "unzipping $state/keploy.zip with ditto failed"
  fi
  set -- "$tmp"/*
  if [ $# -ne 1 ] || [ ! -f "$1" ]; then
    rm -rf "$tmp"
    die 1 "$state/keploy.zip does not hold exactly one file"
  fi
  mkdir -p "$(dirname "$bin")" && cp "$1" "$bin.tmp" && chmod +x "$bin.tmp" &&
    mv "$bin.tmp" "$bin"
  code=$?
  rm -rf "$tmp"
  [ "$code" -eq 0 ] || die 1 "cannot write $bin"
  log "Apple's notary service accepted submission $id (SHA-256 $want); $bin is the binary it accepted"
}

[ $# -ge 1 ] || usage
cmd=$1
shift
case $cmd in
sign) cmd_sign "$@" ;;
submit) cmd_submit "$@" ;;
resume) cmd_resume "$@" ;;
wait) cmd_wait "$@" ;;
*) usage ;;
esac

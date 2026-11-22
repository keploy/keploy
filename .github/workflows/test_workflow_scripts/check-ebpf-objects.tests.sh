#!/bin/bash
# check-ebpf-objects.tests.sh: check-ebpf-objects.sh against a keploy/ebpf of
# its own, a local repository served blobless (file://, uploadpack.allowFilter)
# as GitHub serves the real one. The check is given the https URL it clones in
# CI, https://github.com/keploy/ebpf.git, which the cases' git configuration
# maps to that repository (insteadOf). Its main has a build from before the
# kernel-loads lane (zero), a build the lane was added under (one), a build
# after it (two), and a branch merged without squashing (x, then four); a
# pull request off it has a fifth build (three). The hooks directory passes
# with one, two or four, and fails, naming each file's place, with zero, with
# a mix of two builds, with the pull request's files, with the merged branch's
# first build, with a file main never had, and with no file at all. A token
# the server refuses, whichever way it says so, ends the check after one
# attempt; a server error, a rate limit (429), a name that does not resolve
# and a transfer cut off are tried again, at most five attempts in all,
# waiting 10, 20, 30 and 40 s between them (a sleep in bin/ records each wait
# and returns at once). With no token, unset or empty as GitHub gives a
# missing secret, a check that is not a fork's fails and clones nothing, as
# does a PR_FROM_FORK that is neither true nor false.
#
# A keploy/keploy of its own gives a fork's pull request its checkout: the
# merge commit, two commits deep as actions/checkout fetches it. Each merge
# is made on a main commit whose eBPF files main later changes again. A pull
# request that leaves the files passes; one that changes, adds or removes one
# fails, listing those; a checkout one commit deep, one that is no merge
# commit, and a hooks directory in no checkout fail. A fork's check, its
# token unset or empty, clones nothing. With a token a fork gets the whole
# check.
#
# The check's git is a wrapper (bin/git) that, for each clone, records whether
# git may prompt, whether git gives the clone up once its transfer stays below
# 1000 B/s for 30 s (its stall bound), and what password git's credential
# helpers give for github.com, with a decoy helper configured before them as a
# runner's could be: the check's own must be the one that answers, with the
# token. After a clone it tells the helpers to store the credentials, as git
# does after one GitHub accepted. It records the token on any git command
# line, and in any file of the check's temporary directory, home or working
# directory, or one under /tmp or /dev/shm written since the case began
# (scan), just before or just after each clone; a case fails on either, on the
# token in such a file once the check has ended, and on the token in the
# check's output. The token is looked for as it is and in base64, at each of
# the three byte alignments (so also as an Authorization header gives it,
# x-access-token:TOKEN), in both alphabets. The wrapper can also refuse a
# clone in each of the ways GitHub and git refuse credentials, fail the first
# clone as a server error, a rate limit, a name that does not resolve or a
# transfer cut off does, or fail every clone as a server that is down does.
#
#   bash .github/workflows/test_workflow_scripts/check-ebpf-objects.tests.sh
set -euo pipefail
check="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-ebpf-objects.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
files=(bpf_arm64_bpfel.go bpf_arm64_bpfel.o bpf_x86_bpfel.go bpf_x86_bpfel.o
  unixconn_arm64_bpfel.go unixconn_arm64_bpfel.o unixconn_x86_bpfel.go unixconn_x86_bpfel.o)
# Neither this machine's git configuration nor the developer's reaches the
# cases; the global one holds the decoy helper.
printf '[credential]\n\thelper = "!f() { echo username=decoy; echo password=decoy; }; f"\n' > "$work/gitconfig"
export GIT_CONFIG_GLOBAL="$work/gitconfig" GIT_CONFIG_NOSYSTEM=1
token="ghp_test$RANDOM$RANDOM$RANDOM$RANDOM${RANDOM}never"
# needles: the token, and the base64 any bytes holding it have, at each of
# the three alignments, less the four characters at each end that the bytes
# around it change; in the standard alphabet and the URL-safe one.
needles=("$token")
for k in 0 1 2; do
  b=$(printf '%.*s%s' "$k" xx "$token" | base64 | tr -d '\n=')
  b=${b:4:$((${#b} - 8))}
  needles+=("$b" "$(printf '%s' "$b" | tr '+/' '-_')")
done

src="$work/ebpf"
git init -q -b main "$src"
git -C "$src" config uploadpack.allowFilter true
git -C "$src" config user.name test
git -C "$src" config user.email test@example.com
# The developer's own settings must not reach these commits: a signing
# requirement with no agent would fail them.
git -C "$src" config commit.gpgsign false
build() { # NAME: a build of every file, its bytes NAME's
  for f in "${files[@]}"; do printf '%s %s\n' "$1" "$f" > "$src/$f"; done
  git -C "$src" add -A && git -C "$src" commit -q -m "$1"
}
build zero
build one
mkdir -p "$src/.woodpecker" && printf 'when: []\n' > "$src/.woodpecker/kernel-loads.yml"
git -C "$src" add -A && git -C "$src" commit -q -m "ci: the kernel-loads lane"
build two
git -C "$src" checkout -q -b feature
build x
build four
git -C "$src" checkout -q main
git -C "$src" merge -q --no-ff -m "merge feature" feature
git -C "$src" checkout -q -b pr
build three
git -C "$src" checkout -q main
printf '[url "file://%s"]\n\tinsteadOf = https://github.com/keploy/ebpf.git\n' "$src" >> "$work/gitconfig"
url=https://github.com/keploy/ebpf.git

# keploy/keploy: main has build one of every file at pkg/agent/hooks/linux,
# beside a file of another kind and one elsewhere that the check must not
# count; then build two of the unixconn files. Branch same, off one, changes
# only the docs; branch change, off two, changes the bpf_x86 files (three).
# Each is merged on two's commit with --no-ff, as GitHub makes a pull
# request's merge commit on main's tip; then main changes every file (four).
kp="$work/keploy"
git init -q -b main "$kp"
kgit() { git -C "$kp" -c user.name=test -c user.email=test@example.com -c commit.gpgsign=false "$@"; }
kset() { # NAME [FILE...]: the FILEs (every file if none) get NAME's bytes, committed as NAME
  local name=$1 f
  shift
  [ $# -eq 0 ] && set -- "${files[@]}"
  for f in "$@"; do printf '%s %s\n' "$name" "$f" > "$kp/pkg/agent/hooks/linux/$f"; done
  kgit add -A && kgit commit -q -m "$name"
}
mkdir -p "$kp/pkg/agent/hooks/linux"
printf 'package linux\n' > "$kp/pkg/agent/hooks/linux/hooks.go"
printf 'elsewhere\n' > "$kp/pkg/agent/other_x86_bpfel.o"
kset one
kgit branch same
kset two unixconn_arm64_bpfel.go unixconn_arm64_bpfel.o unixconn_x86_bpfel.go unixconn_x86_bpfel.o
kgit branch change
kgit checkout -q same
printf 'docs\n' > "$kp/README.md"
kgit add -A && kgit commit -q -m "a change to the docs"
kgit checkout -q change
kset three bpf_x86_bpfel.go bpf_x86_bpfel.o
for b in same change; do
  kgit checkout -q -b "merge-$b" main
  kgit merge -q --no-ff -m "Merge $b into main" "$b"
done
kgit checkout -q main
kset four
# co NAME BRANCH [DEPTH]: a checkout of BRANCH, DEPTH (2) commits deep, as
# actions/checkout makes it; its hooks directory.
co() {
  git clone -q --depth="${3:-2}" --branch "$2" "file://$kp" "$work/co-$1"
  echo "$work/co-$1/pkg/agent/hooks/linux"
}

# scan: the files that hold one of $SHIM_NEEDLES: any under $SHIM_SCAN, and
# any under /tmp and /dev/shm (of less than 16 MB) written since $SHIM_MARK.
# There it skips each directory that has not changed since then: creating,
# renaming or removing an entry changes a directory, so a file created since
# then is in a changed directory. So a /tmp of a million files (a shared
# box's) is not read whole for each case. It misses a file rewritten in place
# in a directory that did not change, and one created in a directory below
# one that did not change.
cat > "$work/scan" <<'EOF'
#!/bin/bash
set -f
needles=($SHIM_NEEDLES)
set +f
e=()
for n in "${needles[@]}"; do e+=(-e "$n"); done
{
  grep -rlF "${e[@]}" "$SHIM_SCAN"
  find /tmp /dev/shm -xdev -mindepth 1 -type d ! -newer "$SHIM_MARK" -prune -o \
    -type f -newer "$SHIM_MARK" -size -16M -print0 | xargs -0r grep -lF "${e[@]}"
} 2> /dev/null
exit 0
EOF
chmod +x "$work/scan"

# bin/git: the real git, but it records in $SHIM_LOG "argv-has-token" if one
# of $SHIM_NEEDLES is in its arguments; and for a clone, "prompts" if git may
# prompt for credentials, "stall-bound:LIMIT/TIME" if its environment does not
# give git the stall bound of 1000 B/s for 30 s, "helper:" with the password
# git's helpers give for github.com under its -c options (or "none"), and
# "file-has-token" if scan finds one in a file just before or just after it. A
# clone that succeeds has the helpers store the credentials, as git does once
# GitHub accepts them. With SHIM_REFUSE it refuses the clone instead, as
# GitHub refuses a token that cannot read the repository (notfound), is not
# valid (auth) or may not read it (forbidden: an organization's SSO), or as
# git does when no helper gives a username (username) or a password
# (password); or fails the first clone as a server error (blip), a rate limit
# (ratelimit), a name that does not resolve (dns) or a transfer cut off (eof)
# does, and runs the next; or fails every clone as a server that is down does
# (down).
real=$(command -v git)
mkdir -p "$work/bin"
cat > "$work/bin/git" <<EOF
#!/bin/bash
real='$real' scan='$work/scan'
EOF
cat >> "$work/bin/git" <<'EOF'
args=("$@") cfg=() i=0
set -f
needles=($SHIM_NEEDLES)
set +f
for a in "${args[@]}"; do
  for n in "${needles[@]}"; do
    case $a in *"$n"*) echo argv-has-token >> "$SHIM_LOG" ;; esac
  done
done
while [ "$i" -lt ${#args[@]} ] && [ "${args[$i]}" = -c ]; do cfg+=(-c "${args[$((i + 1))]}"); i=$((i + 2)); done
[ "${args[$i]:-}" = clone ] || exec "$real" "$@"
[ "${GIT_TERMINAL_PROMPT:-}" = 0 ] || echo prompts >> "$SHIM_LOG"
[ "${GIT_HTTP_LOW_SPEED_LIMIT:-}/${GIT_HTTP_LOW_SPEED_TIME:-}" = 1000/30 ] ||
  echo "stall-bound:${GIT_HTTP_LOW_SPEED_LIMIT:-none}/${GIT_HTTP_LOW_SPEED_TIME:-none}" >> "$SHIM_LOG"
cred=$(printf 'protocol=https\nhost=github.com\npath=keploy/ebpf.git\n\n' |
  GIT_TERMINAL_PROMPT=0 "$real" "${cfg[@]}" credential fill 2> /dev/null) || cred=""
pw=""
case $'\n'$cred in *$'\n'password=*) pw=${cred#*password=} pw=${pw%%$'\n'*} ;; esac
if [ "$pw" = "$SHIM_TOKEN" ]; then echo helper:token >> "$SHIM_LOG"; else echo "helper:${pw:-none}" >> "$SHIM_LOG"; fi
infile() { "$scan" | grep -q .; }
clone() {
  "$real" "${args[@]}"
  rc=$?
  if [ "$rc" -eq 0 ] && [ -n "$cred" ]; then
    printf '%s\n\n' "$cred" | "$real" "${cfg[@]}" credential approve > /dev/null 2>&1
  fi
}
leak=""
infile && leak=1
case ${SHIM_REFUSE:-} in
  "") clone ;;
  notfound)
    echo "remote: Repository not found." >&2
    echo "fatal: repository 'https://github.com/keploy/ebpf.git/' not found" >&2
    rc=128
    ;;
  auth)
    echo "remote: Invalid username or token. Password authentication is not supported for Git operations." >&2
    echo "fatal: Authentication failed for 'https://github.com/keploy/ebpf.git/'" >&2
    rc=128
    ;;
  username)
    echo "fatal: could not read Username for 'https://github.com': terminal prompts disabled" >&2
    rc=128
    ;;
  password)
    echo "fatal: could not read Password for 'https://x-access-token@github.com': terminal prompts disabled" >&2
    rc=128
    ;;
  forbidden)
    echo "remote: The 'keploy' organization has enabled or enforced SAML SSO." >&2
    echo "fatal: unable to access 'https://github.com/keploy/ebpf.git/': The requested URL returned error: 403" >&2
    rc=128
    ;;
  down)
    echo "fatal: unable to access 'https://github.com/keploy/ebpf.git/': The requested URL returned error: 502" >&2
    rc=128
    ;;
  ratelimit)
    if [ "$(grep -c '^helper:' "$SHIM_LOG")" -le 1 ]; then
      echo "fatal: unable to access 'https://github.com/keploy/ebpf.git/': The requested URL returned error: 429" >&2
      rc=128
    else
      clone
    fi
    ;;
  dns)
    if [ "$(grep -c '^helper:' "$SHIM_LOG")" -le 1 ]; then
      echo "fatal: unable to access 'https://github.com/keploy/ebpf.git/': Could not resolve host: github.com" >&2
      rc=128
    else
      clone
    fi
    ;;
  eof)
    if [ "$(grep -c '^helper:' "$SHIM_LOG")" -le 1 ]; then
      echo "error: RPC failed; curl 92 HTTP/2 stream 5 was not closed cleanly: CANCEL (err 8)" >&2
      echo "error: 2771 bytes of body are still expected" >&2
      echo "fetch-pack: unexpected disconnect while reading sideband packet" >&2
      echo "fatal: early EOF" >&2
      echo "fatal: fetch-pack: invalid index-pack output" >&2
      rc=128
    else
      clone
    fi
    ;;
  blip)
    if [ "$(grep -c '^helper:' "$SHIM_LOG")" -le 1 ]; then
      echo "fatal: unable to access 'https://github.com/keploy/ebpf.git/': The requested URL returned error: 502" >&2
      rc=128
    else
      clone
    fi
    ;;
  *)
    echo "bin/git: SHIM_REFUSE is '$SHIM_REFUSE', none of notfound, auth, username, password, forbidden, down, ratelimit, dns, eof, blip" >&2
    exit 2
    ;;
esac
infile && leak=1
[ -z "$leak" ] || echo file-has-token >> "$SHIM_LOG"
exit "$rc"
EOF
chmod +x "$work/bin/git"
# bin/sleep: the check waits between attempts only through sleep, found on
# PATH; this one records "sleep N" in $SHIM_LOG and returns at once.
printf '#!/bin/bash\necho "sleep $*" >> "$SHIM_LOG"\n' > "$work/bin/sleep"
chmod +x "$work/bin/sleep"

failed=0
# hooks NAME BUILD...: a hooks directory NAME holding, for each file, the
# bytes of the first BUILD, the bpf ones those of the second if given.
hooks() {
  local d="$work/hooks-$1" f b; mkdir -p "$d"
  for f in "${files[@]}"; do
    b=$2; case $f in bpf_*) b=${3:-$2} ;; esac
    printf '%s %s\n' "$b" "$f" > "$d/$f"
  done
  echo "$d"
}
# expect NAME RC PATTERN DIR [VAR=VALUE...]: the check of DIR, with no
# PRO_ACCESS_TOKEN or PR_FROM_FORK but those of the VARs, exits RC and says
# PATTERN. It runs with a temporary directory, home and working directory of
# its own, under $run; neither its output, a git command line it runs, nor a
# file under $run, or one under /tmp or /dev/shm written since the case
# began (scan), just before or just after each clone or once it has ended,
# holds the token in any of its forms (needles). Its clones and waits are
# recorded in $work/shim.log. The caller's GIT_TERMINAL_PROMPT and
# GIT_HTTP_LOW_SPEED_* do not reach it: the check must set them itself.
run="$work/run"
export SHIM_NEEDLES="${needles[*]}" SHIM_SCAN="$run" SHIM_MARK="$work/mark"
expect() {
  local out rc=0 leak n
  : > "$work/shim.log"
  rm -rf "$run" && mkdir -p "$run/tmp" "$run/home" "$run/cwd"
  # A second early: a file's time is from the kernel's coarse clock, which
  # may still read the marker's when the check writes its first file.
  touch -d '1 second ago' "$work/mark"
  out=$(cd "$run/cwd" && env -u PRO_ACCESS_TOKEN -u PR_FROM_FORK -u SHIM_REFUSE -u GIT_TERMINAL_PROMPT \
    -u GIT_HTTP_LOW_SPEED_LIMIT -u GIT_HTTP_LOW_SPEED_TIME \
    PATH="$work/bin:$PATH" TMPDIR="$run/tmp" HOME="$run/home" SHIM_LOG="$work/shim.log" SHIM_TOKEN="$token" \
    "${@:5}" bash "$check" "$url" "$4" 2>&1) || rc=$?
  leak=$({ "$work/scan"; grep -xE 'argv-has-token|file-has-token' "$work/shim.log"; } 2> /dev/null || true)
  for n in "${needles[@]}"; do [[ $out != *"$n"* ]] || leak+=" output-has-token"; done
  # The whole output against one extended regular expression: . and a
  # newline in it match a newline.
  if [ "$rc" = "$2" ] && [[ $out =~ $3 ]] && [ -z "$leak" ]; then
    echo "ok   $1"
  else
    echo "FAIL $1: exit $rc (want $2), want /$3/, no token in a file, argv or the output (got: ${leak:-none}) in:"
    sed 's/^/     /' <<< "$out"
    failed=1
  fi
}
# clones NAME WANT: the last case's clones and waits are recorded as WANT (one
# line each).
clones() {
  local got; got=$(paste -sd' ' "$work/shim.log")
  if [ "$got" = "$2" ]; then echo "ok   $1"; else echo "FAIL $1: clones recorded '$got', want '$2'"; failed=1; fi
}
same=(PRO_ACCESS_TOKEN="$token" PR_FROM_FORK=false)

expect "main's tip, a merge" 0 "the 8 eBPF files of .* are keploy/ebpf main's at [0-9a-f]+ \(merge feature\)" "$(hooks tip four)" "${same[@]}"
clones "the token is given by the check's helper, not on the command line" "helper:token"
expect "an older main commit" 0 "are keploy/ebpf main's at [0-9a-f]+ \(two\)" "$(hooks old two)" "${same[@]}"
expect "the build the lane was added under" 0 "are keploy/ebpf main's at [0-9a-f]+ \(one\)" "$(hooks floor one)" "${same[@]}"
expect "a build from before the lane" 1 "but no main commit that holds them has the .woodpecker/kernel-loads.yml lane" "$(hooks before zero)" "${same[@]}"
expect "two builds mixed" 1 "bpf_x86_bpfel.o: keploy/ebpf main's at [0-9a-f]+ \(one\)" "$(hooks mixed two one)" "${same[@]}"
expect "two builds mixed: the other" 1 "unixconn_x86_bpfel.o: keploy/ebpf main's at [0-9a-f]+ \(two\)" "$work/hooks-mixed" "${same[@]}"
expect "a pull request's build" 1 "unixconn_x86_bpfel.o: never on keploy/ebpf's main" "$(hooks pr three)" "${same[@]}"
expect "a merged branch's first build" 1 "unixconn_x86_bpfel.o: never on keploy/ebpf's main" "$(hooks branch x)" "${same[@]}"
d=$(hooks extra two); printf 'x\n' > "$d/other_x86_bpfel.o"
expect "a file main never had" 1 "other_x86_bpfel.o: never on keploy/ebpf's main" "$d" "${same[@]}"
mkdir -p "$work/hooks-none"
expect "no file" 1 "no \*_bpfel.o or \*_bpfel.go file" "$work/hooks-none" "${same[@]}"

# What git says, last, before the check's message.
declare -A refusal=([notfound]="fatal: repository '[^']*' not found" [auth]="fatal: Authentication failed for '[^']*'"
  [username]="fatal: could not read Username for '[^']*': terminal prompts disabled"
  [password]="fatal: could not read Password for '[^']*': terminal prompts disabled"
  [forbidden]="fatal: unable to access '[^']*': The requested URL returned error: 403")
for r in notfound auth username password forbidden; do
  expect "a refused token ($r)" 1 "${refusal[$r]}
PRO_ACCESS_TOKEN cannot read https://.*: the server refused it" "$work/hooks-old" "${same[@]}" SHIM_REFUSE="$r"
  clones "a refused token ($r) is not tried again" "helper:token"
done
# Not a refusal: a server error or a rate limit waits and clones again, at
# most five times in all, each wait 10 s longer.
expect "a server error is tried again" 0 "The requested URL returned error: 502
clone of https://.* failed \(attempt 1 of 5\); retrying in 10s
the 8 eBPF files of .* are keploy/ebpf main's at [0-9a-f]+ \(two\)" "$work/hooks-old" "${same[@]}" SHIM_REFUSE=blip
clones "a server error is tried again: two clones" "helper:token sleep 10 helper:token"
expect "a rate limit is tried again" 0 "returned error: 429
clone of https://.* failed \(attempt 1 of 5\); retrying in 10s
the 8 eBPF files of .* are keploy/ebpf main's at [0-9a-f]+ \(two\)" "$work/hooks-old" "${same[@]}" SHIM_REFUSE=ratelimit
clones "a rate limit is tried again: two clones" "helper:token sleep 10 helper:token"
expect "a name that does not resolve is tried again" 0 "Could not resolve host: github.com
clone of https://.* failed \(attempt 1 of 5\); retrying in 10s
the 8 eBPF files of .* are keploy/ebpf main's at [0-9a-f]+ \(two\)" "$work/hooks-old" "${same[@]}" SHIM_REFUSE=dns
clones "a name that does not resolve is tried again: two clones" "helper:token sleep 10 helper:token"
expect "a transfer cut off is tried again" 0 "fatal: early EOF
.*clone of https://.* failed \(attempt 1 of 5\); retrying in 10s
the 8 eBPF files of .* are keploy/ebpf main's at [0-9a-f]+ \(two\)" "$work/hooks-old" "${same[@]}" SHIM_REFUSE=eof
clones "a transfer cut off is tried again: two clones" "helper:token sleep 10 helper:token"
expect "a server down five times" 1 "attempt 4 of 5\); retrying in 40s
.*could not clone https://.*'s main \(5 attempts\)$" "$work/hooks-old" "${same[@]}" SHIM_REFUSE=down
clones "a server down: five clones, waits growing" "helper:token sleep 10 helper:token sleep 20 helper:token sleep 30 helper:token sleep 40 helper:token"
# GitHub gives a missing or withheld secret as an empty variable.
for t in unset empty; do
  tok=(); [ "$t" = empty ] && tok=(PRO_ACCESS_TOKEN=)
  expect "no token ($t), not a fork" 1 "PRO_ACCESS_TOKEN is empty or not set: this check reads https://" "$work/hooks-old" PR_FROM_FORK=false "${tok[@]}"
  clones "no token ($t), not a fork: nothing is cloned" ""
  expect "no token ($t), no PR_FROM_FORK" 1 "PRO_ACCESS_TOKEN is empty or not set: this check reads https://" "$work/hooks-old" "${tok[@]}"
  clones "no token ($t), no PR_FROM_FORK: nothing is cloned" ""
done
expect "a PR_FROM_FORK that is not true or false" 1 "PR_FROM_FORK is 'yes': it must be true or false" "$work/hooks-old" "${same[@]}" PR_FROM_FORK=yes

fork=(PR_FROM_FORK=true)
merged="the commit its merge commit is made on"
expect "a fork that leaves the files, which main changed since" 0 "from a fork, .* its 8 eBPF files of .* are, byte for byte, those of main at [0-9a-f]+ \(two\), $merged" "$(co same merge-same)" "${fork[@]}"
clones "a fork clones nothing" ""
expect "a fork that leaves the files, its token empty" 0 "from a fork, .* its 8 eBPF files of .* are, byte for byte, those of main at [0-9a-f]+ \(two\), $merged" "$(co emptysame merge-same)" "${fork[@]}" PRO_ACCESS_TOKEN=
clones "a fork, its token empty, clones nothing" ""
expect "a fork that changes files, which main changed since" 1 "^this pull request is from a fork, and its eBPF files of .* are not those of main at [0-9a-f]+ \(two\), $merged:
  bpf_x86_bpfel.go: this pull request changes its bytes
  bpf_x86_bpfel.o: this pull request changes its bytes
A pull request from a fork cannot change the eBPF objects: .*A maintainer must copy them" "$(co change merge-change)" "${fork[@]}"
expect "a fork that changes files, its token empty" 1 "^this pull request is from a fork, and its eBPF files of .* are not those of main at [0-9a-f]+ \(two\), $merged:
  bpf_x86_bpfel.go: this pull request changes its bytes
  bpf_x86_bpfel.o: this pull request changes its bytes
A pull request from a fork cannot change" "$(co emptychange merge-change)" "${fork[@]}" PRO_ACCESS_TOKEN=
clones "a fork, its token empty, that changes files clones nothing" ""
d=$(co add merge-same); printf 'x\n' > "$d/other_x86_bpfel.o"
expect "a fork that adds a file" 1 "not those of main at [0-9a-f]+ \(two\), $merged:
  other_x86_bpfel.o: main has no such file, this pull request adds it
A pull request" "$d" "${fork[@]}"
d=$(co remove merge-same); rm "$d/unixconn_x86_bpfel.go"
expect "a fork that removes a file" 1 "not those of main at [0-9a-f]+ \(two\), $merged:
  unixconn_x86_bpfel.go: main has it, this pull request removes it
A pull request" "$d" "${fork[@]}"
expect "a fork's checkout one commit deep" 1 "the checkout does not hold [0-9a-f]{40}, the main commit its merge commit is made on, .*fetch-depth: 2" "$(co shallow merge-same 1)" "${fork[@]}"
expect "a fork's checkout that is no merge commit" 1 "the checkout, [0-9a-f]+ \(a change to the docs\), has 1 parent\(s\): it is not the pull request's merge commit" "$(co notmerge same)" "${fork[@]}"
expect "a fork's hooks directory in no checkout" 1 "is not in a git checkout" "$work/hooks-old" "${fork[@]}"
expect "a fork with a token gets the whole check" 0 "are keploy/ebpf main's at [0-9a-f]+ \(merge feature\)" "$work/hooks-tip" "${fork[@]}" PRO_ACCESS_TOKEN="$token"
exit "$failed"

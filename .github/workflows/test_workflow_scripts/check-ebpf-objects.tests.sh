#!/bin/bash
# check-ebpf-objects.tests.sh: check-ebpf-objects.sh against a keploy/ebpf of
# its own, a local repository served blobless (file://, uploadpack.allowFilter)
# as GitHub serves the real one. Its main has a build from before the
# kernel-loads lane (zero), a build the lane was added under (one), a build
# after it (two), and a branch merged without squashing (x, then four); a
# pull request off it has a fifth build (three). The hooks directory passes
# with one, two or four, and fails, naming each file's place, with zero, with
# a mix of two builds, with the pull request's files, with the merged branch's
# first build, with a file main never had, and with no file at all. A token
# the server refuses, whichever way it says so, ends the check after one
# attempt. With no token, a check that is not a fork's fails, as does a
# PR_FROM_FORK that is neither true nor false.
#
# A keploy/keploy of its own gives a fork's pull request its checkout: the
# merge commit, two commits deep as actions/checkout fetches it. Each merge
# is made on a main commit whose eBPF files main later changes again. A pull
# request that leaves the files passes; one that changes, adds or removes one
# fails, listing those; a checkout one commit deep, one that is no merge
# commit, and a hooks directory in no checkout fail. A fork's check clones
# nothing. With a token a fork gets the whole check.
#
# The check's git is a wrapper (bin/git) that, for each clone, records
# whether git may prompt and what password git's credential helpers give for
# github.com, with a decoy helper configured before them as a runner's could
# be: the check's own must be the one that answers, with the token. After a
# clone it tells the helpers to store the credentials, as git does after one
# GitHub accepted. It records the token on any git command line, and in any
# file of the check's temporary directory, home or working directory before
# or after a clone; a case fails on either, on the token in such a file once
# the check has ended, and on the token in the check's output. It can also
# refuse a clone in each of the ways GitHub and git refuse credentials.
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
token="ghp_test$RANDOM$RANDOM${RANDOM}never"

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
url="file://$src"

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

# bin/git: the real git, but it records in $SHIM_LOG "argv-has-token" if the
# token is in its arguments; and for a clone, "prompts" if git may prompt for
# credentials, "helper:" with the password git's helpers give for github.com
# under its -c options (or "none"), and "file-has-token" if a file under
# $SHIM_SCAN holds the token before or after it. A clone that succeeds has
# the helpers store the credentials, as git does once GitHub accepts them.
# With SHIM_REFUSE it refuses the clone instead, as GitHub refuses a token
# that cannot read the repository (notfound) or is not valid (auth), or as
# git does when no helper gives a username (username) or a password
# (password).
real=$(command -v git)
mkdir -p "$work/bin"
cat > "$work/bin/git" <<EOF
#!/bin/bash
real='$real'
EOF
cat >> "$work/bin/git" <<'EOF'
args=("$@") cfg=() i=0
for a in "${args[@]}"; do
  case $a in *"$SHIM_TOKEN"*) echo argv-has-token >> "$SHIM_LOG" ;; esac
done
while [ "$i" -lt ${#args[@]} ] && [ "${args[$i]}" = -c ]; do cfg+=(-c "${args[$((i + 1))]}"); i=$((i + 2)); done
[ "${args[$i]:-}" = clone ] || exec "$real" "$@"
[ "${GIT_TERMINAL_PROMPT:-}" = 0 ] || echo prompts >> "$SHIM_LOG"
cred=$(printf 'protocol=https\nhost=github.com\npath=keploy/ebpf.git\n\n' |
  GIT_TERMINAL_PROMPT=0 "$real" "${cfg[@]}" credential fill 2> /dev/null) || cred=""
pw=$(sed -n 's/^password=//p' <<< "$cred")
if [ "$pw" = "$SHIM_TOKEN" ]; then echo helper:token >> "$SHIM_LOG"; else echo "helper:${pw:-none}" >> "$SHIM_LOG"; fi
infile() { grep -rqF -- "$SHIM_TOKEN" "$SHIM_SCAN" 2> /dev/null; }
leak=""
infile && leak=1
case ${SHIM_REFUSE:-} in
  "")
    "$real" "$@"
    rc=$?
    if [ "$rc" -eq 0 ] && [ -n "$cred" ]; then
      printf '%s\n\n' "$cred" | "$real" "${cfg[@]}" credential approve > /dev/null 2>&1
    fi
    ;;
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
  *)
    echo "bin/git: SHIM_REFUSE is '$SHIM_REFUSE', none of notfound, auth, username, password" >&2
    exit 2
    ;;
esac
infile && leak=1
[ -z "$leak" ] || echo file-has-token >> "$SHIM_LOG"
exit "$rc"
EOF
chmod +x "$work/bin/git"

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
# file under $run while it clones or once it has ended holds the token. Its
# clones are recorded in $work/shim.log.
run="$work/run"
expect() {
  local out rc=0 leak
  : > "$work/shim.log"
  rm -rf "$run" && mkdir -p "$run/tmp" "$run/home" "$run/cwd"
  out=$(cd "$run/cwd" && env -u PRO_ACCESS_TOKEN -u PR_FROM_FORK -u SHIM_REFUSE -u GIT_TERMINAL_PROMPT \
    PATH="$work/bin:$PATH" TMPDIR="$run/tmp" HOME="$run/home" \
    SHIM_LOG="$work/shim.log" SHIM_TOKEN="$token" SHIM_SCAN="$run" "${@:5}" bash "$check" "$url" "$4" 2>&1) || rc=$?
  leak=$({ grep -rlF -- "$token" "$run"; grep -xE 'argv-has-token|file-has-token' "$work/shim.log"; } 2> /dev/null || true)
  # The whole output against one extended regular expression: . and a
  # newline in it match a newline.
  if [ "$rc" = "$2" ] && [[ $out =~ $3 ]] && [[ $out != *"$token"* ]] && [ -z "$leak" ]; then
    echo "ok   $1"
  else
    echo "FAIL $1: exit $rc (want $2), want /$3/, no token in a file or argv (got: ${leak:-none}) or in:"
    sed 's/^/     /' <<< "$out"
    failed=1
  fi
}
# clones NAME WANT: the last case's clones are recorded as WANT (one line each).
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
  [password]="fatal: could not read Password for '[^']*': terminal prompts disabled")
for r in notfound auth username password; do
  expect "a refused token ($r)" 1 "${refusal[$r]}
PRO_ACCESS_TOKEN cannot read file://.*: the server refused it" "$work/hooks-old" "${same[@]}" SHIM_REFUSE="$r"
  clones "a refused token ($r) is not tried again" "helper:token"
done
expect "no token, not a fork" 1 "PRO_ACCESS_TOKEN is not set: this check reads file://" "$work/hooks-old" PR_FROM_FORK=false
clones "no token, not a fork: nothing is cloned" ""
expect "no token, no PR_FROM_FORK" 1 "PRO_ACCESS_TOKEN is not set" "$work/hooks-old"
expect "a PR_FROM_FORK that is not true or false" 1 "PR_FROM_FORK is 'yes': it must be true or false" "$work/hooks-old" "${same[@]}" PR_FROM_FORK=yes

fork=(PR_FROM_FORK=true)
merged="the commit its merge commit is made on"
expect "a fork that leaves the files, which main changed since" 0 "from a fork, .* its 8 eBPF files of .* are, byte for byte, those of main at [0-9a-f]+ \(two\), $merged" "$(co same merge-same)" "${fork[@]}"
clones "a fork clones nothing" ""
expect "a fork that changes files, which main changed since" 1 "^this pull request is from a fork, and its eBPF files of .* are not those of main at [0-9a-f]+ \(two\), $merged:
  bpf_x86_bpfel.go: this pull request changes its bytes
  bpf_x86_bpfel.o: this pull request changes its bytes
A pull request from a fork cannot change the eBPF objects: .*A maintainer must copy them" "$(co change merge-change)" "${fork[@]}"
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

#!/bin/bash
# check-ebpf-objects.tests.sh: check-ebpf-objects.sh against a keploy/ebpf of
# its own, a local repository served blobless (file://, uploadpack.allowFilter)
# as GitHub serves the real one. Its main has a build from before the
# kernel-loads lane (zero), a build the lane was added under (one), a build
# after it (two), and a branch merged without squashing (x, then four); a
# pull request off it has a fifth build (three). The hooks directory passes
# with one, two or four, and fails, naming each file's place, with zero, with
# a mix of two builds, with the pull request's files, with the merged branch's
# first build, with a file main never had, and with no file at all.
#
# A keploy/keploy of its own, served the same way, has build two on its main
# at pkg/agent/hooks/linux. A pull request from a fork, which has no token,
# passes with main's files and fails with any other: a file whose bytes are
# not main's, a file main has not, a file main has that it removes. With a
# token it gets the whole check. With no token anything else fails, as does a
# PR_FROM_FORK that is neither true nor false.
#
# The check's git is a wrapper (bin/git) that, for each clone, records
# whether the token is on git's command line and what password git's
# credential helpers give for github.com, with a decoy helper configured
# before them as a runner's could be: the check's own must be the one that
# answers, with the token. It can also refuse the clone as GitHub refuses a
# token that cannot read the repository. No case's output holds the token.
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

# keploy/keploy: build two at pkg/agent/hooks/linux, beside a file of
# another kind and one elsewhere that the check must not count.
kp="$work/keploy"
git init -q -b main "$kp"
git -C "$kp" config uploadpack.allowFilter true
mkdir -p "$kp/pkg/agent/hooks/linux"
for f in "${files[@]}"; do printf 'two %s\n' "$f" > "$kp/pkg/agent/hooks/linux/$f"; done
printf 'package linux\n' > "$kp/pkg/agent/hooks/linux/hooks.go"
printf 'elsewhere\n' > "$kp/pkg/agent/other_x86_bpfel.o"
git -C "$kp" add -A
git -C "$kp" -c user.name=test -c user.email=test@example.com -c commit.gpgsign=false commit -q -m main
kurl="file://$kp"

# bin/git: the real git, but for a clone it first records, in $SHIM_LOG,
# "argv-has-token" if the token is in its arguments, "prompts" if git may
# prompt for credentials, and "helper:" with the password git's helpers give
# for github.com under its -c options (or "none"); and with SHIM_REFUSE set
# it refuses the clone, as GitHub refuses a token that cannot read it.
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
if [ "${args[$i]:-}" = clone ]; then
  [ "${GIT_TERMINAL_PROMPT:-}" = 0 ] || echo prompts >> "$SHIM_LOG"
  pw=$(printf 'protocol=https\nhost=github.com\npath=keploy/ebpf.git\n\n' |
    GIT_TERMINAL_PROMPT=0 "$real" "${cfg[@]}" credential fill 2> /dev/null | sed -n 's/^password=//p')
  if [ "$pw" = "$SHIM_TOKEN" ]; then echo helper:token >> "$SHIM_LOG"; else echo "helper:${pw:-none}" >> "$SHIM_LOG"; fi
  if [ -n "${SHIM_REFUSE:-}" ]; then
    echo "remote: Repository not found."
    echo "fatal: repository 'https://github.com/keploy/ebpf.git/' not found" >&2
    exit 128
  fi
fi
exec "$real" "$@"
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
# PRO_ACCESS_TOKEN or PR_FROM_FORK but those of the VARs, exits RC, says
# PATTERN and never the token. Its clones are recorded in $work/shim.log.
expect() {
  local out rc=0
  : > "$work/shim.log"
  out=$(env -u PRO_ACCESS_TOKEN -u PR_FROM_FORK -u SHIM_REFUSE -u GIT_TERMINAL_PROMPT PATH="$work/bin:$PATH" \
    SHIM_LOG="$work/shim.log" SHIM_TOKEN="$token" "${@:5}" bash "$check" "$url" "$4" "$kurl" 2>&1) || rc=$?
  # The whole output against one extended regular expression: . and a
  # newline in it match a newline.
  if [ "$rc" = "$2" ] && [[ $out =~ $3 ]] && [[ $out != *"$token"* ]]; then
    echo "ok   $1"
  else
    echo "FAIL $1: exit $rc (want $2), want /$3/ and no token in:"; sed 's/^/     /' <<< "$out"; failed=1
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

expect "a token keploy/ebpf refuses" 1 "PRO_ACCESS_TOKEN cannot read file://.*: the server refused it" "$work/hooks-old" "${same[@]}" SHIM_REFUSE=1
clones "a refused token is not tried again" "helper:token"
expect "no token, not a fork" 1 "PRO_ACCESS_TOKEN is not set: this check reads file://" "$work/hooks-old" PR_FROM_FORK=false
clones "no token, not a fork: nothing is cloned" ""
expect "no token, no PR_FROM_FORK" 1 "PRO_ACCESS_TOKEN is not set" "$work/hooks-old"
expect "a PR_FROM_FORK that is not true or false" 1 "PR_FROM_FORK is 'yes': it must be true or false" "$work/hooks-old" "${same[@]}" PR_FROM_FORK=yes

fork=(PR_FROM_FORK=true)
expect "a fork with main's files" 0 "from a fork, .* its 8 eBPF files of .* are, byte for byte, keploy/keploy main's" "$work/hooks-old" "${fork[@]}"
clones "a fork's clone of keploy/keploy offers no token" "helper:decoy"
expect "a fork that changes a file" 1 "bpf_x86_bpfel.o: its bytes are not main's.*A maintainer must copy them" "$work/hooks-mixed" "${fork[@]}"
expect "a fork that changes a file: only the changed ones" 1 "^this pull request is from a fork.*:
  bpf_arm64_bpfel.go: its bytes are not main's
  bpf_arm64_bpfel.o: its bytes are not main's
  bpf_x86_bpfel.go: its bytes are not main's
  bpf_x86_bpfel.o: its bytes are not main's
A pull request from a fork cannot change the eBPF objects" "$work/hooks-mixed" "${fork[@]}"
expect "a fork that adds a file" 1 "other_x86_bpfel.o: main has no such file" "$work/hooks-extra" "${fork[@]}"
d=$(hooks removed two); rm "$d/unixconn_x86_bpfel.go"
expect "a fork that removes a file" 1 "unixconn_x86_bpfel.go: main has it, this pull request removes it" "$d" "${fork[@]}"
expect "a fork that cannot clone keploy/keploy" 1 "could not clone file://.*/keploy's main, whose eBPF files a pull request from a fork must have" "$work/hooks-old" "${fork[@]}" SHIM_REFUSE=1
expect "a fork with a token gets the whole check" 0 "are keploy/ebpf main's at [0-9a-f]+ \(merge feature\)" "$work/hooks-tip" "${fork[@]}" PRO_ACCESS_TOKEN="$token"
exit "$failed"

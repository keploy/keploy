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
#   bash .github/workflows/test_workflow_scripts/check-ebpf-objects.tests.sh
set -euo pipefail
check="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-ebpf-objects.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
files=(bpf_arm64_bpfel.go bpf_arm64_bpfel.o bpf_x86_bpfel.go bpf_x86_bpfel.o
  unixconn_arm64_bpfel.go unixconn_arm64_bpfel.o unixconn_x86_bpfel.go unixconn_x86_bpfel.o)

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
# expect NAME RC PATTERN DIR: the check of DIR exits RC and says PATTERN.
expect() {
  local out rc=0
  out=$(bash "$check" "$url" "$4" 2>&1) || rc=$?
  if [ "$rc" = "$2" ] && grep -qE -- "$3" <<< "$out"; then
    echo "ok   $1"
  else
    echo "FAIL $1: exit $rc (want $2), want /$3/ in:"; sed 's/^/     /' <<< "$out"; failed=1
  fi
}

expect "main's tip, a merge" 0 "the 8 eBPF files of .* are keploy/ebpf main's at [0-9a-f]+ \(merge feature\)" "$(hooks tip four)"
expect "an older main commit" 0 "are keploy/ebpf main's at [0-9a-f]+ \(two\)" "$(hooks old two)"
expect "the build the lane was added under" 0 "are keploy/ebpf main's at [0-9a-f]+ \(one\)" "$(hooks floor one)"
expect "a build from before the lane" 1 "but no main commit that holds them has the .woodpecker/kernel-loads.yml lane" "$(hooks before zero)"
expect "two builds mixed" 1 "bpf_x86_bpfel.o: keploy/ebpf main's at [0-9a-f]+ \(one\)" "$(hooks mixed two one)"
expect "two builds mixed: the other" 1 "unixconn_x86_bpfel.o: keploy/ebpf main's at [0-9a-f]+ \(two\)" "$work/hooks-mixed"
expect "a pull request's build" 1 "unixconn_x86_bpfel.o: never on keploy/ebpf's main" "$(hooks pr three)"
expect "a merged branch's first build" 1 "unixconn_x86_bpfel.o: never on keploy/ebpf's main" "$(hooks branch x)"
d=$(hooks extra two); printf 'x\n' > "$d/other_x86_bpfel.o"
expect "a file main never had" 1 "other_x86_bpfel.o: never on keploy/ebpf's main" "$d"
mkdir -p "$work/hooks-none"
expect "no file" 1 "no \*_bpfel.o or \*_bpfel.go file" "$work/hooks-none"
exit "$failed"

#!/bin/bash
# check-ebpf-objects.sh: the eBPF files the agent's hooks embed
# (pkg/agent/hooks/linux/*_bpfel.o and *_bpfel.go: the bpf and unixconn
# objects and their bindings, both arches) are the files of one commit of
# keploy/ebpf's main, byte for byte.
#
# keploy/ebpf is where they are built and tested: its objects-reproducible
# lane rebuilds them from source with a pinned toolchain, its test lanes load
# them on their runners' kernels (5.10 and 6.12), and its kernel-loads lane
# loads them in VMs of 5.19 and 6.0. Those two verifiers refuse what the
# runners' let through, a helper call that may read stack bytes nothing wrote:
# keploy's unixconn objects did that up to v3.6.160, so on 5.19 and 6.0 the
# agent ran with no AF_UNIX report. An object built anywhere else, or copied
# from a keploy/ebpf pull request before it merged, has passed none of those
# lanes; nor have the files of a main commit from before the kernel-loads
# lane (.woodpecker/kernel-loads.yml), such as the refused unixconn objects:
# the files must still have been main's when that lane was in its tree. And
# one commit, because the unixconn object shares the main object's maps by
# their names, so the two must come from one build (keploy/ebpf's README).
# Main's commits are its first-parent line: a branch merged without squashing
# brings commits that never were main's tip.
#
#   bash .github/workflows/test_workflow_scripts/check-ebpf-objects.sh [EBPF_REPO [HOOKS_DIR]]
#
# EBPF_REPO is https://github.com/keploy/ebpf.git unless given, HOOKS_DIR
# pkg/agent/hooks/linux. The clone holds main's commits and trees and no
# file content (a blobless clone): a tree names each file by the hash of its
# bytes, which `git hash-object` gives for the files here, so the trees are
# all the check reads. It is retried a bounded number of times, each attempt
# given up once its transfer stays below 1000 B/s for 30 s.
set -euo pipefail
repo=${1:-https://github.com/keploy/ebpf.git}
hooks=${2:-pkg/agent/hooks/linux}

shopt -s nullglob
names=()
for f in "$hooks"/*_bpfel.o "$hooks"/*_bpfel.go; do names+=("$(basename "$f")"); done
shopt -u nullglob
if [ ${#names[@]} -eq 0 ]; then
  echo "no *_bpfel.o or *_bpfel.go file in $hooks: nothing to check" >&2
  exit 1
fi
mapfile -t names < <(printf '%s\n' "${names[@]}" | LC_ALL=C sort)
# want: "BLOB NAME" per file, sorted by name, as ls-tree lists a tree's.
want=""
for n in "${names[@]}"; do want+="$(git hash-object "$hooks/$n") $n"$'\n'; done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GIT_HTTP_LOW_SPEED_LIMIT=1000 GIT_HTTP_LOW_SPEED_TIME=30
for i in 1 2 3 4 5; do
  rm -rf "$work/ebpf"
  if git clone -q --bare --filter=blob:none --single-branch --branch main "$repo" "$work/ebpf"; then
    break
  fi
  if [ "$i" -eq 5 ]; then
    echo "could not clone $repo's main ($i attempts)" >&2
    exit 1
  fi
  echo "clone of $repo failed (attempt $i of 5); retrying in $((i * 10))s" >&2
  sleep $((i * 10))
done
g() { git -C "$work/ebpf" "$@"; }

# lane is the keploy/ebpf lane that loads the files on the kernels the test
# lanes cannot show (5.19 and 6.0); the floor.
lane=.woodpecker/kernel-loads.yml

# at COMMIT: "BLOB NAME" for each of the names in COMMIT's tree.
at() { g ls-tree "$1" -- "${names[@]}" | awk '{ split($0, t, "\t"); split(t[1], m, " "); print m[3], t[2] }'; }

# Only a commit that changed one of the files can be the first to hold them
# as they are here; the commits after it, up to the next such (newer), hold
# the same. The last of those, newer's parent or main's tip, must have lane.
newer=""
for c in $(g rev-list --first-parent main -- "${names[@]}"); do
  if [ "$(at "$c")"$'\n' = "$want" ]; then
    last=main
    [ -n "$newer" ] && last="$newer^"
    if [ -z "$(g ls-tree "$last" -- "$lane")" ]; then
      echo "the ${#names[@]} eBPF files of $hooks are keploy/ebpf main's at $(g log -1 --format='%h (%s)' "$c")," >&2
      echo "but no main commit that holds them has the $lane lane: they never passed it." \
        "Copy the files of a main commit that has it." >&2
      exit 1
    fi
    echo "the ${#names[@]} eBPF files of $hooks are keploy/ebpf main's at $(g log -1 --format='%h (%s)' "$c")"
    exit 0
  fi
  newer=$c
done

echo "the eBPF files of $hooks are not the files of any one keploy/ebpf main commit:" >&2
for n in "${names[@]}"; do
  blob=$(git hash-object "$hooks/$n"); seen=""
  for c in $(g rev-list --first-parent main -- "$n"); do
    if [ "$(g ls-tree "$c" -- "$n" | awk '{ print $3 }')" = "$blob" ]; then
      seen=$(g log -1 --format='%h (%s)' "$c"); break
    fi
  done
  if [ -n "$seen" ]; then
    echo "  $n: keploy/ebpf main's at $seen" >&2
  else
    echo "  $n: never on keploy/ebpf's main" >&2
  fi
done
echo "Copy all of them from one keploy/ebpf main commit (its README, step 2); a keploy/ebpf pull request's files once it has merged." >&2
exit 1

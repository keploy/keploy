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
#   PRO_ACCESS_TOKEN=... PR_FROM_FORK=true|false \
#     bash .github/workflows/test_workflow_scripts/check-ebpf-objects.sh [EBPF_REPO [HOOKS_DIR]]
#
# EBPF_REPO is https://github.com/keploy/ebpf.git unless given, HOOKS_DIR
# pkg/agent/hooks/linux. The clone holds main's commits and trees and no file
# content (a blobless clone): a tree names each file by the hash of its
# bytes, which `git hash-object` gives for the files here, so the trees are
# all the check reads. It is retried a bounded number of times, each attempt
# given up once its transfer stays below 1000 B/s for 30 s. A server that
# refuses the credentials ends it at once: one that says they are not valid
# (401), that the repository is not there for them (404) or that they may not
# read it (403, as an organization's SSO or IP allow list says it), and git
# asking for credentials it was not given.
#
# keploy/ebpf is a private repository. PRO_ACCESS_TOKEN must be a token that
# can read it: git gets it from a credential helper that reads it from the
# environment, so it is never in a URL, a command line or a file, and the
# check never prints it. With no token (GitHub gives an empty one when the
# secret is missing or withheld), a pull request from a keploy/keploy branch
# or a push to main fails: those get the repository's secrets, so a missing
# token is a broken workflow.
#
# A pull request from a fork (PR_FROM_FORK=true, from the event's
# pull_request.head.repo.fork) gets no secrets, so with no token it cannot
# read keploy/ebpf. Its checkout is the pull request's merge commit, whose
# first parent is the main commit GitHub made the merge on, a commit whose
# files main's own run checked. The pull request passes only if HOOKS_DIR's
# eBPF files are, byte for byte, that commit's: only if it leaves them as
# main has them. A fork cannot change them; a maintainer copies them. That
# commit, and not main's tip: main may have changed the files since, and a
# re-run reuses the merge commit, so a pull request that never touched them
# would fail for good. The checkout must hold that commit (actions/checkout
# with fetch-depth: 2); nothing is fetched.
set -euo pipefail
repo=${1:-https://github.com/keploy/ebpf.git}
hooks=${2:-pkg/agent/hooks/linux}
fork=${PR_FROM_FORK:-false}
case $fork in
  true | false) ;;
  *)
    echo "PR_FROM_FORK is '$fork': it must be true or false" >&2
    exit 1
    ;;
esac

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

if [ -z "${PRO_ACCESS_TOKEN:-}" ] && [ "$fork" = true ]; then
  h() { git -C "$hooks" "$@"; }
  if ! prefix=$(h rev-parse --show-prefix 2> /dev/null); then
    echo "this pull request is from a fork, and $hooks is not in a git checkout: the check compares" \
      "its eBPF files with those of the main commit the pull request's merge commit is made on" >&2
    exit 1
  fi
  # HEAD's parents as its commit object names them: a shallow checkout
  # does not resolve HEAD^1 when it does not hold that commit.
  mapfile -t parents < <(h cat-file -p HEAD | sed -n '/^$/q; s/^parent //p')
  if [ ${#parents[@]} -ne 2 ]; then
    echo "this pull request is from a fork, and the checkout, $(h log -1 --format='%h (%s)' HEAD)," \
      "has ${#parents[@]} parent(s): it is not the pull request's merge commit, whose first parent" \
      "is the main commit whose files it must have (actions/checkout's default for a pull request)" >&2
    exit 1
  fi
  base=${parents[0]}
  if ! h cat-file -e "$base^{tree}" 2> /dev/null; then
    echo "this pull request is from a fork, and the checkout does not hold $base, the main commit" \
      "its merge commit is made on, whose eBPF files it must have: check out with fetch-depth: 2" >&2
    exit 1
  fi
  at=$(h log -1 --format='%h (%s)' "$base")
  spec=()
  [ -n "$prefix" ] && spec=(-- "$prefix")
  # base's "BLOB NAME" for each *_bpfel.o and *_bpfel.go file directly in
  # HOOKS_DIR.
  main=$(h ls-tree --full-tree "$base" "${spec[@]}" |
    awk -F'\t' -v dir="$prefix" '{ split($1, m, " "); n = substr($2, length(dir) + 1) }
      m[2] == "blob" && n ~ /_bpfel\.(o|go)$/ { print m[3], n }' | LC_ALL=C sort -k2)
  [ -n "$main" ] && main+=$'\n'
  if [ "$main" = "$want" ]; then
    echo "this pull request is from a fork, which cannot read keploy/ebpf (a private repository);" \
      "its ${#names[@]} eBPF files of $hooks are, byte for byte, those of main at $at," \
      "the commit its merge commit is made on, and main's own run checked those"
    exit 0
  fi
  declare -A here=() there=()
  for n in "${names[@]}"; do here[$n]=$(git hash-object "$hooks/$n"); done
  while read -r b n; do [ -n "$n" ] && there[$n]=$b; done <<< "$main"
  echo "this pull request is from a fork, and its eBPF files of $hooks are not those of main at $at," \
    "the commit its merge commit is made on:" >&2
  for n in $(printf '%s\n' "${!here[@]}" "${!there[@]}" | LC_ALL=C sort -u); do
    if [ -z "${there[$n]:-}" ]; then
      echo "  $n: main has no such file, this pull request adds it" >&2
    elif [ -z "${here[$n]:-}" ]; then
      echo "  $n: main has it, this pull request removes it" >&2
    elif [ "${here[$n]}" != "${there[$n]}" ]; then
      echo "  $n: this pull request changes its bytes" >&2
    fi
  done
  echo "A pull request from a fork cannot change the eBPF objects: they are copied from keploy/ebpf," \
    "which this check cannot read from a fork (it gets no secrets). A maintainer must copy them" \
    "(keploy/ebpf's README, step 2); leave $hooks as main has it." >&2
  exit 1
fi
if [ -z "${PRO_ACCESS_TOKEN:-}" ]; then
  echo "PRO_ACCESS_TOKEN is empty or not set: this check reads $repo, and keploy/ebpf is a private repository" \
    "that it can read only with a token that has read access to it. A push to main and a pull request" \
    "from a keploy/keploy branch get the repository's secrets, so the workflow must pass PRO_ACCESS_TOKEN" \
    "(prepare_and_run.yml, run_ebpf_objects)." >&2
  exit 1
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GIT_HTTP_LOW_SPEED_LIMIT=1000 GIT_HTTP_LOW_SPEED_TIME=30
# git never prompts: with no usable credentials a clone fails at once.
export GIT_TERMINAL_PROMPT=0
# The empty helper drops any the runner configured, so this token is the one
# that answers; the helper reads it from the environment when git asks.
helper='!f() { if [ "$1" = get ]; then printf "username=x-access-token\npassword=%s\n" "$PRO_ACCESS_TOKEN"; fi; }; f'
# clone: a bare, blobless clone of repo's main into $work/ebpf. 1 once five
# attempts have failed, 2 at once if the server refused the credentials (401,
# 404 or 403) or asked for some and git had none: no retry changes that. Any
# other failure, such as a name that does not resolve, a connection that
# stalls or a 5xx, is tried again.
clone() {
  local i
  for i in 1 2 3 4 5; do
    rm -rf "$work/ebpf"
    if git -c credential.helper= -c "credential.helper=$helper" \
      clone -q --bare --filter=blob:none --single-branch --branch main "$repo" "$work/ebpf" 2> "$work/clone.err"; then
      return 0
    fi
    cat "$work/clone.err" >&2
    if grep -qE "fatal: (Authentication failed for|repository '.*' not found|could not read (Username|Password) for|unable to access '.*': The requested URL returned error: 403$)" "$work/clone.err"; then
      return 2
    fi
    [ "$i" -eq 5 ] && return 1
    echo "clone of $repo failed (attempt $i of 5); retrying in $((i * 10))s" >&2
    sleep $((i * 10))
  done
}
rc=0
clone || rc=$?
if [ "$rc" -eq 2 ]; then
  echo "PRO_ACCESS_TOKEN cannot read $repo: the server refused it (above). The token must have read" \
    "access to keploy/ebpf, a private repository." >&2
  exit 1
elif [ "$rc" -ne 0 ]; then
  echo "could not clone $repo's main (5 attempts)" >&2
  exit 1
fi
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

#!/usr/bin/env bash
#
# keploy-installer.tests.sh — runs keploy.sh (served as https://keploy.io/install.sh)
# on its default route, the hand-off to the installer at
# https://keploy.io/ent/install.sh, and on its --oss route, and checks that it
# leaves the caller's working directory alone and touches nothing in the temp
# directory but a private directory of its own.
#
#   bash .github/workflows/test_workflow_scripts/keploy-installer.tests.sh
#
# KEEP_TMP=1 keeps each case's directories, output and stub log.
#
# No network and no root. Each case gets its own HOME and TMPDIR, the TMPDIR
# already holding files of someone else's named like the ones in a release
# tarball, and a PATH that starts with stubs:
#   - curl: a download of ent/install.sh writes a stand-in installer (it logs
#     that it ran, from where and with what arguments, and returns
#     FAKE_ENT_STATUS) wherever curl was told to write it, `-o FILE` or, like
#     the real curl, ./install.sh for -O. A download of a release tarball
#     writes a stand-in release (a keploy that logs its runs, LICENSE,
#     README.md, READMEes-Es.md).
#   - rm, mv, tar and mkdir run the real command only when every absolute path
#     they are given is inside this test's own directory, and curl writes only
#     there; anything else is logged as "fenced:" and fails. sudo always fails
#     and is logged. So a keploy.sh that reaches for fixed /tmp paths, as the
#     --oss route once did, fails here without touching the host's /tmp.
# keploy.sh itself runs unmodified, SOURCED the way users run it, under bash
# and under zsh when it is installed.
#
# What it pins down:
#   - run from a directory that holds the caller's own install.sh and
#     keploy-enterprise.sh (a repo root, with keploy.sh downloaded elsewhere),
#     both survive unchanged and nothing is added there.
#   - sourced as ./install.sh the way the docs say, the copy the caller
#     downloaded stays where they put it, and the hand-off does not download
#     the other installer over it.
#   - the other installer is downloaded into a private temp directory under
#     $TMPDIR that is removed afterwards, and gets the caller's arguments.
#   - a failed download fails the install, and never sources an install.sh
#     that happens to be in the caller's directory instead.
#   - the other installer's status is keploy.sh's status.
#   - --oss downloads and extracts the release in a private temp directory
#     under $TMPDIR that is removed afterwards, installs the release's binary
#     (never a keploy that was already in the temp directory), and keeps the
#     files of someone else's that were there.
#   - with --oss, a failed or corrupt download fails the install and installs
#     nothing.
#   - no case touches a path outside its own directories, or runs sudo.

set -u

repo=$(cd "$(dirname "$0")/../../.." && pwd)
installer="$repo/keploy.sh"
tmp=$(mktemp -d)
trap '[ -n "${KEEP_TMP:-}" ] || rm -rf "$tmp"' EXIT

failures=0
pass() { echo "  ok   $*"; }
fail() {
  echo "  FAIL $*"
  failures=$((failures + 1))
}
check() { # check <description> <command...>
  local what=$1
  shift
  if "$@"; then pass "$what"; else fail "$what"; fi
}

# ---------------------------------------------------------------- stubs

stubs="$tmp/stubs"
mkdir -p "$stubs"

cat > "$tmp/fake-ent-install.sh" << 'EOF'
echo "ent-installer ran from $PWD with: $*" >> "$STUB_LOG"
return "${FAKE_ENT_STATUS:-0}" 2> /dev/null || exit "${FAKE_ENT_STATUS:-0}"
EOF

cat > "$stubs/curl" << 'EOF'
#!/bin/sh
echo "curl $*" >> "$STUB_LOG"
out=""
remote_name=""
head=""
prev=""
for a in "$@"; do
  case "$prev" in -o | --output) out=$a ;; esac
  case "$a" in -O | --remote-name) remote_name=1 ;; -I | -sI | --head) head=1 ;; esac
  prev=$a
done
[ -z "$head" ] || exit 0
fenced() {
  case "$out" in
    "$FENCE"/*) return 1 ;;
    /*) echo "fenced: curl $*" >> "$STUB_LOG" ;;
    *) return 1 ;;
  esac
}
case "$*" in
  *keploy.io/ent/install.sh*)
    [ -n "$out" ] || { [ -n "$remote_name" ] && out=./install.sh; }
    [ -n "$out" ] || exit 1
    fenced "$@" && exit 23
    [ -z "${FAKE_DOWNLOAD_FAIL:-}" ] || exit 22
    echo "download-to $out" >> "$STUB_LOG"
    cp "$FAKE_ENT_INSTALLER" "$out"
    ;;
  *github.com/keploy/keploy/releases/*/keploy_*.tar.gz*)
    [ -n "$out" ] || exit 1
    fenced "$@" && exit 23
    [ -z "${FAKE_RELEASE_DOWNLOAD_FAIL:-}" ] || exit 22
    echo "release-download-to $out" >> "$STUB_LOG"
    cp "$FAKE_RELEASE_TARBALL" "$out"
    ;;
esac
exit 0
EOF

cat > "$stubs/fence" << 'EOF'
#!/bin/sh
name=$(basename "$0")
for a in "$@"; do
  case "$a" in
    "$FENCE"/*) ;;
    /*)
      echo "fenced: $name $*" >> "$STUB_LOG"
      exit 1
      ;;
  esac
done
PATH=/usr/bin:/bin
export PATH
exec "$name" "$@"
EOF
for cmd in rm mv tar mkdir; do ln -s fence "$stubs/$cmd"; done

cat > "$stubs/sudo" << 'EOF'
#!/bin/sh
echo "sudo: $*" >> "$STUB_LOG"
exit 1
EOF
chmod +x "$stubs/curl" "$stubs/fence" "$stubs/sudo"

# The stand-in release, laid out like goreleaser's tarball.
release="$tmp/release"
mkdir -p "$release"
cat > "$release/keploy" << 'EOF'
#!/bin/sh
echo "the release's keploy ran: $*" >> "$STUB_LOG"
EOF
chmod +x "$release/keploy"
for f in LICENSE README.md READMEes-Es.md; do echo "the release's $f" > "$release/$f"; done
tar czf "$tmp/release.tar.gz" -C "$release" keploy LICENSE README.md READMEes-Es.md
echo "not a tarball" > "$tmp/corrupt.tar.gz"

# ---------------------------------------------------------------- harness

# The caller's install.sh logs it if anything runs it; $STUB_LOG is the
# runner's to expand.
# shellcheck disable=SC2016
CALLERS_INSTALL='echo "the caller'"'"'s own install.sh ran" >> "$STUB_LOG"'
CALLERS_ENT="the caller's own keploy-enterprise.sh"

# install <case> [keploy.sh args]
# Sources keploy.sh under $run_shell from the case's working directory. By
# default that directory holds keploy.sh as install.sh, sourced from there the
# way the docs say (`curl -O .../install.sh && source install.sh`), next to a
# keploy-enterprise.sh of the caller's. With case_in_repo set it is a repo root
# instead: it holds the caller's own install.sh and keploy-enterprise.sh, and
# keploy.sh is sourced from another directory. Extra environment goes in the
# case_env array. Leaves $work, $out, $log, $scratch and $rc.
install() {
  local dir="$tmp/$run_shell-$1"
  shift
  work="$dir/work"
  out="$dir/out"
  log="$dir/stub.log"
  scratch="$dir/tmp"
  dir_home="$dir/home"
  mkdir -p "$dir/home" "$work" "$scratch" "$dir/dl"
  : > "$log"
  local f
  for f in $THEIRS; do echo "someone else's $f" > "$scratch/$f"; done
  local src=./install.sh
  echo "$CALLERS_ENT" > "$work/keploy-enterprise.sh"
  if [ -n "$case_in_repo" ]; then
    echo "$CALLERS_INSTALL" > "$work/install.sh"
    src="$dir/dl/install.sh"
    cp "$installer" "$src"
  else
    cp "$installer" "$work/install.sh"
  fi
  (
    cd "$work" || exit 1
    # The single-quoted $1, $src and $@ are the inner shell's to expand.
    # shellcheck disable=SC2016
    env -i HOME="$dir/home" PATH="$stubs:/usr/bin:/bin" TMPDIR="$scratch" TERM=dumb \
      STUB_LOG="$log" FENCE="$tmp" FAKE_ENT_INSTALLER="$tmp/fake-ent-install.sh" \
      FAKE_RELEASE_TARBALL="$tmp/release.tar.gz" ${case_env[@]+"${case_env[@]}"} \
      "$run_shell" -c 'src=$1; shift; source "$src" "$@"' install "$src" "$@"
  ) > "$out" 2>&1
  rc=$?
  case_env=()
  case_in_repo=""
}
case_env=()
case_in_repo=""
# Files of someone else's in each case's TMPDIR, named like the ones the --oss
# route downloads and extracts.
THEIRS="keploy keploy.tar.gz LICENSE README.md READMEes-Es.md"

# The caller's directory is as the case set it up: their install.sh (or the
# copy of keploy.sh they downloaded) and their keploy-enterprise.sh, unchanged,
# and nothing else.
callers_dir_untouched() {
  local want_install=$1
  [ "$(cat "$work/keploy-enterprise.sh" 2> /dev/null)" = "$CALLERS_ENT" ] &&
    [ "$(find "$work" -mindepth 1 | wc -l)" -eq 2 ] &&
    if [ "$want_install" = callers ]; then
      [ "$(cat "$work/install.sh" 2> /dev/null)" = "$CALLERS_INSTALL" ]
    else
      cmp -s "$installer" "$work/install.sh"
    fi
}
# The case's TMPDIR holds the files of someone else's it started with,
# unchanged, and nothing else.
tmp_left_as_it_was() {
  local f
  for f in $THEIRS; do
    [ "$(cat "$scratch/$f" 2> /dev/null)" = "someone else's $f" ] || return 1
  done
  [ "$(ls -A "$scratch" | wc -l)" -eq "$(echo "$THEIRS" | wc -w)" ]
}
# The download (the "download-to" or "release-download-to" line) went to
# <file> in a directory of its own under the case's TMPDIR, and that directory
# is gone afterwards.
used_private_dir_and_cleaned_up() { # <stub log key> <file>
  local to
  to=$(sed -n "s/^$1 //p" "$log")
  case "$to" in "$scratch"/*/"$2") ;; *) return 1 ;; esac
  case "${to#"$scratch"/}" in */*/*) return 1 ;; esac
  tmp_left_as_it_was
}
# Nothing outside the case's directories was touched, and sudo never ran.
stayed_inside() { ! grep -qE '^(fenced|sudo):' "$log"; }
installed() { cmp -s "$release/keploy" "$dir_home/.keploy/bin/keploy"; }
nothing_installed() { [ ! -e "$dir_home/.keploy/bin/keploy" ]; }
release_keploy_ran() { grep -qF "the release's keploy ran: example" "$log"; }
release_keploy_did_not_run() { ! grep -qF "the release's keploy ran" "$log"; }
ent_ran_with() { grep -qF "ent-installer ran from $work with: $1" "$log"; }
ent_did_not_run() { ! grep -q '^ent-installer ' "$log"; }
callers_install_not_run() { ! grep -qF "the caller's own install.sh ran" "$log"; }
said() { grep -qF -- "$1" "$out"; }
show_output() { sed 's/^/     | /' "$out" | tail -n 15; }

# ---------------------------------------------------------------- cases

suite() {
  echo "==== sourced under $run_shell"

  echo "== from a repo root that has its own install.sh and keploy-enterprise.sh"
  case_in_repo=1
  install in-repo -noRoot
  [ "$rc" -eq 0 ] || show_output
  check "the install succeeds" test "$rc" -eq 0
  check "the other installer runs, with the caller's arguments" ent_run_with_args
  check "the repo's install.sh and keploy-enterprise.sh are kept unchanged, and nothing is added" callers_dir_untouched callers
  check "the repo's install.sh is never run" callers_install_not_run
  check "the download used a private temp directory, removed afterwards" used_private_dir_and_cleaned_up download-to install.sh
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside

  echo "== sourced as ./install.sh, the way the docs say"
  install documented -noRoot
  [ "$rc" -eq 0 ] || show_output
  check "the install succeeds" test "$rc" -eq 0
  check "the other installer runs, with the caller's arguments" ent_run_with_args
  check "the install.sh the caller downloaded is left as it was, next to their keploy-enterprise.sh" callers_dir_untouched downloaded
  check "the download used a private temp directory, removed afterwards" used_private_dir_and_cleaned_up download-to install.sh
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside

  echo "== the download fails, from a repo root"
  case_in_repo=1
  case_env=(FAKE_DOWNLOAD_FAIL=1)
  install download-fails -noRoot
  check "the install fails" test "$rc" -ne 0
  check "it says so" said "could not download the Keploy installer"
  check "the repo's install.sh is not sourced in its place" callers_install_not_run
  check "nothing else runs either" ent_did_not_run
  check "the repo's files are kept unchanged" callers_dir_untouched callers
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was

  echo "== the other installer refuses (an Intel Mac, a bad -v)"
  case_env=(FAKE_ENT_STATUS=1)
  install ent-refuses
  check "the install fails with it" test "$rc" -eq 1
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was

  echo "== --oss from a repo root, with someone else's keploy, LICENSE and README.md in the temp directory"
  case_in_repo=1
  install oss-in-repo --oss -noRoot
  [ "$rc" -eq 0 ] || show_output
  check "the install succeeds" test "$rc" -eq 0
  check "the release's keploy is installed in ~/.keploy/bin, not the one in the temp directory" installed
  check "keploy example runs the installed keploy" release_keploy_ran
  check "the download and extraction used a private temp directory, removed afterwards; the other files there are kept unchanged" used_private_dir_and_cleaned_up release-download-to keploy.tar.gz
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside
  check "the repo's install.sh and keploy-enterprise.sh are kept unchanged, and nothing is added" callers_dir_untouched callers
  check "the other installer does not run" ent_did_not_run

  echo "== --oss, the download fails"
  case_env=(FAKE_RELEASE_DOWNLOAD_FAIL=1)
  install oss-download-fails --oss -noRoot
  check "the install fails" test "$rc" -ne 0
  check "nothing is installed, and no keploy runs" eval 'nothing_installed && release_keploy_did_not_run'
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside

  echo "== --oss, the download is not a tarball"
  case_env=(FAKE_RELEASE_TARBALL="$tmp/corrupt.tar.gz")
  install oss-corrupt --oss -noRoot
  check "the install fails" test "$rc" -ne 0
  check "nothing is installed, and no keploy runs" eval 'nothing_installed && release_keploy_did_not_run'
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside
}
ent_run_with_args() { ent_ran_with "-noRoot"; }

run_shell=bash
suite
if command -v zsh > /dev/null 2>&1; then
  run_shell=zsh
  suite
elif [ -n "${REQUIRE_ZSH:-}" ]; then
  # CI sets this: macOS users source the installer from zsh, so a run
  # without it must not pass quietly.
  fail "zsh is required (REQUIRE_ZSH is set) but not installed"
else
  echo "==== zsh not installed here: the suite ran under bash only"
fi

echo
if [ "$failures" -ne 0 ]; then
  echo "keploy-installer.tests: $failures check(s) FAILED"
  exit 1
fi
echo "keploy-installer.tests: all checks passed"

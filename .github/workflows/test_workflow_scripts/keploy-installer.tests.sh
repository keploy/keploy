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
#     README.md, READMEes-Es.md). FAKE_HTTP_404 makes the installer URL answer
#     with an error page: written, exit 0, unless curl is told to --fail.
#   - rm, mv, tar and mkdir run the real command only when every absolute path
#     they are given is inside this test's own directory, and curl writes only
#     there; anything else is logged as "fenced:" and fails. sudo fails and is
#     logged, except in the root cases (FAKE_SUDO_ROOT), where `sudo mkdir` and
#     `sudo mv` run as the caller, through those fenced commands, with
#     /usr/local/bin mapped into the case's directory. So a keploy.sh that
#     reaches for fixed /tmp paths, as the --oss route once did, fails here
#     without touching the host's /tmp.
#   - uname reports FAKE_UNAME_S / FAKE_UNAME_M when they are set.
# keploy.sh itself runs unmodified, SOURCED the way users run it, under bash
# and under zsh when it is installed; one case sources it from an interactive
# shell on a terminal (util-linux script(1)). After the source, the shell logs
# that it lived on, job control before and after, and the status variables
# keploy.sh left set.
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
#   - with --oss, a failed, corrupt or cut-off download, or a release with no
#     keploy in it, fails the install and installs nothing, not even the part
#     of keploy that was extracted.
#   - --oss as root puts the release's keploy in /usr/local/bin with sudo,
#     from its private temp directory; when sudo refuses, the install fails.
#   - the same holds for the Linux arm64 and the macOS (Apple Silicon)
#     release, which the uname stub drives on any Linux.
#   - an error page from the installer URL is never sourced.
#   - an unsupported architecture or OS, and Windows, fail the --oss install,
#     with -isCI too, before anything is downloaded.
#   - a failed install returns, never exits: the shell it was sourced into
#     lives on, and the temp directory is still removed. Sourced under
#     `set -eo pipefail` (a CI step's shell) the shell exits on the failure,
#     as it should, but only after the temp directory is removed; and an
#     install that succeeds still does.
#   - job control is as it was: on in an interactive shell, off in a CI step.
#   - keploy.sh leaves no status variable set in the caller's shell.
#   - no case touches a path outside its own directories, or runs sudo for
#     anything but the root cases' mkdir and mv.

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
    if [ -n "${FAKE_HTTP_404:-}" ]; then
      # As curl does with a 404: without --fail it writes the error page and
      # exits 0.
      for a in "$@"; do
        case "$a" in --fail | --fail-with-body | -[!-]*f*) exit 22 ;; esac
      done
      echo 'echo "the error page was sourced" >> "$STUB_LOG"' > "$out"
      exit 0
    fi
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
if [ -n "${FAKE_SUDO_ROOT:-}" ]; then
  case "$1" in
    mkdir | mv)
      echo "sudo-mapped: $*" >> "$STUB_LOG"
      cmd=$1
      shift
      for a in "$@"; do
        case "$a" in /usr/local/bin | /usr/local/bin/*) a="$FAKE_SUDO_ROOT$a" ;; esac
        set -- "$@" "$a"
        shift
      done
      exec "$cmd" "$@"
      ;;
  esac
fi
echo "sudo: $*" >> "$STUB_LOG"
exit 1
EOF

cat > "$stubs/uname" << 'EOF'
#!/bin/sh
case "$*" in
  -s | "") [ -z "${FAKE_UNAME_S:-}" ] || { echo "$FAKE_UNAME_S"; exit 0; } ;;
  -m) [ -z "${FAKE_UNAME_M:-}" ] || { echo "$FAKE_UNAME_M"; exit 0; } ;;
esac
PATH=/usr/bin:/bin
export PATH
exec uname "$@"
EOF
chmod +x "$stubs/curl" "$stubs/fence" "$stubs/sudo" "$stubs/uname"

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
tar czf "$tmp/no-keploy.tar.gz" -C "$release" LICENSE README.md READMEes-Es.md

# A release tarball cut off partway through its keploy, as a dropped
# connection leaves it: tar extracts the start of keploy, then fails.
cut="$tmp/release-cut"
mkdir -p "$cut"
{
  cat "$release/keploy"
  head -c 200000 /dev/urandom | base64 | sed 's/^/# /'
} > "$cut/keploy"
chmod +x "$cut/keploy"
tar czf "$tmp/cut.tar.gz" -C "$cut" keploy
head -c $(($(wc -c < "$tmp/cut.tar.gz") / 2)) "$tmp/cut.tar.gz" > "$tmp/truncated.tar.gz"
# The cut-off case means something only if tar here does leave part of
# keploy behind.
mkdir -p "$tmp/cut-probe"
if tar xzf "$tmp/truncated.tar.gz" -C "$tmp/cut-probe" 2> /dev/null || [ ! -s "$tmp/cut-probe/keploy" ]; then
  fail "fixture: extracting the cut-off tarball fails and leaves part of keploy behind"
fi

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
# case_env array. case_sudo_root lets sudo mkdir and mv run, mapped into
# $root_dir; case_home_blocked makes ~/.keploy a file, so nothing can be put
# in ~/.keploy/bin; case_pty sources keploy.sh from an interactive shell on a
# terminal; case_errexit sources it under `set -eo pipefail`, the shell of a
# GitHub Actions `shell: bash` step. Leaves $work, $out, $log, $scratch,
# $root_dir and $rc.
install() {
  local dir="$tmp/$run_shell-$1"
  shift
  work="$dir/work"
  out="$dir/out"
  log="$dir/stub.log"
  scratch="$dir/tmp"
  dir_home="$dir/home"
  root_dir="$dir/root"
  mkdir -p "$dir/home" "$work" "$scratch" "$dir/dl" "$root_dir"
  : > "$log"
  [ -z "$case_home_blocked" ] || echo "a file, not a directory" > "$dir/home/.keploy"
  [ -z "$case_sudo_root" ] || case_env+=(FAKE_SUDO_ROOT="$root_dir")
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
  local runner body=$BODY
  [ -z "$case_errexit" ] || body="set -eo pipefail; $BODY"
  if [ -n "$case_pty" ]; then
    local cmd flags=(-f)
    [ "$run_shell" = bash ] && flags=(--norc --noprofile)
    printf -v cmd '%q ' "$run_shell" "${flags[@]}" -i -c "$body" install "$src" "$@"
    runner=(timeout 120 script -qec "$cmd" /dev/null)
  else
    runner=("$run_shell" -c "$body" install "$src" "$@")
  fi
  (
    cd "$work" || exit 1
    env -i HOME="$dir/home" PATH="$stubs:/usr/bin:/bin" TMPDIR="$scratch" TERM=dumb \
      STUB_LOG="$log" FENCE="$tmp" FAKE_ENT_INSTALLER="$tmp/fake-ent-install.sh" \
      FAKE_RELEASE_TARBALL="$tmp/release.tar.gz" ${case_env[@]+"${case_env[@]}"} \
      "${runner[@]}" < /dev/null
  ) > "$out" 2>&1
  rc=$?
  case_env=()
  case_in_repo=""
  case_sudo_root=""
  case_home_blocked=""
  case_pty=""
  case_errexit=""
}
# What the inner shell runs: it sources keploy.sh the way users do, then logs
# that it lived on (an `exit` in keploy.sh would have closed it) with the
# status, job control before and after ($-, read outside any $(...): zsh drops
# m inside one), and whether keploy.sh left its status variables set. The
# single-quoted variables are the inner shell's to expand.
# shellcheck disable=SC2016
BODY='src=$1; shift; jc_before=$-; source "$src" "$@"; st=$?; jc_after=$-; echo "the shell lived on: status=$st jobctl-before=$jc_before jobctl-after=$jc_after ent_status=${ent_status-unset} oss_status=${oss_status-unset}" >> "$STUB_LOG"; exit $st'
case_env=()
case_in_repo=""
case_sudo_root=""
case_home_blocked=""
case_pty=""
case_errexit=""
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
# Nothing outside the case's directories was touched, and sudo never ran
# (but for the root cases' mapped mkdir and mv).
stayed_inside() { ! grep -qE '^(fenced|sudo):' "$log"; }
installed() { cmp -s "$release/keploy" "$dir_home/.keploy/bin/keploy"; }
installed_as_root() { cmp -s "$release/keploy" "$root_dir/usr/local/bin/keploy"; }
nothing_installed() { [ ! -e "$dir_home/.keploy/bin/keploy" ] && [ ! -e "$root_dir/usr/local/bin/keploy" ]; }
nothing_downloaded() { ! grep -q '^release-download-to ' "$log"; }
error_page_not_sourced() { ! grep -qF "the error page was sourced" "$log"; }
# The shell keploy.sh was sourced into lived on after it; after_source <name>
# is what that shell logged for <name>.
lived_on() { grep -q '^the shell lived on: ' "$log"; }
after_source() { sed -n 's/^the shell lived on: //p' "$log" | tr ' ' '\n' | sed -n "s/^$1=//p"; }
has_monitor() { case $1 in *m*) return 0 ;; esac; return 1; }
# job_control on|off: job control was on (off) before the install, and still is.
job_control() {
  lived_on || return 1
  local before after
  before=$(after_source jobctl-before)
  after=$(after_source jobctl-after)
  if [ "$1" = on ]; then
    has_monitor "$before" && has_monitor "$after"
  else
    ! has_monitor "$before" && ! has_monitor "$after"
  fi
}
no_status_left() { [ "$(after_source ent_status)" = unset ] && [ "$(after_source oss_status)" = unset ]; }
release_keploy_ran() { grep -qF "the release's keploy ran: example" "$log"; }
release_keploy_did_not_run() { ! grep -qF "the release's keploy ran" "$log"; }
fetched() { grep -qF -- "/$1" "$log"; } # <release tarball name>
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
  check "no status variable is left set in the caller's shell" no_status_left

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
  check "no status variable is left set in the caller's shell" no_status_left

  echo "== the installer URL answers with an error page, from a repo root"
  case_in_repo=1
  case_env=(FAKE_HTTP_404=1)
  install error-page -noRoot
  check "the install fails" test "$rc" -ne 0
  check "it says the download failed" said "could not download the Keploy installer"
  check "the error page is never sourced" error_page_not_sourced
  check "the repo's files are kept unchanged" callers_dir_untouched callers
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was

  echo "== the other installer refuses (an Intel Mac, a bad -v)"
  case_env=(FAKE_ENT_STATUS=1)
  install ent-refuses
  check "the install fails with it" test "$rc" -eq 1
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
  check "no status variable is left set in the caller's shell" no_status_left

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
  check "job control stays off, as it was in this non-interactive shell" job_control off
  check "no status variable is left set in the caller's shell" no_status_left

  echo "== --oss from an interactive shell on a terminal"
  if [ -n "$have_pty" ]; then
    case_pty=1
    install oss-interactive --oss -noRoot
    [ "$rc" -eq 0 ] || show_output
    check "the install succeeds" test "$rc" -eq 0
    check "the release's keploy is installed in ~/.keploy/bin" installed
    check "job control is still on afterwards" job_control on
  else
    echo "     skipped: no util-linux script(1) here to give the shell a terminal"
  fi

  echo "== --oss as root (-isCI), from a repo root"
  case_in_repo=1
  case_sudo_root=1
  install oss-root --oss -isCI
  [ "$rc" -eq 0 ] || show_output
  check "the install succeeds" test "$rc" -eq 0
  check "the release's keploy is put in /usr/local/bin with sudo" installed_as_root
  check "the download and extraction used a private temp directory, removed afterwards; the other files there are kept unchanged" used_private_dir_and_cleaned_up release-download-to keploy.tar.gz
  check "nothing outside its own directories is touched, and sudo runs only mkdir and mv" stayed_inside
  check "the repo's install.sh and keploy-enterprise.sh are kept unchanged, and nothing is added" callers_dir_untouched callers

  echo "== --oss as root (-isCI), from a repo root, and sudo refuses"
  case_in_repo=1
  install oss-root-refused --oss -isCI
  check "the install fails" test "$rc" -ne 0
  check "it says so" said "Failed to move the keploy binary"
  check "nothing is installed, and no keploy runs" eval 'nothing_installed && release_keploy_did_not_run'
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
  check "the repo's install.sh and keploy-enterprise.sh are kept unchanged, and nothing is added" callers_dir_untouched callers

  echo "== --oss, the download fails"
  case_env=(FAKE_RELEASE_DOWNLOAD_FAIL=1)
  install oss-download-fails --oss -noRoot
  check "the install fails" test "$rc" -ne 0
  check "nothing is installed, and no keploy runs" eval 'nothing_installed && release_keploy_did_not_run'
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside
  check "no status variable is left set in the caller's shell" no_status_left

  echo "== --oss, the download is not a tarball"
  case_env=(FAKE_RELEASE_TARBALL="$tmp/corrupt.tar.gz")
  install oss-corrupt --oss -noRoot
  check "the install fails" test "$rc" -ne 0
  check "nothing is installed, and no keploy runs" eval 'nothing_installed && release_keploy_did_not_run'
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside
  check "no status variable is left set in the caller's shell" no_status_left

  echo "== --oss, the download is cut off partway through keploy"
  case_env=(FAKE_RELEASE_TARBALL="$tmp/truncated.tar.gz")
  install oss-cut-off --oss -noRoot
  check "the install fails" test "$rc" -ne 0
  check "the part of keploy that was extracted is not installed, and no keploy runs" eval 'nothing_installed && release_keploy_did_not_run'
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside
  check "no status variable is left set in the caller's shell" no_status_left

  echo "== --oss, the release tarball has no keploy in it"
  case_env=(FAKE_RELEASE_TARBALL="$tmp/no-keploy.tar.gz")
  install oss-no-binary --oss -noRoot
  check "the install fails" test "$rc" -ne 0
  check "the shell it was sourced into lives on: keploy.sh returns, never exits" lived_on
  check "nothing is installed, and no keploy runs" eval 'nothing_installed && release_keploy_did_not_run'
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside
  check "no status variable is left set in the caller's shell" no_status_left

  echo "== --oss, ~/.keploy is a file, so keploy cannot be put in ~/.keploy/bin"
  case_home_blocked=1
  install oss-cannot-place --oss -noRoot
  check "the install fails" test "$rc" -ne 0
  check "the shell it was sourced into lives on: keploy.sh returns, never exits" lived_on
  check "no keploy runs" release_keploy_did_not_run
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside

  echo "== --oss on Linux arm64"
  case_env=(FAKE_UNAME_M=aarch64)
  install oss-arm64 --oss -noRoot
  [ "$rc" -eq 0 ] || show_output
  check "the install succeeds" test "$rc" -eq 0
  check "it downloads the Linux arm64 release" fetched keploy_linux_arm64.tar.gz
  check "the release's keploy is installed in ~/.keploy/bin, and keploy example runs it" eval 'installed && release_keploy_ran'
  check "the download and extraction used a private temp directory, removed afterwards; the other files there are kept unchanged" used_private_dir_and_cleaned_up release-download-to keploy.tar.gz
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside

  echo "== --oss on Linux arm64, the download is cut off partway through keploy"
  case_env=(FAKE_UNAME_M=aarch64 FAKE_RELEASE_TARBALL="$tmp/truncated.tar.gz")
  install oss-arm64-cut-off --oss -noRoot
  check "the install fails" test "$rc" -ne 0
  check "the part of keploy that was extracted is not installed, and no keploy runs" eval 'nothing_installed && release_keploy_did_not_run'
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was

  # macOS installs without root whatever the flags say, so these run without
  # -noRoot.
  echo "== --oss on macOS (Apple Silicon)"
  case_env=(FAKE_UNAME_S=Darwin FAKE_UNAME_M=arm64)
  install oss-macos --oss
  [ "$rc" -eq 0 ] || show_output
  check "the install succeeds" test "$rc" -eq 0
  check "it downloads the macOS arm64 release" fetched keploy_darwin_arm64.tar.gz
  check "the release's keploy is installed in ~/.keploy/bin, and keploy example runs it" eval 'installed && release_keploy_ran'
  check "the download and extraction used a private temp directory, removed afterwards; the other files there are kept unchanged" used_private_dir_and_cleaned_up release-download-to keploy.tar.gz
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside

  echo "== --oss on macOS, the download is cut off partway through keploy"
  case_env=(FAKE_UNAME_S=Darwin FAKE_UNAME_M=arm64 FAKE_RELEASE_TARBALL="$tmp/truncated.tar.gz")
  install oss-macos-cut-off --oss
  check "the install fails" test "$rc" -ne 0
  check "the part of keploy that was extracted is not installed, and no keploy runs" eval 'nothing_installed && release_keploy_did_not_run'
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside

  echo "== --oss on macOS, ~/.keploy is a file, so keploy cannot be put in ~/.keploy/bin"
  case_env=(FAKE_UNAME_S=Darwin FAKE_UNAME_M=arm64)
  case_home_blocked=1
  install oss-macos-cannot-place --oss
  check "the install fails" test "$rc" -ne 0
  check "the shell it was sourced into lives on: keploy.sh returns, never exits" lived_on
  check "no keploy runs" release_keploy_did_not_run
  check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
  check "nothing outside its own directories is touched, and sudo is not run" stayed_inside

  local row name what env args said_what argv
  for row in \
    "oss-riscv64|an unsupported architecture|FAKE_UNAME_M=riscv64|-noRoot|Unsupported architecture: riscv64" \
    "oss-riscv64-ci|an unsupported architecture, with -isCI|FAKE_UNAME_M=riscv64|-isCI|Unsupported architecture: riscv64" \
    "oss-windows|Windows, from Git Bash|FAKE_UNAME_S=MINGW64_NT-10.0-19045|-noRoot|no native Windows support" \
    "oss-windows-ci|Windows, from Git Bash, with -isCI|FAKE_UNAME_S=MINGW64_NT-10.0-19045|-isCI|no native Windows support" \
    "oss-windows-ci-noroot|Windows, from Git Bash, with -isCI -noRoot|FAKE_UNAME_S=MINGW64_NT-10.0-19045|-isCI -noRoot|no native Windows support" \
    "oss-freebsd|an OS it has no build for|FAKE_UNAME_S=FreeBSD|-noRoot|Unknown OS" \
    "oss-freebsd-ci|an OS it has no build for, with -isCI|FAKE_UNAME_S=FreeBSD|-isCI|Unknown OS"; do
    IFS='|' read -r name what env args said_what <<< "$row"
    read -r -a argv <<< "$args"
    echo "== --oss on $what"
    case_env=("$env")
    install "$name" --oss "${argv[@]}"
    check "the install fails" test "$rc" -ne 0
    check "it says why" said "$said_what"
    check "nothing is downloaded or installed, and no keploy runs" eval 'nothing_downloaded && nothing_installed && release_keploy_did_not_run'
    check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
    check "nothing outside its own directories is touched, and sudo is not run" stayed_inside
  done

  # A CI step's shell: GitHub Actions runs `shell: bash` as
  # `bash --noprofile --norc -eo pipefail {0}`, and GitLab runs its script
  # under `set -e` too. A failure there exits the shell as soon as keploy.sh
  # returns it, which is right, but keploy.sh must have removed its temp
  # directory by then.
  for row in \
    "errexit-ent-refuses|the other installer refuses|FAKE_ENT_STATUS=1|-noRoot" \
    "errexit-oss-download-fails|--oss, the download fails|FAKE_RELEASE_DOWNLOAD_FAIL=1|--oss -noRoot" \
    "errexit-oss-cut-off|--oss, the download is cut off partway through keploy|FAKE_RELEASE_TARBALL=$tmp/truncated.tar.gz|--oss -noRoot" \
    "errexit-oss-riscv64|--oss on an unsupported architecture|FAKE_UNAME_M=riscv64|--oss -noRoot"; do
    IFS='|' read -r name what env args <<< "$row"
    read -r -a argv <<< "$args"
    echo "== under set -eo pipefail: $what, from a repo root"
    case_in_repo=1
    case_errexit=1
    case_env=("$env")
    install "$name" "${argv[@]}"
    check "the install fails" test "$rc" -ne 0
    check "nothing is installed, and no keploy runs" eval 'nothing_installed && release_keploy_did_not_run'
    check "the private temp directory is still removed, and the temp directory is otherwise as it was" tmp_left_as_it_was
    check "the repo's install.sh and keploy-enterprise.sh are kept unchanged, and nothing is added" callers_dir_untouched callers
  done

  echo "== under set -eo pipefail: from a repo root"
  case_in_repo=1
  case_errexit=1
  install errexit-in-repo -noRoot
  [ "$rc" -eq 0 ] || show_output
  check "the install succeeds" test "$rc" -eq 0
  check "the other installer runs, with the caller's arguments" ent_run_with_args
  check "the download used a private temp directory, removed afterwards" used_private_dir_and_cleaned_up download-to install.sh
  check "the shell lives on, with no status variable left set" eval 'lived_on && no_status_left'

  echo "== under set -eo pipefail: --oss from a repo root"
  case_in_repo=1
  case_errexit=1
  install errexit-oss-in-repo --oss -noRoot
  [ "$rc" -eq 0 ] || show_output
  check "the install succeeds" test "$rc" -eq 0
  check "the release's keploy is installed in ~/.keploy/bin, and keploy example runs it" eval 'installed && release_keploy_ran'
  check "the download and extraction used a private temp directory, removed afterwards; the other files there are kept unchanged" used_private_dir_and_cleaned_up release-download-to keploy.tar.gz
  check "the shell lives on, with no status variable left set" eval 'lived_on && no_status_left'
}
ent_run_with_args() { ent_ran_with "-noRoot"; }

have_pty=""
if script -qec true /dev/null < /dev/null > /dev/null 2>&1; then
  have_pty=1
elif [ -n "${REQUIRE_PTY:-}" ]; then
  # CI sets this: the interactive case is the only one that sees job control on.
  fail "util-linux script(1) is required (REQUIRE_PTY is set) but is missing or does not work"
fi

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

#!/usr/bin/env bash
#
# keploy-installer.tests.sh — runs keploy.sh (served as https://keploy.io/install.sh)
# on its default route, the hand-off to the installer at
# https://keploy.io/ent/install.sh, and checks that it leaves the caller's
# working directory alone.
#
#   bash .github/workflows/test_workflow_scripts/keploy-installer.tests.sh
#
# KEEP_TMP=1 keeps each case's directories, output and stub log.
#
# No network and no root. Each case gets its own HOME and TMPDIR and a PATH
# that starts with a curl stub: a download of ent/install.sh writes a stand-in
# installer (it logs that it ran, from where and with what arguments, and
# returns FAKE_ENT_STATUS) wherever curl was told to write it, `-o FILE` or,
# like the real curl, ./install.sh for -O. keploy.sh itself runs unmodified,
# SOURCED the way users run it, under bash and under zsh when it is installed.
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
#
# Not covered: the --oss route, which still downloads and extracts at fixed
# /tmp paths (and removes /tmp/LICENSE and /tmp/README.md), so a run of it here
# would touch the host's /tmp.

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
prev=""
for a in "$@"; do
  case "$prev" in -o | --output) out=$a ;; esac
  case "$a" in -O | --remote-name) remote_name=1 ;; esac
  prev=$a
done
case "$*" in
  *keploy.io/ent/install.sh*)
    [ -z "${FAKE_DOWNLOAD_FAIL:-}" ] || exit 22
    [ -n "$out" ] || { [ -n "$remote_name" ] && out=./install.sh; }
    [ -n "$out" ] || exit 1
    echo "download-to $out" >> "$STUB_LOG"
    cp "$FAKE_ENT_INSTALLER" "$out"
    ;;
esac
exit 0
EOF
chmod +x "$stubs"/*

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
  mkdir -p "$dir/home" "$work" "$scratch" "$dir/dl"
  : > "$log"
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
      STUB_LOG="$log" FAKE_ENT_INSTALLER="$tmp/fake-ent-install.sh" ${case_env[@]+"${case_env[@]}"} \
      "$run_shell" -c 'src=$1; shift; source "$src" "$@"' install "$src" "$@"
  ) > "$out" 2>&1
  rc=$?
  case_env=()
  case_in_repo=""
}
case_env=()
case_in_repo=""

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
# The other installer went to a directory of its own under the case's TMPDIR,
# and that directory is gone afterwards.
used_private_dir_and_cleaned_up() {
  local to
  to=$(sed -n 's/^download-to //p' "$log")
  case "$to" in "$scratch"/*/install.sh) ;; *) return 1 ;; esac
  [ -z "$(ls -A "$scratch")" ]
}
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
  check "the download used a private temp directory, removed afterwards" used_private_dir_and_cleaned_up

  echo "== sourced as ./install.sh, the way the docs say"
  install documented -noRoot
  [ "$rc" -eq 0 ] || show_output
  check "the install succeeds" test "$rc" -eq 0
  check "the other installer runs, with the caller's arguments" ent_run_with_args
  check "the install.sh the caller downloaded is left as it was, next to their keploy-enterprise.sh" callers_dir_untouched downloaded
  check "the download used a private temp directory, removed afterwards" used_private_dir_and_cleaned_up

  echo "== the download fails, from a repo root"
  case_in_repo=1
  case_env=(FAKE_DOWNLOAD_FAIL=1)
  install download-fails -noRoot
  check "the install fails" test "$rc" -ne 0
  check "it says so" said "could not download the Keploy installer"
  check "the repo's install.sh is not sourced in its place" callers_install_not_run
  check "nothing else runs either" ent_did_not_run
  check "the repo's files are kept unchanged" callers_dir_untouched callers
  check "the private temp directory is still removed" test -z "$(ls -A "$scratch")"

  echo "== the other installer refuses (an Intel Mac, a bad -v)"
  case_env=(FAKE_ENT_STATUS=1)
  install ent-refuses
  check "the install fails with it" test "$rc" -eq 1
  check "the private temp directory is still removed" test -z "$(ls -A "$scratch")"
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

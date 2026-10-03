#!/usr/bin/env bash
# Shared `apt-get install` for CI: Ubuntu mirror failover plus bounded apt.
#
# Usage:  bash .../apt-install.sh [--all-sources] [apt-get install option ...] package ...
#
# Every other argument goes to `apt-get install` as-is. Runs
# `apt-get update` first, always: the sources may have just been rewritten, and
# an install from a runner image's stale package lists can 404 on a version the
# archive has since superseded. Uses sudo unless already root.
#
# By default the update and the install read the Ubuntu archive and nothing
# else (see UBUNTU SOURCES ONLY). --all-sources reads every configured source,
# for a caller that installs from a third-party repository; so does a
# self-hosted runner, always.
#
# WHY A MIRROR LIST, NOT A MIRROR
#
# GitHub-hosted runners live in Azure, and archive.ubuntu.com is served from
# outside it -- measured at 196 kB/s from the WSL lane, which is slow enough
# that `timeout 600` killed apt mid-download (exit 124) with no hint as to why.
# azure.archive.ubuntu.com is the in-region mirror, so that lane pointed every
# Ubuntu source at it -- and made that one host a single point of failure. On
# run 36811538424 (job 110208231113) it answered pkg-config's .deb with
# `502 Proxy Error` for ~10 s; Acquire::Retries=3 spent every attempt on that
# same host inside the window, and the job died with exit 100:
#
#   Err:61 http://azure.archive.ubuntu.com/ubuntu jammy/main amd64 pkg-config amd64 0.29.2-1ubuntu3
#     502  Proxy Error [IP: 20.106.104.242 80]
#   E: Unable to fetch some archives, maybe run apt-get update or try with --fix-missing?
#
# Retrying the same host harder is not the fix; a second host is. apt's
# built-in mirror method (apt-transport-mirror(1)) takes a list of mirrors, and
# a file that still fails on one after its Acquire::Retries is fetched from the
# next. The Ubuntu archive URIs (WHAT IS REWRITTEN) are pointed at the list
# below: azure first so the fast path is unchanged, then archive.ubuntu.com,
# then security.ubuntu.com, the same order GitHub's hosted images use. Both
# fallbacks serve the whole archive -- every suite, every pool file, -security
# included -- so one list serves every suite. The order needs the explicit
# `priority:`; without it apt picks among the mirrors at random. Plain http,
# not https: the WSL and container images may lack ca-certificates, and apt
# authenticates every file against the signed InRelease whatever the transport.
#
# The fallbacks are the slow path measured above. They carry a lane through a
# failing file or a short azure outage, like the one on that job. A long, full
# azure outage can still push a large install (WSL's ~154 MB) past
# `timeout 600`, and the step then fails with exit 124.
#
# GitHub's hosted Ubuntu images already ship such a list
# (mirror+file:/etc/apt/apt-mirrors.txt), so there this leaves the sources as
# they are. The WSL distro and stock Ubuntu containers have none.
#
# WHAT IS REWRITTEN
#
# Only http(s) .../ubuntu URIs on archive.ubuntu.com, security.ubuntu.com and
# their subdomains (azure.archive..., us.archive...), on lines that are not
# comments, in /etc/apt/sources.list and /etc/apt/sources.list.d/*.list /
# *.sources -- the one-line format (jammy) and deb822 (noble). Left alone:
# sources that already go through a mirror method (GitHub's hosted images),
# .../ubuntu-ports (on ports.ubuntu.com or not), old-releases, PPAs, and
# third-party repos such as Docker's or Microsoft's. Running it again is a
# no-op: a rewritten URI no longer matches.
#
# Nothing is rewritten on a self-hosted runner (RUNNER_ENVIRONMENT=self-hosted).
# That machine outlives the job and its apt setup belongs to its owner, who may
# not be in Azure at all. It still gets the bounded update and install, from
# every source it has: its owner's Ubuntu mirror need not be on ubuntu.com, so
# it cannot be told apart from a third-party repository.
#
# UBUNTU SOURCES ONLY
#
# Runner images carry third-party sources that an Ubuntu package never needs.
# GitHub's hosted images keep packages.microsoft.com, and that host at times
# serves a corrupt InRelease ("Clearsigned file isn't valid, got 'NOSPLIT'"),
# which makes a plain `apt-get update` exit 100. So by default apt reads only
# the entries that fetch from the Ubuntu archive: copies of them in a temporary
# directory, given to apt as Dir::Etc::SourceParts, with Dir::Etc::SourceList
# set to /dev/null. An entry counts when every URI it names, over http or
# https, is .../ubuntu on archive.ubuntu.com, security.ubuntu.com or a
# subdomain of either (azure.archive..., us.archive...), .../ubuntu-ports on
# ports.ubuntu.com or a subdomain of it, or .../ubuntu on
# old-releases.ubuntu.com; or a mirror+file: list whose entries are all of
# those (this script's list, or the hosted images'). PPAs and every other host
# are third-party. No file under /etc/apt changes for this. The script names
# the source files it left entries out of.
#
# The install reads the same entries as the update. The third-party lists were
# not refreshed, so a version picked from them could 404 like any stale list.
# APT::Get::List-Cleanup=0 keeps the update from deleting the third-party
# lists, so a later `apt-get install` from every source still finds them. It
# also leaves behind the lists of sources this script rewrote, which nothing
# reads. If no Ubuntu entry is found, the script stops and names --all-sources
# rather than run apt on nothing.
#
# Tests: apt-install.tests.sh, run by linux-ci-scripts.yml.

set -euo pipefail
shopt -s nullglob

all_sources=no
if [ "${1:-}" = --all-sources ]; then
  all_sources=yes
  shift
fi
if [ "$#" -eq 0 ]; then
  echo "usage: $0 [--all-sources] [apt-get install option ...] package ..." >&2
  exit 2
fi

if [ "$(id -u)" -eq 0 ]; then
  as_root() { "$@"; }
else
  as_root() { sudo "$@"; }
fi

mirror_list=/etc/apt/ubuntu-mirrors.txt
# Groups: 1 = what precedes the URI, 4 = what follows it. What precedes it is
# the line start, whitespace, the `:` of a deb822 field, or the `]` that ends a
# one-line entry's options: apt reads `deb [arch=amd64 ]http://...` too. The
# trailing `/?([[:space:]]|$)` keeps .../ubuntu-ports (ports.ubuntu.com's path)
# out.
ubuntu_uri_re='(^|[][:space:]:])https?://([A-Za-z0-9-]+\.)*(archive|security)\.ubuntu\.com/ubuntu/?([[:space:]]|$)'

list_tmp="$(mktemp)"
src_tmp="$(mktemp)"
parts_dir="$(mktemp -d)"
trap 'rm -rf "$list_tmp" "$src_tmp" "$parts_dir"' EXIT

use_mirror_list() {
  local src
  cat >"$list_tmp" <<'EOF'
# Written by keploy's .github/workflows/test_workflow_scripts/apt-install.sh.
# apt tries the mirror with the lowest priority: number first and moves to the
# next when a fetch fails. See that script for why azure is first.
http://azure.archive.ubuntu.com/ubuntu/	priority:1
http://archive.ubuntu.com/ubuntu/	priority:2
http://security.ubuntu.com/ubuntu/	priority:3
EOF

  for src in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
    [ -f "$src" ] || continue
    # Loop per line (`t again`) rather than /g: a match consumes the whitespace
    # after its URI, which a second URI on the same deb822 `URIs:` line needs as
    # its own left boundary. Read as root too: apt does, so a source file need
    # not be readable by anyone else.
    as_root sed -E \
      -e '/^[[:space:]]*#/b' \
      -e ':again' \
      -e "s#${ubuntu_uri_re}#\\1mirror+file:${mirror_list}\\4#" \
      -e 't again' \
      "$src" >"$src_tmp"
    # The list is written when a source file mentions it -- so not on GitHub's
    # hosted images, which keep their own -- and before any source names it.
    # Checked on every run, so a deleted or outdated list is put back.
    if grep -qF "mirror+file:${mirror_list}" "$src_tmp"; then
      cmp -s "$list_tmp" "$mirror_list" || as_root install -m 0644 "$list_tmp" "$mirror_list"
    fi
    as_root cmp -s "$src" "$src_tmp" && continue
    # cp onto the existing file keeps its owner and mode.
    as_root cp "$src_tmp" "$src"
    echo "apt-install: $src now fetches the Ubuntu archive via mirror+file:$mirror_list"
  done
}

# Prints the entries of one source file that fetch from the Ubuntu archive and
# from nothing else; fmt=list for the one-line format, fmt=sources for deb822.
# Exits 3 when it left an entry out. Runs as root, so it can read root-only
# source files and mirror lists.
# shellcheck disable=SC2016 # awk, not shell, expands these $ fields
ubuntu_entries_awk='
function archive(uri) {
  return uri ~ /^https?:\/\/(([A-Za-z0-9-]+\.)*(archive|security)\.ubuntu\.com\/ubuntu|([A-Za-z0-9-]+\.)*ports\.ubuntu\.com\/ubuntu-ports|old-releases\.ubuntu\.com\/ubuntu)\/?$/
}
# A mirror+file: list counts when it names at least one mirror and every one
# is the archive.
function ubuntu_uri(uri,    list, line, f, ok) {
  if (archive(uri)) return 1
  if (uri !~ /^mirror\+file:\//) return 0
  list = substr(uri, length("mirror+file:") + 1)
  ok = 0
  while ((getline line < list) > 0) {
    sub(/\r+$/, "", line); sub(/^[ \t]+/, "", line)
    if (line == "" || line ~ /^#/) continue
    split(line, f, /[ \t]+/)
    if (!archive(f[1])) { ok = 0; break }
    ok = 1
  }
  close(list)
  return ok
}
# One deb822 stanza, collected in stanza[1..ns]. A stanza apt reads as
# disabled is no entry; one with fields but no URIs is left out, and named.
# The Enabled values are those StringToBool in apt reads as false; a rarer
# spelling only makes a disabled stanza count as an entry, which apt skips.
function flush(    i, uris, in_uris, has_uris, has_fields, n, u, keep) {
  uris = ""; in_uris = 0; has_uris = 0; has_fields = 0
  for (i = 1; i <= ns; i++) {
    if (stanza[i] ~ /^#/) continue
    if (stanza[i] ~ /^[ \t]/) { if (in_uris) uris = uris " " stanza[i]; continue }
    has_fields = 1
    if (tolower(stanza[i]) ~ /^enabled[ \t]*:[ \t]*(no|false|without|off|disable|[+-]?0+|[+-]?0x0+)[ \t]*$/) { ns = 0; return }
    in_uris = (tolower(stanza[i]) ~ /^uris[ \t]*:/)
    if (in_uris) { has_uris = 1; uris = uris " " substr(stanza[i], index(stanza[i], ":") + 1) }
  }
  if (has_uris) {
    n = split(uris, u, /[ \t]+/); keep = 0
    for (i = 1; i <= n; i++) {
      if (u[i] == "") continue
      if (!ubuntu_uri(u[i])) { keep = 0; break }
      keep = 1
    }
    if (keep) {
      if (kept++) print ""
      for (i = 1; i <= ns; i++) print stanza[i]
    } else dropped = 1
  } else if (has_fields) dropped = 1
  ns = 0
}
fmt == "list" {
  line = $0
  sub(/^[ \t]+/, "", line)
  if (line !~ /^deb(-src)?[ \t]/) next
  sub(/^deb(-src)?[ \t]+/, "", line)
  if (line ~ /^\[/) sub(/^\[[^]]*\][ \t]*/, "", line)
  split(line, f, /[ \t]+/)
  if (ubuntu_uri(f[1])) print $0; else dropped = 1
}
# Stanzas end at an empty line, as in apt, which also reads CRLF files: the
# CRs are dropped before that test, so a CRLF file splits where apt splits it.
fmt == "sources" {
  sub(/\r+$/, "")
  if ($0 == "") flush(); else stanza[++ns] = $0
}
END {
  if (fmt == "sources") flush()
  if (dropped) exit 3
}'

# Fills $parts_dir with the Ubuntu entries, one file per source file that has
# any, named so apt parses each in its own format.
ubuntu_parts() {
  local src fmt n=0 part rc left_out=()
  for src in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
    [ -f "$src" ] || continue
    fmt=list
    case "$src" in *.sources) fmt=sources ;; esac
    part="$parts_dir/$(printf %03d "$n")-${src##*/}"
    rc=0
    as_root awk -v fmt="$fmt" "$ubuntu_entries_awk" "$src" >"$part" || rc=$?
    case "$rc" in
      0) ;;
      3) left_out+=("$src") ;;
      *) echo "apt-install: could not filter $src (exit $rc)" >&2; exit "$rc" ;;
    esac
    if [ -s "$part" ]; then n=$((n + 1)); else rm -f "$part"; fi
  done
  if [ "$n" -eq 0 ]; then
    echo "apt-install: no apt source under /etc/apt fetches from the Ubuntu archive;" \
      "pass --all-sources to install from the sources this machine has" >&2
    exit 1
  fi
  if [ "${#left_out[@]}" -gt 0 ]; then
    echo "apt-install: apt reads only the Ubuntu archive entries;" \
      "pass --all-sources to also read the rest of: ${left_out[*]}"
  fi
}

if [ "${RUNNER_ENVIRONMENT:-}" = self-hosted ]; then
  echo "apt-install: self-hosted runner, leaving its apt sources as they are and reading all of them"
  all_sources=yes
else
  use_mirror_list
fi

apt_opts=(
  -o Acquire::Retries=3
  -o Acquire::http::Timeout=30
  -o Acquire::https::Timeout=30
  -o DPkg::Lock::Timeout=120
)
if [ "$all_sources" = no ]; then
  ubuntu_parts
  apt_opts+=(
    -o Dir::Etc::SourceList=/dev/null
    -o "Dir::Etc::SourceParts=$parts_dir"
    -o APT::Get::List-Cleanup=0
  )
fi
apt_opts+=(-y)
as_root env DEBIAN_FRONTEND=noninteractive timeout 600 apt-get "${apt_opts[@]}" update
as_root env DEBIAN_FRONTEND=noninteractive timeout 600 apt-get "${apt_opts[@]}" install "$@"

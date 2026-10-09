#!/usr/bin/env bash
# Shared `apt-get install` for CI: Ubuntu mirror failover plus bounded apt.
#
# Usage:  bash .../apt-install.sh [--all-sources] [apt-get install option ...] package ...
#
# Every other argument goes to `apt-get install` as-is, twice: once to download
# the packages and once to install them (see A MIRROR THAT IS UP BUT SLOW). Runs
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
# Neither host is the fast one for good. archive.ubuntu.com was at 196 kB/s
# when that was measured; on job 113027730238 it delivered 154 MB in 14 s
# while azure refused every file. The order says where apt starts, not which
# mirror is quicker on the day.
#
# GitHub's hosted Ubuntu images already ship such a list
# (mirror+file:/etc/apt/apt-mirrors.txt), so there this leaves the sources as
# they are. The WSL distro and stock Ubuntu containers have none.
#
# A MIRROR THAT IS UP BUT SLOW
#
# apt leaves a mirror when a fetch fails, or when the connection is silent for
# Acquire::http::Timeout. A mirror that answers and then trickles does neither,
# so apt stays on it to the end. On 2026-10-07 azure served the package lists
# at 2-9 MB/s and the .debs at 60-185 kB/s to runners in some regions. The WSL
# lane then needed 154 MB, the single `timeout 600` around the install ran out,
# and five jobs ended like 113004700461, without one line from
# archive.ubuntu.com, which was never tried:
#
#   Get:42 http://azure.archive.ubuntu.com/ubuntu jammy-updates/main amd64 golang-1.18-go amd64 1.18.1-1ubuntu1.3 [66.1 MB]
#   ##[error]Process completed with exit code 124.
#
# So the script does what apt does not, and spends the time limit of the
# download in slices. `apt-get install --download-only` runs for
# APT_INSTALL_SLICE_SECONDS (150) at a time within APT_INSTALL_BUDGET_SECONDS
# (600). When a slice runs out, apt is stopped, and the script works out
# whether what is still to fetch would arrive in the time left at the rate of
# that slice. If it would not, the next mirror is put in front before apt is
# started again. A mirror that is on course stays in front, so a download that
# is merely large is not handed to a mirror that may be slower.
#
# Moving on rewrites the mirror list with the next mirror first and the one
# that was in front last; after the last mirror the first is in front again.
# Stopping apt loses nothing: a finished .deb stays in apt's archive cache, and
# the one that was cut short is resumed where it stopped, by another mirror
# too. Only when every .deb is there does `apt-get install --no-download`
# install them. That run fetches nothing and has the same budget again, in one
# piece, so dpkg no longer gets only the time a slow download left over.
#
# `apt-get update` is not sliced: it has the budget in one piece, as it always
# had. None of the five jobs above failed in it: the same mirror served them
# the 50.7 MB of package lists in 5 to 22 s, and of the jobs of that day whose
# logs were read, the slowest update took 28 s (job 113006293757). apt also
# does not say how much of the lists is still to come, so there would be no
# rate to judge a mirror by, only a guess at how long an update may take.
#
# The lists reordered are the ones an entry that apt reads here names, with two
# or more mirrors that are all the Ubuntu archive: this script's own, or the
# one a GitHub-hosted image ships. Each is put back as it was when the script
# exits. A run that is killed outright leaves the order it had reached, which
# is still a correct list, and the next run rewrites this script's own. Without
# such a list (a self-hosted runner, whose files are never touched;
# ubuntu-ports; old-releases) there is no other mirror to put in front, and the
# download gets its budget in one piece, as before.
#
# If every mirror is slow the budget still runs out. The script then exits 124,
# as it used to, and its last line is an ::error:: that says how much was
# fetched, at what rate, and which mirror was in front for each slice. An
# update or an install that outlasts its budget ends the same way, with an
# ::error:: that says which of the two it was. The two variables above exist
# for the tests, which cannot wait ten minutes.
#
# A SCRIPT THAT IS STOPPED ITSELF
#
# On SIGTERM or SIGHUP the script puts the mirror lists back and exits at once
# (143, 129). The apt it had started is not stopped with it. apt runs in the
# foreground, where the shell has no handle on it, and goes on to the end of
# its slice or budget, as the single `timeout 600 apt-get` did before there
# were slices. Until then it holds the dpkg lock, for which a next run of this
# script waits 120 s (DPkg::Lock::Timeout). A SIGINT sent to the script alone
# stops nothing: bash turns to it when apt has ended, finds that apt was not
# interrupted, and goes on.
#
# Stopping apt from here would take running it in the background. A shell
# without job control gives a background command an ignored SIGINT and SIGQUIT,
# which apt, dpkg and every maintainer script would inherit, and the kill would
# have to go through sudo from a trap. That changes what every package
# installation runs under, for the sake of a job that is being given up.
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
# A slice is longer than the 120 s apt waits for the dpkg lock
# (DPkg::Lock::Timeout below), which the download takes too: an apt that only
# waited for the lock then ends with its own error, as it always has, and not
# with the slice, which would read as a slow mirror.
slice="${APT_INSTALL_SLICE_SECONDS:-150}"
budget="${APT_INSTALL_BUDGET_SECONDS:-600}"
for seconds in "$slice" "$budget"; do
  case "$seconds" in
    '' | 0* | *[!0-9]*)
      echo "apt-install: APT_INSTALL_SLICE_SECONDS and APT_INSTALL_BUDGET_SECONDS are whole numbers of seconds, 1 or more" >&2
      exit 2
      ;;
  esac
done

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
# Holds each mirror list in $lists as it was found, under its index there.
keep_dir="$(mktemp -d)"
# The mirror lists a slow slice may reorder, and how many times the mirror in
# front has been sent to the back since they were found.
lists=()
turns=0

restore_mirror_lists() {
  local i
  [ "$turns" -gt 0 ] || return 0
  for i in "${!lists[@]}"; do
    as_root cp "$keep_dir/$i" "${lists[$i]}" ||
      echo "apt-install: could not put ${lists[$i]} back in its own order" >&2
  done
}
trap 'restore_mirror_lists; rm -rf "$list_tmp" "$src_tmp" "$parts_dir" "$keep_dir"' EXIT

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

# Whether a URI is the Ubuntu archive. Shared by the two awk programs below.
archive_awk='
function archive(uri) {
  return uri ~ /^https?:\/\/(([A-Za-z0-9-]+\.)*(archive|security)\.ubuntu\.com\/ubuntu|([A-Za-z0-9-]+\.)*ports\.ubuntu\.com\/ubuntu-ports|old-releases\.ubuntu\.com\/ubuntu)\/?$/
}'

# Prints the entries of one source file that fetch from the Ubuntu archive and
# from nothing else; fmt=list for the one-line format, fmt=sources for deb822.
# Exits 3 when it left an entry out. Runs as root, so it can read root-only
# source files and mirror lists.
# shellcheck disable=SC2016 # awk, not shell, expands these $ fields
ubuntu_entries_awk="$archive_awk"'
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

# Prints one mirror list with its mirrors `turn` places further on: the mirror
# that many places down is first, and those before it follow the last. With
# front=1, prints only the mirror that is then first. Comment lines stay on
# top, and the priority: numbers are written anew, since they are what orders
# the mirrors for apt. Exits 3 without printing for a list that is not to be
# reordered: fewer than two mirrors, or one that is not the Ubuntu archive.
# shellcheck disable=SC2016 # awk, not shell, expands these $ fields
turn_awk="$archive_awk"'
{
  line = $0
  sub(/\r+$/, "", line); sub(/^[ \t]+/, "", line)
  if (line == "" || line ~ /^#/) { note[++notes] = $0; next }
  split(line, f, /[ \t]+/)
  n++
  uri[n] = f[1]; rank[n] = 1e9; rest[n] = ""
  for (i = 2; i in f; i++) {
    if (f[i] ~ /^priority:[0-9]+$/) rank[n] = substr(f[i], 10) + 0
    else if (f[i] != "") rest[n] = rest[n] "\t" f[i]
  }
  if (!archive(uri[n])) other = 1
}
END {
  if (other || n < 2) exit 3
  # Lowest priority: number first, as apt tries them; equal ones as listed.
  for (i = 1; i <= n; i++) {
    at[i] = i
    for (j = i; j > 1 && rank[at[j - 1]] > rank[at[j]]; j--) {
      k = at[j]; at[j] = at[j - 1]; at[j - 1] = k
    }
  }
  if (front) { print uri[at[turn % n + 1]]; exit }
  for (i = 1; i <= notes; i++) print note[i]
  for (i = 1; i <= n; i++) {
    k = at[(i - 1 + turn) % n + 1]
    print uri[k] "\tpriority:" i rest[k]
  }
}'

# Prints the path of every mirror+file: list that a source file names outside
# a comment. Matched inside a field, since `]mirror+file:` and
# `URIs:mirror+file:` are one field each.
# shellcheck disable=SC2016 # awk, not shell, expands these $ fields
named_lists_awk='!/^[ \t]*#/ {
  for (i = 1; i <= NF; i++)
    if (match($i, /mirror\+file:\//)) print substr($i, RSTART + length("mirror+file:"))
}'

# Fills $lists with the mirror lists that the entries apt reads here name and
# that can be reordered, and keeps a copy of each.
find_mirror_lists() {
  local srcs=() src list
  if [ "$all_sources" = no ]; then
    srcs=("$parts_dir"/*)
  else
    for src in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
      if [ -f "$src" ]; then srcs+=("$src"); fi
    done
  fi
  [ "${#srcs[@]}" -gt 0 ] || return 0
  while IFS= read -r list; do
    # Turn 0 reorders nothing: it only tells whether the list can be.
    as_root awk -v turn=0 "$turn_awk" "$list" >/dev/null 2>&1 || continue
    as_root cat "$list" >"$keep_dir/${#lists[@]}"
    lists+=("$list")
  done < <(as_root awk "$named_lists_awk" "${srcs[@]}" | sort -u)
}

# The mirror apt tries first now; nothing when there is no list to reorder.
# With more than one list, the first speaks for all.
front_mirror() {
  [ "${#lists[@]}" -gt 0 ] || return 0
  awk -v turn="$turns" -v front=1 "$turn_awk" "$keep_dir/0"
}

# Puts the next mirror in front, in every list.
next_mirror() {
  local i
  turns=$((turns + 1))
  for i in "${!lists[@]}"; do
    awk -v turn="$turns" "$turn_awk" "$keep_dir/$i" >"$list_tmp"
    # cp onto the existing file keeps its owner and mode.
    as_root cp "$list_tmp" "${lists[$i]}"
  done
}

# The bytes in the files under a directory: for apt's archive cache, the .debs
# it has finished and the one it was stopped in. %.0f, because mawk prints a
# large sum as 2.5e+09 otherwise.
bytes_under() {
  { as_root find "$1" -type f -printf '%s\n' 2>/dev/null || true; } |
    awk '{ sum += $1 } END { printf "%.0f\n", sum }'
}

# 19800000 -> 19.8 MB, in the units apt prints.
human() {
  awk -v bytes="$1" 'BEGIN {
    if (bytes >= 1e6) printf "%.1f MB", bytes / 1e6
    else if (bytes >= 1e3) printf "%.0f kB", bytes / 1e3
    else printf "%d B", bytes
  }'
}

# apt_for <seconds> <apt-get argument ...>: apt-get, stopped after that long
# (exit 124). Under a second counts as stopped already: to timeout(1), 0 means
# no limit at all.
#
# apt is stopped wherever it is, and that can be in the middle of a line: it
# prints "Reading package lists..." and "Building dependency tree..." when it
# begins each, and the line end when it is done. Whatever is written next would
# go on with that line, and an ::error:: that does not start its line is no
# annotation to GitHub. So the line of an apt that was stopped is ended here,
# for every caller. On stdout, where apt's line is: with stderr in the same
# stream the next message then starts a line of its own, and a stderr that goes
# elsewhere has no such line to end. After an apt that had ended its last line
# this is an empty line.
apt_for() {
  local seconds="$1" rc=0
  shift
  [ "$seconds" -ge 1 ] || return 124
  as_root env DEBIAN_FRONTEND=noninteractive timeout "$seconds" apt-get "${apt_opts[@]}" "$@" || rc=$?
  if [ "$rc" -eq 124 ]; then echo; fi
  return "$rc"
}

# download_in_slices <apt-get install argument ...>
# Downloads the packages within $budget, $slice at a time while there is a
# mirror list to reorder; see A MIRROR THAT IS UP BUT SLOW. Returns apt-get's
# own exit code, and exits 124 when the budget runs out.
download_in_slices() {
  local began=$SECONDS had now got=0 all=0 took left run rc front next to_fetch still said slices=""
  had="$(bytes_under "$archives")"
  while :; do
    left=$((began + budget - SECONDS))
    # Out of time, or apt was stopped and there is no other mirror to put in
    # front. Looked at here, right before apt runs, and not only after a
    # slice: finding out what is still to fetch takes time as well.
    if [ "$left" -le 0 ] || { [ -n "$slices" ] && [ "${#lists[@]}" -eq 0 ]; }; then
      said="the packages were not downloaded within $budget s, so none was installed (exit 124)."
      said+=" Fetched $(human "$all") in that time ($(human $((all / budget)))/s)."
      if [ "${#lists[@]}" -gt 0 ]; then
        said+=" Mirror in front, slice by slice: $slices."
      else
        said+=" $no_other_mirror"
      fi
      echo "::error::apt-install.sh: $said" >&2
      exit 124
    fi
    run=$left
    if [ "${#lists[@]}" -gt 0 ] && [ "$slice" -lt "$run" ]; then run=$slice; fi
    took=$SECONDS
    rc=0
    apt_for "$run" install --download-only "$@" || rc=$?
    # Anything but the time limit is apt's own answer: a file that failed on
    # every mirror, a package that does not exist. Another mirror order would
    # not change it.
    [ "$rc" -eq 124 ] || return "$rc"
    took=$((SECONDS - took))
    if [ "$took" -lt 1 ]; then took=1; fi
    front="$(front_mirror)"
    # The next slice is measured from what the cache holds now, which can be
    # less than it held: apt begins a .deb anew when the copy it is offered is
    # not the one it has part of. Measured from the old, higher count, the
    # slices after that would seem to fetch nothing, and a fast mirror would
    # be left for a slow one.
    now="$(bytes_under "$archives")"
    got=$((now - had))
    if [ "$got" -lt 0 ]; then got=0; fi
    had=$now
    all=$((all + got))
    slices+="${slices:+; }$front $(human "$got") in $took s ($(human $((got / took)))/s)"
    # That was the last slice: the top of the loop says so.
    if [ $((began + budget - SECONDS)) -le 0 ] || [ "${#lists[@]}" -eq 0 ]; then continue; fi

    # What apt would still fetch, less the part it has of the .deb it was
    # stopped in. --print-uris only lists it: nothing is downloaded. If apt
    # cannot say, the mirror is taken to be too slow.
    still="what is still to fetch"
    to_fetch="$(apt_for 120 install --download-only "$@" --print-uris -qq |
      awk -v quote="'" 'index($0, quote) == 1 { sum += $3 } END { printf "%.0f\n", sum }')" || to_fetch=""
    # The time left is read now, and not before apt was asked: its answer took
    # some of it, and may have taken all. Then no mirror is put in front for a
    # slice that cannot start, and the top of the loop says the budget is spent.
    left=$((began + budget - SECONDS))
    if [ "$left" -le 0 ]; then continue; fi
    if [ -n "$to_fetch" ]; then
      to_fetch=$((to_fetch - $(bytes_under "$archives/partial")))
      if [ "$to_fetch" -lt 0 ]; then to_fetch=0; fi
      still="the $(human "$to_fetch") still to fetch"
      if [ $((got * left / took)) -ge "$to_fetch" ]; then
        echo "apt-install: $took s with $front in front fetched $(human "$got") ($(human $((got / took)))/s)." \
          "At that rate $still arrive within the $left s left, so it stays in front."
        continue
      fi
    fi
    next_mirror
    next="$(front_mirror)"
    echo "apt-install: $took s with $front in front fetched $(human "$got") ($(human $((got / took)))/s)," \
      "too slow for $still in the $left s left." \
      "Putting $next in front; apt keeps what it has fetched."
  done
}

# What the ::error:: line says when the budget ran out with no mirror list to
# reorder.
no_other_mirror="apt reads no mirror list with a second Ubuntu mirror here, so there was no other mirror to try."
if [ "${RUNNER_ENVIRONMENT:-}" = self-hosted ]; then
  echo "apt-install: self-hosted runner, leaving its apt sources as they are and reading all of them"
  all_sources=yes
  no_other_mirror="This is a self-hosted runner, whose mirror lists the script does not reorder, so no other mirror was tried."
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
if [ "${RUNNER_ENVIRONMENT:-}" != self-hosted ]; then
  find_mirror_lists
fi

# Where apt keeps the .debs it downloads: a finished one in the directory, the
# one it is fetching under partial/.
archives=/var/cache/apt/archives
eval "$(apt-config shell archives Dir::Cache::Archives/d)"

# Not sliced; see A MIRROR THAT IS UP BUT SLOW for why.
apt_for "$budget" update || {
  rc=$?
  if [ "$rc" -eq 124 ]; then
    echo "::error::apt-install.sh: apt-get update was not done within $budget s (exit 124)." \
      "No other mirror was put in front for it: the script does that while it downloads packages, not for the package lists." >&2
  fi
  exit "$rc"
}
download_in_slices "$@"
# Every .deb is in the cache now, so this fetches nothing and cannot be slow
# for a mirror's sake. --no-download holds it to that.
apt_for "$budget" install --no-download "$@" || {
  rc=$?
  if [ "$rc" -eq 124 ]; then
    echo "::error::apt-install.sh: the packages were downloaded, but installing them was not done within $budget s (exit 124)." >&2
  fi
  exit "$rc"
}

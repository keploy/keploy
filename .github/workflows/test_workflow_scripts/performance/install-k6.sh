#!/bin/bash
# install-k6.sh: puts k6 PERF_K6_VERSION on the job's PATH, from its
# linux-amd64 release archive, which must have the sha256 PERF_K6_SHA256 (the
# digest the release's k6-v<version>-checksums.txt gives it).
#
# The archive is kept in PERF_K6_CACHE (default ~/.cache/perf-k6), which the
# performance workflow restores from and saves to the Actions cache under the
# version and the digest, so it is downloaded from the release only when that
# directory has no copy. The digest is checked on every run, on a cached copy
# as on a downloaded one, and k6 is never run from an archive that fails it:
# the step fails, naming both digests, and the archive is removed so that it
# is not saved.
#
#   PERF_K6_VERSION=2.4.0 PERF_K6_SHA256=<sha256> GITHUB_PATH=<file> \
#     RUNNER_TEMP=<dir> bash install-k6.sh
set -euo pipefail
version=${PERF_K6_VERSION:?PERF_K6_VERSION is not set}
want=${PERF_K6_SHA256:?PERF_K6_SHA256 is not set}
cache=${PERF_K6_CACHE:-$HOME/.cache/perf-k6}
: "${GITHUB_PATH:?GITHUB_PATH is not set}" "${RUNNER_TEMP:?RUNNER_TEMP is not set}"

name="k6-v$version-linux-amd64"
archive="$cache/$name.tar.gz"
if [ -f "$archive" ]; then
  echo "k6 $version: using the cached release archive"
else
  mkdir -p "$cache"
  echo "k6 $version: no cached copy; downloading its release archive"
  curl -fsSL -o "$archive.part" "https://github.com/grafana/k6/releases/download/v$version/$name.tar.gz"
  mv "$archive.part" "$archive"
fi

got=$(sha256sum <"$archive" | cut -c1-64)
if [ "$got" != "$want" ]; then
  rm -f "$archive"
  echo "::error title=k6 archive digest::the k6 $version release archive has sha256 $got, not $want (PERF_K6_SHA256); not running it."
  exit 1
fi

bin="$RUNNER_TEMP/perf-k6"
rm -rf "$bin"
mkdir -p "$bin"
tar -xzf "$archive" -C "$bin" --strip-components=1 "$name/k6"
echo "$bin" >>"$GITHUB_PATH"
"$bin/k6" version

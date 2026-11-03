#!/usr/bin/env bash
#
# Runs, on a Mac, every test the given test files declare, in the package they
# belong to, and fails unless each one of them ran and passed.
#
#   go-test-darwin-rows.sh <package dir> <test file>...
#
# For a package whose whole run cannot be the check on macOS. pkg/agent/proxy
# and pkg/agent/routes each hold a test that does not end on macOS (a DNS
# server that cannot bind, a bind to a port still held), so go_macos.yaml runs
# their macOS files' rows instead of the package.
#
# The rows are read from the files, so a test added to one runs without a
# workflow edit. A row that does not run is no proof: a `-run` pattern that
# matches nothing, a test that skips, or a name the pattern misses still
# passes `go test`. So every name read must come back `--- PASS`.
#
# bash 3.2 (macOS's /bin/bash): no mapfile, no associative arrays.
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: $0 <package dir> <test file>..." >&2
  exit 2
fi
pkg="$1"
shift

names=$(sed -n 's/^func \(Test[A-Za-z0-9_]*\)(t \*testing\.T).*/\1/p' "$@" | sort -u)
if [ -z "$names" ]; then
  echo "::error::no tests declared in $*" >&2
  exit 1
fi
pattern="^($(printf '%s\n' "$names" | paste -sd '|' -))\$"

log=$(mktemp)
trap 'rm -f "$log"' EXIT

go test -count=1 -v -run "$pattern" "./$pkg/" 2>&1 | tee "$log"

missing=0
for name in $names; do
  if ! grep -q "^--- PASS: $name " "$log"; then
    echo "::error::$pkg: $name did not run and pass on macOS" >&2
    missing=1
  fi
done
exit "$missing"

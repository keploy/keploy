#!/usr/bin/env bash
# fuzz-targets.sh: prints every Go fuzz target in the module as the JSON
# matrix fuzz-nightly.yml runs, one entry per target:
#
#   [{"pkg":"go.keploy.io/server/v3/pkg/...","dir":"pkg/...","name":"FuzzX"}, ...]
#
# Run from the repository root: bash .github/workflows/test_workflow_scripts/fuzz-targets.sh
#
# Tested by fuzz-targets.tests.sh (run by linux-ci-scripts.yml).
#
# The targets come from `go test -list '^Fuzz' ./...`, not from a grep of the
# sources: go test applies the build tags of the machine it runs on and knows
# which package each file belongs to, so a target behind //go:build linux is
# found on a Linux runner and a renamed target is never stale. It prints a
# package's matching names, then that package's `ok` line. A package that
# fails to build fails the listing, and so does a listing with no target in
# it: either means discovery is broken, not that there is nothing to fuzz.
set -u

module=$(awk '$1 == "module" { print $2; exit }' go.mod)
if ! out=$(go test -list '^Fuzz' ./... 2>&1); then
    printf '%s\n' "$out" >&2
    echo "::error::go test -list failed (above), so the fuzz targets are unknown" >&2
    exit 1
fi

json=$(printf '%s\n' "$out" | awk -v module="$module" '
    /^Fuzz[A-Za-z0-9_]*$/ { names[++n] = $0; next }
    $1 == "ok" && NF >= 2 {
        dir = $2
        if (index(dir, module "/") == 1) dir = substr(dir, length(module) + 2)
        else if (dir == module) dir = "."
        for (i = 1; i <= n; i++)
            printf "%s{\"pkg\":\"%s\",\"dir\":\"%s\",\"name\":\"%s\"}", (count++ ? "," : ""), $2, dir, names[i]
        n = 0
    }
    END { if (count) printf "\n" }')

if [ -z "$json" ]; then
    printf '%s\n' "$out" >&2
    echo "::error::go test -list found no Fuzz targets (its output is above); discovery is broken" >&2
    exit 1
fi
printf '[%s]\n' "$json"

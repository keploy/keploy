#!/bin/bash
set -euo pipefail
# -------------------------------
# Allowlisted deprecated deps
# (kept for legacy / reference)
# -------------------------------
ALLOWLIST=(
  "go.mongodb.org/mongo-driver"
)

# Extract direct dependencies from go.mod
direct_deps=$(go mod edit -json | jq -r '.Require[] | select(.Indirect == null) | .Path')

# List all modules with their update / deprecation status.
#
# `go list -m -u all` contacts the module proxy for EVERY module in the graph to
# learn about updates and retractions, so it fails whenever any single one of
# those reads does — a proxy stream error, a sumdb verification read, a slow
# mirror. Under `set -e` that failed a pull request for an upstream hiccup, and
# because it is a different module that flakes each time, re-running looked like
# luck rather than a fix.
#
# A network error is not evidence of a deprecated dependency, so it must not be
# reported as one. Retry a few times for the genuinely transient case, and if the
# listing still cannot be produced, say so LOUDLY and exit successfully: this
# check's subject is the dependency list, and it has no opinion to offer when it
# could not read one. It never reports "no deprecated dependencies" in that case,
# so a real deprecation is postponed to the next run, not hidden.
output=""
for attempt in 1 2 3; do
    if output=$(go list -m -u all 2>/tmp/deprecated-deps-err.txt); then
        break
    fi
    output=""
    if [ "$attempt" -lt 3 ]; then
        echo "go list -m -u all failed (attempt ${attempt}/3); retrying in $((attempt * 5))s"
        sed -n '1,5p' /tmp/deprecated-deps-err.txt || true
        sleep "$((attempt * 5))"
    fi
done

if [ -z "$output" ]; then
    echo "::warning::Could not read the module list after 3 attempts, so the deprecated-dependency check DID NOT RUN."
    echo "The failure was in reaching the module proxy, not in the dependency graph:"
    sed -n '1,10p' /tmp/deprecated-deps-err.txt || true
    exit 0
fi

found_deprecated=false

while IFS= read -r line; do
    mod_path=$(echo "$line" | awk '{print $1}')

    # Skip allowlisted modules
    for allowed in "${ALLOWLIST[@]}"; do
        if [[ "$mod_path" == "$allowed" ]]; then
            continue 2
        fi
    done

    # Check only direct dependencies
    if echo "$direct_deps" | grep -qx "$mod_path"; then
        if [[ "$line" == *"deprecated"* || "$line" == *"retracted"* ]]; then
            echo "Deprecated/retracted direct dependency found: $line"
            found_deprecated=true
        fi
    fi
done <<< "$output"

if [ "$found_deprecated" = true ]; then
    echo "Exiting with failure due to deprecated direct dependencies."
    exit 1
fi

echo "✅ No disallowed deprecated direct dependencies found."
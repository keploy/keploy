#!/bin/bash
source "$(dirname "${BASH_SOURCE[0]}")/go-retry.sh"
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

# List all modules with their update / deprecation status
# Proxy-only retry: `go list -m -u all` asks about EVERY module in the graph,
# so a stream error here kills the lint job the same way it kills a sample
# lane — but GO_RETRY_DIRECT_FROM is pushed past max attempts because going
# direct would git ls-remote hundreds of upstream repositories.
#
# If every attempt still fails, do NOT fail the job. A network error is not
# evidence of a deprecated dependency, and reporting it as one is how this
# check came to fail pull requests whose go.mod was byte-identical to main —
# a different module flaking each run, so re-running looked like a fix and was
# luck. This check's subject is the dependency list, and it has no opinion to
# offer when it could not read one. It never prints its success line in that
# case either, so a real deprecation is postponed to the next run, not hidden.
if ! output=$(GO_RETRY_DIRECT_FROM=99 go_retry list -m -u all 2>/tmp/deprecated-deps-err.txt); then
    echo "::warning::Could not read the module list, so the deprecated-dependency check DID NOT RUN."
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
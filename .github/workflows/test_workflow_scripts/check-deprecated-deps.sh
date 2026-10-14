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

# List the direct dependencies with their update / deprecation status. Only
# they are judged below, so only they are asked about: `go list -m -u all`
# also loads the newest version (and its retractions) of every indirect module
# in the graph, so one upstream module whose just-published tag the checksum
# database cannot resolve yet failed this job for every pull request
# (github.com/aws/aws-sdk-go-v2/service/s3 v1.114.0, an indirect dependency,
# for about an hour after its release: "verifying go.mod: ... not found:
# unknown revision").
# Proxy-only retry: a stream error here kills the lint job the same way it
# kills a sample lane — but GO_RETRY_DIRECT_FROM is pushed past max attempts:
# under direct, one renamed, deleted or retagged upstream fails permanently
# where the proxy would still serve it. A just-published tag of a DIRECT
# dependency that the checksum database cannot resolve yet can still fail this
# step: reading its deprecation and retractions means verifying its newest
# go.mod, which is what this check exists to do.
# shellcheck disable=SC2086 # one argument per module path, on purpose
output=$(GO_RETRY_DIRECT_FROM=99 go_retry list -m -u $direct_deps)

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
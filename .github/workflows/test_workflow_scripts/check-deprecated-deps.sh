#!/bin/bash

# Run `go list -m -u all` to list all dependencies.
output=$(go list -m -u all)

found_deprecated=false

while IFS= read -r line; do
    if [[ "$line" == *"deprecated"* || "$line" == *"retracted"* ]]; then
        echo "Deprecated/retracted dependency found: $line"
        found_deprecated=true
    fi
done <<< "$output"

if [ "$found_deprecated" = true ]; then
    echo "Exiting with failure due to deprecated dependencies."
    exit 1
fi
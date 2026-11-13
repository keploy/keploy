#!/usr/bin/env bash
# coverage-floor.sh: checks keploy's own test coverage against the floors in
# .github/coverage-floor.yaml, and checks that a change never lowers a floor.
# Run from the repository root:
#
#   bash .github/workflows/test_workflow_scripts/coverage-floor.sh check LAYER COVDATA_DIR
#       LAYER is a key of the floor file (unit or merged). COVDATA_DIR holds
#       Go's binary coverage data: a `go test -cover ... -args
#       -test.gocoverdir=DIR` run, the GOCOVERDIR of a keploy built with
#       -cover, or the output of `go tool covdata merge`. Prints the measured
#       percentage, the floor and the headroom, pass or fail, and fails when
#       the percentage is below the floor less the layer's tolerance (exit 3,
#       so a caller can tell a drop from data or a floor file it could not
#       read, exit 1).
#   bash .github/workflows/test_workflow_scripts/coverage-floor.sh ratchet BASE_REF
#       Fails when a floor in the working copy is lower than at BASE_REF, or
#       is gone, and when a floor less its tolerance is lower than at
#       BASE_REF. A floor file BASE_REF does not have yet passes; a BASE_REF
#       that is not there (a shallow checkout) fails, as it would otherwise
#       pass every change.
#
# Tested by coverage-floor.tests.sh (run by linux-ci-scripts.yml).
#
# The percentage is statements covered over statements, counted here from
# the profile `go tool covdata textfmt` writes, not read from `go tool cover
# -func`. It is the same sum, since every block lies in a function, but this
# needs no source tree beside the data. A block that appears more than once
# (two binaries' data for one package) counts once, covered when any copy
# is. Files that carry Go's generated-code header (the eBPF loaders, mocks)
# are left out: each profile path is looked up in the checkout under the
# module path that ./go.mod declares.
set -u

floor_file=.github/coverage-floor.yaml
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

usage() {
    echo "usage: $0 check LAYER COVDATA_DIR | ratchet BASE_REF" >&2
    exit 2
}

# floor_value FILE KEY: prints the value of the top-level KEY in FILE, or
# nothing when FILE has no such key.
floor_value() {
    awk -v key="$2" '
        /^[[:space:]]*(#|$)/ { next }
        {
            k = $0
            sub(/:.*/, "", k)
            if (k != key) next
            v = $0
            sub(/^[^:]*:[[:space:]]*/, "", v)
            sub(/[[:space:]]*(#.*)?$/, "", v)
            print v
            exit
        }' "$1"
}

is_number() { [[ $1 =~ ^[0-9]+(\.[0-9]+)?$ ]]; }

# measure DIR: prints the percentage of statements DIR's coverage data
# covers, to two decimals, or fails.
measure() {
    local dir=$1 module
    if ! go tool covdata textfmt -i="$dir" -o="$work/profile.txt"; then
        echo "::error::go tool covdata could not read the coverage data in $dir" >&2
        return 1
    fi
    module=$(awk '$1 == "module" { print $2; exit }' go.mod 2>/dev/null)
    # The profile's files by their path in the checkout, one grep over all of
    # them, and the generated ones back under their import path.
    awk -v m="$module/" '!/^mode:/ {
            f = $1
            sub(/:[^:]*$/, "", f)
            if (m != "/" && index(f, m) == 1) print substr(f, length(m) + 1)
        }' "$work/profile.txt" | sort -u | tr '\n' '\0' |
        xargs -0 grep -lsE '^// Code generated .* DO NOT EDIT\.$' -- |
        sed "s|^|$module/|" >"$work/generated"
    awk -v gl="$work/generated" '
        FILENAME == gl { generated[$0] = 1; next }
        /^mode:/ { next }
        {
            f = $1
            sub(/:[^:]*$/, "", f)
            if (f in generated) next
            if (!($1 in stmts)) { stmts[$1] = $2; total += $2 }
            if ($3 > 0 && !($1 in hit)) { hit[$1] = 1; covered += $2 }
        }
        END {
            if (total == 0) exit 1
            printf "%.2f\n", 100 * covered / total
        }' "$work/generated" "$work/profile.txt" || {
        echo "::error::the coverage data in $dir counts no statements outside generated files" >&2
        return 1
    }
}

check() {
    local layer=$1 dir=$2 floor tolerance measured verdict headroom
    floor=$(floor_value "$floor_file" "$layer")
    tolerance=$(floor_value "$floor_file" "${layer}_tolerance")
    if ! is_number "$floor" || ! is_number "$tolerance"; then
        echo "::error::$floor_file has no number for '$layer' and '${layer}_tolerance' (found '$floor' and '$tolerance')"
        return 1
    fi
    measured=$(measure "$dir") || return 1
    # Compared in hundredths, so a run exactly at the floor less its
    # tolerance passes whatever the float rounding.
    read -r verdict headroom < <(awk -v m="$measured" -v f="$floor" -v t="$tolerance" 'BEGIN {
        lim = (f - t) * 100
        lim = lim < 0 ? -int(-lim + 0.5) : int(lim + 0.5)
        printf "%s %.2f\n", (int(m * 100 + 0.5) >= lim ? "pass" : "fail"), m - f
    }')
    if [ "$verdict" = pass ]; then
        echo "$layer coverage ${measured}%: floor $floor, tolerance $tolerance, headroom $headroom"
        return 0
    fi
    echo "::error::$layer coverage ${measured}% is below its floor $floor less its tolerance $tolerance (headroom $headroom)"
    return 3
}

# bar FLOOR TOLERANCE: prints the floor less its tolerance, in hundredths, the
# line check holds a run to.
bar() {
    awk -v f="$1" -v t="$2" 'BEGIN { x = (f - t) * 100; print (x < 0 ? -int(-x + 0.5) : int(x + 0.5)) }'
}

ratchet() {
    local base=$1 base_copy key was now was_tol now_tol was_bar now_bar fails=0
    if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
        echo "::error::$base is not a commit here, so the floors cannot be compared with it (is the checkout shallow?)"
        return 1
    fi
    if ! git cat-file -e "$base:$floor_file" 2>/dev/null; then
        echo "$floor_file is new since $base: nothing to compare"
        return 0
    fi
    base_copy=$work/base-floor.yaml
    git show "$base:$floor_file" >"$base_copy"
    while IFS= read -r key; do
        was=$(floor_value "$base_copy" "$key")
        now=$(floor_value "$floor_file" "$key")
        was_tol=$(floor_value "$base_copy" "${key}_tolerance")
        now_tol=$(floor_value "$floor_file" "${key}_tolerance")
        is_number "$was" || continue
        if ! is_number "$now"; then
            echo "::error::the '$key' floor was $was at $base and is gone or not a number here ('$now')"
            fails=$((fails + 1))
        elif awk -v n="$now" -v w="$was" 'BEGIN { exit !(n < w) }'; then
            echo "::error::the '$key' floor was $was at $base and is $now here; a floor can only go up"
            fails=$((fails + 1))
        elif is_number "$was_tol" && ! is_number "$now_tol"; then
            echo "::error::'${key}_tolerance' was $was_tol at $base and is gone or not a number here ('$now_tol')"
            fails=$((fails + 1))
        elif is_number "$was_tol" && [ "$(bar "$now" "$now_tol")" -lt "$(bar "$was" "$was_tol")" ]; then
            # The tolerance is checked too: raising it lowers the line a run
            # must reach as surely as lowering the floor does.
            was_bar=$(awk -v x="$(bar "$was" "$was_tol")" 'BEGIN { printf "%.2f", x / 100 }')
            now_bar=$(awk -v x="$(bar "$now" "$now_tol")" 'BEGIN { printf "%.2f", x / 100 }')
            echo "::error::the '$key' floor less its tolerance was $was_bar at $base and is $now_bar here; a tolerance may grow only as much as its floor does"
            fails=$((fails + 1))
        else
            echo "'$key' floor $was at $base, $now here: ok"
        fi
    done < <(awk '/^[a-z_]+:/ && !/_tolerance:/ { sub(/:.*/, ""); print }' "$base_copy")
    [ "$fails" -eq 0 ]
}

case "${1:-}" in
check) [ $# -eq 3 ] || usage; check "$2" "$3" ;;
ratchet) [ $# -eq 2 ] || usage; ratchet "$2" ;;
*) usage ;;
esac

#!/usr/bin/env bash
# Tests for covdata-assert.sh. Run by linux-ci-scripts.yml, or locally (it
# needs only bash and awk, and leaves nothing behind):
#
#   bash .github/workflows/test_workflow_scripts/covdata-assert.tests.sh
#
# Each case writes empty covmeta/covcounters files (or none) into a
# directory and runs the script with a stub `go` first on PATH, whose
# `go tool covdata func` prints the case's canned output. So the cases check
# the script's verdict on that output; that a real agent writes counters is
# the nightly run's to show.
#
# The named function at a non-zero percentage passes; at 0.0% or absent it
# fails, naming the function; a directory with no coverage data fails and
# says to check that GOCOVERDIR reached keploy.
set -u
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$here/covdata-assert.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0

mkdir -p "$tmp/bin"
cat >"$tmp/bin/go" <<'EOF'
#!/usr/bin/env bash
# go tool covdata func -i=DIR: prints $STUB_OUT.
printf '%s\n' "$STUB_OUT"
EOF
chmod +x "$tmp/bin/go"

file=pkg/agent/proxy/proxy.go
func='*Proxy.StartProxy'
mod=go.keploy.io/server/v3

# run_case NAME WANT_RC WANT_TEXT DATA STUB_OUT: DATA is "yes" for a
# directory with coverage data in it, "no" for an empty one.
run_case() {
    local name=$1 want_rc=$2 want=$3 data=$4 stub_out=$5 d="$tmp/$1" out rc
    mkdir -p "$d"
    if [ "$data" = yes ]; then
        : >"$d/covmeta.0123abcd"
        : >"$d/covcounters.0123abcd.100.1"
        : >"$d/covcounters.0123abcd.200.2"
    fi
    out=$(STUB_OUT=$stub_out PATH="$tmp/bin:$PATH" bash "$script" "$d" "$file" "$func" 2>&1)
    rc=$?
    [ "$rc" -eq 0 ] || rc=1
    if [ "$rc" -ne "$want_rc" ] || [[ $out != *"$want"* ]]; then
        echo "FAIL $name: returned $rc, want $want_rc"
        echo "     printed: $out"
        echo "     want it to contain: $want"
        fails=$((fails + 1))
    else
        echo "ok   $name (rc $rc): $want"
    fi
}

# line FILE:LINE: FUNC PCT: one line as covdata func prints it, its columns
# padded with a run of tabs that varies with the path's length (these runs
# are copied from a real one).
line() { printf '%s\t\t\t\t\t\t\t\t\t%s\t\t\t\t\t%s\n' "$1" "$2" "$3"; }

ran=$(
    line "$mod/pkg/agent/proxy/mockmanager.go:30:" init 100.0%
    line "$mod/pkg/agent/proxy/proxy.go:2153:" "$func" 37.5%
    line "$mod/cli/record.go:40:" RecordCmd 80.0%
)
run_case agent-ran 0 'agent coverage present: *Proxy.StartProxy in pkg/agent/proxy/proxy.go ran (37.5%' yes "$ran"

# The CLI ran the package's init, but the agent's function never ran.
not_ran=$(
    line "$mod/pkg/agent/proxy/mockmanager.go:30:" init 100.0%
    line "$mod/pkg/agent/proxy/proxy.go:2153:" "$func" 0.0%
)
run_case agent-never-ran 1 'no agent counters: *Proxy.StartProxy in pkg/agent/proxy/proxy.go never ran' yes "$not_ran"

# A function of the same name in another file does not count.
elsewhere=$(line "$mod/pkg/agent/other/proxy.go:12:" "$func" 50.0%)
run_case function-absent 1 'is not in the coverage data' yes "$elsewhere"

run_case no-data 1 'no coverage data was written to' no "$ran"

if [ "$fails" -gt 0 ]; then
    echo "$fails case(s) failed"
    exit 1
fi
echo "all cases passed"

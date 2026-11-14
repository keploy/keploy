#!/usr/bin/env bash
# covdata-assert.sh: checks that a lane of coverage-nightly.yml collected
# coverage from the keploy agent, not only from the keploy CLI.
#
#   bash .github/workflows/test_workflow_scripts/covdata-assert.sh DIR FILE FUNC
#
# DIR is the lane's GOCOVERDIR. FILE is a source file's path within the
# module (pkg/agent/proxy/proxy.go) and FUNC a function in it as
# `go tool covdata func` names it (*Proxy.StartProxy). It passes when DIR
# holds coverage data and FUNC ran at least once.
#
# Tested by covdata-assert.tests.sh (run by linux-ci-scripts.yml).
#
# Why a function, and one only the agent runs: keploy record and keploy test
# start the agent under sudo, which drops GOCOVERDIR unless the runner keeps
# it, so a lane can collect the CLI's data and none of the agent's. A count
# of counter files cannot tell: the record and test CLIs write two between
# them. A package cannot either: the CLI is the same binary, so it runs
# every linked package's init, and a package with an init shows coverage
# without the agent. The proxy's start function runs in the agent alone.
set -u

if [ $# -ne 3 ]; then
    echo "usage: $0 DIR FILE FUNC" >&2
    exit 2
fi
dir=$1 file=$2 func=$3

if ! compgen -G "$dir/covmeta.*" >/dev/null || ! compgen -G "$dir/covcounters.*" >/dev/null; then
    echo "::error::no coverage data was written to $dir: check that GOCOVERDIR reached keploy, and that keploy was built with -cover"
    exit 1
fi

if ! out=$(go tool covdata func -i="$dir" 2>&1); then
    printf '%s\n' "$out"
    echo "::error::go tool covdata could not read the coverage data in $dir"
    exit 1
fi

# covdata func pads its columns with a varying run of tabs, so the line is
# split on any whitespace: the file and line, the function, its percentage.
pct=$(printf '%s\n' "$out" | awk -v file="$file" -v func_="$func" '
    {
        f = $1
        sub(/:[0-9]+:$/, "", f)
        name = $2
        if (name == func_ && (f == file || substr(f, length(f) - length(file)) == "/" file)) {
            print $NF
            exit
        }
    }')

if [ -z "$pct" ]; then
    echo "::error::$func in $file is not in the coverage data in $dir: has it been renamed or moved? This check needs a function only the keploy agent runs"
    exit 1
fi
if [ "${pct%\%}" = "0.0" ]; then
    echo "::error::no agent counters: $func in $file never ran in this lane's data ($dir), so the agent wrote none; check that GOCOVERDIR reached the agent (sudo keeps it only when told to)"
    exit 1
fi
echo "agent coverage present: $func in $file ran ($pct of its statements)"

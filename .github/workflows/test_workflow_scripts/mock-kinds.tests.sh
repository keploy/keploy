#!/usr/bin/env bash
# Tests for mock-kinds.sh's http_mocks_are and http_mocks_only. Run by
# linux-ci-scripts.yml, or
# locally (it needs only bash and awk, and leaves nothing behind):
#
#   bash .github/workflows/test_workflow_scripts/mock-kinds.tests.sh
#
# Each case writes a mocks.yaml of the kinds it names, in keploy's layout (one
# YAML document per mock, its kind at the top level), every mock with an
# indented `kind:` in its spec, which is not a mock's kind. It checks the
# verdict and the line the check prints.
#
# aws-runner is the recording of the busy --sync pass of the repeated-headers
# lane on an AWS runner: the DNS mocks (A, AAAA, PTR) of the runner's own
# name, which app.py's HTTPServer looks up at start, then 200 Http mocks. It
# passes, as the same recording without them does, or with them last; one
# Http mock more (duplicated) or fewer (lost) fails with them as without them.
# A mock of any other kind fails, Http2 included, and so does an empty file or
# a missing one.
#
# http_mocks_only (the only- cases) is the check of the mock lanes, whose
# pytest suite calls its dependency at an IP address: exactly N Http mocks
# pass, and any DNS mock fails beside them, one included, as do the shapes
# that fail http_mocks_are. other-in-place-of-http is the recording a count
# of every mock passed: the right number of mocks, one of them not Http.
#
# The named- cases have mocks of two other kinds: the line names each, with
# its count.
set -u
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR
source "$here/mock-kinds.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0

# mocks FILE KIND:COUNT...: writes FILE with COUNT mocks of each KIND, in the
# order given. An empty KIND writes a `kind:` line with no kind.
mocks() {
    local f=$1 spec kind n i id=0
    shift
    : > "$f"
    for spec in "$@"; do
        kind=${spec%:*} n=${spec##*:}
        for ((i = 0; i < n; i++)); do
            [ "$id" -gt 0 ] && echo --- >> "$f"
            printf 'version: api.keploy.io/v1beta1\nkind:%s\nname: mock-%d\nspec:\n    metadata:\n        kind: Generic\n        type: config\n' \
                "${kind:+ $kind}" "$id" >> "$f"
            id=$((id + 1))
        done
    done
}

# check_with FN NAME WANT_RC N WANT_LINE [KIND:COUNT...]: FN FILE N, FILE
# written by mocks from the KIND:COUNTs (with none, FILE is not there), must
# return WANT_RC (0, or 1 for any failure) and print "FILE: WANT_LINE".
check_with() {
    local fn=$1 name=$2 want_rc=$3 n=$4 want=$5 f="$tmp/$2.yaml" out rc
    shift 5
    [ $# -gt 0 ] && mocks "$f" "$@"
    out=$("$fn" "$f" "$n")
    rc=$?
    [ "$rc" -eq 0 ] || rc=1
    if [ "$rc" -ne "$want_rc" ] || [ "$out" != "$f: $want" ]; then
        echo "FAIL $name: $fn FILE $n returned $rc, want $want_rc"
        echo "     printed: $out"
        echo "     want:    $f: $want"
        fails=$((fails + 1))
    else
        echo "ok   $name (rc $rc): $want"
    fi
}
check() { check_with http_mocks_are "$@"; }
only() { check_with http_mocks_only "$@"; }

# named FN NAME N HEAD KIND=COUNT... -- KIND:COUNT...: FN FILE N, FILE written
# by mocks from the KIND:COUNTs after --, must fail and print
# "FILE: HEAD (LIST)", LIST the KIND=COUNTs before --, each once, in any
# order: awk's for-in order is unspecified.
named() {
    local fn=$1 name=$2 n=$3 head=$4 f="$tmp/$2.yaml" out rc list got want
    local -a listed=() items=()
    shift 4
    while [ $# -gt 0 ] && [ "$1" != -- ]; do listed+=("$1"); shift; done
    shift
    mocks "$f" "$@"
    out=$("$fn" "$f" "$n")
    rc=$?
    want=$(printf '%s\n' "${listed[@]}" | sort | paste -sd ' ' -)
    got="(not \"$f: $head (...)\")"
    case $out in
    "$f: $head ("*")")
        list=${out#"$f: $head ("}
        read -ra items <<<"${list%)}"
        got=$(printf '%s\n' "${items[@]}" | sort | paste -sd ' ' -)
        ;;
    esac
    if [ "$rc" -eq 0 ] || [ "$got" != "$want" ]; then
        echo "FAIL $name: $fn FILE $n returned $rc, want a failure"
        echo "     printed: $out"
        echo "     want:    $f: $head (each of: $want)"
        fails=$((fails + 1))
    else
        echo "ok   $name (rc 1): $head ($want)"
    fi
}

check http-only            0 3   "3 Http mocks, 0 DNS, 0 of other kinds"                 Http:3
check aws-runner           0 200 "200 Http mocks, 3 DNS, 0 of other kinds"               DNS:3 Http:200
check dns-last             0 200 "200 Http mocks, 3 DNS, 0 of other kinds"               Http:200 DNS:3
check duplicated-http      1 200 "201 Http mocks, 0 DNS, 0 of other kinds"               Http:201
check duplicated-http-dns  1 200 "201 Http mocks, 3 DNS, 0 of other kinds"               Http:201 DNS:3
check lost-http            1 200 "199 Http mocks, 0 DNS, 0 of other kinds"               Http:199
check lost-http-dns        1 200 "199 Http mocks, 3 DNS, 0 of other kinds"               Http:199 DNS:3
check dns-in-place-of-http 1 3   "2 Http mocks, 1 DNS, 0 of other kinds"                 Http:2 DNS:1
check other-kind           1 200 "200 Http mocks, 0 DNS, 1 of other kinds (MySQL=1)"     Http:200 MySQL:1
check other-kind-dns       1 200 "200 Http mocks, 3 DNS, 2 of other kinds (Generic=2)"   Http:200 DNS:3 Generic:2
check http2-is-not-http    1 3   "2 Http mocks, 0 DNS, 1 of other kinds (Http2=1)"       Http:2 Http2:1
check no-kind              1 3   "3 Http mocks, 0 DNS, 1 of other kinds ((none)=1)"      Http:3 :1
check no-mocks             1 3   "0 Http mocks, 0 DNS, 0 of other kinds"                 Http:0
check no-file              1 3   "0 Http mocks, 0 DNS, 0 of other kinds"

only only-http                  0 3 "3 Http mocks, 0 DNS, 0 of other kinds"               Http:3
only only-http-2                0 2 "2 Http mocks, 0 DNS, 0 of other kinds"               Http:2
only only-dns                   1 3 "3 Http mocks, 3 DNS, 0 of other kinds"               DNS:3 Http:3
only only-one-dns-last          1 3 "3 Http mocks, 1 DNS, 0 of other kinds"               Http:3 DNS:1
only only-duplicated-http       1 3 "4 Http mocks, 0 DNS, 0 of other kinds"               Http:4
only only-lost-http             1 3 "2 Http mocks, 0 DNS, 0 of other kinds"               Http:2
only only-other-kind            1 3 "3 Http mocks, 0 DNS, 1 of other kinds (MySQL=1)"     Http:3 MySQL:1
only only-other-in-place-of-http 1 3 "2 Http mocks, 0 DNS, 1 of other kinds (Generic=1)"  Http:2 Generic:1
only only-http2-is-not-http     1 3 "2 Http mocks, 0 DNS, 1 of other kinds (Http2=1)"     Http:2 Http2:1
only only-no-kind               1 3 "3 Http mocks, 0 DNS, 1 of other kinds ((none)=1)"    Http:3 :1
only only-no-file               1 3 "0 Http mocks, 0 DNS, 0 of other kinds"

named http_mocks_are  named-are  3 "3 Http mocks, 0 DNS, 2 of other kinds" Generic=1 MySQL=1 -- Http:3 Generic:1 MySQL:1
named http_mocks_only named-only 3 "3 Http mocks, 1 DNS, 3 of other kinds" Generic=2 MySQL=1 -- Http:3 DNS:1 Generic:2 MySQL:1

if [ "$fails" -ne 0 ]; then
    echo "mock-kinds.tests.sh: $fails case(s) failed"
    exit 1
fi
echo "mock-kinds.tests.sh: all cases passed"

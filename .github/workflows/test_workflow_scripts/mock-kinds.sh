# shellcheck shell=bash
# http_mocks_are and http_mocks_only: the checks a lane makes when the mocks
# it records are its own HTTP calls; http_mock_count: the count of its Http
# mocks, for a lane that records other kinds beside them. Sourced from the
# checkout, as
#
#   source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/mock-kinds.sh"
#
# or from the lane script's own directory, by a path relative to it.
#
# Tested by mock-kinds.tests.sh (run by linux-ci-scripts.yml).
#
# Why DNS mocks are let through: a lane's code may look a name up on
# purpose, and a recording can hold DNS mocks the lane's code never asked
# for too. Python's HTTPServer.server_bind, for one, calls
# socket.getfqdn, which looks up the host's own name. On a runner whose
# /etc/hosts lists that name (GitHub's Azure-hosted ubuntu-latest) the lookup
# never reaches DNS; on one whose name only its resolver knows (GitHub's
# AWS-hosted ubuntu-latest, ip-10-0-x-y.<region>.compute.internal) it does,
# and keploy records the answers (A, AAAA and PTR) as DNS mocks, rightly:
# the replay serves them. So a count of every mock against the lane's HTTP
# calls depends on which runner the job got. Counting the Http mocks alone
# would let through a mock of a kind the lane never makes (a MySQL or
# Generic mock from a parser that took the HTTP call for something else), so
# every mock that is not Http has to be DNS.
#
# A lane whose code makes no DNS query on any runner (it calls its
# dependencies at IP addresses, or by names its own /etc/hosts answers, and
# starts no server that looks up the host's name) has no DNS mock to let
# through, so a DNS mock in its recording is a fault there: it checks with
# http_mocks_only, which fails on one.

# mock_kind_counts FILE: prints "H D O KINDS": FILE's (a mocks.yaml) count of
# mocks of kind Http, of kind DNS, and of any other kind, then each other kind
# as KIND=COUNT (a `kind:` with no kind is "(none)"). A missing or unreadable
# FILE holds none.
mock_kind_counts() {
    # mocks.yaml is one YAML document per mock, its kind at the top level, so
    # a `kind:` that starts its line is a mock's own.
    awk '
        /^kind:/ {
            if ($0 == "kind: Http") h++
            else if ($0 == "kind: DNS") d++
            else { o++; seen[NF < 2 ? "(none)" : $2]++ }
        }
        END {
            s = ""
            for (k in seen) s = s (s == "" ? "" : " ") k "=" seen[k]
            printf "%d %d %d %s\n", h, d, o, s
        }' "$1" 2>/dev/null
}

# http_mock_count FILE: prints the number of mocks of kind Http in FILE (a
# mocks.yaml; a missing or unreadable FILE holds none). The kind is matched
# whole, as by the checks below: an Http2 or HttpFault mock is not an Http
# mock, though `grep -c '^kind: Http'` counts it as one.
http_mock_count() {
    local http
    read -r http _ < <(mock_kind_counts "$1")
    echo "${http:-0}"
}

# http_mocks_are FILE N: succeeds when FILE (a mocks.yaml) holds exactly N
# mocks of kind Http and no mock of any kind but Http and DNS. Prints what it
# counted on one line, as
# "FILE: H Http mocks, D DNS, O of other kinds (KIND=COUNT ...)".
http_mocks_are() {
    local file=$1 want=$2 http dns other kinds
    read -r http dns other kinds < <(mock_kind_counts "$file")
    echo "$file: ${http:-0} Http mocks, ${dns:-0} DNS, ${other:-0} of other kinds${kinds:+ ($kinds)}"
    [ "${http:-0}" -eq "$want" ] && [ "${other:-0}" -eq 0 ]
}

# http_mocks_only FILE N: succeeds when FILE holds exactly N mocks of kind
# Http and no mock of any other kind, DNS included. Prints as http_mocks_are.
http_mocks_only() {
    local file=$1 want=$2 http dns other kinds
    read -r http dns other kinds < <(mock_kind_counts "$file")
    echo "$file: ${http:-0} Http mocks, ${dns:-0} DNS, ${other:-0} of other kinds${kinds:+ ($kinds)}"
    [ "${http:-0}" -eq "$want" ] && [ "${dns:-0}" -eq 0 ] && [ "${other:-0}" -eq 0 ]
}

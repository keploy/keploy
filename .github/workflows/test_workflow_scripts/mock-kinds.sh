# shellcheck shell=bash
# http_mocks_are: the check a lane makes when the mocks it records are its
# own HTTP calls. Sourced via:
#
#   source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/mock-kinds.sh"
#
# Tested by mock-kinds.tests.sh (run by linux-ci-scripts.yml).
#
# Why DNS mocks are let through: a recording can hold DNS mocks the lane's
# code never asked for. Python's HTTPServer.server_bind, for one, calls
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

# http_mocks_are FILE N: succeeds when FILE (a mocks.yaml) holds exactly N
# mocks of kind Http and no mock of any kind but Http and DNS. A missing or
# unreadable FILE holds none. Prints what it counted on one line, as
# "FILE: H Http mocks, D DNS, O of other kinds (KIND=COUNT ...)".
http_mocks_are() {
    local file=$1 want=$2 http dns other kinds
    # mocks.yaml is one YAML document per mock, its kind at the top level, so
    # a `kind:` that starts its line is a mock's own.
    read -r http dns other kinds < <(awk '
        /^kind:/ {
            if ($0 == "kind: Http") h++
            else if ($0 == "kind: DNS") d++
            else { o++; seen[NF < 2 ? "(none)" : $2]++ }
        }
        END {
            s = ""
            for (k in seen) s = s (s == "" ? "" : " ") k "=" seen[k]
            printf "%d %d %d %s\n", h, d, o, s
        }' "$file" 2>/dev/null)
    echo "$file: ${http:-0} Http mocks, ${dns:-0} DNS, ${other:-0} of other kinds${kinds:+ ($kinds)}"
    [ "${http:-0}" -eq "$want" ] && [ "${other:-0}" -eq 0 ]
}

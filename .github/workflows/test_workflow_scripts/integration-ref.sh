# shellcheck shell=bash
# Reads the keploy/integrations ref a keploy PR's body names. Sourced by
# setup-private-parsers (the build and lint lanes) and by integrations-tests.sh,
# so both read a body the same way. integration-ref.ps1 is its Windows twin.
#
# integration_ref_of_body BODY
#   Prints the first word after the first `Integration-Ref:` line of BODY, or
#   nothing when there is none. The tag is case-insensitive, may have spaces
#   before its colon, and may follow leading spaces and a list or quote marker
#   ("- ", "* ", "> "). Only the first word counts: "main other" is main.
#   A body edited in GitHub's web form has CRLF line ends, so every CR is read
#   as a line end: a ref that ends its line would otherwise be read with a CR
#   on it, fail the ref allow-list, and the lanes would build against another
#   integrations ref without a word.
integration_ref_of_body() {
  printf '%s\n' "$1" | tr '\r' '\n' |
    grep -iE '^[[:space:]]*([-*>][[:space:]]+)?integration-ref[[:space:]]*:' | head -1 |
    sed -E 's/^[^:]+:[[:space:]]*//' | awk '{print $1; exit}' || true
}

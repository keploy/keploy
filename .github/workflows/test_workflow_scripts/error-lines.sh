# shellcheck shell=bash
# error-lines.sh: the lines a log recorded at ERROR level.
#
#   source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/error-lines.sh"
#   grep -aE "$ERROR_LINE_RE" app.txt        # print them
#   grep -qaE "$ERROR_LINE_RE" app.txt       # is there one?
#
# A lane fails when keploy's (or the app's) log has an ERROR line, so ERROR is
# matched as a level TOKEN, never as a substring. keploy's healthy NODATA relay
# once logged "upstream NODATA (empty NOERROR)" at INFO, and every
# `grep "ERROR"` in these scripts would have failed a passing replay on it.
#
# The token is ERROR with no letter, digit or _ on either side. On the left the
# colour code keploy's console encoder writes before a level also counts as a
# side, since its last byte is a letter:
#
#   keploy, coloured    ...Z \t\x1b[31mERROR\x1b[0m\tmessage
#   keploy, plain       ...Z \tERROR\tmessage      (--disable-ansi)
#   JSON                "level":"ERROR"
#   Logback / Log4j     2026-10-14 10:00:00.000 ERROR 123 --- [main] ...
#   Python logging      ERROR:root:message
#
# and NOERROR, SQL_ERROR or ERRORS are not ERROR lines. A pattern with spaces
# around the word (' ERROR ') misses keploy's own lines, whose level sits
# between tabs and colour codes.
#
# Always pass -a: GNU grep reads a log with a NUL byte in it as binary and
# prints "Binary file ... matches" instead of the line, so a pipeline like
# `grep ... | grep "Keploy:"` would see no line and pass.
#
# error-lines-test.sh tests the pattern, and fails if a script, workflow or
# action under .github looks for ERROR as a substring.
# shellcheck disable=SC2034 # read by the scripts that source this file
ERROR_LINE_RE='(^|[^[:alnum:]_]|\[[0-9;]*m)ERROR([^[:alnum:]_]|$)'

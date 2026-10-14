#!/usr/bin/env bash
# CLI exit-code guard for keploy command GROUPS. An unknown verb must NOT exit 0.
#
# Before the fix, a cobra group with no RunE (mock, ca, contract, ...) printed
# help and returned 0 for an unknown verb, so `keploy mock bogus` was a false
# success a CI job calling a mistyped verb would pass on. An unknown verb is a
# usage error -> exit 8 (utils.ExitUsageError), and so is a flag that cannot
# be parsed or the wrong number of arguments; a group with no verb still shows
# help (0); a valid subcommand resolves normally. (design §P0b.)
#
# Pure CLI behaviour: no sudo, no eBPF, no dependencies.
set -uo pipefail

RECORD_BIN="${RECORD_BIN:-keploy}"
FAIL=0

check() {
  local want="$1"
  shift
  "$RECORD_BIN" "$@" >/dev/null 2>&1
  local got=$?
  if [ "$got" -ne "$want" ]; then
    echo "FAIL: 'keploy $*' exited $got, want $want"
    FAIL=1
  else
    echo "ok: 'keploy $*' -> $got"
  fi
}

# Unknown verb on a command group is a usage error (8), never a false 0.
check 8 mock bogusverb
check 8 ca bogusverb
check 8 contract bogusverb
# Unknown top-level command is also a usage error.
check 8 totallybogus
# So is a flag that cannot be parsed: unknown, mistyped value, missing value.
check 8 mock record --bogusflag
check 8 test --bogusflag
check 8 test --delay notanumber
# And so is the wrong number of arguments.
check 8 diff a b c
# A group with no verb shows help and succeeds.
check 0 mock
# A known subcommand is routed to (its --help exits 0). NOTE: --help short-circuits
# before any RunE, so this line proves the verb is recognized, not that the parent's
# reject RunE is bypassed — that guarantee is the unit test
# TestHardenUnknownSubcommands/"valid subcommand still runs".
check 0 mock record --help
check 0 mock replay --help

if [ "$FAIL" -eq 0 ]; then echo "MOCK CLI EXIT-CODES E2E: PASSED"; else echo "MOCK CLI EXIT-CODES E2E: FAILED"; fi
exit "$FAIL"

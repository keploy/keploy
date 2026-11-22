#!/usr/bin/env bash
# CLI exit-code guard for keploy command GROUPS. An unknown verb must NOT exit 0.
#
# Before the fix, a cobra group with no RunE (mock, ca, contract, ...) printed
# help and returned 0 for an unknown verb, so `keploy mock bogus` was a false
# success a CI job calling a mistyped verb would pass on. An unknown verb is a
# usage error -> exit 8 (utils.ExitUsageError), and so is a flag that cannot
# be parsed or the wrong number of arguments; a group with no verb still shows
# help (0); a valid subcommand resolves normally. (design §P0b.) A usage error
# is said in one line, with a pointer to that command's --help, not under the
# command's whole usage text; and the `keploy record` line keploy prints as a
# hint is one keploy record accepts (it said `--delay 6`, a flag record does
# not have).
#
# Pure CLI behaviour: no sudo, no eBPF, no dependencies. Every check runs
# twice: without the proven switch (today's default) and under KEPLOY_PROVEN=1,
# which the flip makes the default. MOCK_LANE_SWITCH (lane_switch.sh) picks
# the half a run runs; CI runs each in a job of its own.
set -uo pipefail

# This lane's directory: lane_switch.sh.
# shellcheck source-path=SCRIPTDIR
MOCK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# A lane that could not load it would skip both halves and pass: fail instead.
# shellcheck source=lane_switch.sh
if ! source "$MOCK_DIR/lane_switch.sh" || ! declare -F half >/dev/null; then
  echo "FAIL: could not load lane_switch.sh from $MOCK_DIR"
  exit 1
fi
RECORD_BIN="${RECORD_BIN:-keploy}"
FAIL=0

check() {
  local want="$1" proven got
  shift
  for proven in "${SWITCH_VALUES[@]}"; do
    KEPLOY_PROVEN="$proven" "$RECORD_BIN" "$@" >/dev/null 2>&1
    got=$?
    if [ "$got" -ne "$want" ]; then
      echo "FAIL: 'KEPLOY_PROVEN=$proven keploy $*' exited $got, want $want"
      FAIL=1
    else
      echo "ok: 'KEPLOY_PROVEN=$proven keploy $*' -> $got"
    fi
  done
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

# check_said <command line...>: a command line keploy cannot parse is answered
# with the line that says what is wrong with it and where that command's help
# is -- not with the command's whole usage text. `keploy record --delay 10`,
# the first command of the docs' and the samples' walkthrough, printed every
# flag record has (some 80 lines) under the one line that said
# `unknown flag: --delay`, and that line scrolled away.
check_said() {
  local said="$1" help="$2" proven out got
  shift 2
  for proven in "${SWITCH_VALUES[@]}"; do
    out="$(KEPLOY_PROVEN="$proven" "$RECORD_BIN" "$@" --disable-ansi 2>&1 </dev/null)"
    got=$?
    if [ "$got" -ne 8 ]; then
      echo "FAIL: 'KEPLOY_PROVEN=$proven keploy $*' exited $got, want 8"
      FAIL=1
    elif ! grep -qF -- "$said" <<<"$out" || ! grep -qF -- "Run '$help --help' for usage." <<<"$out"; then
      echo "FAIL: 'KEPLOY_PROVEN=$proven keploy $*' did not say '$said' and point at '$help --help':"
      printf '%s\n' "$out" | tail -5
      FAIL=1
    elif grep -qE '^(Usage|Flags|Global Flags):' <<<"$out"; then
      echo "FAIL: 'KEPLOY_PROVEN=$proven keploy $*' printed its usage text under the error:"
      printf '%s\n' "$out" | grep -nE '^(Usage|Flags|Global Flags):'
      FAIL=1
    else
      echo "ok: 'KEPLOY_PROVEN=$proven keploy $*' -> 8, said in one line"
    fi
  done
}
check_said "unknown flag: --delay" "keploy record" record -c ./app --container-name api --delay 10
check_said "invalid argument \"notanumber\"" "keploy test" test --delay notanumber
check_said "accepts at most 2 arg(s)" "keploy diff" diff a b c

# The `keploy record` line `keploy test` prints when there is nothing to
# replay is one `keploy record` accepts. It said `keploy record ... --delay 6`,
# a flag record does not have. Parsed by record itself: with --help appended,
# a command line whose flags parse prints help (0), one that does not is a
# usage error (8).
HINT_DIR="$(mktemp -d)"
trap 'rm -rf "$HINT_DIR"' EXIT
for proven in "${SWITCH_VALUES[@]}"; do
  out="$(cd "$HINT_DIR" && KEPLOY_PROVEN="$proven" "$RECORD_BIN" test -c ./app --disable-ansi 2>&1 </dev/null)"
  hint="$(sed -n 's/.*Example: \(keploy record .*\)$/\1/p' <<<"$out" | head -1)"
  if [ -z "$hint" ]; then
    echo "FAIL: 'KEPLOY_PROVEN=$proven keploy test' with no test sets printed no 'Example: keploy record ...' line:"
    printf '%s\n' "$out" | tail -5
    FAIL=1
    continue
  fi
  # xargs splits the line as a shell does (quotes and all), without eval.
  (cd "$HINT_DIR" && printf '%s\n' "${hint#keploy }" |
    KEPLOY_PROVEN="$proven" xargs sh -c 'exec "$0" "$@" --help' "$RECORD_BIN" >/dev/null 2>&1)
  got=$?
  if [ "$got" -ne 0 ]; then
    echo "FAIL: KEPLOY_PROVEN=$proven: the hint '$hint' is not a command line keploy record accepts"
    FAIL=1
  else
    echo "ok: KEPLOY_PROVEN=$proven: the hint '$hint' parses"
  fi
done
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

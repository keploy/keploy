#!/usr/bin/env bash
# Tests for error-lines.sh, and a scan that keeps the lanes on it.
#
#   bash .github/workflows/test_workflow_scripts/error-lines-test.sh
#
# 1. ERROR_LINE_RE matches an ERROR line in every shape the lanes' logs carry
#    (keploy coloured and plain, JSON, Logback, Python logging) and no line
#    that only contains the letters: keploy's healthy NODATA relay, which once
#    logged "empty NOERROR" at INFO, among them.
# 2. A log with a NUL byte in it still prints its ERROR line under -a.
# 3. No script, workflow or action under .github looks for an error by
#    substring (grep "ERROR", awk '/ERROR/', ' ERROR '), every script that
#    uses ERROR_LINE_RE sources error-lines.sh, and no awk program tries to
#    report a match with `{ exit 0 } END { exit 1 }` (END runs after that exit
#    and its exit 1 wins, so it reports "no match" for every input).
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=error-lines.sh
source "$HERE/error-lines.sh"

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "  PASS  $1"; }
bad() {
  fail=$((fail + 1))
  echo "  FAIL  $1"
  [[ -n "${2:-}" ]] && echo "          $2" || true
}

esc=$'\x1b'
ts='🐰 Keploy: 2026-10-14T10:00:00.123456789Z '
info="$ts	${esc}[34mINFO${esc}[0m	"
error="$ts	${esc}[31mERROR${esc}[0m	"
# What keploy's NODATA relay logged at INFO before it was reworded.
old_nodata="${info}DNS mock miss: upstream NODATA (empty NOERROR), relaying as valid negative answer	{\"query\": \"db.example.svc.cluster.local.\", \"qtype\": \"AAAA\"}"

echo "the pattern:"
while IFS='|' read -r want what line; do
  line=$(printf '%b' "$line")
  got=no
  printf '%s\n' "$line" | grep -qaE "$ERROR_LINE_RE" && got=yes
  if [[ $got == "$want" ]]; then ok "$what: $want"; else bad "$what" "matched=$got, want $want: $line"; fi
done <<EOF
yes|keploy, coloured|${error}failed to mock the outgoing message
yes|keploy, --disable-ansi|${ts}\tERROR\tfailed to mock the outgoing message
yes|keploy, JSON|{"level":"ERROR","msg":"failed to mock the outgoing message"}
yes|Logback|2026-10-14 10:00:00.000 ERROR 4242 --- [main] o.s.b.SpringApplication : Application run failed
yes|Python logging|ERROR:root:connection lost
yes|bracketed|[ERROR] connection lost
yes|first word of the line|ERROR connection lost
no|keploy's old NODATA INFO line|$old_nodata
no|keploy's NODATA INFO line now|${info}DNS mock miss: upstream NODATA (no records of this type, rcode 0), relaying as valid negative answer
no|an identifier that ends in ERROR|${info}rcode SQL_ERROR mapped
no|a longer word|${info}ERRORS: 0
no|lower case|${info}no error
no|a WARN line|$ts\t${esc}[33mWARN${esc}[0m\tretrying
EOF

# The trap the pattern replaces: a substring grep fails a healthy replay.
if printf '%s\n' "$old_nodata" | grep -q "ERROR"; then
  ok "a substring grep for ERROR flags the old NODATA INFO line (why the lanes use the token)"
else bad "substring control" "grep \"ERROR\" no longer matches the old line; the control is wrong"; fi

echo "a log with a NUL byte in it:"
t=$(mktemp -d)
printf 'binary\0junk\n%s\n' "${error}failed to mock the outgoing message" >"$t/log"
if grep -aE "$ERROR_LINE_RE" "$t/log" | grep -q "Keploy:"; then
  ok "grep -aE prints the ERROR line, so a pipeline after it sees it"
else bad "NUL" "$(grep -aE "$ERROR_LINE_RE" "$t/log" | od -c | head -3)"; fi
rm -rf "$t"

echo "the lanes:"
ROOT="$(cd "$HERE/../.." && pwd -P)" # .github
mapfile -t files < <(find "$ROOT" -type f \( -name '*.sh' -o -name '*.yml' -o -name '*.yaml' \) ! -name 'error-lines*.sh' | sort)
substr=0 unsourced=0 awkexit=0
for f in "${files[@]}"; do
  rel=${f#"$ROOT"/}
  # Code only: comment lines are not read. Read once into a variable: a
  # `... | grep -q` under pipefail reports the writer's SIGPIPE, not the match.
  c=$(grep -nvE '^[[:space:]]*#' "$f")
  while IFS= read -r hit; do
    [[ -n $hit ]] || continue
    bad "$rel:${hit%%:*} looks for ERROR as a substring" "${hit#*:}"
    substr=$((substr + 1))
  done <<<"$(grep -E "grep([[:space:]]+-[[:alnum:]]+)*[[:space:]]+[\"']ERROR[\"']|grep[^|;]*[\"' ] ERROR [\"']|awk[[:space:]]+[\"']/ERROR/" <<<"$c")"
  if [[ $f == *.sh ]] && grep -q 'ERROR_LINE_RE' <<<"$c" &&
    ! grep -qE '^[0-9]+:[[:space:]]*(source|\.)[[:space:]].*error-lines\.sh' <<<"$c"; then
    bad "$rel uses ERROR_LINE_RE without sourcing error-lines.sh"
    unsourced=$((unsourced + 1))
  fi
  while IFS= read -r hit; do
    [[ -n $hit ]] || continue
    bad "$rel:${hit%%:*} has an awk program whose END exit overrides the match" "${hit#*:}"
    awkexit=$((awkexit + 1))
  done <<<"$(grep -E 'exit 0[[:space:]]*\}[[:space:]]*END[[:space:]]*\{[[:space:]]*exit 1' <<<"$c")"
done
[[ $substr == 0 ]] && ok "no lane looks for ERROR as a substring (${#files[@]} files)"
[[ $unsourced == 0 ]] && ok "every script that uses ERROR_LINE_RE sources error-lines.sh"
[[ $awkexit == 0 ]] && ok "no awk program reports a match through { exit 0 } END { exit 1 }"

echo
echo "  $pass passed, $fail failed"
[[ $fail == 0 ]]

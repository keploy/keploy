# Shared by the native and docker legs of the upstream-refusal e2e.

# compare BASELINE RECORDED: every destination's connect result under keploy
# must be the one it got without keploy. Timings are printed, not compared:
# the classes already tell a refused, unreachable or unanswered connect from
# a connected one.
compare() {
  local baseline=$1 recorded=$2 fail=0
  echo "== without keploy"; cat "$baseline"
  echo "== under keploy record"; cat "$recorded"
  while read -r tag addr want _; do
    [ "$tag" = PROBE ] || continue
    got=$(awk -v a="$addr" '$1 == "PROBE" && $2 == a { print $3 }' "$recorded")
    if [ "$got" != "$want" ]; then
      echo "::error::$addr: connect under keploy record was '${got:-<no result>}', without keploy '$want'"
      fail=1
    else
      echo "ok: $addr -> $want, as without keploy"
    fi
  done < "$baseline"
  return $fail
}

# expect BASELINE ADDR CLASS: the fixture produced what it exists to produce
# (otherwise the comparison proves nothing for that case).
expect() {
  local got
  got=$(awk -v a="$2" '$1 == "PROBE" && $2 == a { print $3 }' "$1")
  if [ "$got" != "$3" ]; then
    echo "::error::fixture: without keploy $2 gave '${got:-<no result>}', expected $3; the runner's network differs from what this test assumes"
    return 1
  fi
}

#!/usr/bin/env bash
# Runs keploy/integrations' Go tests against this keploy tree.
#
# keploy/integrations builds its parsers and learn rules on go.keploy.io/server/v3
# (the matcher, the learn step, the models, the proxy's integration
# interfaces), but its own CI pins a released keploy. So a keploy change that
# breaks an integrations test passes both repos' CI, and is first seen when
# integrations moves its pin or when a keploy release builds against
# integrations main. This runs integrations' `go test ./...` with
# go.keploy.io/server/v3 replaced by this tree, once with KEPLOY_PROVEN unset
# and once with KEPLOY_PROVEN=1, and names every failing package and test.
#
# Usage:
#   integrations-tests.sh resolve INTEGRATIONS_DIR
#   integrations-tests.sh test KEPLOY_DIR INTEGRATIONS_DIR
#   integrations-tests.sh parsers KEPLOY_DIR INTEGRATIONS_DIR
#
# resolve
#   INTEGRATIONS_DIR is a clone of keploy/integrations that holds every branch
#   and tag (actions/checkout with fetch-depth: 0). Picks the integrations ref
#   to test and checks it out detached. First hit wins:
#     1. an `Integration-Ref: <ref>` line in the PR's body, read by
#        integration-ref.sh as setup-private-parsers reads it. The body is
#        read from the API, not the event, so an edited body counts on a
#        re-run. A ref named there must exist: the PR is tested against it or
#        fails. It never falls back to main, which is the ref the PR said it
#        does not build against.
#     2. a keploy/integrations ref named like the PR's head branch.
#     3. INTEGRATION_REF (vars.INTEGRATION_REF, else main). It must exist.
#   A ref names what `go get` takes it to name, as setup-private-parsers
#   fetches it with go get: a tag, else a branch, else a commit (7 to 40 hex
#   digits, or the commit of a pseudo-version). So the build lanes and this
#   job pair a PR with the same integrations commit, but in five cases:
#   - the lanes read the body from the event that started the run, so a body
#     edited since counts there only from the next push;
#   - a body ref that names no integrations tag, branch or commit falls
#     through to the next ref there, and fails here;
#   - a ref go get takes that names no tag, branch or commit (latest, a
#     version prefix) is built there; here it fails when the body or
#     INTEGRATION_REF names it, and a head branch so named falls through to
#     INTEGRATION_REF;
#   - a head branch with characters outside the allow-list falls through
#     there, and is looked up here;
#   - a pseudo-version go get refuses (a wrong timestamp or base version)
#     falls through there, or fails there as INTEGRATION_REF, and its commit
#     is tested here.
#   Env: PR_NUMBER and GH_TOKEN (pull requests only), GITHUB_REPOSITORY,
#   HEAD_REF, INTEGRATION_REF. Writes ref=, source= and sha= to GITHUB_OUTPUT
#   when it is set.
#
# test
#   Replaces go.keploy.io/server/v3 by KEPLOY_DIR in INTEGRATIONS_DIR's go.mod,
#   tidies, and runs `go test -count=1 ./...` under each switch. Both switches
#   run even when the first fails. Every failing package and test is named on
#   stdout, as a GitHub error annotation, and in GITHUB_STEP_SUMMARY when it is
#   set. INTEGRATIONS_DESC names the integrations ref in those lines. go runs
#   with no GOFLAGS. Exit 0 only when both runs exit 0 and each ran a test in
#   at least one package.
#
# parsers
#   Adds INTEGRATIONS_DIR's parsers to KEPLOY_DIR as setup-private-parsers adds
#   them to a build (the integrations module required, here from
#   INTEGRATIONS_DIR by a replace, and pkg/agent/proxy/extraparsers.go
#   importing its parser packages), and runs keploy's row that every
#   registered parser's recorder stamps each mock it emits with its request
#   time (TestEveryRegisteredParsersRecorderStampsEachMocksRequestTime in
#   pkg/agent/proxy). keploy's own go test ./... runs that row over the OSS
#   parsers alone. The row sets each switch itself, so it runs
#   once. go runs with no GOFLAGS. Exit 0 only when pkg/agent/proxy links the
#   integrations parsers and the row passed; a failure names the parsers whose
#   rows failed.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
keploy_module=go.keploy.io/server/v3
integrations_module=github.com/keploy/integrations
stamp_row=TestEveryRegisteredParsersRecorderStampsEachMocksRequestTime
# The characters of a git branch, tag or commit. The PR body is untrusted, so a
# ref read from it is refused unless it is made of these alone.
safe_ref_re='^[A-Za-z0-9._/+-]+$'
# A Go pseudo-version, which names a commit by its first 12 hex digits.
pseudo_re='^v[0-9]+\.[0-9]+\.[0-9]+-(.*\.)?[0-9]{14}-([0-9a-f]{12})$'
# shellcheck source=/dev/null
source "$here/integration-ref.sh"

die() {
  echo "::error title=keploy/integrations tests::$*"
  exit 1
}

# Prints the commit REF names in the clone at DIR, looked up as go get and
# git rev-parse look it up: a tag, else a branch of origin, else a commit (7 to
# 40 hex digits, or a pseudo-version's 12). Fails when it names none.
commit_of() {
  local dir="$1" ref="$2" sha
  if [[ "$ref" =~ $pseudo_re ]]; then
    ref="${BASH_REMATCH[2]}"
  fi
  for cand in "refs/tags/$ref" "refs/remotes/origin/$ref"; do
    if sha="$(git -C "$dir" rev-parse --verify --quiet "$cand^{commit}")"; then
      echo "$sha"
      return 0
    fi
  done
  if [[ "$ref" =~ ^[0-9a-f]{7,40}$ ]] &&
    sha="$(git -C "$dir" rev-parse --verify --quiet "$ref^{commit}")"; then
    echo "$sha"
    return 0
  fi
  return 1
}

# The PR's body as it is now. Three tries: a transient API error must not
# decide which integrations ref a PR is tested against.
pr_body() {
  local try out
  for try in 1 2 3; do
    if out="$(gh api "repos/${GITHUB_REPOSITORY}/pulls/${PR_NUMBER}" --jq '.body // ""')"; then
      printf '%s\n' "$out"
      return 0
    fi
    if [ "$try" -lt 3 ]; then
      sleep 5
    fi
  done
  return 1
}

resolve() {
  local dir="$1" ref="" source="" sha="" body body_ref
  if [ -n "${PR_NUMBER:-}" ]; then
    body="$(pr_body)" || die "cannot read the body of PR #${PR_NUMBER}, which may name the integrations ref to test"
    body_ref="$(integration_ref_of_body "$body")"
    if [ -n "$body_ref" ]; then
      [[ "$body_ref" =~ $safe_ref_re ]] ||
        die "the PR body's Integration-Ref \"$body_ref\" has characters outside [A-Za-z0-9._/+-]"
      sha="$(commit_of "$dir" "$body_ref")" ||
        die "the PR body's Integration-Ref \"$body_ref\" names no branch, tag or commit of keploy/integrations"
      ref="$body_ref"
      source="the PR body's Integration-Ref"
    fi
  fi
  if [ -z "$ref" ] && [ -n "${HEAD_REF:-}" ] && sha="$(commit_of "$dir" "$HEAD_REF")"; then
    ref="$HEAD_REF"
    source="the keploy/integrations ref named like this PR's branch"
  fi
  if [ -z "$ref" ]; then
    ref="${INTEGRATION_REF:-main}"
    [[ "$ref" =~ $safe_ref_re ]] ||
      die "INTEGRATION_REF \"$ref\" has characters outside [A-Za-z0-9._/+-]"
    sha="$(commit_of "$dir" "$ref")" ||
      die "INTEGRATION_REF \"$ref\" names no branch, tag or commit of keploy/integrations"
    source="INTEGRATION_REF"
  fi
  git -C "$dir" checkout --quiet --detach "$sha"
  echo "keploy/integrations ${ref} at ${sha} (from ${source})"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    {
      echo "ref=${ref}"
      echo "source=${source}"
      echo "sha=${sha}"
    } >>"$GITHUB_OUTPUT"
  fi
}

# Prints one line per failing package of a `go test` log, in the order go
# printed them: the package (relative to the integrations module), then its
# failing tests and subtests. A package's --- FAIL lines come before its FAIL
# line, as go prints each package's output in one piece.
failures_of() {
  awk -v mod="$integrations_module/" '
    /^[[:space:]]*--- FAIL: / {
      name = $3
      tests = (tests == "" ? name : tests ", " name)
      next
    }
    /^FAIL[[:space:]]+[^[:space:]]/ {
      pkg = $2
      sub("^" mod, "", pkg)
      note = (index($0, "[") ? " " substr($0, index($0, "[")) : "")
      print pkg note (tests == "" ? "" : ": " tests)
      tests = ""
      next
    }
  ' "$1"
}

run_tests() {
  local keploy_dir integ_dir desc replaced mode rc failed=0 lines line ok_count flags
  keploy_dir="$(cd "$1" && pwd -P)" || die "no keploy tree at $1"
  integ_dir="$(cd "$2" && pwd -P)" || die "no keploy/integrations tree at $2"
  desc="${INTEGRATIONS_DESC:-keploy/integrations}"
  # shellcheck source=/dev/null
  source "$here/go-retry.sh"

  cd "$integ_dir"
  # A GOFLAGS such as -run=^$, set by an earlier step through GITHUB_ENV or
  # with go env -w, would change what go test runs: each package would pass
  # with no test run. So go runs with none: the env's is dropped, and go's env
  # file, which go reads when the env has none, must set none either.
  unset GOFLAGS
  flags="$(go env GOFLAGS)" || die "go env GOFLAGS failed"
  [ -z "$flags" ] ||
    die "go's env file sets GOFLAGS \"${flags}\", which would change what go test runs; this job runs go with none"
  go mod edit -replace="${keploy_module}=${keploy_dir}" ||
    die "cannot replace ${keploy_module} in ${desc}'s go.mod"
  # Adds the sums of modules this keploy tree requires and integrations' pin
  # did not. GO_RETRY_DIRECT_FROM=99 keeps every attempt on the module proxy,
  # as setup-private-parsers' tidy does.
  GO_RETRY_DIRECT_FROM=99 go_retry mod tidy ||
    die "${desc} does not resolve its modules with ${keploy_module} replaced by this keploy tree (go mod tidy failed, above)"
  # A replace that is not in effect would test integrations' pinned keploy and
  # pass whatever this tree does.
  replaced="$(go list -m -f '{{with .Replace}}{{.Path}}{{end}}' "$keploy_module" 2>&1)" || true
  [ "$replaced" = "$keploy_dir" ] ||
    die "${desc} does not take ${keploy_module} from this keploy tree (its replace reads \"${replaced}\")"

  # Global, not local: the EXIT trap runs after this function has returned.
  test_log="$(mktemp)"
  trap 'rm -f "$test_log"' EXIT
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    printf '### %s tests against this keploy tree\n\n' "$desc" >>"$GITHUB_STEP_SUMMARY"
  fi
  for mode in "KEPLOY_PROVEN unset" "KEPLOY_PROVEN=1"; do
    echo "::group::go test ./... in ${desc}, ${mode}"
    if [ "$mode" = "KEPLOY_PROVEN=1" ]; then
      if KEPLOY_PROVEN=1 go test -count=1 ./... 2>&1 | tee "$test_log"; then rc=0; else rc=$?; fi
    else
      if env -u KEPLOY_PROVEN go test -count=1 ./... 2>&1 | tee "$test_log"; then rc=0; else rc=$?; fi
    fi
    echo "::endgroup::"
    lines="$(failures_of "$test_log")"
    # A package whose tests ran, not one that ran none ("[no tests to run]").
    ok_count="$(grep -E '^ok[[:space:]]' "$test_log" | grep -cvF '[no tests to run]' || true)"
    if [ "$rc" -eq 0 ] && [ "$ok_count" -gt 0 ]; then
      echo "PASS: ${desc}, ${mode}: ${ok_count} packages"
      if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
        printf -- '- %s: pass (%s packages)\n' "$mode" "$ok_count" >>"$GITHUB_STEP_SUMMARY"
      fi
      continue
    fi
    failed=1
    if [ "$rc" -eq 0 ]; then
      lines="go test ran no test in any package"
    elif [ -z "$lines" ]; then
      lines="go test exited ${rc} with no FAIL line; see its output in the group above"
    fi
    while IFS= read -r line; do
      echo "FAIL: ${desc}, ${mode}: ${line}"
      echo "::error title=keploy/integrations tests fail against this keploy tree (${mode})::${desc}: ${line}"
      if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
        printf -- '- %s: FAIL %s\n' "$mode" "$line" >>"$GITHUB_STEP_SUMMARY"
      fi
    done <<<"$lines"
  done
  if [ "$failed" -ne 0 ]; then
    echo "The tests named above are keploy/integrations' (${desc}), run with ${keploy_module} replaced by this keploy tree."
    echo "If the change is meant to, adapt integrations on a branch and name it in the PR body as 'Integration-Ref: <branch>'."
    return 1
  fi
}

# The packages setup-private-parsers' extraparsers.go imports.
extra_parsers=(pkg/parsers pkg/postgres/v2/types pkg/postgres/v3)

run_parsers_row() {
  local keploy_dir integ_dir desc replaced flags rc pkg deps lines line
  keploy_dir="$(cd "$1" && pwd -P)" || die "no keploy tree at $1"
  integ_dir="$(cd "$2" && pwd -P)" || die "no keploy/integrations tree at $2"
  desc="${INTEGRATIONS_DESC:-keploy/integrations}"
  # shellcheck source=/dev/null
  source "$here/go-retry.sh"

  cd "$keploy_dir"
  # As in run_tests: a GOFLAGS such as -run=^$ would pass the row unrun.
  unset GOFLAGS
  flags="$(go env GOFLAGS)" || die "go env GOFLAGS failed"
  [ -z "$flags" ] ||
    die "go's env file sets GOFLAGS \"${flags}\", which would change what go test runs; this job runs go with none"
  go mod edit -require="${integrations_module}@v0.0.0-00010101000000-000000000000" \
    -replace="${integrations_module}=${integ_dir}" ||
    die "cannot require ${integrations_module} from ${desc} in keploy's go.mod"
  {
    echo "package proxy"
    echo
    echo "import ("
    for pkg in "${extra_parsers[@]}"; do
      echo "	_ \"${integrations_module}/${pkg}\""
    done
    echo ")"
  } >pkg/agent/proxy/extraparsers.go || die "cannot write pkg/agent/proxy/extraparsers.go"
  GO_RETRY_DIRECT_FROM=99 go_retry mod tidy ||
    die "keploy does not resolve its modules with ${desc}'s parsers added (go mod tidy failed, above)"
  replaced="$(go list -m -f '{{with .Replace}}{{.Path}}{{end}}' "$integrations_module" 2>&1)" || true
  [ "$replaced" = "$integ_dir" ] ||
    die "keploy does not take ${integrations_module} from ${desc} (its replace reads \"${replaced}\")"
  # The row runs over the parsers this build registers: with none of
  # integrations' linked, it would check the OSS parsers again and pass.
  deps="$(go list -deps ./pkg/agent/proxy/ 2>&1)" || die "go list -deps ./pkg/agent/proxy/ failed: ${deps}"
  for pkg in "${extra_parsers[@]}"; do
    grep -qxF "${integrations_module}/${pkg}" <<<"$deps" ||
      die "pkg/agent/proxy does not link ${integrations_module}/${pkg} from ${desc}, so the row would not run over its parsers"
  done

  test_log="$(mktemp)"
  trap 'rm -f "$test_log"' EXIT
  echo "::group::go test -run ^${stamp_row}\$ ./pkg/agent/proxy/ with ${desc}'s parsers"
  if env -u KEPLOY_PROVEN go test -count=1 -v -run "^${stamp_row}\$" ./pkg/agent/proxy/ 2>&1 | tee "$test_log"; then rc=0; else rc=$?; fi
  echo "::endgroup::"
  if [ "$rc" -eq 0 ] && grep -qE "^--- PASS: ${stamp_row} " "$test_log"; then
    line="$(grep -oE 'registered parsers: \[[^]]*\]' "$test_log" | head -n 1)"
    echo "PASS: ${stamp_row} with ${desc}'s parsers (${line:-registered parsers not printed})"
    if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
      printf -- '- %s with %s: pass (%s)\n' "$stamp_row" "$desc" "${line:-registered parsers not printed}" >>"$GITHUB_STEP_SUMMARY"
    fi
    return 0
  fi
  # Each failing subtest, the deepest first as go prints them.
  lines="$(grep -oE -- "--- FAIL: ${stamp_row}/[^ ]+" "$test_log" | sed 's/^--- FAIL: //' || true)"
  if [ -z "$lines" ]; then
    lines="go test exited ${rc} with no failing row of a parser; see its output in the group above"
  fi
  while IFS= read -r line; do
    echo "FAIL: ${desc}'s parsers: ${line}"
    echo "::error title=a recorder emits a mock with no request time::${desc}'s parsers: ${line}"
    if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
      printf -- '- %s with %s: FAIL %s\n' "$stamp_row" "$desc" "$line" >>"$GITHUB_STEP_SUMMARY"
    fi
  done <<<"$lines"
  echo "Each row named above records one exchange of its parser's protocol through that parser's recorder, in keploy's tree with ${desc}'s parsers, and checks that every mock it emits carries its request time (Spec.ReqTimestampMock)."
  return 1
}

case "${1:-}" in
resolve)
  [ "$#" -eq 2 ] || die "usage: $0 resolve INTEGRATIONS_DIR"
  resolve "$2"
  ;;
test)
  [ "$#" -eq 3 ] || die "usage: $0 test KEPLOY_DIR INTEGRATIONS_DIR"
  run_tests "$2" "$3"
  ;;
parsers)
  [ "$#" -eq 3 ] || die "usage: $0 parsers KEPLOY_DIR INTEGRATIONS_DIR"
  run_parsers_row "$2" "$3"
  ;;
*)
  die "usage: $0 resolve INTEGRATIONS_DIR | test KEPLOY_DIR INTEGRATIONS_DIR | parsers KEPLOY_DIR INTEGRATIONS_DIR"
  ;;
esac

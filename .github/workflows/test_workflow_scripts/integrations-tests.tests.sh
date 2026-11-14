#!/usr/bin/env bash
# Tests for integrations-tests.sh, against a stand-in go and gh and a real git
# repository built here, and for integration-ref.sh and integration-ref.ps1,
# which read a PR body's Integration-Ref for it and for setup-private-parsers.
# Needs bash, git, awk and pwsh (GitHub's Ubuntu runners have it); no network,
# no Go toolchain.
# Run by integrations-tests.yml before the real run, or locally:
#
#   bash .github/workflows/test_workflow_scripts/integrations-tests.tests.sh
#
# resolve: which integrations ref a run tests, in setup-private-parsers'
#   order (the PR body's Integration-Ref, a ref named like the PR's head,
#   INTEGRATION_REF), that a name is looked up as go get looks it up (a tag,
#   then a branch, then a commit or a pseudo-version's commit), that a ref the
#   body names must exist, that refs with
#   characters outside the allow-list are refused, that the body is read again
#   from the API with a bounded retry, and that the clone is left detached at
#   the commit it names.
# test: that both switches run, the first without KEPLOY_PROVEN even when the
#   caller exports it, the second with KEPLOY_PROVEN=1; that a failure under
#   either fails the run and names the package, its failing tests and the
#   switch, after the other switch has run too; that a failing go test with no
#   FAIL line, a passing one that tested no package, a failed tidy and a
#   replace that is not in effect each fail it.
# parsers: that keploy's tree gets the integrations module from the clone by a
#   replace and the parser packages setup-private-parsers imports, in an
#   extraparsers.go that imports the same ones; that the request-time row runs
#   once, with KEPLOY_PROVEN unset and no GOFLAGS, and names each parser whose
#   row failed; that a row not run, a build that does not link a parser
#   package, a failed tidy and a replace not in effect each fail it.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/integrations-tests.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
# Outside any git repository, so a broken resolve cannot touch the one it runs
# from.
cd "$work" || exit 1
failures=0
cases=0

fail() {
  echo "FAIL [$case_name]: $*"
  echo "----- output:"
  cat "$work/out"
  echo "-----"
  failures=$((failures + 1))
}
expect_rc() { [ "$rc" -eq "$1" ] || fail "exit $rc, want $1"; }
expect_has() { grep -qF -- "$1" "$work/out" || fail "output lacks: $1"; }
expect_lacks() { ! grep -qF -- "$1" "$work/out" || fail "output has: $1"; }
expect_line() { grep -qxF -- "$1" "$work/out" || fail "output lacks the line: $1"; }

# ---- stand-ins -------------------------------------------------------------
bin="$work/bin"
mkdir -p "$bin"
cat >"$bin/go" <<'EOF'
#!/usr/bin/env bash
# Stand-in go: records each call with the KEPLOY_PROVEN and GOFLAGS it saw.
s="$STUB_STATE"
echo "go $* KEPLOY_PROVEN=${KEPLOY_PROVEN-<unset>}" >>"$s/calls"
echo "go $1 GOFLAGS=${GOFLAGS-<unset>}" >>"$s/goflags"
case "$1 $2" in
"env GOFLAGS")
  # As go does: the env's GOFLAGS, else the env file's (goenv.flags).
  if [ -n "${GOFLAGS:-}" ]; then echo "$GOFLAGS"; else cat "$s/goenv.flags" 2>/dev/null; echo; fi
  exit 0
  ;;
"mod edit")
  for a in "$@"; do
    case "$a" in -replace=*) r="${a#-replace=}" && printf '%s' "${r#*=}" >"$s/replace" ;; esac
  done
  exit "$(cat "$s/edit.rc" 2>/dev/null || echo 0)"
  ;;
"list -deps")
  if [ -f "$s/deps.out" ]; then cat "$s/deps.out"; else printf '%s\n' go.keploy.io/server/v3/pkg/models \
    github.com/keploy/integrations/pkg/parsers github.com/keploy/integrations/pkg/postgres/v2/types \
    github.com/keploy/integrations/pkg/postgres/v3 go.keploy.io/server/v3/pkg/agent/proxy; fi
  exit 0
  ;;
"mod tidy")
  exit "$(cat "$s/tidy.rc" 2>/dev/null || echo 0)"
  ;;
"list -m")
  if [ -f "$s/list.out" ]; then cat "$s/list.out"; else cat "$s/replace"; echo; fi
  exit "$(cat "$s/list.rc" 2>/dev/null || echo 0)"
  ;;
"test -count=1")
  if [ "${3:-}" = -v ]; then
    cat "$s/row.out"
    exit "$(cat "$s/row.rc")"
  fi
  m=unset
  [ "${KEPLOY_PROVEN-}" = 1 ] && m=proven
  cat "$s/test-$m.out"
  exit "$(cat "$s/test-$m.rc")"
  ;;
esac
echo "stand-in go: unexpected arguments: $*" >&2
exit 99
EOF
cat >"$bin/gh" <<'EOF'
#!/usr/bin/env bash
# Stand-in gh: fails its first $(cat gh.fails) calls, then prints gh.body.
s="$STUB_STATE"
echo "gh $*" >>"$s/gh.calls"
n=$(($(cat "$s/gh.n" 2>/dev/null || echo 0) + 1))
echo "$n" >"$s/gh.n"
if [ "$n" -le "$(cat "$s/gh.fails" 2>/dev/null || echo 0)" ]; then
  echo "HTTP 502: Bad Gateway" >&2
  exit 1
fi
cat "$s/gh.body"
EOF
cat >"$bin/sleep" <<'EOF'
#!/usr/bin/env bash
# Stand-in sleep: records the wait and returns at once.
echo "sleep $*" >>"$STUB_STATE/sleeps"
EOF
chmod +x "$bin/go" "$bin/gh" "$bin/sleep"
export PATH="$bin:$PATH" GO_RETRY_BACKOFF=0

# ---- test ------------------------------------------------------------------
pass_out=$'ok  \tgithub.com/keploy/integrations/pkg/parsers\t0.1s\n?   \tgithub.com/keploy/integrations/cmd/tool\t[no test files]\nok  \tgithub.com/keploy/integrations/pkg/postgres/v3/recorder\t0.4s'
fail_out=$'ok  \tgithub.com/keploy/integrations/pkg/parsers\t0.1s\n--- FAIL: TestLearn_AFreshWord (0.00s)\n    learn_test.go:10: FALSE GREEN?\n--- FAIL: TestServedView_AWord (0.01s)\n    --- FAIL: TestServedView_AWord/at_F (0.00s)\nFAIL\nFAIL\tgithub.com/keploy/integrations/pkg/mongo/v2\t2.1s\nok  \tgithub.com/keploy/integrations/pkg/postgres/v3/recorder\t0.4s\nFAIL'
build_out=$'# github.com/keploy/integrations/pkg/mongo/v2\npkg/mongo/v2/learn.go:1520:5: not enough arguments in call to learn.Named\nFAIL\tgithub.com/keploy/integrations/pkg/mongo/v2 [build failed]\nok  \tgithub.com/keploy/integrations/pkg/parsers\t0.1s\nFAIL'
two_out=$'--- FAIL: TestA (0.00s)\nFAIL\nFAIL\tgithub.com/keploy/integrations/pkg/a\t0.1s\n# github.com/keploy/integrations/pkg/b\npkg/b/b.go:1:1: undefined: x\nFAIL\tgithub.com/keploy/integrations/pkg/b [build failed]\nok  \tgithub.com/keploy/integrations/pkg/c\t0.1s\nFAIL'
none_out=$'?   \tgithub.com/keploy/integrations/cmd/lookup\t[no test files]'
# What go test prints when a -run or -skip matches no test in any package.
notests_out=$'ok  \tgithub.com/keploy/integrations/pkg/parsers\t0.01s [no tests to run]\n?   \tgithub.com/keploy/integrations/cmd/tool\t[no test files]\nok  \tgithub.com/keploy/integrations/pkg/postgres/v3/recorder\t0.02s [no tests to run]'

# test_case NAME UNSET_RC UNSET_OUT PROVEN_RC PROVEN_OUT: runs `test` with go
# test answering those under each switch.
test_case() {
  case_name="$1"
  cases=$((cases + 1))
  s="$work/state-$cases"
  mkdir -p "$s" "$work/k-$cases" "$work/i-$cases"
  printf '%s\n' "$3" >"$s/test-unset.out"
  echo "$2" >"$s/test-unset.rc"
  printf '%s\n' "$5" >"$s/test-proven.out"
  echo "$4" >"$s/test-proven.rc"
  [ -z "${TIDY_RC:-}" ] || echo "$TIDY_RC" >"$s/tidy.rc"
  [ -z "${EDIT_RC:-}" ] || echo "$EDIT_RC" >"$s/edit.rc"
  [ -z "${LIST_OUT+x}" ] || printf '%s\n' "$LIST_OUT" >"$s/list.out"
  [ -z "${LIST_RC:-}" ] || echo "$LIST_RC" >"$s/list.rc"
  [ -z "${GOENV_FLAGS:-}" ] || printf '%s' "$GOENV_FLAGS" >"$s/goenv.flags"
  : >"$work/summary"
  local summary=(GITHUB_STEP_SUMMARY="$work/summary")
  [ -z "${NO_SUMMARY:-}" ] || summary=(-u GITHUB_STEP_SUMMARY)
  env "${summary[@]}" STUB_STATE="$s" INTEGRATIONS_DESC="keploy/integrations main at abc1234" \
    bash "$script" test "$work/k-$cases" "$work/i-$cases" >"$work/out" 2>&1
  rc=$?
}

test_case "both switches pass" 0 "$pass_out" 0 "$pass_out"
expect_rc 0
expect_has "PASS: keploy/integrations main at abc1234, KEPLOY_PROVEN unset: 2 packages"
expect_has "PASS: keploy/integrations main at abc1234, KEPLOY_PROVEN=1: 2 packages"
expect_lacks "::error"
grep -qF -- "- KEPLOY_PROVEN=1: pass (2 packages)" "$work/summary" || fail "summary lacks the proven pass"
calls="$work/state-$cases/calls"
[ "$(grep -c '^go test ' "$calls")" -eq 2 ] || fail "go test ran $(grep -c '^go test ' "$calls") times, want 2"
grep -qxF "go test -count=1 ./... KEPLOY_PROVEN=<unset>" "$calls" || fail "no go test -count=1 ./... with KEPLOY_PROVEN unset: $(cat "$calls")"
grep -qxF "go test -count=1 ./... KEPLOY_PROVEN=1" "$calls" || fail "no go test -count=1 ./... with KEPLOY_PROVEN=1: $(cat "$calls")"
grep -qxF "go mod edit -replace=go.keploy.io/server/v3=$(cd "$work/k-$cases" && pwd -P) KEPLOY_PROVEN=<unset>" "$calls" ||
  fail "go.keploy.io/server/v3 is not replaced by the keploy tree: $(cat "$calls")"

export KEPLOY_PROVEN=1
test_case "the unset switch is unset even when the caller exports it" 0 "$pass_out" 0 "$pass_out"
unset KEPLOY_PROVEN
expect_rc 0
grep -qxF "go test -count=1 ./... KEPLOY_PROVEN=<unset>" "$work/state-$cases/calls" || fail "the unset run saw KEPLOY_PROVEN: $(cat "$work/state-$cases/calls")"

test_case "a failure under KEPLOY_PROVEN=1 is named" 0 "$pass_out" 1 "$fail_out"
expect_rc 1
expect_has "PASS: keploy/integrations main at abc1234, KEPLOY_PROVEN unset: 2 packages"
expect_line "FAIL: keploy/integrations main at abc1234, KEPLOY_PROVEN=1: pkg/mongo/v2: TestLearn_AFreshWord, TestServedView_AWord, TestServedView_AWord/at_F"
expect_has "::error title=keploy/integrations tests fail against this keploy tree (KEPLOY_PROVEN=1)::keploy/integrations main at abc1234: pkg/mongo/v2: TestLearn_AFreshWord"
expect_lacks "pkg/parsers:"
expect_lacks "KEPLOY_PROVEN unset: pkg/"
grep -qF -- "- KEPLOY_PROVEN=1: FAIL pkg/mongo/v2: TestLearn_AFreshWord" "$work/summary" || fail "summary lacks the failure"

test_case "a failure with KEPLOY_PROVEN unset is named, and the other switch still runs" 1 "$fail_out" 0 "$pass_out"
expect_rc 1
expect_has "FAIL: keploy/integrations main at abc1234, KEPLOY_PROVEN unset: pkg/mongo/v2: TestLearn_AFreshWord"
expect_has "PASS: keploy/integrations main at abc1234, KEPLOY_PROVEN=1: 2 packages"

test_case "failures under both switches are both named" 1 "$fail_out" 1 "$build_out"
expect_rc 1
expect_has "FAIL: keploy/integrations main at abc1234, KEPLOY_PROVEN unset: pkg/mongo/v2: TestLearn_AFreshWord"
expect_line "FAIL: keploy/integrations main at abc1234, KEPLOY_PROVEN=1: pkg/mongo/v2 [build failed]"
expect_has "run with go.keploy.io/server/v3 replaced by this keploy tree"

test_case "each failing package is named with its own tests" 0 "$pass_out" 1 "$two_out"
expect_rc 1
expect_line "FAIL: keploy/integrations main at abc1234, KEPLOY_PROVEN=1: pkg/a: TestA"
expect_line "FAIL: keploy/integrations main at abc1234, KEPLOY_PROVEN=1: pkg/b [build failed]"

test_case "a failing go test with no FAIL line still fails" 0 "$pass_out" 2 "go: updates to go.mod needed"
expect_rc 1
expect_has "KEPLOY_PROVEN=1: go test exited 2 with no FAIL line"

test_case "a passing go test that tested no package fails" 0 "$none_out" 0 "$pass_out"
expect_rc 1
expect_has "KEPLOY_PROVEN unset: go test ran no test in any package"

test_case "a passing go test that ran no test in any package fails" 0 "$pass_out" 0 "$notests_out"
expect_rc 1
expect_has "KEPLOY_PROVEN=1: go test ran no test in any package"
expect_has "PASS: keploy/integrations main at abc1234, KEPLOY_PROVEN unset: 2 packages"

# A GOFLAGS of -run=^$ would pass every package with no test run.
export GOFLAGS='-run=^$'
test_case "go runs with no GOFLAGS, even when the caller exports one" 0 "$pass_out" 0 "$pass_out"
unset GOFLAGS
expect_rc 0
flags_seen="$(sort -u "$work/state-$cases/goflags" | tr '\n' ' ')"
[ "$flags_seen" = "go env GOFLAGS=<unset> go list GOFLAGS=<unset> go mod GOFLAGS=<unset> go test GOFLAGS=<unset> " ] ||
  fail "go saw GOFLAGS: $flags_seen"

GOENV_FLAGS='-run=^$' test_case "a GOFLAGS in go's env file fails before any go command" 0 "$pass_out" 0 "$pass_out"
expect_rc 1
expect_has "go's env file sets GOFLAGS \"-run=^\$\", which would change what go test runs"
[ "$(cat "$work/state-$cases/calls")" = "go env GOFLAGS KEPLOY_PROVEN=<unset>" ] ||
  fail "go ran after a GOFLAGS in its env file: $(cat "$work/state-$cases/calls")"

TIDY_RC=1 test_case "a failed tidy fails before any test" 0 "$pass_out" 0 "$pass_out"
expect_rc 1
expect_has "go mod tidy failed"
! grep -q '^go test ' "$work/state-$cases/calls" || fail "go test ran after a failed tidy"

EDIT_RC=1 test_case "a failed go mod edit fails before any test" 0 "$pass_out" 0 "$pass_out"
expect_rc 1
expect_has "cannot replace go.keploy.io/server/v3"
! grep -q '^go test ' "$work/state-$cases/calls" || fail "go test ran after a failed go mod edit"

NO_SUMMARY=1 test_case "no step summary: a pass" 0 "$pass_out" 0 "$pass_out"
expect_rc 0
expect_has "PASS: keploy/integrations main at abc1234, KEPLOY_PROVEN=1: 2 packages"

NO_SUMMARY=1 test_case "no step summary: failures" 1 "$fail_out" 1 "$build_out"
expect_rc 1
expect_has "FAIL: keploy/integrations main at abc1234, KEPLOY_PROVEN unset: pkg/mongo/v2: TestLearn_AFreshWord"
expect_line "FAIL: keploy/integrations main at abc1234, KEPLOY_PROVEN=1: pkg/mongo/v2 [build failed]"

LIST_OUT="/somewhere/else" test_case "a replace by another tree fails" 0 "$pass_out" 0 "$pass_out"
expect_rc 1
expect_has "does not take go.keploy.io/server/v3 from this keploy tree (its replace reads \"/somewhere/else\")"
! grep -q '^go test ' "$work/state-$cases/calls" || fail "go test ran with the replace not in effect"

LIST_OUT="" test_case "no replace fails" 0 "$pass_out" 0 "$pass_out"
expect_rc 1
expect_has "does not take go.keploy.io/server/v3 from this keploy tree"

LIST_RC=1 LIST_OUT="go: module go.keploy.io/server/v3: not a known dependency" test_case "integrations not requiring keploy fails" 0 "$pass_out" 0 "$pass_out"
expect_rc 1
expect_has "not a known dependency"

# ---- resolve ---------------------------------------------------------------
origin="$work/origin"
git init -q -b main "$origin"
commit() { git -C "$origin" -c user.name=t -c user.email=t@t commit -q --allow-empty -m "$1" && git -C "$origin" rev-parse HEAD; }
c_main="$(commit main)"
git -C "$origin" checkout -q -b feat/x
c_x="$(commit x)"
git -C "$origin" checkout -q -b feat/pr-head main
c_head="$(commit head)"
git -C "$origin" checkout -q -b tagged main
c_tag="$(commit tagged)"
git -C "$origin" tag v1.0.0
# An annotated tag names a tag object, whose commit is the one to test.
git -C "$origin" -c user.name=t -c user.email=t@t tag -a -m annotated annotated "$c_head"
git -C "$origin" tag only-a-tag "$c_x"
git -C "$origin" checkout -q main
git -C "$origin" branch -q -D tagged
# A branch and a tag of one name, on different commits.
git -C "$origin" branch -q both "$c_head"
git -C "$origin" tag both "$c_x"

# resolve_case NAME BODY|- [ENV...]: runs `resolve` in a fresh clone, with gh
# answering BODY ("-": no PR).
resolve_case() {
  case_name="$1"
  cases=$((cases + 1))
  s="$work/state-$cases"
  mkdir -p "$s"
  clone="$work/clone-$cases"
  git clone -q "$origin" "$clone"
  local pr=""
  if [ "$2" != "-" ]; then
    printf '%s' "$2" >"$s/gh.body"
    pr=42
  fi
  [ -z "${GH_FAILS:-}" ] || echo "$GH_FAILS" >"$s/gh.fails"
  : >"$work/output"
  shift 2
  local out=(GITHUB_OUTPUT="$work/output")
  [ -z "${NO_OUTPUT:-}" ] || out=(-u GITHUB_OUTPUT)
  env -u PR_NUMBER -u HEAD_REF -u INTEGRATION_REF "${out[@]}" STUB_STATE="$s" GITHUB_REPOSITORY=keploy/keploy \
    PR_NUMBER="$pr" "$@" bash "$script" resolve "$clone" >"$work/out" 2>&1
  rc=$?
}
expect_at() {
  local got
  got="$(git -C "$clone" rev-parse HEAD)"
  [ "$got" = "$1" ] || fail "checked out $got, want $1"
  git -C "$clone" symbolic-ref -q HEAD >/dev/null && fail "HEAD is on a branch, want detached"
  grep -qxF "sha=$1" "$work/output" || fail "GITHUB_OUTPUT lacks sha=$1: $(cat "$work/output")"
}

resolve_case "no PR: INTEGRATION_REF" - INTEGRATION_REF=main
expect_rc 0
expect_at "$c_main"
expect_has "keploy/integrations main at $c_main (from INTEGRATION_REF)"
grep -qxF "ref=main" "$work/output" || fail "GITHUB_OUTPUT lacks ref=main"
grep -qxF "source=INTEGRATION_REF" "$work/output" || fail "GITHUB_OUTPUT lacks source=INTEGRATION_REF"
[ ! -e "$s/gh.calls" ] || fail "gh was called with no PR"

resolve_case "no PR, no INTEGRATION_REF: main" -
expect_rc 0
expect_at "$c_main"

resolve_case "the body's Integration-Ref wins over the head branch" $'Fixes it.\r\n\r\n- integration-REF:  feat/x  and more\r\nIntegration-Ref: feat/pr-head\r\n' HEAD_REF=feat/pr-head INTEGRATION_REF=main
expect_rc 0
expect_at "$c_x"
expect_has "keploy/integrations feat/x at $c_x (from the PR body's Integration-Ref)"
grep -qxF "gh api repos/keploy/keploy/pulls/42 --jq .body // \"\"" "$s/gh.calls" || fail "gh was not asked for PR 42's body: $(cat "$s/gh.calls")"

resolve_case "a CRLF body whose ref ends its line" $'Integration-Ref: feat/x\r\nmore\r\n' INTEGRATION_REF=main
expect_rc 0
expect_at "$c_x"

resolve_case "the body names a tag" $'Integration-Ref: v1.0.0\n'
expect_rc 0
expect_at "$c_tag"

resolve_case "the body names a commit" "Integration-Ref: $c_x"
expect_rc 0
expect_at "$c_x"

resolve_case "the body names a short commit" "> Integration-Ref: ${c_head:0:9}" INTEGRATION_REF=main
expect_rc 0
expect_at "$c_head"

resolve_case "the body names a commit by 7 digits, the fewest go get takes" "Integration-Ref: ${c_head:0:7}" INTEGRATION_REF=main
expect_rc 0
expect_at "$c_head"

resolve_case "the body names an annotated tag" $'Integration-Ref: annotated\n' INTEGRATION_REF=main
expect_rc 0
expect_at "$c_head"

resolve_case "a ref the body names that does not exist fails" $'Integration-Ref: feat/gone\n' HEAD_REF=feat/pr-head INTEGRATION_REF=main
expect_rc 1
expect_has "the PR body's Integration-Ref \"feat/gone\" names no branch, tag or commit of keploy/integrations"

resolve_case "a commit the body names that does not exist fails" $'Integration-Ref: deadbeefdeadbeef\n' INTEGRATION_REF=main
expect_rc 1
expect_has "names no branch, tag or commit"

resolve_case "a body ref outside the allow-list fails" $'Integration-Ref: feat/x;rm\n' INTEGRATION_REF=main
expect_rc 1
expect_has "Integration-Ref \"feat/x;rm\" has characters outside"

resolve_case "a branch named like the PR's head" $'No ref line here.\n' HEAD_REF=feat/pr-head INTEGRATION_REF=main
expect_rc 0
expect_at "$c_head"
expect_has "(from the keploy/integrations ref named like this PR's branch)"

resolve_case "a head branch integrations does not have falls to INTEGRATION_REF" "" HEAD_REF=feat/absent INTEGRATION_REF=main
expect_rc 0
expect_at "$c_main"

resolve_case "a head name that is a tag there pairs with the tag, as go get does" "" HEAD_REF=only-a-tag INTEGRATION_REF=main
expect_rc 0
expect_at "$c_x"

# go get and git rev-parse both take the tag of a name that is a tag and a
# branch.
both_want="$(git -C "$origin" rev-parse --quiet --verify 'both^{commit}' 2>/dev/null)"
resolve_case "a name that is a tag and a branch is the tag, as git rev-parse has it" $'Integration-Ref: both\n' INTEGRATION_REF=main
[ "$both_want" = "$c_x" ] || fail "git rev-parse both: $both_want, want the tag's $c_x"
expect_rc 0
expect_at "$both_want"

resolve_case "a head name that is a tag and a branch is the tag" "" HEAD_REF=both INTEGRATION_REF=main
expect_rc 0
expect_at "$c_x"

resolve_case "INTEGRATION_REF that is a tag and a branch is the tag" - INTEGRATION_REF=both
expect_rc 0
expect_at "$c_x"

resolve_case "the body names a pseudo-version" "Integration-Ref: v0.0.0-20261109182141-${c_x:0:12}" INTEGRATION_REF=main
expect_rc 0
expect_at "$c_x"

resolve_case "the body names a pre-release pseudo-version" "Integration-Ref: v1.2.4-0.20261109182141-${c_head:0:12}" INTEGRATION_REF=main
expect_rc 0
expect_at "$c_head"

resolve_case "a pseudo-version of a commit integrations lacks fails" $'Integration-Ref: v0.0.0-20261109182141-deadbeefdead\n' INTEGRATION_REF=main
expect_rc 1
expect_has "Integration-Ref \"v0.0.0-20261109182141-deadbeefdead\" names no branch, tag or commit"

resolve_case "a pseudo-version with a short hash is no pseudo-version" "Integration-Ref: v0.0.0-20261109182141-${c_x:0:11}" INTEGRATION_REF=main
expect_rc 1
expect_has "names no branch, tag or commit"

resolve_case "INTEGRATION_REF names a tag" - INTEGRATION_REF=v1.0.0
expect_rc 0
expect_at "$c_tag"

resolve_case "INTEGRATION_REF names a short commit" - INTEGRATION_REF="${c_x:0:8}"
expect_rc 0
expect_at "$c_x"

resolve_case "INTEGRATION_REF that does not exist fails" - INTEGRATION_REF=feat/gone
expect_rc 1
expect_has "INTEGRATION_REF \"feat/gone\" names no branch, tag or commit"

resolve_case "INTEGRATION_REF outside the allow-list fails" - 'INTEGRATION_REF=main;x'
expect_rc 1
expect_has "INTEGRATION_REF \"main;x\" has characters outside"

GH_FAILS=2 resolve_case "the body is read on the third try" $'Integration-Ref: feat/x\n' INTEGRATION_REF=main
expect_rc 0
expect_at "$c_x"
[ "$(wc -l <"$s/gh.calls")" -eq 3 ] || fail "gh called $(wc -l <"$s/gh.calls") times, want 3"
[ "$(cat "$s/sleeps")" = $'sleep 5\nsleep 5' ] || fail "waits between tries: $(cat "$s/sleeps"), want two of 5 s"

GH_FAILS=3 resolve_case "a body that cannot be read fails" $'Integration-Ref: feat/x\n' INTEGRATION_REF=main
expect_rc 1
expect_has "cannot read the body of PR #42"
[ "$(wc -l <"$s/gh.calls")" -eq 3 ] || fail "gh called $(wc -l <"$s/gh.calls") times, want 3"
[ "$(wc -l <"$s/sleeps")" -eq 2 ] || fail "waited $(wc -l <"$s/sleeps") times, want 2: none after the last try"

resolve_case "a ref only the clone's own names resolve fails" $'Integration-Ref: origin/feat/x\n' INTEGRATION_REF=main
expect_rc 1
expect_has "Integration-Ref \"origin/feat/x\" names no branch, tag or commit"

resolve_case "a commit prefix under 7 digits fails" "Integration-Ref: ${c_x:0:6}" INTEGRATION_REF=main
expect_rc 1
expect_has "names no branch, tag or commit"

NO_OUTPUT=1 resolve_case "no GITHUB_OUTPUT" - INTEGRATION_REF=main
expect_rc 0
expect_has "keploy/integrations main at $c_main (from INTEGRATION_REF)"

# ---- integration-ref.sh and integration-ref.ps1 ------------------------------
# body_case NAME BODY WANT: both readers give WANT for BODY ("" for none).
# shellcheck source=/dev/null
source "$here/integration-ref.sh"
body_case() {
  case_name="$1"
  cases=$((cases + 1))
  local got
  : >"$work/out"
  got="$(integration_ref_of_body "$2")"
  [ "$got" = "$3" ] || fail "integration-ref.sh read [$got], want [$3]"
  if ! command -v pwsh >/dev/null; then
    fail "pwsh is not installed, so integration-ref.ps1 is not tested"
    return
  fi
  # shellcheck disable=SC2016 # PowerShell text
  got="$(BODY="$2" pwsh -NoProfile -NonInteractive -Command \
    '. $env:INTEGRATION_REF_PS1; $r = Get-IntegrationRefOfBody $env:BODY; if ($null -eq $r) { "<none>" } else { "[$r]" }' 2>&1)" ||
    got="pwsh failed: $got"
  local want="<none>"
  [ -z "$3" ] || want="[$3]"
  [ "$got" = "$want" ] || fail "integration-ref.ps1 read $got, want $want"
}
export INTEGRATION_REF_PS1="$here/integration-ref.ps1"

# setup-private-parsers reads the body with these two, not a copy of its own.
case_name="setup-private-parsers reads the body with integration-ref.sh and .ps1"
cases=$((cases + 1))
action="$here/../../actions/setup-private-parsers/action.yaml"
: >"$work/out"
# shellcheck disable=SC2016 # the text the action holds
for want in 'source "${GITHUB_WORKSPACE}/.github/workflows/test_workflow_scripts/integration-ref.sh"' \
  'BODY_REF="$(integration_ref_of_body "$PR_BODY")"' \
  '. "$env:GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/integration-ref.ps1"' \
  '$BodyRef = Get-IntegrationRefOfBody $env:PR_BODY'; do
  [ "$(grep -cxF -- "        $want" "$action")" = 1 ] || fail "action.yaml does not hold, once: $want"
done
! grep -iE '(grep|imatch).*integration-ref' "$action" >"$work/out" ||
  fail "action.yaml still matches Integration-Ref lines itself"
body_case "a CRLF body whose ref ends its line" $'Fixes it.\r\nIntegration-Ref: feat/x\r\nmore\r\n' feat/x
body_case "a CRLF body whose ref line is its last" $'Fixes it.\r\nIntegration-Ref: feat/x\r' feat/x
body_case "a CR alone ends no ref" $'Integration-Ref:\r\nfeat/x\r\n' ""
body_case "a lone CR ends a line" $'Fixes it.\rIntegration-Ref: feat/x\rmore' feat/x
body_case "an LF body" $'Fixes it.\n\nIntegration-Ref: feat/x\n' feat/x
body_case "the first word only" $'Integration-Ref: main other-tag\n' main
body_case "a later colon on the line is not the ref's" $'Integration-Ref: feat/x (see: y)\n' feat/x
body_case "the first Integration-Ref line wins" $'- integration-REF:  feat/x  and more\nIntegration-Ref: feat/y\n' feat/x
body_case "a quote marker and spaces before the colon" $'  > INTEGRATION-REF :feat/x\n' feat/x
body_case "a tag in a sentence is no ref" $'See Integration-Ref: feat/x\n' ""
body_case "no ref line" $'Fixes it.\n' ""
body_case "an empty body" "" ""
unset INTEGRATION_REF_PS1

# ---- parsers ---------------------------------------------------------------
row=TestEveryRegisteredParsersRecorderStampsEachMocksRequestTime
row_pass=$'=== RUN   '"$row"$'\n    parser_request_time_test.go:53: registered parsers: [generic grpc http http2 mongo_v2 mysql postgres_v3]\n--- PASS: '"$row"$' (9.10s)\n    --- PASS: '"$row"$'/http (2.01s)\nPASS\nok  \tgo.keploy.io/server/v3/pkg/agent/proxy\t9.2s'
row_fail=$'=== RUN   '"$row"$'\n            --- FAIL: '"$row"$'/postgres_v3/KEPLOY_PROVEN=1 (0.50s)\n        --- FAIL: '"$row"$'/postgres_v3 (1.00s)\n--- FAIL: '"$row"$' (9.10s)\nFAIL\nFAIL\tgo.keploy.io/server/v3/pkg/agent/proxy\t9.2s\nFAIL'
row_none=$'ok  \tgo.keploy.io/server/v3/pkg/agent/proxy\t0.02s [no tests to run]'

# parsers_case NAME ROW_RC ROW_OUT: runs `parsers` with go test answering those.
parsers_case() {
  case_name="$1"
  cases=$((cases + 1))
  s="$work/state-$cases"
  mkdir -p "$s" "$work/k-$cases/pkg/agent/proxy" "$work/i-$cases"
  printf '%s\n' "$3" >"$s/row.out"
  echo "$2" >"$s/row.rc"
  [ -z "${TIDY_RC:-}" ] || echo "$TIDY_RC" >"$s/tidy.rc"
  [ -z "${LIST_OUT+x}" ] || printf '%s\n' "$LIST_OUT" >"$s/list.out"
  [ -z "${DEPS_OUT+x}" ] || printf '%s\n' "$DEPS_OUT" >"$s/deps.out"
  : >"$work/summary"
  env GITHUB_STEP_SUMMARY="$work/summary" STUB_STATE="$s" INTEGRATIONS_DESC="keploy/integrations main at abc1234" \
    bash "$script" parsers "$work/k-$cases" "$work/i-$cases" >"$work/out" 2>&1
  rc=$?
}

export KEPLOY_PROVEN=1 GOFLAGS='-run=^$'
parsers_case "the row passes with the integrations parsers" 0 "$row_pass"
unset KEPLOY_PROVEN GOFLAGS
expect_rc 0
expect_has "PASS: $row with keploy/integrations main at abc1234's parsers (registered parsers: [generic grpc http http2 mongo_v2 mysql postgres_v3])"
expect_lacks "::error"
grep -qF -- "- $row with keploy/integrations main at abc1234: pass" "$work/summary" || fail "summary lacks the pass"
calls="$work/state-$cases/calls"
[ "$(grep -c '^go test ' "$calls")" -eq 1 ] || fail "go test ran $(grep -c '^go test ' "$calls") times, want 1"
grep -qxF "go test -count=1 -v -run ^$row\$ ./pkg/agent/proxy/ KEPLOY_PROVEN=<unset>" "$calls" ||
  fail "no go test of the row with KEPLOY_PROVEN unset: $(cat "$calls")"
[ "$(sort -u "$work/state-$cases/goflags" | grep -cv 'GOFLAGS=<unset>$')" -eq 0 ] || fail "go saw GOFLAGS: $(sort -u "$work/state-$cases/goflags")"
idir="$(cd "$work/i-$cases" && pwd -P)"
grep -qF "go mod edit -require=github.com/keploy/integrations@v0.0.0-00010101000000-000000000000 -replace=github.com/keploy/integrations=$idir KEPLOY_PROVEN=" "$calls" ||
  fail "github.com/keploy/integrations is not required from the clone: $(cat "$calls")"
# The parser packages are the ones setup-private-parsers' extraparsers.go
# imports, on every OS it builds for.
got_imports="$(grep -oE '_ "github.com/keploy/integrations/[^"]+"' "$work/k-$cases/pkg/agent/proxy/extraparsers.go" | sort | tr '\n' ' ')"
action_imports="$(grep -oE '_ "github.com/keploy/integrations/[^"]+"' "$here/../../actions/setup-private-parsers/action.yaml" | sort -u | tr '\n' ' ')"
[ -n "$got_imports" ] && [ "$got_imports" = "$action_imports" ] ||
  fail "extraparsers.go imports '$got_imports', setup-private-parsers' '$action_imports'"
head -n 1 "$work/k-$cases/pkg/agent/proxy/extraparsers.go" | grep -qxF "package proxy" || fail "extraparsers.go is not package proxy"

parsers_case "a parser's failing row is named" 1 "$row_fail"
expect_rc 1
expect_line "FAIL: keploy/integrations main at abc1234's parsers: $row/postgres_v3/KEPLOY_PROVEN=1"
expect_line "FAIL: keploy/integrations main at abc1234's parsers: $row/postgres_v3"
expect_has "::error title=a recorder emits a mock with no request time::keploy/integrations main at abc1234's parsers: $row/postgres_v3"
grep -qF -- "FAIL $row/postgres_v3" "$work/summary" || fail "summary lacks the failure"

parsers_case "a failing go test with no failing row still fails" 2 "go: updates to go.mod needed"
expect_rc 1
expect_has "go test exited 2 with no failing row of a parser"

parsers_case "a passing go test that ran no row fails" 0 "$row_none"
expect_rc 1
expect_has "go test exited 0 with no failing row of a parser"

DEPS_OUT=$'github.com/keploy/integrations/pkg/parsers\ngithub.com/keploy/integrations/pkg/postgres/v2/types' \
  parsers_case "a build that does not link a parser package fails before the row" 0 "$row_pass"
expect_rc 1
expect_has "pkg/agent/proxy does not link github.com/keploy/integrations/pkg/postgres/v3"
! grep -q '^go test ' "$work/state-$cases/calls" || fail "go test ran with a parser package not linked"

TIDY_RC=1 parsers_case "a failed tidy fails before the row" 0 "$row_pass"
expect_rc 1
expect_has "go mod tidy failed"
! grep -q '^go test ' "$work/state-$cases/calls" || fail "go test ran after a failed tidy"

LIST_OUT="/somewhere/else" parsers_case "a replace by another tree fails" 0 "$row_pass"
expect_rc 1
expect_has "does not take github.com/keploy/integrations from keploy/integrations main at abc1234 (its replace reads \"/somewhere/else\")"
! grep -q '^go test ' "$work/state-$cases/calls" || fail "go test ran with the replace not in effect"

# ---- usage -----------------------------------------------------------------
case_name="no command"
cases=$((cases + 1))
bash "$script" >"$work/out" 2>&1
rc=$?
expect_rc 1
expect_has "usage:"
case_name="resolve with no directory"
cases=$((cases + 1))
bash "$script" resolve >"$work/out" 2>&1
rc=$?
expect_rc 1
expect_has "usage:"
case_name="test with one directory"
cases=$((cases + 1))
bash "$script" test "$work" >"$work/out" 2>&1
rc=$?
expect_rc 1
expect_has "usage:"
case_name="parsers with one directory"
cases=$((cases + 1))
bash "$script" parsers "$work" >"$work/out" 2>&1
rc=$?
expect_rc 1
expect_has "usage:"

if [ "$failures" -ne 0 ]; then
  echo "integrations-tests.sh: $failures failed checks in $cases cases"
  exit 1
fi
echo "integrations-tests.sh: all $cases cases pass"

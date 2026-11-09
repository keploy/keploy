#!/usr/bin/env bash
# Checks of integrations-tests.yml and of release.yml's gate on it: which runs
# the job runs on, that a run with no key to keploy/integrations fails, that
# nothing lets the job pass without running the tests (continue-on-error, an
# if: or a changed command on the steps that matter), which runs a new push
# cancels, which runs save the cache, that release.yml's publishing jobs wait
# for the job and do not run after it failed, and that setup-private-parsers'
# build lanes read the PR body from the event. Needs bash and a POSIX awk. Run
# by integrations-tests.yml before the real run, or locally:
#
#   bash .github/workflows/test_workflow_scripts/integrations-tests.workflows.tests.sh
#
# flatten reads the YAML forms these two files use (block mappings, block
# lists, one-line values, block scalars it skips) into path/value lines, and
# evaluate reads the ${{ }} expression forms they use (string, number,
# true/false/null literals, context paths, !, ==, !=, &&, ||, parentheses,
# the status functions, contains, startsWith and endsWith) under a context
# written here for each kind of run. An `if:` with no status function gets
# GitHub's implied success() &&. Neither is a YAML parser or GitHub's
# evaluator: they are a tripwire for mistakes in those forms, and a form they
# cannot read fails a check rather than passing it.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
job_yml="$here/../integrations-tests.yml"
release_yml="$here/../release.yml"
failures=0
cases=0
problems=''

problem() { problems="$problems       $1"$'\n'; }
report() {
  cases=$((cases + 1))
  if [ -z "$problems" ]; then
    echo "ok   - $1"
  else
    failures=$((failures + 1))
    echo "FAIL - $1"
    printf '%s' "$problems"
  fi
  problems=''
}

# flatten FILE: prints "path<TAB>value" for every key and list item, list
# items numbered from 0 under their parent (jobs.x.steps.3.if). A key that
# opens a block prints an empty value; a block scalar (run: |) prints its
# indicator and its lines are skipped; a plain scalar continued on the next
# lines is joined with spaces, as YAML folds it. Comments are dropped.
# shellcheck disable=SC2016 # awk program text, not shell
flatten_awk='
function strip(v) {
  sub(/[[:space:]]+#.*$/, "", v)
  sub(/^[[:space:]]+/, "", v)
  sub(/[[:space:]]+$/, "", v)
  return v
}
function flush() {
  if (have) print held_path "\t" held_val
  have = 0
}
function pop(ind, item_ok) {
  while (depth > 0 && (ind_of[depth] > ind || (ind_of[depth] == ind && !(item_ok && !item_of[depth])))) depth--
}
function parent(   i, p) {
  p = ""
  for (i = 1; i <= depth; i++) p = p (i > 1 ? "." : "") key_of[i]
  return p
}
function push(ind, key, item,   p) {
  p = parent()
  depth++
  ind_of[depth] = ind
  key_of[depth] = key
  item_of[depth] = item
  return (p == "" ? key : p "." key)
}
function hold(path, val) {
  flush()
  held_path = path
  held_val = val
  have = 1
  held_ind = ind_of[depth]
  if (val ~ /^[|>][-+0-9]*$/) scalar_ind = ind_of[depth]
}
BEGIN { scalar_ind = -1; depth = 0; have = 0 }
{
  line = $0
  match(line, /^ */)
  ind = RLENGTH
  if (scalar_ind >= 0) {
    if (line ~ /^[[:space:]]*$/ || ind > scalar_ind) next
    scalar_ind = -1
  }
  if (line ~ /^[[:space:]]*(#.*)?$/) next
  rest = substr(line, ind + 1)
  if (have && ind > held_ind && rest !~ /^- / && rest != "-" && rest !~ /^[A-Za-z0-9_"'"'"'][^:]*:( |$)/) {
    held_val = held_val " " strip(rest)
    next
  }
  item = 0
  while (rest ~ /^-( |$)/) {
    pop(ind, 1)
    p = parent()
    idx = count[p]++
    path = push(ind, idx, 1)
    match(rest, /^- */)
    ind += RLENGTH
    rest = substr(rest, RLENGTH + 1)
    item = 1
    if (rest == "") { hold(path, ""); next }
  }
  if (match(rest, /^[A-Za-z0-9_][A-Za-z0-9_.-]*:( |$)/) || match(rest, /^"[^"]*":( |$)/) || match(rest, /^'"'"'[^'"'"']*'"'"':( |$)/)) {
    key = substr(rest, 1, RLENGTH)
    val = strip(substr(rest, RLENGTH + 1))
    sub(/:( )?$/, "", key)
    gsub(/^["'"'"']|["'"'"']$/, "", key)
    pop(ind, 0)
    hold(push(ind, key, 0), val)
    next
  }
  if (item) { hold(path, strip(rest)); next }
  print "flatten: cannot read line " NR ": " line > "/dev/stderr"
  bad = 1
  exit 2
}
END { if (!bad) flush() }
'
flatten() { awk "$flatten_awk" "$1"; }

# get FLAT PATH: the value at PATH, or nothing with exit 1 when PATH is absent.
get() { awk -F '\t' -v p="$2" '$1 == p { print substr($0, length($1) + 2); found = 1; exit } END { exit !found }' <<<"$1"; }
# has FLAT PATH: whether PATH or anything under it is present.
has() { awk -F '\t' -v p="$2" '$1 == p || index($1, p ".") == 1 { found = 1; exit } END { exit !found }' <<<"$1"; }
# children FLAT PATH: the keys directly under PATH ("": the top-level keys),
# one per line.
children() {
  awk -F '\t' -v p="${2:+$2.}" '(p == "" || index($1, p) == 1) { k = substr($1, length(p) + 1); sub(/\..*/, "", k); if (!(k in seen)) { seen[k] = 1; print k } }' <<<"$1"
}
# step FLAT JOB NAME: the path of JOB's step with that name.
step() {
  awk -F '\t' -v j="jobs.$2.steps." -v n="$3" 'index($1, j) == 1 && substr($1, length(j) + 1) ~ /^[0-9]+\.name$/ && substr($0, length($1) + 2) == n { print substr($1, 1, length($1) - 5); found = 1; exit } END { exit !found }' <<<"$1"
}
# list VALUE: the items of a [a, b] or one-word value, one per line.
list() { printf '%s\n' "$1" | sed -e 's/^\[//' -e 's/\]$//' | tr ',' '\n' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e '/^$/d'; }
# needs FLAT JOB: JOB's needs, whether written [a, b], one word or a block list.
needs() {
  local v
  v="$(get "$1" "jobs.$2.needs")" || return 0
  if [ -n "$v" ]; then list "$v"; return 0; fi
  awk -F '\t' -v p="jobs.$2.needs." 'index($1, p) == 1 && substr($1, length(p) + 1) ~ /^[0-9]+$/ { print substr($0, length($1) + 2) }' <<<"$1"
}

# evaluate EXPR CONTEXT: EXPR's value, typed: s:<string>, n:<number>,
# b:true, b:false or null. CONTEXT has one name=value line per context path
# or status function (cancelled()=b:false), value typed the same way. A path
# not in it is null, or an object when a path below it is in it. A function
# not in it, or a form the reader does not take, fails with exit 2.
# shellcheck disable=SC2016 # awk program text, not shell
evaluate_awk='
function die(msg) { print "evaluate: " msg " in: " ENVIRON["EXPR"] > "/dev/stderr"; bad = 1; exit 2 }
function tokenize(s,   i, c, two, t, n) {
  n = 0
  i = 1
  while (i <= length(s)) {
    c = substr(s, i, 1)
    two = substr(s, i, 2)
    if (c ~ /[ \t\n]/) { i++; continue }
    if (c == "'"'"'") {
      t = ""
      i++
      for (;;) {
        if (i > length(s)) die("an unclosed string")
        c = substr(s, i, 1)
        if (c == "'"'"'") {
          if (substr(s, i + 1, 1) != "'"'"'") break
          i++
        }
        t = t c
        i++
      }
      i++
      tok[++n] = "s:" t
      continue
    }
    if (two == "==" || two == "!=" || two == "&&" || two == "||") { tok[++n] = "o:" two; i += 2; continue }
    if (c == "!" || c == "(" || c == ")" || c == ",") { tok[++n] = "o:" c; i++; continue }
    if (c ~ /[A-Za-z_]/) {
      match(substr(s, i), /^[A-Za-z_][A-Za-z0-9_.-]*/)
      tok[++n] = "i:" substr(s, i, RLENGTH)
      i += RLENGTH
      continue
    }
    if (c ~ /[0-9]/) {
      match(substr(s, i), /^[0-9]+/)
      tok[++n] = "n:" substr(s, i, RLENGTH)
      i += RLENGTH
      continue
    }
    die("an unread character " c)
  }
  ntok = n
}
function peek() { return pos <= ntok ? tok[pos] : "" }
function truthy(v) { return !(v == "null" || v == "b:false" || v == "s:" || v == "n:0") }
function str(v) {
  if (v == "null") return ""
  if (v ~ /^[sn]:/) return substr(v, 3)
  if (v ~ /^b:/) return substr(v, 3)
  return "Object"
}
function num(v,   s) {
  if (v == "null" || v == "b:false") return "0"
  if (v == "b:true") return "1"
  if (v ~ /^n:/) return substr(v, 3)
  if (v ~ /^s:/) {
    s = substr(v, 3)
    if (s == "") return "0"
    if (s ~ /^[0-9]+$/) return s
  }
  return "NaN"
}
function eq(a, b,   x, y) {
  if (a ~ /^s:/ && b ~ /^s:/) return tolower(a) == tolower(b)
  if (a == b) return 1
  x = num(a)
  y = num(b)
  if (x == "NaN" || y == "NaN") return 0
  return x + 0 == y + 0
}
function lookup(name,   k, l) {
  l = tolower(name)
  if (l in ctx) return ctx[l]
  for (k in ctx) if (index(k, l ".") == 1) return "o:"
  return "null"
}
function call(name, argc, args,   f, a, b) {
  f = tolower(name)
  if (f == "contains" || f == "startswith" || f == "endswith") {
    if (argc != 2) die(name " with " argc " arguments")
    a = tolower(str(args[1]))
    b = tolower(str(args[2]))
    if (f == "contains") return index(a, b) > 0 ? "b:true" : "b:false"
    if (f == "startswith") return index(a, b) == 1 ? "b:true" : "b:false"
    return (length(a) >= length(b) && substr(a, length(a) - length(b) + 1) == b) ? "b:true" : "b:false"
  }
  if (argc != 0) die(name " with arguments")
  if ((f "()") in ctx) return ctx[f "()"]
  die("no value for " name "()")
}
function p_or(   l, r) {
  l = p_and()
  while (peek() == "o:||") { pos++; r = p_and(); if (!truthy(l)) l = r }
  return l
}
function p_and(   l, r) {
  l = p_cmp()
  while (peek() == "o:&&") { pos++; r = p_cmp(); if (truthy(l)) l = r }
  return l
}
function p_cmp(   l, op, r) {
  l = p_unary()
  while (peek() == "o:==" || peek() == "o:!=") {
    op = peek()
    pos++
    r = p_unary()
    l = (eq(l, r) == (op == "o:==")) ? "b:true" : "b:false"
  }
  return l
}
function p_unary() {
  if (peek() == "o:!") { pos++; return truthy(p_unary()) ? "b:false" : "b:true" }
  return p_primary()
}
function p_primary(   t, v, name, argc, args) {
  t = peek()
  pos++
  if (t == "o:(") {
    v = p_or()
    if (peek() != "o:)") die("a missing )")
    pos++
    return v
  }
  if (t ~ /^[sn]:/) return t
  if (t ~ /^i:/) {
    name = substr(t, 3)
    if (peek() == "o:(") {
      pos++
      argc = 0
      if (peek() != "o:)") {
        args[++argc] = p_or()
        while (peek() == "o:,") { pos++; args[++argc] = p_or() }
      }
      if (peek() != "o:)") die("a missing ) after the arguments of " name)
      pos++
      return call(name, argc, args)
    }
    if (name == "true" || name == "false") return "b:" name
    if (name == "null") return "null"
    return lookup(name)
  }
  die("an unexpected " (t == "" ? "end" : substr(t, 3)))
}
BEGIN {
  n = split(ENVIRON["CTX"], lines, "\n")
  for (i = 1; i <= n; i++) {
    e = index(lines[i], "=")
    if (e > 1) ctx[tolower(substr(lines[i], 1, e - 1))] = substr(lines[i], e + 1)
  }
  tokenize(ENVIRON["EXPR"])
  if (ntok == 0) die("nothing")
  pos = 1
  v = p_or()
  if (pos <= ntok) die("an unexpected " substr(tok[pos], 3))
  print v
  exit 0
}
'
evaluate() { EXPR="$1" CTX="$2" awk "$evaluate_awk"; }

# shellcheck disable=SC2016 # the literal that opens a workflow expression
open='${{'
close='}}'
# inner VALUE: the expression of a "${{ expr }}" value, or VALUE itself.
inner() {
  local v="$1"
  case "$v" in "$open"*"$close")
    v="${v#"$open"}"
    v="${v%"$close"}"
    ;;
  esac
  printf '%s' "$v"
}
# runs IF CONTEXT: whether a job or step with that if: runs (exit 0) or not
# (exit 1); 2 when it cannot be evaluated. No if: (empty) is success(). An if:
# with no status function gets the implied success() &&.
runs() {
  local e v
  e="$(inner "$1")"
  [ -n "$e" ] || e='success()'
  if ! printf '%s' "$e" | grep -qiE '(^|[^A-Za-z0-9_.])(success|failure|cancelled|always)[[:space:]]*\('; then
    e="success() && ($e)"
  fi
  v="$(evaluate "$e" "$2")" || return 2
  case "$v" in null | b:false | s: | n:0) return 1 ;; esac
  return 0
}
# interpolate VALUE CONTEXT: VALUE with each ${{ expr }} in it replaced by its
# value as a string.
interpolate() {
  local v="$1" out='' e r
  while :; do
    case "$v" in *"$open"*"$close"*) ;; *) break ;; esac
    out="$out${v%%"$open"*}"
    v="${v#*"$open"}"
    e="${v%%"$close"*}"
    v="${v#*"$close"}"
    r="$(evaluate "$e" "$2")" || return 2
    case "$r" in null) r='' ;; s:* | n:* | b:*) r="${r#??}" ;; *) r=Object ;; esac
    out="$out$r"
  done
  printf '%s' "$out$v"
}

# The kinds of run, as context lines. A status line set comes after each.
nl=$'\n'
pr="github.event_name=s:pull_request${nl}github.ref=s:refs/pull/42/merge${nl}github.head_ref=s:feat/x${nl}github.run_id=s:9001${nl}github.event.pull_request.number=n:42${nl}github.event.pull_request.head.repo.fork=b:false"
fork_pr="${pr/fork=b:false/fork=b:true}"
main_push="github.event_name=s:push${nl}github.ref=s:refs/heads/main${nl}github.ref_name=s:main${nl}github.run_id=s:9002"
# release.yml runs on a tag push and calls the workflow, which then sees the
# tag push's github context.
tag_push="github.event_name=s:push${nl}github.ref=s:refs/tags/v3.6.228${nl}github.ref_name=s:v3.6.228${nl}github.run_id=s:9003"
passed="success()=b:true${nl}failure()=b:false${nl}cancelled()=b:false${nl}always()=b:true"
failed="success()=b:false${nl}failure()=b:true${nl}cancelled()=b:false${nl}always()=b:true"
cancelled="success()=b:false${nl}failure()=b:false${nl}cancelled()=b:true${nl}always()=b:true"

# ---- the readers ---------------------------------------------------------------

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cat >"$work/forms.yml" <<'EOF'
on:
  push:
    tags:
    - 'v*'   # a compact list
jobs:
  a:
    needs: [b, c]
    if: ${{ x == 'y' }}
    steps:
      - name: one
        run: |
          not: a key
            - nor an item
        with:
          k: v
      - name: two
        if: a &&
          b
  "q":
    needs:
      - b
EOF
want=$'on\t\non.push\t\non.push.tags\t\non.push.tags.0\t\'v*\'\njobs\t\njobs.a\t\njobs.a.needs\t[b, c]\njobs.a.if\t${{ x == \'y\' }}\njobs.a.steps\t\njobs.a.steps.0.name\tone\njobs.a.steps.0.run\t|\njobs.a.steps.0.with\t\njobs.a.steps.0.with.k\tv\njobs.a.steps.1.name\ttwo\njobs.a.steps.1.if\ta && b\njobs.q\t\njobs.q.needs\t\njobs.q.needs.0\tb'
got="$(flatten "$work/forms.yml")" || problem "flatten failed"
[ "$got" = "$want" ] || problem "flatten read:"$'\n'"$got"
[ "$(needs "$got" a | tr '\n' ' ')" = "b c " ] || problem "needs a: $(needs "$got" a | tr '\n' ' ')"
[ "$(needs "$got" q | tr '\n' ' ')" = "b " ] || problem "needs q: $(needs "$got" q | tr '\n' ' ')"
printf 'a:\n  b: c\n  {d: e}\n' >"$work/bad.yml"
! flatten "$work/bad.yml" >/dev/null 2>&1 || problem "flatten read a flow mapping line"
report "flatten reads the forms these files use and fails on one it does not take"

# eval_case EXPR CONTEXT WANT: WANT is the typed value, or "error".
eval_case() {
  local got
  got="$(evaluate "$1" "$2" 2>/dev/null)" || got=error
  [ "$got" = "$3" ] || problem "$1: $got, want $3"
}
eval_case "'PUSH' == 'push'" "" b:true
eval_case "a.b != 'x'" "" b:true
eval_case "!a.b" "" b:true
eval_case "!a" "a.b=s:x" b:false
eval_case "a == 1" "a=s:1" b:true
eval_case "a || 'd'" "a=s:" s:d
eval_case "a && 'd'" "a=n:0" n:0
eval_case "!(a || b) && c" "a=b:false${nl}b=b:true${nl}c=b:true" b:false
eval_case "contains(a, '-')" "a=s:v1.2.3-rc1" b:true
eval_case "startsWith(a, 'V1')" "a=s:v1.2.3" b:true
eval_case "endsWith(a, '.3')" "a=s:v1.2.3" b:true
eval_case "'it''s' == a" "a=s:it's" b:true
eval_case "cancelled()" "cancelled()=b:false" b:false
eval_case "failure()" "" error
eval_case "fromJSON(a)" "a=s:1" error
eval_case "a == 'b" "" error
eval_case "a ==" "" error
eval_case "(a" "" error
eval_case "a b" "" error
eval_case "a[0]" "" error
report "evaluate reads the expression forms these files use and fails on others"

if ! job="$(flatten "$job_yml")"; then
  echo "FAIL - cannot read $job_yml"
  exit 1
fi
if ! rel="$(flatten "$release_yml")"; then
  echo "FAIL - cannot read $release_yml"
  exit 1
fi

# ---- integrations-tests.yml: triggers ------------------------------------------

# A required check must report on every PR and on every push to one: a
# pull_request filter (types, branches, paths) leaves some PRs waiting on a
# check that never starts.
if ! has "$job" on.pull_request; then
  problem "on: has no pull_request"
elif [ -n "$(children "$job" on.pull_request)" ] || [ -n "$(get "$job" on.pull_request)" ]; then
  problem "on.pull_request is filtered: $(children "$job" on.pull_request | tr '\n' ' ')$(get "$job" on.pull_request)"
fi
report "every PR runs it, with no filter"

if ! list "$(get "$job" on.push.branches)" | grep -qxF main; then
  problem "on.push.branches is '$(get "$job" on.push.branches)', want it to take main"
fi
for k in $(children "$job" on.push); do
  [ "$k" = branches ] || problem "on.push has a $k filter"
done
report "every push to main runs it"

if [ "$(get "$job" on.workflow_call.secrets.INTEGRATIONS_REPO_DEPLOY_KEY_PRIVATE.required)" != true ]; then
  problem "on.workflow_call does not require the INTEGRATIONS_REPO_DEPLOY_KEY_PRIVATE secret"
fi
report "a caller must pass the deploy key"

# ---- integrations-tests.yml: when the job runs ---------------------------------

# runs_case NAME IF CONTEXT WANT: WANT is run or skip.
runs_case() {
  local got
  runs "$2" "$3"
  case $? in 0) got=run ;; 1) got=skip ;; *) got="unreadable if: $2" ;; esac
  [ "$got" = "$4" ] || problem "$1: $got, want $4"
}
mapfile -t jobs < <(children "$job" jobs)
if [ "${#jobs[@]}" -ne 1 ] || [ "${jobs[0]}" != integrations-tests ]; then
  problem "jobs: ${jobs[*]}, want integrations-tests alone"
fi
# A skipped job passes a required check, so the job runs on every run, a PR
# from a fork too: there its first step fails (below).
job_if="$(get "$job" jobs.integrations-tests.if)"
runs_case "a PR from a branch of keploy/keploy" "$job_if" "$pr$nl$passed" run
runs_case "a PR from a fork, which gets no secrets" "$job_if" "$fork_pr$nl$passed" run
runs_case "a push to main" "$job_if" "$main_push$nl$passed" run
runs_case "release.yml's call on a tag push" "$job_if" "$tag_push$nl$passed" run
report "the job runs on every PR, a fork's too, on main and on release.yml's call"

# ---- integrations-tests.yml: nothing passes it without the tests ---------------

# only_keys FLAT PATH KEY...: PATH has no key but those named.
only_keys() {
  local flat="$1" path="$2" k
  shift 2
  for k in $(children "$flat" "$path"); do
    case " $* " in *" $k "*) ;; *) problem "$path has a $k key; only $* are expected" ;; esac
  done
}
# A key the steps below do not expect could skip, soften or redirect them:
# continue-on-error, if:, shell:, working-directory:, or an env such as
# GOFLAGS that changes what go test runs.
only_keys "$job" "" name on permissions concurrency jobs
only_keys "$job" jobs.integrations-tests name timeout-minutes runs-on env steps
only_keys "$job" jobs.integrations-tests.env CGO_ENABLED
while IFS=$'\t' read -r p _; do
  case "$p" in *continue-on-error) problem "$p: a failure there would pass the check" ;; esac
done <<<"$job"
p="$(get "$rel" jobs.integrations-tests.continue-on-error)" && problem "release.yml's integrations-tests has continue-on-error: $p"
s_tests="Test integrations-tests.sh"
s_wf="Check this workflow and release.yml's gate on it"
s_ref="Pick the keploy/integrations ref"
s_run="Run keploy/integrations' tests against this keploy tree"
scripts=keploy/.github/workflows/test_workflow_scripts
# need_step VAR NAME RUN KEY...: sets VAR to the index of the step NAME, whose
# run: must be exactly RUN and whose keys only name, run and the KEYs; to ""
# when there is no such step.
need_step() {
  local var="$1" n="$2" want_run="$3" at
  shift 3
  printf -v "$var" '%s' ''
  if ! at="$(step "$job" integrations-tests "$n")"; then
    problem "no step named '$n'"
    return
  fi
  only_keys "$job" "$at" name run "$@"
  got="$(get "$job" "$at.run")"
  if [[ "$got" =~ ^[|\>] ]]; then
    # flatten skips a block scalar's lines, so it cannot see what it runs.
    problem "$n runs a block scalar ('$got'), which this check refuses on purpose, since it cannot read the block: want 'run: $want_run' on one line"
  elif [ "$got" != "$want_run" ]; then
    problem "$n runs '$got', want '$want_run'"
  fi
  printf -v "$var" '%s' "${at##*.}"
}
need_step i_tests "$s_tests" "bash $scripts/integrations-tests.tests.sh"
need_step i_wf "$s_wf" "bash $scripts/integrations-tests.workflows.tests.sh"
need_step i_ref "$s_ref" "bash $scripts/integrations-tests.sh resolve integrations" id env
need_step i_run "$s_run" "bash $scripts/integrations-tests.sh test keploy integrations" env
only_keys "$job" "jobs.integrations-tests.steps.$i_ref.env" PR_NUMBER GH_TOKEN HEAD_REF INTEGRATION_REF
only_keys "$job" "jobs.integrations-tests.steps.$i_run.env" INTEGRATIONS_DESC
# The integrations checkout leaves its default branch checked out; the tests
# run on the ref resolve picked only when resolve runs between the two.
i_co=''
for k in $(children "$job" jobs.integrations-tests.steps); do
  [ "$(get "$job" "jobs.integrations-tests.steps.$k.with.repository")" = keploy/integrations ] && i_co="$k"
done
if [ -n "$i_co" ] && [ -n "$i_ref" ] && [ -n "$i_run" ] && [ -n "$i_tests" ] && [ -n "$i_wf" ]; then
  [ "$i_co" -lt "$i_ref" ] && [ "$i_ref" -lt "$i_run" ] ||
    problem "steps: integrations checkout $i_co, resolve $i_ref, tests $i_run; want them in that order"
  [ "$i_tests" -lt "$i_run" ] && [ "$i_wf" -lt "$i_run" ] ||
    problem "the self-tests (steps $i_tests and $i_wf) do not run before the tests (step $i_run)"
else
  problem "a step the checks above need is missing (integrations checkout: '${i_co}')"
fi
# The keploy checkout takes the tree the run is for: a PR merged onto main, or
# the pushed commit. A ref: (main, the PR's base) or a repository: there would
# make every PR test another tree and pass, so it takes no input but its path.
k_co=()
for k in $(children "$job" jobs.integrations-tests.steps); do
  case "$(get "$job" "jobs.integrations-tests.steps.$k.uses")" in actions/checkout@*) ;; *) continue ;; esac
  [ "$k" = "$i_co" ] || k_co+=("$k")
done
if [ "${#k_co[@]}" -ne 1 ]; then
  problem "steps '${k_co[*]}' check out keploy, want one step"
else
  at="jobs.integrations-tests.steps.${k_co[0]}"
  only_keys "$job" "$at" uses with
  only_keys "$job" "$at.with" path
  [ "$(get "$job" "$at.with.path")" = keploy ] ||
    problem "the keploy checkout's path is '$(get "$job" "$at.with.path")', want keploy"
fi
report "no continue-on-error, if: or other command on the steps that decide the check, and keploy's own tree checked out"

# The first step fails a run that has no key to keploy/integrations, so a PR
# from a fork fails the check, saying why and naming the way out, instead of
# passing it with nothing run.
s_key="Check this run can read keploy/integrations"
key_ok=1
if [ "$(get "$job" jobs.integrations-tests.steps.0.name)" != "$s_key" ]; then
  problem "the first step is '$(get "$job" jobs.integrations-tests.steps.0.name)', want '$s_key'"
  key_ok=0
fi
only_keys "$job" jobs.integrations-tests.steps.0 name env run
only_keys "$job" jobs.integrations-tests.steps.0.env HAS_KEY
has_key="$(get "$job" jobs.integrations-tests.steps.0.env.HAS_KEY)" || has_key=''
key_run="$(get "$job" jobs.integrations-tests.steps.0.run)" || key_run=''
# key_case NAME SECRET WANT_HAS_KEY WANT_RC: HAS_KEY under that secret, and the
# step's exit code under the value it got.
key_case() {
  local v rc
  v="$(interpolate "$has_key" "$pr${nl}secrets.INTEGRATIONS_REPO_DEPLOY_KEY_PRIVATE=s:$2")" || v="unreadable HAS_KEY: $has_key"
  [ "$v" = "$3" ] || problem "$1: HAS_KEY is '$v', want '$3'"
  printf '%s\n' "$key_run" >"$work/key.sh"
  HAS_KEY="$v" bash -e "$work/key.sh" >"$work/key.out" 2>&1
  rc=$?
  [ "$rc" = "$4" ] || problem "$1: the step exits $rc, want $4"
  if [ "$4" = 0 ]; then
    [ ! -s "$work/key.out" ] || problem "$1: the step says: $(cat "$work/key.out")"
  elif [ "$(grep -c '^::error title=keploy/integrations tests::' "$work/key.out")" != 1 ] ||
    ! grep -q 'a PR from a fork' "$work/key.out" || ! grep -q 'branch of keploy/keploy' "$work/key.out"; then
    problem "$1: the step does not fail with one error naming forks and the way out: $(cat "$work/key.out")"
  fi
}
if [ "$key_ok" = 1 ]; then
  key_case "a run with the key" "-----BEGIN OPENSSH PRIVATE KEY-----" true 0
  key_case "a run without it (a PR from a fork)" "" false 1
fi
report "the first step fails a run that has no key to keploy/integrations, naming the way out"

# ---- integrations-tests.yml: concurrency ---------------------------------------

# A new push to a PR cancels its older run; no run of main or of a release
# shares a group with another run, so none waits for or cancels another.
group="$(get "$job" concurrency.group)"
cancel="$(get "$job" concurrency.cancel-in-progress)"
concurrency_case() {
  local g c
  g="$(interpolate "$group" "$2")" || g="unreadable group: $group"
  c="$(interpolate "$cancel" "$2")" || c="unreadable cancel-in-progress: $cancel"
  [ "$g" = "$3" ] || problem "$1: group '$g', want '$3'"
  [ "$c" = "$4" ] || problem "$1: cancel-in-progress '$c', want '$4'"
}
concurrency_case "PR 42" "$pr" integrations-tests-42 true
concurrency_case "a push to main, run 9002" "$main_push" integrations-tests-9002 false
concurrency_case "release.yml's call, run 9003" "$tag_push" integrations-tests-9003 false
report "a PR's new push cancels its older run, and main's and releases' runs each have a group of their own"

# ---- integrations-tests.yml: saving the cache ----------------------------------

# Only main's runs save (a PR's cache is visible to that PR alone), and only
# when the restore step ran and found no exact hit. A failed test run of main
# still saves: its modules and build cache are good. cache-hit is 'true' on an
# exact hit, 'false' on a restore-keys hit and empty on a miss.
save="$(step "$job" integrations-tests "Save the Go module cache")" || save=''
restore="$(step "$job" integrations-tests "Restore the Go module cache")" || restore=''
if [ -z "$save" ] || [ -z "$restore" ]; then
  problem "no step named 'Save the Go module cache' or 'Restore the Go module cache'"
else
  # The module cache only: the build cache saves under a minute a run and
  # would evict other workflows' caches (see the restore step).
  for at in "$restore" "$save"; do
    # shellcheck disable=SC2088 # the text the file holds, not a path here
    [ "$(get "$job" "$at.with.path")" = "~/go/pkg/mod" ] || problem "$at caches '$(get "$job" "$at.with.path")', want ~/go/pkg/mod alone"
  done
  save_if="$(get "$job" "$save.if")"
  hit() { printf 'steps.cache.outcome=s:%s%ssteps.cache.outputs.cache-hit=s:%s' "$1" "$nl" "$2"; }
  runs_case "main, a miss" "$save_if" "$main_push$nl$passed$nl$(hit success '')" run
  runs_case "main, a restore-keys hit" "$save_if" "$main_push$nl$passed$nl$(hit success false)" run
  runs_case "main, a restore-keys hit, tests failed" "$save_if" "$main_push$nl$failed$nl$(hit success false)" run
  runs_case "main, an exact hit" "$save_if" "$main_push$nl$passed$nl$(hit success true)" skip
  runs_case "main, the restore step failed" "$save_if" "$main_push$nl$failed$nl$(hit failure '')" skip
  runs_case "main, the restore step was skipped" "$save_if" "$main_push$nl$failed$nl$(hit skipped '')" skip
  runs_case "main, cancelled" "$save_if" "$main_push$nl$cancelled$nl$(hit success '')" skip
  runs_case "a PR, a miss" "$save_if" "$pr$nl$passed$nl$(hit success '')" skip
  runs_case "release.yml's call on a tag push, a miss" "$save_if" "$tag_push$nl$passed$nl$(hit success '')" skip
fi
report "only main's runs save the module cache, after a restore with no exact hit"

# ---- setup-private-parsers: the PR body the build lanes read -----------------

# Both build lanes read the PR body's Integration-Ref from PR_BODY: the body of
# the event that started the run, on a PR, and empty on any other run. A lane
# given another value, or that sets PR_BODY itself, would build against
# another integrations ref than the one this job tests.
action_yml="$here/../../actions/setup-private-parsers/action.yaml"
act="$(flatten "$action_yml")" || problem "cannot read $action_yml"
want_body="\${{ github.event_name == 'pull_request' && github.event.pull_request.body || '' }}"
for lane in "Config Git & Fetch (non-Windows)" "Configure Git, Fetch Dependencies, and Create Parsers File (Windows)"; do
  if ! at="$(awk -F '\t' -v n="$lane" '$1 ~ /^runs\.steps\.[0-9]+\.name$/ && substr($0, length($1) + 2) == n { print substr($1, 1, length($1) - 5); found = 1; exit } END { exit !found }' <<<"$act")"; then
    problem "setup-private-parsers has no step '$lane'"
    continue
  fi
  only_keys "$act" "$at.env" INTEGRATION_REF GITHUB_HEAD_REF_VAL PR_BODY
  got="$(get "$act" "$at.env.PR_BODY")" || got="(none)"
  [ "$got" = "$want_body" ] || problem "$lane: PR_BODY is '$got', want '$want_body'"
done
# Outside comments, PR_BODY is named only where the two lanes set it and where
# each reads it once.
got="$(grep -vE '^[[:space:]]*#' "$action_yml" | grep -F PR_BODY | sed -e 's/^[[:space:]]*//' | sort)"
# shellcheck disable=SC2016 # the text the action holds
want="$(printf '%s\n' "PR_BODY: $want_body" "PR_BODY: $want_body" '$BodyRef = Get-IntegrationRefOfBody $env:PR_BODY' 'BODY_REF="$(integration_ref_of_body "$PR_BODY")"' | sort)"
[ "$got" = "$want" ] || problem "action.yaml names PR_BODY in:$nl$got${nl}want only:$nl$want"
report "both build lanes read the body of the PR event, as PR_BODY, and nothing else sets it"

# ---- release.yml: the gate -------------------------------------------------------

if [ "$(get "$rel" jobs.integrations-tests.uses)" != ./.github/workflows/integrations-tests.yml ]; then
  problem "jobs.integrations-tests.uses is '$(get "$rel" jobs.integrations-tests.uses)'"
fi
# shellcheck disable=SC2016 # the expression is the text the file holds
if [ "$(get "$rel" jobs.integrations-tests.secrets.INTEGRATIONS_REPO_DEPLOY_KEY_PRIVATE)" != '${{ secrets.INTEGRATIONS_REPO_DEPLOY_KEY_PRIVATE }}' ]; then
  problem "jobs.integrations-tests does not pass secrets.INTEGRATIONS_REPO_DEPLOY_KEY_PRIVATE"
fi
# A called workflow's token gets no more than its caller job grants: a
# permission integrations-tests.yml asks for and the job does not grant stops
# the whole release run before it starts.
for k in $(children "$job" permissions); do
  want="$(get "$job" "permissions.$k")"
  got="$(get "$rel" "jobs.integrations-tests.permissions.$k")" || got=none
  case "$want:$got" in read:read | read:write | write:write) ;; *)
    problem "integrations-tests.yml asks for $k: $want; release.yml's job grants $k: $got"
    ;;
  esac
done
report "release.yml calls integrations-tests.yml with its secret and the permissions it asks for"

# The jobs that wait for integrations-tests, directly or through another job.
mapfile -t rel_jobs < <(children "$rel" jobs)
gated=" integrations-tests "
grew=1
while [ "$grew" -eq 1 ]; do
  grew=0
  for j in "${rel_jobs[@]}"; do
    case "$gated" in *" $j "*) continue ;; esac
    for n in $(needs "$rel" "$j"); do
      case "$gated" in *" $n "*)
        gated="$gated$j "
        grew=1
        break
        ;;
      esac
    done
  done
done
# The builds run beside it and publish nothing; every other job waits for it.
for j in "${rel_jobs[@]}"; do
  case "$j" in integrations-tests | build-linux | build-darwin | build-windows) continue ;; esac
  case "$gated" in *" $j "*) ;; *) problem "$j does not wait for integrations-tests" ;; esac
done
for j in release build-docker; do
  needs "$rel" "$j" | grep -qxF integrations-tests || problem "$j's needs: lacks integrations-tests"
done
report "release, build-docker and every job after them wait for integrations-tests"

# A gated job whose if: has a status function (always(), !cancelled(),
# failure()) can run after integrations-tests failed and so publish.
for j in $gated; do
  [ "$j" = integrations-tests ] && continue
  runs_case "$j after integrations-tests failed" "$(get "$rel" "jobs.$j.if")" "$tag_push$nl$failed" skip
  runs_case "$j after integrations-tests was cancelled" "$(get "$rel" "jobs.$j.if")" "$tag_push$nl$cancelled" skip
done
report "no job that waits for integrations-tests runs after it failed or was cancelled"

if [ "$failures" -ne 0 ]; then
  echo "integrations-tests workflows: $failures of $cases checks failed"
  exit 1
fi
echo "integrations-tests workflows: all $cases checks pass"

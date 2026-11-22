#!/usr/bin/env bash
# Checks of the workflows around reap-stale-containers.sh, the sweep of the
# self-hosted macOS runners' shared Docker daemon. The sweep's own cases are in
# reap-stale-containers.tests.sh; these read the workflow files, so a mistake in
# any workflow can fail them. That is why they run only on GitHub's runners
# (macos-ci-scripts.yml), never in precheck-macos, whose failure would skip the
# macOS test jobs. Plain bash 3.2, like the script. Locally:
#
#   bash .github/scripts/macos/reap-stale-containers.workflows.tests.sh
#
# Each check is a reader of the YAML text, written for the forms this
# repository uses, with fixtures of the forms it must read and the mistakes it
# must flag. It is a tripwire for those mistakes, not a proof that the jobs
# behave.
set -u

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
reaper="$here/reap-stale-containers.sh"
repo_root="$(cd "$here/../../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
echo "bash $BASH_VERSION on $(uname -s)"

# The threshold the script ships with.
min_age="$(sed -n 's/^MIN_AGE_MINUTES=\([0-9][0-9]*\)$/\1/p' "$reaper")"
if [ -z "$min_age" ]; then
  echo "FAIL - cannot read MIN_AGE_MINUTES from $reaper"
  exit 1
fi

out=''
failures=0
problems=''
problem() { problems="$problems       $1"$'\n'; }
report() {
  if [ -z "$problems" ]; then
    echo "ok   - $1"
  else
    failures=$((failures + 1))
    echo "FAIL - $1"
    printf '%s' "$problems"
    if [ -n "$out" ]; then printf '%s\n' "$out" | sed 's/^/       | /'; fi
  fi
  problems=''
}

# ---- the threshold against every job that runs on these runners ---------------

# MIN_AGE_MINUTES is only safe while no job on this Docker daemon can outlive
# it, and a job's if: always() steps still run after it is cancelled at its
# timeout-minutes (with none, it inherits GitHub's 360). So every job that can
# land on the self-hosted macOS runners must carry a timeout-minutes at least
# $timeout_margin below it. A job that carries one is fine wherever it runs, so
# only a job without one, or with a larger one, needs its runs-on read.
#
# A runner takes any job whose labels it all has, and these runners carry
# self-hosted, macOS, ARM64 and native. So a job can land on them when its
# labels name them (macOS, or native), and also when they name no system at
# all: plain self-hosted, or [self-hosted, ARM64], goes to whichever
# self-hosted runner is free. One that names Windows or Linux cannot.
#
# jobs_in reads a job's runs-on as labels on the line ([self-hosted, macOS,
# native] or one label), a block list under it, or a labels: mapping. An
# expression is followed to every value the workflows can feed it: its own
# quoted literals, and for each inputs.X or matrix.X it reads, every value any
# workflow gives an X: key (on the line, as a block list, or as the default: of
# an input), following expressions among those. What it cannot resolve might
# land on these runners, so such a job is held to the same timeout: a runner
# group, an alias, a block scalar (runs-on: >-), a list that goes on past the
# end of the line, an expression reading anything else (matrix.config.runner,
# a condition), an inputs.X or matrix.X that no workflow gives a value. A jobs:
# or job key it cannot read fails the check whatever its timeout, since its
# timeout-minutes cannot be read either.
#
# What it does not see: a label given to these runners after this was
# written, a value written inside a flow mapping ({os: ..., runner: ...}), a
# value a caller in another repository passes to an input of a reusable
# workflow here, and the workflows of the other repositories whose jobs these
# runners take (reap-stale-containers.sh says where that leaves the
# threshold).
timeout_margin=5
blank_re='^[[:space:]]*(#.*)?$'
jobs_re='^jobs:[[:space:]]*(#.*)?$'
top_re='^[^[:space:]#]'
bad_jobs_re='^jobs[[:space:]]*:'
job_re='^[[:space:]]*([A-Za-z_][A-Za-z0-9_-]*):[[:space:]]*(#.*)?$'
timeout_re='^[[:space:]]*timeout-minutes:[[:space:]]*(.*)$'
# A timeout-minutes this reads: 1 to 4 digits, no leading zero. Anything else
# (0050, which shell arithmetic takes for octal 40, a number past 64 bits, an
# expression) is unreadable, and is no timeout for the rule.
minutes_re='^[1-9][0-9]{0,3}$'
runs_on_re='^[[:space:]]*runs-on:(.*)$'
item_re='^[[:space:]]*-[[:space:]]+(.*)$'
default_re='^[[:space:]]*default:[[:space:]]*(.*)$'
group_re='(^|[[:space:]{,])group:'
open_flow_re='^(\[[^]]*|\{[^}]*)$'
alias_re='(^|[[:space:],[])\*'
block_scalar_re='^[|>]'
# shellcheck disable=SC2016 # the literal that opens a workflow expression
expr_open='${{'
nl=$'\n'

# The value without a trailing comment or the spaces around it.
trim() {
  local v="${1%%[[:space:]]#*}"
  case "$v" in '#'*) v='' ;; esac
  v="${v#"${v%%[![:space:]]*}"}"
  printf '%s' "${v%"${v##*[![:space:]]}"}"
}
# Sets $kind for one runs-on value with no expression in it: macos (a job with
# these labels can land on the self-hosted macOS runners), other, or
# "unknown: <why>". GitHub matches labels without regard to case.
label_kind() {
  local words
  if [[ "$1" =~ $group_re ]] || [[ "$1" =~ $open_flow_re ]] || [[ "$1" =~ $alias_re ]] ||
    [[ "$1" =~ $block_scalar_re ]]; then
    kind="unknown: runs-on: $1"
    return
  fi
  words=" $(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr '\n\t' '  ' | sed -E "s/[][,:\"{}']/ /g") "
  kind=other
  case "$words" in
    *' macos '* | *' native '*) kind=macos ;;
    *' windows '* | *' linux '*) ;;
    *' self-hosted '* | *' arm64 '*) kind=macos ;;
  esac
}
# Every value any workflow in $wf_dir gives a key named $1, one per line.
values_of_key() {
  local key_re="^([[:space:]]*(-[[:space:]]+)?)$1:(.*)\$" file line lead v col
  for file in "$wf_dir"/*.yml "$wf_dir"/*.yaml; do
    if [ ! -f "$file" ]; then continue; fi
    col=-1
    while IFS= read -r line || [ -n "$line" ]; do
      if [ "$col" -ge 0 ]; then
        # The block under the key: deeper lines, or a list at its own depth.
        if [[ "$line" =~ $blank_re ]]; then continue; fi
        lead="${line%%[! ]*}"
        if [ "${#lead}" -gt "$col" ] || { [ "${#lead}" -eq "$col" ] && [[ "$line" =~ $item_re ]]; }; then
          if [[ "$line" =~ $item_re ]] || [[ "$line" =~ $default_re ]]; then trim "${BASH_REMATCH[1]}" && echo; fi
          continue
        fi
        col=-1
      fi
      if [[ "$line" =~ $key_re ]]; then
        lead="${BASH_REMATCH[1]}"
        v="$(trim "${BASH_REMATCH[3]}")"
        if [ -n "$v" ]; then echo "$v"; else col="${#lead}"; fi
      fi
    done <"$file"
  done
}
# Sets $kind for a job's runs-on, as label_kind does: an expression is macos
# when any value it can take is, and unknown when any part of it cannot be
# resolved. Remembers what an expression resolved to: most jobs here share one
# of a few.
resolved_exprs=()
resolved_kinds=()
classify() {
  local runs="$1" i todo e bare names rest n found values v seen=' ' all=other
  case "$runs" in
    *"$expr_open"*) ;;
    *)
      label_kind "$runs"
      return
      ;;
  esac
  for ((i = 0; i < ${#resolved_exprs[@]}; i++)); do
    if [ "${resolved_exprs[$i]}" = "$wf_dir $runs" ]; then
      kind="${resolved_kinds[$i]}"
      return
    fi
  done
  todo="$runs"
  while [ -n "$todo" ]; do
    e="${todo%%"$nl"*}"
    if [ "$e" = "$todo" ]; then todo=''; else todo="${todo#*"$nl"}"; fi
    values="$(printf '%s' "$e" | grep -oE "'[^']*'")"
    bare="$(printf '%s' "$e" | sed -E "s/'[^']*'/ /g")"
    names="$(printf '%s' "$bare" | grep -oE '(inputs|matrix)\.[A-Za-z0-9_-]+' | sed -E 's/^(inputs|matrix)\.//')"
    rest="$(printf '%s' "$bare" | sed -E 's/(inputs|matrix)\.[A-Za-z0-9_-]+//g; s/\$\{\{|\}\}|[|][|]|fromJSON\(|\)|[[:space:]]//g')"
    if [ -n "$rest" ]; then
      all="unknown: runs-on: $runs"
      break
    fi
    for n in $names; do
      case "$seen" in *" $n "*) continue ;; esac
      seen="$seen$n "
      found="$(values_of_key "$n")"
      if [ -z "$found" ]; then
        all="unknown: runs-on: $runs (no workflow gives $n a value)"
        break 2
      fi
      values="$values$nl$found"
    done
    while IFS= read -r v; do
      case "$v" in
        '') continue ;;
        *"$expr_open"*)
          todo="$todo${todo:+$nl}$v"
          continue
          ;;
      esac
      label_kind "$v"
      case "$kind" in
        unknown:*)
          all="unknown: runs-on: $runs (one value of it is $v)"
          break 2
          ;;
        macos) all=macos ;;
      esac
    done <<<"$values"
  done
  kind="$all"
  resolved_exprs[${#resolved_exprs[@]}]="$wf_dir $runs"
  resolved_kinds[${#resolved_kinds[@]}]="$kind"
}
# A line per job of every workflow in $wf_dir: <file>:<job>|<timeout-minutes,
# or nothing>|<kind>. Jobs are the keys one level under a top-level jobs:, at
# whatever indentation the file uses, and a job's timeout-minutes and runs-on
# are the keys one level under it (a step's timeout does not count for the
# job).
jobs_in() {
  local file name line lead n in_jobs job_at key_at block job='' timeout='' runs='' unreadable=''
  flush() {
    if [ -z "$job" ]; then return; fi
    # Under a job key it cannot read, the timeout-minutes it found may be
    # another job's, so it does not count.
    if [ -n "$unreadable" ]; then kind="unknown: $unreadable" timeout=''; else classify "$runs"; fi
    # The record is split on |, and only its last field (kind) may hold one:
    # a timeout it cannot read (${{ inputs.timeout || 20 }}) is written as ?,
    # and a | in a file name as ?. A job key cannot hold one (job_re).
    if [ -n "$timeout" ] && ! [[ "$timeout" =~ $minutes_re ]]; then timeout='?'; fi
    printf '%s|%s|%s\n' "${name//|/?}:$job" "$timeout" "$kind"
    job=''
  }
  for file in "$wf_dir"/*.yml "$wf_dir"/*.yaml; do
    if [ ! -f "$file" ]; then continue; fi
    name="${file##*/}"
    n=0 in_jobs=0 job_at=-1 key_at=-1 job='' block=0
    while IFS= read -r line || [ -n "$line" ]; do
      n=$((n + 1))
      line="${line%$'\r'}"
      if [[ "$line" =~ $jobs_re ]]; then
        in_jobs=1 job_at=-1
        continue
      fi
      if [[ "$line" =~ $top_re ]]; then
        flush
        in_jobs=0
        if [[ "$line" =~ $bad_jobs_re ]]; then
          job="line$n" timeout='' runs='' unreadable="a jobs: line it cannot read: $line"
          flush
        fi
        continue
      fi
      if [ "$in_jobs" -eq 0 ] || [[ "$line" =~ $blank_re ]]; then continue; fi
      lead="${line%%[! ]*}"
      if [ "$job_at" -lt 0 ]; then job_at="${#lead}"; fi
      if [ "${#lead}" -le "$job_at" ]; then
        flush
        key_at=-1 block=0 timeout='' runs='' unreadable=''
        if [ "${#lead}" -eq "$job_at" ] && [[ "$line" =~ $job_re ]]; then
          job="${BASH_REMATCH[1]}"
        else
          job="line$n" unreadable="a line at job-key indentation that is not a job key: $(trim "$line")"
        fi
        continue
      fi
      if [ "$key_at" -lt 0 ]; then key_at="${#lead}"; fi
      # The lines of a runs-on written as a block under the key.
      if [ "$block" -eq 1 ]; then
        if [ "${#lead}" -gt "$key_at" ] || { [ "${#lead}" -eq "$key_at" ] && [[ "$line" =~ $item_re ]]; }; then
          runs="${runs:+$runs }$(trim "$line")"
          continue
        fi
        block=0
      fi
      if [ "${#lead}" -ne "$key_at" ]; then continue; fi
      if [[ "$line" =~ $timeout_re ]]; then
        timeout="$(trim "${BASH_REMATCH[1]}")"
      elif [[ "$line" =~ $runs_on_re ]]; then
        runs="$(trim "${BASH_REMATCH[1]}")"
        if [ -z "$runs" ]; then block=1; fi
      fi
    done <"$file"
    flush
  done
}
# The jobs among $1 (jobs_in's lines) that break the rule for threshold $2: a
# job that can land on these runners, or might, without a timeout-minutes at
# least timeout_margin below $2.
timeout_problems() {
  local where timeout kind
  local shown
  while IFS='|' read -r where timeout kind; do
    if [ "$kind" = other ]; then continue; fi
    if [[ "$timeout" =~ $minutes_re ]] && [ $((timeout + timeout_margin)) -le "$2" ]; then continue; fi
    shown="${timeout:-none}"
    if [ "$timeout" = '?' ]; then shown="a value that is not 1 to 4 digits"; fi
    case "$kind" in
      unknown:*) echo "$where (cannot tell whether it runs on these runners from ${kind#unknown: }, and its timeout-minutes is $shown)" ;;
      *) echo "$where (timeout-minutes $shown)" ;;
    esac
  done <<<"$1"
}

# The parser itself, over every runs-on form, on a workflows dir of its own.
wf_dir="$work/workflows"
mkdir "$wf_dir"
cat >"$wf_dir/forms.yml" <<'EOF'
on:
  workflow_call:
    inputs:
      pool:
        type: string
        default: '["self-hosted", "macOS"]'
      runner:
        type: string
        default: 'ubuntu-latest' # a hosted runner
      winpool:
        type: string
        default: '["self-hosted", "Windows"]'
jobs:
  listform:
    runs-on:
      - self-hosted
      - macOS
    steps: []
  grouped:
    runs-on:
      group: mac-pool
      labels: ARM64
  labelsmap:
    runs-on:
      labels: [self-hosted, macOS]
    timeout-minutes: 41
  viainput:
    runs-on: ${{ fromJSON(inputs.pool) }}
    timeout-minutes: 42
  linuxinput:
    runs-on: ${{ inputs.runner || 'ubuntu-24.04-arm' }}
  chained:
    runs-on: ${{ inputs.via }}
    timeout-minutes: 44
    strategy:
      matrix:
        hop:
          - ubuntu-latest
          - [self-hosted, macOS]
  fromvars:
    runs-on: ${{ vars.POOL }}
  flow:
    runs-on: [self-hosted, macOS, native]
    timeout-minutes: 30
  lowercase:
    runs-on: [ self-hosted, macos ]  # GitHub ignores the case of a label
  openlist:
    runs-on: [self-hosted,
      macOS]
    timeout-minutes: 60
  anyself:
    runs-on: self-hosted # whichever self-hosted runner is free
  armself:
    runs-on: [self-hosted, ARM64]
    timeout-minutes: 41
  bynative:
    runs-on: native
  bymacos:
    runs-on: macOS
  anyarm:
    runs-on: ARM64
  linuxself:
    runs-on: [self-hosted, Linux, ARM64]
  aliased:
    runs-on: [self-hosted, *os]
    timeout-minutes: 41
  folded:
    runs-on: >-
      self-hosted
  mixed:
    runs-on: ${{ matrix.box }}
    strategy:
      matrix:
        box: [ubuntu-latest, self-hosted]
  viaalias:
    runs-on: ${{ matrix.host }}
    timeout-minutes: 60
    strategy:
      matrix:
        host: [ubuntu-latest, *mac]
  winvia:
    runs-on: ${{ fromJSON(inputs.winpool) }}
  dangling:
    runs-on: ${{ matrix.where || 'ubuntu-latest' }}
    strategy:
      matrix: ${{ fromJSON(needs.plan.outputs.matrix) }}
  nestedok:
    runs-on: ${{ matrix.config.runner }}
    timeout-minutes: 20
    strategy:
      matrix:
        config:
          - runner: ubuntu-latest
  nestedbare:
    runs-on: ${{ matrix.config.runner }}
  choiceok:
    runs-on: ${{ github.event_name == 'push' && 'ubuntu-latest' || 'ubuntu-24.04-arm' }}
    timeout-minutes: 40
  choicelong:
    runs-on: ${{ github.event_name == 'push' && 'ubuntu-latest' || 'ubuntu-24.04-arm' }}
    timeout-minutes: 41
  groupedok:
    runs-on:
      group: mac-pool
    timeout-minutes: 30
  caller:
    uses: ./.github/workflows/forms.yml
    with:
      runner: ubuntu-latest
      via: ${{ inputs.runner }}
  relay:
    uses: ./.github/workflows/forms.yml
    with:
      via: ${{ matrix.hop }}
  commented: # a trailing comment on the job key
    runs-on: [self-hosted, macOS]
  'quoted':
    runs-on: [self-hosted, macOS]
    timeout-minutes: 10
  hosted:
    runs-on: macos-latest
  windows:
    runs-on: [self-hosted, Windows, X64]
  after:
    runs-on: ubuntu-latest
    timeout-minutes: 5
  octal:
    runs-on: [self-hosted, macOS]
    timeout-minutes: 0050 # octal 40 to shell arithmetic
  overflow:
    runs-on: [self-hosted, macOS]
    timeout-minutes: 18446744073709551600
  badoctal:
    runs-on: [self-hosted, macOS]
    timeout-minutes: 099
  byinput:
    runs-on: [self-hosted, macOS]
    timeout-minutes: ${{ inputs.minutes }}
  linuxexpr:
    runs-on: ubuntu-latest
    timeout-minutes: ${{ inputs.timeout || 20 }}
  macexpr:
    runs-on: [self-hosted, macOS]
    timeout-minutes: ${{ inputs.timeout || 20 }}
EOF
# A job whose runs-on cannot be read is held to the timeout, not failed for it:
# nestedok, choiceok and groupedok pass with theirs. Jobs indented by four, a
# comment on jobs:, and timeout-minutes read only at the job's own level (a
# step's does not count for the job).
cat >"$wf_dir/deep.yml" <<'EOF'
on: push
jobs: # four-space job keys
    deepbare:
        runs-on: [self-hosted, macOS]
        steps:
          - run: echo
            timeout-minutes: 5
    deepok:   # with a comment
        runs-on: [self-hosted, macOS]
        timeout-minutes: 20 # minutes
EOF
cat >"$wf_dir/flow.yml" <<'EOF'
on: push
jobs: { flowjob: { runs-on: [self-hosted, macOS] } }
EOF
quoted="$(grep -n "^  'quoted':" "$wf_dir/forms.yml" | cut -d: -f1)"
out="$(timeout_problems "$(jobs_in)" 45 | sed -E 's/ \(cannot tell.*$/=unresolved/; s/ \(timeout-minutes.*$/=timeout/' | tr '\n' ',')"
want="deep.yml:deepbare=timeout,flow.yml:line2=unresolved,forms.yml:listform=timeout,forms.yml:grouped=unresolved,forms.yml:labelsmap=timeout,forms.yml:viainput=timeout,forms.yml:chained=timeout,forms.yml:fromvars=unresolved,forms.yml:lowercase=timeout,forms.yml:openlist=unresolved,forms.yml:anyself=timeout,forms.yml:armself=timeout,forms.yml:bynative=timeout,forms.yml:bymacos=timeout,forms.yml:anyarm=timeout,forms.yml:aliased=unresolved,forms.yml:folded=unresolved,forms.yml:mixed=timeout,forms.yml:viaalias=unresolved,forms.yml:dangling=unresolved,forms.yml:nestedbare=unresolved,forms.yml:choicelong=unresolved,forms.yml:commented=timeout,forms.yml:line$quoted=unresolved,forms.yml:octal=timeout,forms.yml:overflow=timeout,forms.yml:badoctal=timeout,forms.yml:byinput=timeout,forms.yml:macexpr=timeout,"
if [ "$out" != "$want" ]; then problem "flagged [$out], want [$want]"; fi
report "the timeout guard reads list, group, flow, alias, block and expression runs-on forms and every job-key form, knows the labels that reach these runners, and lets a job it cannot place pass on its timeout"

# The two test jobs that use Docker on these runners, by name, each with a
# timeout-minutes: a reader that missed their files would pass everything else
# here. (One whose runs-on it could not read would still be held to the rule.)
wf_dir="$repo_root/.github/workflows"
jobs="$(jobs_in)"
macos_jobs="$(grep -c '|macos$' <<<"$jobs")"
bad="$(timeout_problems "$jobs" "$min_age" | tr '\n' ',')"
out="$(grep -v '|other$' <<<"$jobs")"
for j in golang_docker_macos.yml:golang_docker_macos python_docker_macos.yml:python_docker_macos; do
  if ! grep -Eq "^${j}[|][1-9][0-9]*[|](macos|unknown:.*)\$" <<<"$jobs"; then
    problem "did not find $j, with a timeout-minutes, among the jobs that can land on these runners"
  fi
done
if [ -n "$bad" ]; then
  problem "jobs without a timeout-minutes at least $timeout_margin below MIN_AGE_MINUTES ($min_age): ${bad%,}"
fi
report "MIN_AGE_MINUTES ($min_age) outlasts the timeout-minutes of all $macos_jobs jobs that can land on the self-hosted macOS runners by at least $timeout_margin"

# ---- the job that runs the sweep ------------------------------------------------

# clean_up_docker_macos needs the repository only for the sweep's script, and
# gate_macos needs that job. So a checkout that fails there must cost the run
# its sweep and nothing else: with continue-on-error it neither fails the job
# nor skips the run's own cleanup after it. That alone would let the sweep run
# on whatever the runner's last job left in the workspace, which can be a
# commit whose cases failed. So the sweep's if: must require the checkout's
# outcome (its conclusion is 'success' under continue-on-error), and
# precheck-macos' result, the job the script's cases run in on that machine.
# That result is 'success' only for a job listed in needs: (for any other it is
# empty, and the sweep would never run, without a word), and it says the cases
# passed only while precheck-macos runs them in a step with no if: and no
# continue-on-error. precheck-macos must not run these workflow checks: a
# mistake in an unrelated workflow would then skip the macOS test jobs.
#
# The sweep step itself has continue-on-error and a timeout-minutes below the
# job's: the script never fails, but a docker call can hang (a wedged Docker
# Desktop VM), and that must cost the run its sweep, not the job, the gate and
# the run's own cleanup after it.
#
# These checks read the workflow's text for those mistakes. They are not a
# proof that the jobs behave: a `|| true` after the command passes them.
#
# steps_of prints a line per step of job $2 in workflow file $1, in order:
# <id>|<uses>|<continue-on-error>|<1 if it calls $3>|<timeout-minutes>|<if>.
# Only a key at the step's own level counts, not a line of its run: script or
# its with: block, and a step's name: or a comment does not call $3.
steps_re='^[[:space:]]*steps:[[:space:]]*(#.*)?$'
step_key_re='^[[:space:]]*(id|uses|if|continue-on-error|timeout-minutes):[[:space:]]*(.*)$'
step_name_re='^[[:space:]]*name:'
steps_of() {
  local job_key_re="^[[:space:]]+$2:[[:space:]]*(#.*)?\$"
  local line lead job_at=-1 key_at=-1 steps_at=-1 item_at=-1 open=0 id uses coe calls cond tm
  # The record is split on |, and only its last field (the if:) may hold one,
  # so a | in any other (${{ inputs.t || 5 }} as a timeout) is written as ?.
  put() {
    if [ "$open" -eq 1 ]; then
      printf '%s|%s|%s|%s|%s|%s\n' "${id//|/?}" "${uses//|/?}" "${coe//|/?}" "$calls" "${tm//|/?}" "$cond"
    fi
    open=0
  }
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    if [[ "$line" =~ $blank_re ]]; then continue; fi
    lead="${line%%[! ]*}"
    if [ "$job_at" -lt 0 ]; then
      if [[ "$line" =~ $job_key_re ]]; then job_at="${#lead}"; fi
      continue
    fi
    if [ "${#lead}" -le "$job_at" ]; then break; fi
    if [ "$key_at" -lt 0 ]; then key_at="${#lead}"; fi
    if [ "$steps_at" -lt 0 ]; then
      if [ "${#lead}" -eq "$key_at" ] && [[ "$line" =~ $steps_re ]]; then steps_at="${#lead}"; fi
      continue
    fi
    if [ "$item_at" -lt 0 ]; then item_at="${#lead}"; fi
    if [ "${#lead}" -lt "$item_at" ]; then break; fi
    if [ "${#lead}" -eq "$item_at" ]; then
      # A line at the items' depth that is not an item is the job's next key.
      if ! [[ "$line" =~ $item_re ]]; then break; fi
      put
      open=1 id='' uses='' coe='' calls=0 cond='' tm=''
      # The key on the "- " line belongs with the keys under it.
      lead="$lead  "
      line="$lead${BASH_REMATCH[1]}"
    fi
    if [ "${#lead}" -eq $((item_at + 2)) ]; then
      if [[ "$line" =~ $step_key_re ]]; then
        case "${BASH_REMATCH[1]}" in
          id) id="$(trim "${BASH_REMATCH[2]}")" ;;
          uses) uses="$(trim "${BASH_REMATCH[2]}")" ;;
          if) cond="$(trim "${BASH_REMATCH[2]}")" ;;
          continue-on-error) coe="$(trim "${BASH_REMATCH[2]}")" ;;
          timeout-minutes) tm="$(trim "${BASH_REMATCH[2]}")" ;;
        esac
        continue
      fi
      if [[ "$line" =~ $step_name_re ]]; then continue; fi
    fi
    case "$line" in *"$3"*) calls=1 ;; esac
  done <"$1"
  put
}
# The value of key $3 of job $2 in workflow file $1, as words on one line: what
# is written on the key's line (one value, or a [flow, list]), or the items of
# a block list under it, without quotes. Only the key at the job's own level
# counts, not one under its steps or strategy, and only within that job.
job_key_of() {
  local job_key_re="^[[:space:]]+$2:[[:space:]]*(#.*)?\$" key_re="^[[:space:]]*$3:[[:space:]]*(.*)\$"
  local line lead v job_at=-1 key_at=-1 under=0 words=''
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    if [[ "$line" =~ $blank_re ]]; then continue; fi
    lead="${line%%[! ]*}"
    if [ "$job_at" -lt 0 ]; then
      if [[ "$line" =~ $job_key_re ]]; then job_at="${#lead}"; fi
      continue
    fi
    if [ "${#lead}" -le "$job_at" ]; then break; fi
    if [ "$key_at" -lt 0 ]; then key_at="${#lead}"; fi
    if [ "$under" -eq 1 ]; then
      # The list ends at the first line that is not one of its items.
      if [ "${#lead}" -lt "$key_at" ] || ! [[ "$line" =~ $item_re ]]; then break; fi
      words="$words $(trim "${BASH_REMATCH[1]}")"
    elif [ "${#lead}" -eq "$key_at" ] && [[ "$line" =~ $key_re ]]; then
      v="$(trim "${BASH_REMATCH[1]}")"
      if [ -n "$v" ]; then
        words="$v"
        break
      fi
      under=1
    fi
  done <"$1"
  printf '%s' "$words" | sed -E "s/[][,\"']/ /g" | tr -s ' ' '\n' | grep -v '^$' | tr '\n' ' ' | sed 's/ $//'
}
# What is wrong with how job $2 of workflow file $1 runs the sweep, a line
# each. An if: is read as conditions joined by &&; with an || in it, none of
# them is required.
sweep_problems() {
  local id uses coe calls cond expr c fetched='' required sweeps=0
  case " $(job_key_of "$1" "$2" needs) " in
    *' precheck-macos '*) ;;
    *) echo "the job does not list precheck-macos under needs:, so needs.precheck-macos.result is never 'success' there and the sweep never runs" ;;
  esac
  while IFS='|' read -r id uses coe calls tm cond; do
    case "$uses" in
      actions/checkout@*)
        fetched="$fetched $id"
        if [ "$coe" != true ]; then
          echo "its checkout step has no continue-on-error: true, so a failed fetch fails the job and skips the cleanup after it"
        fi
        ;;
    esac
    if [ "$calls" != 1 ]; then continue; fi
    sweeps=$((sweeps + 1))
    expr="&&$(printf '%s' "$cond" | sed -E 's/\$\{\{|\}\}//g' | tr -d '[:space:]')&&"
    case "$expr" in *'||'*) expr='' ;; esac
    required=0
    for c in $fetched; do
      case "$expr" in *"&&steps.$c.outcome=='success'&&"*) required=1 ;; esac
    done
    if [ "$required" -eq 0 ]; then
      echo "the sweep's if: does not require steps.<id>.outcome == 'success' of a checkout step before it, so it can run the copy of the script the runner's last job left"
    fi
    case "$expr" in
      *"&&needs.precheck-macos.result=='success'&&"*) ;;
      *) echo "the sweep's if: does not require needs.precheck-macos.result == 'success', so it can run without its cases having passed on this machine" ;;
    esac
  done <<<"$(steps_of "$1" "$2" reap-stale-containers.sh)"
  if [ "$sweeps" -ne 1 ]; then echo "$sweeps steps call reap-stale-containers.sh, want 1"; fi
}
# What is wrong with the sweep step of job $2 of workflow file $1 itself, a
# line each.
sweep_step_problems() {
  local id uses coe calls tm cond job_tm
  job_tm="$(job_key_of "$1" "$2" timeout-minutes)"
  while IFS='|' read -r id uses coe calls tm cond; do
    if [ "$calls" != 1 ]; then continue; fi
    if [ "$coe" != true ]; then
      echo "the sweep step has no continue-on-error: true, so a docker call that fails or hangs fails the job"
    fi
    if ! [[ "$tm" =~ $minutes_re ]] || ! [[ "$job_tm" =~ $minutes_re ]] || [ "$tm" -ge "$job_tm" ]; then
      echo "the sweep step's timeout-minutes (${tm:-none}) is not below the job's (${job_tm:-none}), so a docker call that hangs uses up the job"
    fi
  done <<<"$(steps_of "$1" "$2" reap-stale-containers.sh)"
}
# What is wrong with how job $2 of workflow file $1 runs the script's cases, a
# line each: one step, with no if: and no continue-on-error, and no step that
# runs these workflow checks.
suite_problems() {
  local id uses coe calls tm cond suites=0 checks=0
  while IFS='|' read -r id uses coe calls tm cond; do
    if [ "$calls" != 1 ]; then continue; fi
    suites=$((suites + 1))
    if [ -n "$coe" ] || [ -n "$cond" ]; then
      echo "the step that runs the cases has a continue-on-error or an if:, so the job can succeed without them passing"
    fi
  done <<<"$(steps_of "$1" "$2" reap-stale-containers.tests.sh)"
  if [ "$suites" -ne 1 ]; then echo "$suites steps run reap-stale-containers.tests.sh, want 1"; fi
  while IFS='|' read -r id uses coe calls tm cond; do
    if [ "$calls" = 1 ]; then checks=$((checks + 1)); fi
  done <<<"$(steps_of "$1" "$2" reap-stale-containers.workflows.tests.sh)"
  if [ "$checks" -ne 0 ]; then echo "it runs reap-stale-containers.workflows.tests.sh, so a mistake in any workflow fails it"; fi
}
# What is wrong with the steps of job $2 of workflow file $1 that sweep a real
# Docker daemon (they name reap-e2e-created): each must run only on a
# GitHub-hosted runner, whose daemon no other job uses.
hosted_problems() {
  local id uses coe calls tm cond expr steps=0
  while IFS='|' read -r id uses coe calls tm cond; do
    if [ "$calls" != 1 ]; then continue; fi
    steps=$((steps + 1))
    expr="&&$(printf '%s' "$cond" | sed -E 's/\$\{\{|\}\}//g' | tr -d '[:space:]')&&"
    case "$expr" in
      *'||'*) echo "the if: of the step that sweeps a real daemon has an ||" ;;
      *"&&runner.environment=='github-hosted'&&"*) ;;
      *) echo "the step that sweeps a real daemon does not require runner.environment == 'github-hosted'" ;;
    esac
  done <<<"$(steps_of "$1" "$2" reap-e2e-created)"
  if [ "$steps" -eq 0 ]; then echo "no step sweeps a real daemon"; fi
}
# What is wrong with how workflow file $1 keeps that step's sweeps to its own
# container, a line each: every `docker ps` in it lists by that one name, and
# every run of the script there goes through that docker. This is the second
# guard, for when the step does run where other jobs' containers are.
#
# A line runs the script if it names reap-stale-containers.sh or $script,
# however it is called (bash "$script", bash .github/.../...sh, ./...sh), except
# a line that only puts the path in a variable and one that reads
# MIN_AGE_MINUTES out of the file. reap-stale-containers.tests.sh and
# ...workflows.tests.sh do not contain the name.
names_script_re='reap-stale-containers[.]sh|[$][{]?script([^A-Za-z0-9_]|$)'
sets_script_re='^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*reap-stale-containers[.]sh[^[:space:]]*[[:space:]]*$'
# shellcheck disable=SC2016 # the step's text, $script and $scoped included
scoped_problems() {
  local line listings=0 sweeps=0
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ "$line" =~ $blank_re ]]; then continue; fi
    case "$line" in
      *'docker ps'*)
        listings=$((listings + 1))
        case "$line" in
          *"--filter 'name=^/?reap-e2e-created\$'"*) ;;
          *) echo "a docker ps that lists more than reap-e2e-created: $(trim "$line")" ;;
        esac
        ;;
    esac
    if [[ "$line" =~ $names_script_re ]] && ! [[ "$line" =~ $sets_script_re ]]; then
      case "$line" in
        *MIN_AGE_MINUTES*) ;;
        *'REAP_DOCKER="$scoped"'*) sweeps=$((sweeps + 1)) ;;
        *)
          sweeps=$((sweeps + 1))
          echo "a sweep that does not go through the scoped docker: $(trim "$line")"
          ;;
      esac
    fi
  done <"$1"
  if [ "$listings" -eq 0 ]; then echo "no docker ps that lists only reap-e2e-created"; fi
  if [ "$sweeps" -eq 0 ]; then echo "no sweep of a real daemon"; fi
}

# The step reader and the check, on each way of getting it wrong. In
# "hardfail" a failed checkout fails the job and the sweep does not ask for
# it; "unchecked" lets the job go on and still does not ask. "either" sweeps
# after a failed step whatever the two conditions say. In "decoys" the steps
# sit at the depth of steps:, the real continue-on-error and if: are missing
# or wrong, and a with: block, a run: script, a name: and a comment carry the
# right text. "unneeded" and "neednot" do not list precheck-macos under
# needs:, on the line and as a list. "exprstep" is right, with a | in its
# step's timeout that must not spill into the if:. The next five are for the job that runs
# the script's cases: "tested" is right, in "unrun" only the step's name
# mentions them, and "checkstoo" also runs these workflow checks. The last
# five are for the step that sweeps a real daemon.
cat >"$work/sweep.yml" <<'EOF'
on: push
jobs:
  right:
    runs-on: [self-hosted, macOS]
    needs: [precheck-macos, run_python_docker_macos, run_golang_docker_macos]
    steps:
      - name: Checkout repository
        id: checkout
        continue-on-error: true
        uses: actions/checkout@v4
      - name: Remove stale containers (macOS)
        if: steps.checkout.outcome == 'success' && needs.precheck-macos.result == 'success'
        run: bash "$GITHUB_WORKSPACE/.github/scripts/macos/reap-stale-containers.sh"
      - run: bash .github/scripts/macos/reap-stale-containers.tests.sh
  wrapped:
    steps:
      - uses: actions/checkout@v4 # the key on the "- " line
        continue-on-error: true # so that the job goes on
        id: fetch
      - if: ${{ needs.precheck-macos.result == 'success' && success() && steps.fetch.outcome == 'success' }}
        run: |
          bash .github/scripts/macos/reap-stale-containers.sh
    timeout-minutes: 10
    needs: precheck-macos # after the steps, and not a list
  hardfail:
    needs:
      - run_python_docker_macos
      - precheck-macos
    steps:
      - name: Checkout repository
        uses: actions/checkout@v4
      - name: Remove stale containers (macOS)
        if: needs.precheck-macos.result == 'success'
        run: bash "$GITHUB_WORKSPACE/.github/scripts/macos/reap-stale-containers.sh"
  stops:
    needs: [precheck-macos]
    steps:
      - id: checkout
        uses: actions/checkout@v4
      - if: steps.checkout.outcome == 'success' && needs.precheck-macos.result == 'success'
        run: bash .github/scripts/macos/reap-stale-containers.sh
  unchecked:
    needs: [precheck-macos]
    steps:
      - id: checkout
        continue-on-error: true
        uses: actions/checkout@v4
      - if: needs.precheck-macos.result == 'success'
        run: bash .github/scripts/macos/reap-stale-containers.sh
  unproven:
    needs: [precheck-macos]
    steps:
      - id: checkout
        continue-on-error: true
        uses: actions/checkout@v4
      - if: steps.checkout.outcome == 'success'
        run: bash .github/scripts/macos/reap-stale-containers.sh
  conclusion:
    needs: [precheck-macos]
    steps:
      - id: checkout
        continue-on-error: true
        uses: actions/checkout@v4
      - if: steps.checkout.conclusion == 'success' && needs.precheck-macos.result == 'success'
        run: bash .github/scripts/macos/reap-stale-containers.sh
  either:
    needs: [precheck-macos]
    steps:
      - id: checkout
        continue-on-error: true
        uses: actions/checkout@v4
      - if: failure() || success() && steps.checkout.outcome == 'success' && needs.precheck-macos.result == 'success' && runner.os == 'macOS'
        run: bash .github/scripts/macos/reap-stale-containers.sh
  early:
    needs: [precheck-macos]
    steps:
      - if: steps.checkout.outcome == 'success' && needs.precheck-macos.result == 'success'
        run: bash .github/scripts/macos/reap-stale-containers.sh
      - id: checkout
        continue-on-error: true
        uses: actions/checkout@v4
  decoys:
    needs: [precheck-macos]
    steps:
    - name: Fetch reap-stale-containers.sh
      id: checkout
      uses: actions/checkout@v4
      with:
        continue-on-error: true
    # run: bash .github/scripts/macos/reap-stale-containers.sh
    - if: always()
      run: |
        bash .github/scripts/macos/reap-stale-containers.sh
        if: steps.checkout.outcome == 'success' && needs.precheck-macos.result == 'success'
  unneeded:
    needs: [run_python_docker_macos, run_golang_docker_macos]
    steps:
      - id: checkout
        continue-on-error: true
        uses: actions/checkout@v4
      - if: steps.checkout.outcome == 'success' && needs.precheck-macos.result == 'success'
        run: bash .github/scripts/macos/reap-stale-containers.sh
  neednot:
    needs:
      - run_python_docker_macos
      # - precheck-macos
    steps:
      - id: checkout
        continue-on-error: true
        uses: actions/checkout@v4
      - if: steps.checkout.outcome == 'success' && needs.precheck-macos.result == 'success'
        run: bash .github/scripts/macos/reap-stale-containers.sh
  exprstep:
    needs: [precheck-macos]
    steps:
      - id: checkout
        continue-on-error: true
        uses: actions/checkout@v4
      - if: steps.checkout.outcome == 'success' && needs.precheck-macos.result == 'success'
        timeout-minutes: ${{ inputs.t || 5 }}
        run: bash .github/scripts/macos/reap-stale-containers.sh
  nosweep:
    needs: [precheck-macos]
    steps:
      - run: bash .github/scripts/macos/reap-stale-containers.tests.sh
  tested:
    steps:
      - uses: actions/checkout@v4
      - name: Test the macOS Docker cleanup script
        timeout-minutes: 5
        run: bash "$GITHUB_WORKSPACE/.github/scripts/macos/reap-stale-containers.tests.sh"
  optional:
    steps:
      - continue-on-error: true
        run: bash .github/scripts/macos/reap-stale-containers.tests.sh
  sometimes:
    steps:
      - if: runner.os == 'macOS'
        run: bash .github/scripts/macos/reap-stale-containers.tests.sh
  unrun:
    steps:
      - name: Test with reap-stale-containers.tests.sh
        run: bash .github/scripts/macos/reap-stale-containers.sh
  checkstoo:
    steps:
      - run: bash .github/scripts/macos/reap-stale-containers.tests.sh
      - run: bash .github/scripts/macos/reap-stale-containers.workflows.tests.sh
  hosted:
    steps:
      - name: Sweep a real Docker daemon (this runner's own)
        if: runner.environment == 'github-hosted'
        run: |
          docker create --name reap-e2e-created reap-e2e:empty /none
  hostedwrapped:
    steps:
      - if: ${{ runner.os == 'Linux' && runner.environment == 'github-hosted' }}
        run: docker create --name reap-e2e-created reap-e2e:empty /none
  anywhere:
    steps:
      - name: Sweep a real Docker daemon (this runner's own)
        run: docker create --name reap-e2e-created reap-e2e:empty /none
  orhosted:
    steps:
      - if: runner.environment == 'github-hosted' || always()
        run: docker create --name reap-e2e-created reap-e2e:empty /none
  selfhosted:
    steps:
      - if: runner.environment == 'self-hosted'
        run: docker create --name reap-e2e-created reap-e2e:empty /none
EOF
out=''
for job in right wrapped hardfail stops unchecked unproven conclusion either early decoys unneeded neednot exprstep nosweep; do
  out="$out$job=$(sweep_problems "$work/sweep.yml" "$job" | sed -E 's/^its checkout step has no continue-on-error.*$/fails/; s/^.* does not require steps\..*$/stale/; s/^.* does not require needs\..*$/untested/; s/^the job does not list precheck-macos.*$/unlisted/; s/^[0-9]+ steps call.*$/count/' | tr '\n' '+')$nl"
done
want="right=${nl}wrapped=${nl}hardfail=fails+stale+${nl}stops=fails+${nl}unchecked=stale+${nl}unproven=untested+${nl}conclusion=stale+${nl}either=stale+untested+${nl}early=stale+${nl}decoys=fails+stale+untested+${nl}unneeded=unlisted+${nl}neednot=unlisted+${nl}exprstep=${nl}nosweep=count+$nl"
if [ "$out" != "$want" ]; then problem "flagged [$(tr '\n' ' ' <<<"$out")], want [$(tr '\n' ' ' <<<"$want")]"; fi
report "the check of the sweep's job reads a step's own keys and the job's needs:, and flags a checkout that can fail the job and an if: that lets an unfetched or untested script run, or none"

out=''
for job in tested optional sometimes unrun; do
  out="$out$job=$(suite_problems "$work/sweep.yml" "$job" | sed -E 's/^the step that runs the cases has.*$/soft/; s/^[0-9]+ steps run.*$/count/' | tr '\n' '+')$nl"
done
want="tested=${nl}optional=soft+${nl}sometimes=soft+${nl}unrun=count+$nl"
if [ "$out" != "$want" ]; then problem "flagged [$(tr '\n' ' ' <<<"$out")], want [$(tr '\n' ' ' <<<"$want")]"; fi
out="$(suite_problems "$work/sweep.yml" checkstoo)"
case "$out" in
  'it runs reap-stale-containers.workflows.tests.sh'*) ;;
  *) problem "on a job that also runs these workflow checks, said [$out]" ;;
esac
report "the check of the job that runs the script's cases flags a step with an if: or a continue-on-error, a job with no such step, and one that also runs these workflow checks"

out=''
for job in hosted hostedwrapped anywhere orhosted selfhosted nosweep; do
  out="$out$job=$(hosted_problems "$work/sweep.yml" "$job" | sed -E "s/^.*has an [|][|]$/or/; s/^.* does not require runner.environment.*$/anywhere/; s/^no step sweeps.*$/none/" | tr '\n' '+')$nl"
done
want="hosted=${nl}hostedwrapped=${nl}anywhere=anywhere+${nl}orhosted=or+${nl}selfhosted=anywhere+${nl}nosweep=none+$nl"
if [ "$out" != "$want" ]; then problem "flagged [$(tr '\n' ' ' <<<"$out")], want [$(tr '\n' ' ' <<<"$want")]"; fi
report "the check of the step that sweeps a real daemon flags one that can run on a runner that is not GitHub-hosted"

# How the real-daemon step keeps to its own container: right, a listing of
# every container, a sweep of $script straight on docker, one of the script's
# path straight on docker, and no wrapper at all. In "right", the line that
# sets $script and the one that reads its threshold are not sweeps.
mkdir "$work/scoped"
cat >"$work/scoped/right.yml" <<'EOF'
          script=.github/scripts/macos/reap-stale-containers.sh
          minutes=$(($(sed -n 's/^MIN_AGE_MINUTES=\([0-9][0-9]*\)$/\1/p' "$script") + 1))
          cat >"$scoped" <<'EOS'
          # docker, listing only reap-e2e-created
          if [ "$1" = ps ]; then shift; exec docker ps --filter 'name=^/?reap-e2e-created$' "$@"; fi
          EOS
          out="$(REAP_DOCKER="$scoped" bash "$script")"
          out="$(AHEAD_MINUTES="$minutes" REAP_DOCKER="$scoped" bash "$script")"
EOF
sed "s/ps --filter 'name=^\/?reap-e2e-created\$' /ps /" "$work/scoped/right.yml" >"$work/scoped/everything.yml"
sed '$s/REAP_DOCKER="$scoped" //' "$work/scoped/right.yml" >"$work/scoped/direct.yml"
cp "$work/scoped/right.yml" "$work/scoped/bypath.yml"
echo '          bash .github/scripts/macos/reap-stale-containers.sh' >>"$work/scoped/bypath.yml"
# shellcheck disable=SC2016 # the step's text
grep -v 'docker ps' "$work/scoped/right.yml" | grep -v 'bash "$script"' >"$work/scoped/none.yml"
out=''
for f in right everything direct bypath none; do
  out="$out$f=$(scoped_problems "$work/scoped/$f.yml" | sed -E 's/^a docker ps that lists more.*$/lists/; s/^a sweep that does not go through.*$/direct/; s/^no docker ps.*$/nolist/; s/^no sweep.*$/nosweep/' | tr '\n' '+') "
done
want="right= everything=lists+ direct=direct+ bypath=direct+ none=nolist+nosweep+ "
if [ "$out" != "$want" ]; then problem "flagged [$out], want [$want]"; fi
report "the check of the real-daemon step's docker flags a listing of every container, and a sweep that is not made through it"

# job_key_of, on the forms needs: and timeout-minutes take, and on a key of the
# same name that belongs to something else.
cat >"$work/keys.yml" <<'EOF'
on: push
jobs:
  noneeds:
    runs-on: ubuntu-latest
    steps:
      - run: echo
  nextjob:
    needs: [precheck-macos]
    steps: []
  decoyed:
    steps:
      - uses: some/action@v1
        with:
          needs: precheck-macos
      - run: |
          needs: [precheck-macos]
    needs: [a] # after the steps
  listthen:
    needs:
      - a
    strategy:
      matrix:
        x:
          - precheck-macos
  sameindent:
    needs:
    - a
    - 'precheck-macos'
    steps: []
  quoted:
    needs: ["precheck-macos", "b"]
  plain:
    needs: precheck-macos # one job
    timeout-minutes: 10 # minutes
    steps:
      - run: echo
        timeout-minutes: 99
EOF
out=''
for job in noneeds nextjob decoyed listthen sameindent quoted plain; do
  out="$out$job=[$(job_key_of "$work/keys.yml" "$job" needs)]/[$(job_key_of "$work/keys.yml" "$job" timeout-minutes)] "
done
want="noneeds=[]/[] nextjob=[precheck-macos]/[] decoyed=[a]/[] listthen=[a]/[] sameindent=[a precheck-macos]/[] quoted=[precheck-macos b]/[] plain=[precheck-macos]/[10] "
if [ "$out" != "$want" ]; then problem "read [$out], want [$want]"; fi
report "the reader of a job's needs: and timeout-minutes stops at the end of the job and of the list, and reads only the job's own key"

# The sweep step's own keys: "right" is right, the others each get one wrong.
cat >"$work/step.yml" <<'EOF'
on: push
jobs:
  right:
    timeout-minutes: 10
    steps:
      - name: Remove stale containers (macOS)
        if: steps.checkout.outcome == 'success'
        timeout-minutes: 5
        continue-on-error: true
        run: bash "$GITHUB_WORKSPACE/.github/scripts/macos/reap-stale-containers.sh"
  hard:
    timeout-minutes: 10
    steps:
      - timeout-minutes: 5
        run: bash .github/scripts/macos/reap-stale-containers.sh
  untimed:
    timeout-minutes: 10
    steps:
      - continue-on-error: true
        run: bash .github/scripts/macos/reap-stale-containers.sh
  aslong:
    timeout-minutes: 10
    steps:
      - continue-on-error: true
        timeout-minutes: 10
        run: bash .github/scripts/macos/reap-stale-containers.sh
  jobuntimed:
    steps:
      - continue-on-error: true
        timeout-minutes: 5
        run: bash .github/scripts/macos/reap-stale-containers.sh
  exprtime:
    timeout-minutes: 10
    steps:
      - continue-on-error: true
        timeout-minutes: ${{ inputs.t || 5 }}
        run: bash .github/scripts/macos/reap-stale-containers.sh
EOF
out=''
for job in right hard untimed aslong jobuntimed exprtime; do
  out="$out$job=$(sweep_step_problems "$work/step.yml" "$job" | sed -E 's/^the sweep step has no continue-on-error.*$/fails/; s/^the sweep step.s timeout-minutes.*$/time/' | tr '\n' '+')$nl"
done
want="right=${nl}hard=fails+${nl}untimed=time+${nl}aslong=time+${nl}jobuntimed=time+${nl}exprtime=time+$nl"
if [ "$out" != "$want" ]; then problem "flagged [$(tr '\n' ' ' <<<"$out")], want [$(tr '\n' ' ' <<<"$want")]"; fi
report "the check of the sweep step flags one that can fail the job, and one without a timeout-minutes below the job's"

out="$(sweep_problems "$repo_root/.github/workflows/prepare_and_run.yml" clean_up_docker_macos)"
out="$out${out:+$nl}$(sweep_step_problems "$repo_root/.github/workflows/prepare_and_run.yml" clean_up_docker_macos)"
out="${out%"$nl"}"
if [ -n "$out" ]; then problem "clean_up_docker_macos in prepare_and_run.yml:"; fi
report "clean_up_docker_macos lets a failed checkout pass, its sweep asks for that checkout and for precheck-macos, which it needs, and a sweep that hangs or fails cannot fail the job"

out="$(suite_problems "$repo_root/.github/workflows/prepare_and_run.yml" precheck-macos)"
if [ -n "$out" ]; then problem "precheck-macos in prepare_and_run.yml:"; fi
report "precheck-macos, whose result the sweep asks for, runs the script's cases in a step with no if: and no continue-on-error, and not these workflow checks"

out="$(hosted_problems "$repo_root/.github/workflows/macos-ci-scripts.yml" reaper-linux)"
out="$out${out:+$nl}$(scoped_problems "$repo_root/.github/workflows/macos-ci-scripts.yml")"
out="${out%"$nl"}"
if [ -n "$out" ]; then problem "reaper-linux in macos-ci-scripts.yml:"; fi
report "in macos-ci-scripts.yml, the step that sweeps a real Docker daemon runs only on a GitHub-hosted runner, and sees no container but its own"

if [ "$failures" -gt 0 ]; then
  echo "$failures case(s) failed"
  exit 1
fi
echo "all cases passed"

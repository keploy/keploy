#!/bin/bash
# check-setup-java.tests.sh: check-setup-java.sh on this repository, and on a
# repository made for each case below. Run by the lint workflow, or locally
# (it leaves nothing behind):
#
#   bash .github/workflows/test_workflow_scripts/check-setup-java.tests.sh
#
# It passes on this repository; where no file names setup-java; on uses of
# setup-java-maven and of setup-java-maven/jdk; on setup-java named in a YAML
# comment, at the start of a line or after one, in a shell script (not YAML),
# or in a step's script; and on setup-java-maven/jdk's own steps: distribution
# jdkfile with a jdkFile, in block or flow style, and distribution temurin of
# ${{ steps.pin.outputs.version }} under if: steps.pin.outputs.source ==
# 'image' (with or without ${{ }}).
# It fails, naming the step's line, on a step that uses actions/setup-java in
# a workflow (.yml and .yaml; quoted or not; at a tag, a commit or in other
# letter cases; with uses: first or after name:; in flow style; with CRLF line
# ends; with distribution jdkfile too), in a composite action, and in an
# action nested in setup-java-maven's directory; and on setup-java-maven/jdk's
# own step when it is neither: temurin with no if:, with an if: that is not
# exactly the image source's ("|| always()", 'archive'), or of a major version
# rather than the pinned build; when either form adds an input outside the
# allow-list (check-latest, architecture, java-package, an input setup-java
# may add later, in block or flow style) or a step key (env:); and on that
# gated temurin step in a workflow. Every allowed input together passes.
# Output is matched in full (holds), never through a pipe into grep -q, whose
# early exit can kill the writer (SIGPIPE) and fail the row under pipefail: a
# row checks that on a body larger than a pipe's buffer.
# shellcheck disable=SC2016 # ${{ }} in the cases is the YAML's expression, not the shell's
set -euo pipefail

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$dir/check-setup-java.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Nothing from the runner's or the user's git configuration (insteadOf rules,
# hooks, signing) reaches these repositories.
export GIT_CONFIG_GLOBAL="$tmp/gitconfig" GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
: >"$GIT_CONFIG_GLOBAL"

fails=0
n=0

# repo NAME PATH=CONTENT...: a git repository of its own, holding each file.
repo() {
  local r="$tmp/$1" spec path
  shift
  mkdir -p "$r"
  git init -q "$r"
  for spec in "$@"; do
    path=${spec%%=*}
    mkdir -p "$r/$(dirname "$path")"
    printf '%s' "${spec#*=}" >"$r/$path"
  done
  git -C "$r" add -A
  git -C "$r" commit -q --allow-empty -m t
}

# holds TEXT PART: TEXT holds PART. A match on the variable itself: piped into
# a reader that stops at its first match (grep -q), the writer could die of
# SIGPIPE, which pipefail reports as a failure although the text is there.
holds() { [[ $1 == *"$2"* ]]; }

# expect CASE pass|fail [TEXT...]: the check on repository CASE ends so, and
# its output holds each TEXT.
expect() {
  local name=$1 want=$2 out rc=0 got text
  shift 2
  n=$((n + 1))
  out=$(cd "$tmp/$name" && bash "$script" 2>&1) || rc=$?
  got=pass
  [ "$rc" -eq 0 ] || got=fail
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: check ended $got (exit $rc), want $want; it said:"
    printf '%s\n' "$out" | sed 's/^/    /'
    fails=$((fails + 1))
    return
  fi
  for text in "$@"; do
    if ! holds "$out" "$text"; then
      echo "FAIL $name: output lacks '$text'; it said:"
      printf '%s\n' "$out" | sed 's/^/    /'
      fails=$((fails + 1))
      return
    fi
  done
  echo "ok   $name ($want)"
}

# wf USES [WITH...]: a workflow whose second step is `uses: USES` with these lines.
wf() {
  local uses=$1 l
  shift
  printf 'jobs:\n  t:\n    steps:\n      - uses: actions/checkout@v4\n      - uses: %s\n' "$uses"
  if [ "$#" -gt 0 ]; then
    printf '        with:\n'
    for l in "$@"; do printf '          %s\n' "$l"; done
  fi
}

# own [WITH...]: setup-java-maven/jdk's action, its setup-java step with these lines.
own() {
  local l
  printf 'runs:\n  using: composite\n  steps:\n    - id: pin\n      shell: bash\n      run: bash jdk.sh pin\n    - uses: actions/setup-java@v4\n'
  if [ "$#" -gt 0 ]; then
    printf '      with:\n'
    for l in "$@"; do printf '        %s\n' "$l"; done
  fi
}
O=.github/actions/setup-java-maven/jdk/action.yml
J='jdkFile: ${{ steps.pin.outputs.file }}'

n=$((n + 1))
rc=0
out=$(cd "$dir" && bash "$script" 2>&1) || rc=$?
if [ "$rc" -eq 0 ]; then echo "ok   this repository (pass)"; else
  echo "FAIL this repository: check ended fail (exit $rc); it said:"; printf '%s\n' "$out" | sed 's/^/    /'; fails=$((fails + 1))
fi

n=$((n + 1))
big="needle on the first line"$'\n'$(head -c 300000 /dev/zero | tr '\0' x | fold -w 100)
if holds "$big" "needle on the first line"; then echo "ok   holds() reads all of a ${#big}-byte body, the text on its first line"; else
  echo "FAIL holds() missed the text on the first line of a ${#big}-byte body"; fails=$((fails + 1))
fi

repo none ".github/workflows/w.yml=$(wf actions/setup-go@v6 'go-version: "1.21"')"
expect none pass
repo composites ".github/workflows/w.yml=$(wf ./.github/actions/setup-java-maven 'java-version: "8"')
      - uses: ./.github/actions/setup-java-maven/save
      - uses: ./.github/actions/setup-java-maven/jdk
        with:
          java-version: \"17\"
      - uses: ./keploy/.github/actions/setup-java-maven/jdk
"
expect composites pass
repo comments ".github/workflows/w.yml=jobs:
  t:
    steps:
      # - uses: actions/setup-java@v4
      - run: true  # uses: actions/setup-java@v4
"
expect comments pass
repo shell ".github/workflows/x.sh=- uses: actions/setup-java@v4
" ".github/workflows/w.yml=jobs:
  t:
    steps:
      - run: |
          echo 'uses: actions/setup-java@v4'
          grep -n uses:\ actions/setup-java@ .github
"
expect shell pass
repo own-block "$O=$(own 'distribution: jdkfile' "$J" 'java-version: ${{ steps.pin.outputs.version }}')"
expect own-block pass
repo own-flow "$O=runs:
  using: composite
  steps:
    - { uses: actions/setup-java@v4, with: { distribution: 'jdkfile', $J } }
"
expect own-flow pass

for c in plain:actions/setup-java@v4 double:'"actions/setup-java@v4"' single:"'actions/setup-java@v4'" \
  sha:actions/setup-java@cf277c60eb25467037889841efdb72551f06f6c3 caps:Actions/Setup-Java@v4; do
  repo "wf-${c%%:*}" ".github/workflows/w.yml=$(wf "${c#*:}" 'distribution: temurin' 'java-version: "8"')"
  expect "wf-${c%%:*}" fail ".github/workflows/w.yml:5" "setup-java-maven/jdk"
done
repo yaml ".github/workflows/w.yaml=$(wf actions/setup-java@v4 'java-version: "17"')"
expect yaml fail ".github/workflows/w.yaml:5"
repo name-first ".github/workflows/w.yml=jobs:
  t:
    steps:
      - name: Set up Java
        uses: actions/setup-java@v4  # the JDK
        with:
          java-version: \"17\"
"
expect name-first fail ".github/workflows/w.yml:4"
repo flow ".github/workflows/w.yml=jobs:
  t:
    steps:
      - { name: java, uses: actions/setup-java@v4, with: { java-version: \"17\" } }
"
expect flow fail ".github/workflows/w.yml:4"
repo crlf ".github/workflows/w.yml=$(wf actions/setup-java@v4 'java-version: "17"' | sed 's/$/\r/')"
expect crlf fail ".github/workflows/w.yml:5"
repo wf-jdkfile ".github/workflows/w.yml=$(wf actions/setup-java@v4 'distribution: jdkfile' "$J" 'java-version: "17"')"
expect wf-jdkfile fail ".github/workflows/w.yml:5"
repo composite ".github/actions/java/action.yml=runs:
  using: composite
  steps:
    - uses: actions/setup-java@v4
      with:
        java-version: \"17\"
"
expect composite fail ".github/actions/java/action.yml:4"
repo nested ".github/actions/setup-java-maven/other/action.yml=$(own 'distribution: jdkfile' "$J")"
expect nested fail ".github/actions/setup-java-maven/other/action.yml:7"
repo own-temurin "$O=$(own 'distribution: temurin' 'java-version: "8"')"
expect own-temurin fail "$O:7 (setup-java-maven/jdk: neither distribution jdkfile with a jdkFile"
repo own-no-file "$O=$(own 'distribution: jdkfile' 'java-version: "8"')"
expect own-no-file fail "$O:7 (setup-java-maven/jdk: neither distribution jdkfile with a jdkFile"
# imagestep IF VERSION: setup-java-maven/jdk with a temurin step under IF (none if empty) for VERSION.
imagestep() {
  printf 'runs:\n  using: composite\n  steps:\n    - id: pin\n      shell: bash\n      run: bash jdk.sh pin\n'
  [ -z "$1" ] || printf '    - if: %s\n      uses: actions/setup-java@v4\n' "$1"
  [ -n "$1" ] || printf '    - uses: actions/setup-java@v4\n'
  printf '      with:\n        distribution: temurin\n        java-version: %s\n' "$2"
}
V='${{ steps.pin.outputs.version }}'
repo own-image "$O=$(imagestep "steps.pin.outputs.source == 'image'" "$V")"
expect own-image pass
repo own-image-expr "$O=$(imagestep "\${{ steps.pin.outputs.source == 'image' }}" "$V")"
expect own-image-expr pass
repo own-image-ungated "$O=$(imagestep "" "$V")"
expect own-image-ungated fail "$O:7 (setup-java-maven/jdk: neither"
repo own-image-or "$O=$(imagestep "steps.pin.outputs.source == 'image' || always()" "$V")"
expect own-image-or fail "$O:7 (setup-java-maven/jdk: neither"
repo own-image-archive "$O=$(imagestep "steps.pin.outputs.source == 'archive'" "$V")"
expect own-image-archive fail "$O:7 (setup-java-maven/jdk: neither"
repo own-image-major "$O=$(imagestep "steps.pin.outputs.source == 'image'" '"17"')"
expect own-image-major fail "$O:7 (setup-java-maven/jdk: neither"
# Inputs that make setup-java resolve and download a JDK itself, a step key,
# and an input setup-java may add later: none is on the allow-lists.
G="steps.pin.outputs.source == 'image'"
for c in check-latest:true architecture:x86 java-package:jre verify-signature:true; do
  repo "own-image-${c%%:*}" "$O=$(imagestep "$G" "$V")
        ${c%%:*}: ${c#*:}
"
  expect "own-image-${c%%:*}" fail "$O:7 (setup-java-maven/jdk: not allowed on a setup-java step: input ${c%%:*}:"
done
repo own-image-env "$O=$(imagestep "$G" "$V" | sed "s/^    - if: .*/&\n      env:\n        INPUT_CHECK-LATEST: 'true'/")"
expect own-image-env fail "$O:7 (setup-java-maven/jdk: not allowed on a setup-java step: step key env:"
repo own-flow-extra "$O=runs:
  using: composite
  steps:
    - { uses: actions/setup-java@v4, with: { distribution: 'jdkfile', $J, check-latest: true } }
"
expect own-flow-extra fail "$O:4 (setup-java-maven/jdk: not allowed on a setup-java step: input check-latest:"
repo own-every-input "$O=$(imagestep "$G" "$V")
        jdkFile: x
        mvn-toolchain-id: temurin_17
        mvn-toolchain-vendor: temurin
"
expect own-every-input pass
repo wf-image ".github/workflows/w.yml=$(imagestep "steps.pin.outputs.source == 'image'" "$V")"
expect wf-image fail ".github/workflows/w.yml:7"
repo own-and-wf "$O=$(own 'distribution: jdkfile' "$J")" \
  ".github/workflows/w.yml=$(wf actions/setup-java@v4 'java-version: "17"')"
expect own-and-wf fail ".github/workflows/w.yml:5"

echo "$((n - fails))/$n passed"
[ "$fails" -eq 0 ]

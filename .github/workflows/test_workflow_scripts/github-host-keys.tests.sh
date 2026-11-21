#!/usr/bin/env bash
# Rows for GitHub's pinned SSH host keys (.github/ssh/github_known_hosts):
#   - each pinned key's SHA256 fingerprint is one GitHub publishes (the file's
#     "# fingerprint" lines, copied from GitHub's documentation), and each
#     published fingerprint has its key;
#   - the setup-private-parsers action's non-Windows step writes exactly the
#     pinned keys while an ssh-keyscan on PATH fails as a DNS blip does, and
#     never calls it;
#   - a sweep that no workflow or script takes host keys from the network at
#     run time, or trusts the first one it sees: no ssh-keyscan outside a
#     comment, no StrictHostKeyChecking=no, off, false or accept-new, no read
#     of api.github.com/meta but the live check's (github-host-keys-live.sh);
#     and every github.com host key written anywhere is the pinned set, whole.
# The PowerShell writers (setup-git-isolation.ps1, prepare_and_run.yml's
# pre-checkout "Setup per-job git isolation" step and the action's Windows
# step) have their rows in .github/scripts/windows/setup-git-isolation.tests.ps1.
# Runs on any host with bash 4 or later and ssh-keygen (no root, no network).
#
# What the sweep reads: every workflow, action and other YAML file, every
# shell or PowerShell script, Dockerfile and Makefile in the repository, and
# the JavaScript and Python under .github; not prose or test data. A comment
# line (first word #, or a PowerShell <# ... #> block) is not code. Not looked
# for, as no file uses them: a host key written without its "github.com" (or
# with [github.com]:port, or hashed), a mention after code on the same line
# taken as a comment, ssh-keyscan reached through a variable's value, and the
# meta API read by another name ("gh api meta", "$GITHUB_API_URL/meta",
# github-script's meta.get()).
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
pinned="$root/.github/ssh/github_known_hosts"
live=.github/workflows/test_workflow_scripts/github-host-keys-live.sh
self=.github/workflows/test_workflow_scripts/github-host-keys.tests.sh
pstests=.github/scripts/windows/setup-git-isolation.tests.ps1
FAILS=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

command -v ssh-keygen >/dev/null || { bad "ssh-keygen is not on PATH; the fingerprint rows need it"; exit 1; }
[ -r "$pinned" ] || { bad "no pinned host keys at $pinned"; exit 1; }

# ---- the pinned file --------------------------------------------------------

# Every line is a comment, blank, or a github.com key of a type GitHub serves:
# the scripts take the lines that start with "github.com ", so any other line
# would be dropped without a word.
odd=$(grep -nvE '^(#.*|[[:space:]]*|github\.com (ssh-ed25519|ecdsa-sha2-nistp256|ssh-rsa) [A-Za-z0-9+/]+=*)$' "$pinned")
[ -z "$odd" ] && ok "the pinned file holds only comments and github.com key lines" ||
  bad "lines of the pinned file that are neither a comment nor a github.com key:
$odd"
grep '^github\.com ' "$pinned" >"$tmp/pinned-keys"
[ -s "$tmp/pinned-keys" ] || { bad "the pinned file has no github.com key"; exit 1; }

# fingerprints FILE: "SHA256:... TYPE" for each key of a known_hosts FILE, as
# OpenSSH reads it, sorted.
fingerprints() { ssh-keygen -l -f "$1" | awk '{ t = $NF; gsub(/[()]/, "", t); print $2, t }' | sort; }
got=$(fingerprints "$tmp/pinned-keys") || bad "ssh-keygen could not read a pinned key"
want=$(awk '$1 == "#" && $2 == "fingerprint" { print $3, $4 }' "$pinned" | sort)
[ -n "$want" ] || bad "the pinned file lists no published fingerprint"
[ "$got" = "$want" ] && ok "each pinned key has a fingerprint GitHub publishes, and each published one its key ($(wc -l <"$tmp/pinned-keys") keys)" ||
  bad "the pinned keys' fingerprints are
$got
want the published ones:
$want"

# ---- the action's non-Windows step ------------------------------------------

# run_block FILE STEP: the run: | block of the step named STEP, dedented.
run_block() {
  awk -v step="$2" '
    !found && /^[[:space:]]*- name: / {
      t = $0; sub(/^[[:space:]]*- name: /, "", t); sub(/[[:space:]]+$/, "", t)
      if (t == step) found = 1
      next
    }
    found && !inrun && /^[[:space:]]*run: \|[[:space:]]*$/ { match($0, /^[[:space:]]*/); ind = RLENGTH; inrun = 1; next }
    inrun {
      if ($0 ~ /^[[:space:]]*$/) { print ""; next }
      match($0, /^[[:space:]]*/); if (RLENGTH <= ind) exit
      print substr($0, ind + 3)
    }
  ' "$1"
}
# A stand-in ssh-keyscan that fails as run 44095089090's did on its runner,
# and says it was called.
mkdir -p "$tmp/fakebin"
cat >"$tmp/fakebin/ssh-keyscan" <<'E'
#!/bin/sh
echo "ssh-keyscan $*" >>"$FAKE_KEYSCAN_LOG"
echo "getaddrinfo github.com: No such host is known." >&2
exit 255
E
chmod +x "$tmp/fakebin/ssh-keyscan"
# The action's path as the runner sets it for a "uses: ./.github/actions/..."
action="$root/./.github/actions/setup-private-parsers"
run_block "$action/action.yaml" 'Add GitHub to known hosts (non-Windows)' >"$tmp/action-step.sh"
if [ ! -s "$tmp/action-step.sh" ]; then
  bad "setup-private-parsers has no 'Add GitHub to known hosts (non-Windows)' step with a run: | block"
else
  mkdir -p "$tmp/home"
  : >"$tmp/keyscan-calls"
  # As a composite step runs it: bash -e -o pipefail.
  (cd "$tmp" && HOME="$tmp/home" GITHUB_ACTION_PATH="$action" FAKE_KEYSCAN_LOG="$tmp/keyscan-calls" \
    PATH="$tmp/fakebin:$PATH" bash --noprofile --norc -eo pipefail "$tmp/action-step.sh") >"$tmp/action-out" 2>&1
  rc=$?
  if [ "$rc" = 0 ] && cmp -s "$tmp/home/.ssh/known_hosts" "$tmp/pinned-keys" && [ ! -s "$tmp/keyscan-calls" ]; then
    ok "the action's non-Windows step writes the pinned keys, ssh-keyscan failing and never called"
  else
    bad "the action's non-Windows step: exit $rc, ssh-keyscan calls: $(tr '\n' ';' <"$tmp/keyscan-calls"); output: $(cat "$tmp/action-out")"
    diff "$tmp/pinned-keys" "$tmp/home/.ssh/known_hosts"
  fi
fi

# ---- the sweep ----------------------------------------------------------------

# fetchers FILE...: FILE:LINE: WHAT for each code line that takes host keys
# from the network or trusts the first key it sees.
fetchers() {
  awk -v live="$live" -v tofu='stricthostkeychecking[[:space:]]*(=|[[:space:]])[[:space:]]*["'"'"']?(no|off|false|accept-new)([^a-z0-9-]|$)' '
    FNR == 1 { blk = 0 }
    blk { if ($0 ~ /#>/) blk = 0; next }
    /^[[:space:]]*<#/ { if ($0 !~ /#>/) blk = 1; next }
    /^[[:space:]]*#/ { next }
    {
      l = tolower($0)
      if (l ~ /ssh-keyscan/) print FILENAME ":" FNR ": ssh-keyscan"
      if (l ~ tofu) print FILENAME ":" FNR ": StrictHostKeyChecking that trusts a first key"
      if (l ~ /api\.github\.com\/meta/ && FILENAME != live) print FILENAME ":" FNR ": api.github.com/meta"
    }
  ' "$@"
}
# key_drift FILE...: FILE: WHY for each file that writes a github.com host key
# that is not pinned, or only some of the pinned ones.
key_drift() {
  local f keys
  for f in "$@"; do
    keys=$(grep -oE 'github\.com[[:space:]]+(ssh-[a-z0-9-]+|ecdsa-sha2-[a-z0-9]+|sk-[a-z0-9@.-]+)[[:space:]]+[A-Za-z0-9+/]+=*' "$f" |
      sed -E 's/[[:space:]]+/ /g' | sort -u)
    [ -n "$keys" ] || continue
    [ "$keys" = "$(sort -u "$tmp/pinned-keys")" ] && continue
    if comm -23 <(echo "$keys") <(sort -u "$tmp/pinned-keys") | grep -q .; then
      echo "$f: a github.com host key that is not pinned"
    else
      echo "$f: only some of the pinned github.com host keys"
    fi
  done
}

# Fixtures: each spelling in takes must be found, and nothing in clean.
cat >"$tmp/takes.sh" <<'E'
ssh-keyscan github.com >> ~/.ssh/known_hosts
  $hostKeys = & ssh-keyscan github.com 2>$null
RUN mkdir -p /root/.ssh && ssh-keyscan -t ed25519 github.com > /root/.ssh/known_hosts
export GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=no"
ssh -oStrictHostKeyChecking=accept-new -T git@github.com
  StrictHostKeyChecking no
curl -fsS https://api.github.com/meta | jq -r '.ssh_keys[]'
Write-Host "SSH-KEYSCAN unavailable"
ssh -o StrictHostKeyChecking=false -T git@github.com
E
cat >"$tmp/clean.sh" <<'E'
# ssh-keyscan github.com >> ~/.ssh/known_hosts
    # curl https://api.github.com/meta; StrictHostKeyChecking=no
<#
  populates a per-job known_hosts via ssh-keyscan
#>
<# ssh-keyscan #>
GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=yes -o UserKnownHostsFile=kh"
ssh -o StrictHostKeyChecking=yes -o NoHostAuthenticationForLocalhost=no
E
want="$tmp/takes.sh:1: ssh-keyscan|$tmp/takes.sh:2: ssh-keyscan|$tmp/takes.sh:3: ssh-keyscan|$tmp/takes.sh:4: StrictHostKeyChecking that trusts a first key|$tmp/takes.sh:5: StrictHostKeyChecking that trusts a first key|$tmp/takes.sh:6: StrictHostKeyChecking that trusts a first key|$tmp/takes.sh:7: api.github.com/meta|$tmp/takes.sh:8: ssh-keyscan|$tmp/takes.sh:9: StrictHostKeyChecking that trusts a first key"
got=$(fetchers "$tmp/takes.sh" "$tmp/clean.sh" | paste -sd'|' -)
[ "$got" = "$want" ] && ok "the sweep finds each spelling that fetches host keys or trusts a first one, and no comment" ||
  bad "the sweep found \"$got\", want \"$want\""

# The live check alone may read api.github.com/meta.
mkdir -p "$tmp/sub/$(dirname "$live")"
echo 'curl -fsS https://api.github.com/meta' >"$tmp/sub/$live"
got=$(cd "$tmp/sub" && fetchers "$live")
[ -z "$got" ] && ok "the live check may read api.github.com/meta" || bad "the sweep flags the live check: $got"

# Key copies: the whole pinned set passes, in any quoting; a key GitHub no
# longer serves (its RSA key before 2023-03-24) or a part of the set does not.
sed "s/^\(.*\)$/          '\1',/" "$tmp/pinned-keys" >"$tmp/whole.yml"
{ cat "$tmp/pinned-keys"
  echo 'github.com ssh-rsa AAAAB3NzaC1yc2EAAAABIwAAAQEAq2A7hRGmdnm9tUDbO9IDSwBK6TbQa+PXYPCPy6rbTrTtw7PHkccKrpp0yVhp5HdEIcKr6pLlVDBfOLX9QUsyCOV0wzfjIJNlGEYsdlLJizHhbn2mUjvSAHQqZETYP81eFzLQNnPHt4EVVUh7VfDESU84KezmD5QlWpXLmvU31/yMf+Se8xhHTvKSCZIFImWwoG6mbUoWf9nzpIoaSjB+weqqUUmpaaasXVal72J+UX2B+2RPW3RcT0eOzQgqlJL3RKrTJvdsjE3JEAvGq3lGHSZXy28G3skua2SmVi/w4yCE6gbODqnTWlg7+wC604ydGXA8VJiS5ap43JXiUFFAaQ=='
} >"$tmp/stale.sh"
head -n 1 "$tmp/pinned-keys" | sed 's/^/  "/; s/$/",/' >"$tmp/part.ps1"
echo 'no keys here' >"$tmp/none.sh"
want="$tmp/stale.sh: a github.com host key that is not pinned|$tmp/part.ps1: only some of the pinned github.com host keys"
got=$(key_drift "$tmp/whole.yml" "$tmp/stale.sh" "$tmp/part.ps1" "$tmp/none.sh" | paste -sd'|' -)
[ "$got" = "$want" ] && ok "a copy of the keys passes whole, and fails with a key not pinned or one missing" ||
  bad "the key check found \"$got\", want \"$want\""

# The repository: every workflow, action and YAML file, script, Dockerfile
# and Makefile, and the JavaScript and Python under .github, but no testdata
# directory's recordings; and not this file, whose fixtures above are such
# lines on purpose, nor setup-git-isolation.tests.ps1, which puts a stand-in
# ssh-keyscan first on PATH to show that no writer calls it.
mapfile -t files < <(cd "$root" && { git ls-files 2>/dev/null || find . -path ./.git -prune -o -type f -print | sed 's|^\./||'; } |
  grep -E '(^|/)(Dockerfile[^/]*|Makefile|[^/]*\.mk|[^/]*\.(sh|bash|ps1|psm1|yml|yaml))$|^\.github/.*\.(js|mjs|cjs|py)$' | grep -vE '(^|/)testdata/' | grep -vxF -e "$self" -e "$pstests" |
  while IFS= read -r f; do [ -f "$f" ] && grep -Iq . "$f" && echo "$f"; done)
[ "${#files[@]}" -gt 100 ] || bad "the sweep found only ${#files[@]} files to read under $root"
found=$(cd "$root" && fetchers "${files[@]}")
[ -z "$found" ] && ok "no workflow or script takes host keys from the network or trusts a first one (${#files[@]} files)" ||
  bad "these lines take SSH host keys from the network at run time, or trust the first one; write .github/ssh/github_known_hosts' keys instead:
$found"
# The pinned file is the set; the live check holds GitHub's old RSA key on
# purpose, to show that a key not GitHub's fails its handshake.
copies=()
for f in "${files[@]}"; do case $f in .github/ssh/github_known_hosts | "$live") ;; *) copies+=("$f") ;; esac; done
found=$(cd "$root" && key_drift "${copies[@]}")
[ -z "$found" ] && ok "every copy of a github.com host key is the pinned set" ||
  bad "these files' github.com host keys differ from .github/ssh/github_known_hosts:
$found"

if [ "$FAILS" -ne 0 ]; then echo "github-host-keys.tests.sh: $FAILS FAILED"; exit 1; fi
echo "github-host-keys.tests.sh: all passed"

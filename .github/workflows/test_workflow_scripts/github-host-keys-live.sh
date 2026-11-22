#!/usr/bin/env bash
# Checks the pinned SSH host keys (.github/ssh/github_known_hosts) against
# GitHub itself. github-host-keys-live.yml runs it weekly, on demand, and on a
# pull request that changes the pin or this check; it needs the network, so no
# job that builds runs it or waits on it. It fails when GitHub's published
# keys, or the keys its SSH server proves, differ from the pin.
#   1. https://api.github.com/meta's "ssh_keys" are the pinned keys, and its
#      "ssh_key_fingerprints" the fingerprints the pinned file lists.
#   2. github.com's SSH server proves each pinned key: ssh, offered only that
#      key's algorithm and only the pinned keys, with StrictHostKeyChecking=yes,
#      gets past the host check to "Permission denied (publickey)" (it offers
#      no user key). A known_hosts holding GitHub's RSA key from before
#      2023-03-24 instead fails "Host key verification failed": the check can
#      fail.
# GitHub's API and its SSH front end now and then drop a request under load
# (go-retry.sh lists how a shed SSH session reads). An attempt that got no
# answer says nothing about the keys, so it is made again, three tries at
# most; an answer (a key list, a host check passed or failed) is never asked
# again. Needs bash, curl 7.71 or later, jq and ssh. GITHUB_TOKEN, when set,
# only raises the API's rate limit.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
pinned="$root/.github/ssh/github_known_hosts"
FAILS=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

grep '^github\.com ' "$pinned" >"$tmp/keys" || { bad "no github.com key in $pinned"; exit 1; }

# ---- 1. GitHub's published list ---------------------------------------------
auth=()
[ -n "${GITHUB_TOKEN:-}" ] && auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
if ! curl -fsS --max-time 30 --retry 2 --retry-all-errors --retry-delay 5 \
  -H 'Accept: application/vnd.github+json' "${auth[@]}" -o "$tmp/meta.json" https://api.github.com/meta; then
  bad "could not read https://api.github.com/meta"
else
  got=$(jq -r '.ssh_keys[]' "$tmp/meta.json" | sort)
  want=$(cut -d' ' -f2- "$tmp/keys" | sort)
  [ -n "$got" ] && [ "$got" = "$want" ] && ok "api.github.com/meta lists the pinned keys, and only those" ||
    bad "api.github.com/meta lists the SSH host keys
$got
want the pinned ones:
$want"
  got=$(jq -r '.ssh_key_fingerprints | to_entries[] | "SHA256:\(.value) \(.key | sub("^SHA256_"; ""))"' "$tmp/meta.json" | sort)
  want=$(awk '$1 == "#" && $2 == "fingerprint" { print $3, $4 }' "$pinned" | sort)
  [ -n "$got" ] && [ "$got" = "$want" ] && ok "api.github.com/meta publishes the fingerprints the pinned file lists" ||
    bad "api.github.com/meta publishes the fingerprints
$got
want the pinned file's:
$want"
fi

# ---- 2. github.com's SSH server -----------------------------------------------
# handshake ALGORITHMS KNOWN_HOSTS: ssh's output for a connection that may
# accept only those host key algorithms and only the keys in KNOWN_HOSTS, and
# offers no way to log in.
handshake() {
  SSH_AUTH_SOCK='' ssh -F /dev/null -T -o BatchMode=yes -o ConnectTimeout=20 \
    -o ServerAliveInterval=10 -o ServerAliveCountMax=2 \
    -o HostKeyAlgorithms="$1" -o UserKnownHostsFile="$2" -o GlobalKnownHostsFile=/dev/null \
    -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o CheckHostIP=no \
    -o PubkeyAuthentication=no -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no \
    -o IdentitiesOnly=yes -o IdentityAgent=none git@github.com 2>&1 </dev/null
}
# answer ALGORITHMS KNOWN_HOSTS: handshake's output once it answers about the
# host key, passed or failed; else the last of three tries' output.
answer() {
  local try out
  for try in 1 2 3; do
    out=$(handshake "$1" "$2")
    case $out in *"Permission denied (publickey)"* | *"Host key verification failed"*) break ;; esac
    [ "$try" = 3 ] || sleep 10
  done
  printf '%s\n' "$out"
}
while read -r _ type _; do
  case $type in
    ssh-rsa) algs=rsa-sha2-512,rsa-sha2-256 ;;
    *) algs=$type ;;
  esac
  out=$(answer "$algs" "$tmp/keys")
  case $out in
    *"Permission denied (publickey)"*) ok "github.com proves its pinned $type key ($algs)" ;;
    *) bad "github.com, offered only $algs, did not get past the host check against the pinned keys: $out" ;;
  esac
done <"$tmp/keys"

echo 'github.com ssh-rsa AAAAB3NzaC1yc2EAAAABIwAAAQEAq2A7hRGmdnm9tUDbO9IDSwBK6TbQa+PXYPCPy6rbTrTtw7PHkccKrpp0yVhp5HdEIcKr6pLlVDBfOLX9QUsyCOV0wzfjIJNlGEYsdlLJizHhbn2mUjvSAHQqZETYP81eFzLQNnPHt4EVVUh7VfDESU84KezmD5QlWpXLmvU31/yMf+Se8xhHTvKSCZIFImWwoG6mbUoWf9nzpIoaSjB+weqqUUmpaaasXVal72J+UX2B+2RPW3RcT0eOzQgqlJL3RKrTJvdsjE3JEAvGq3lGHSZXy28G3skua2SmVi/w4yCE6gbODqnTWlg7+wC604ydGXA8VJiS5ap43JXiUFFAaQ==' >"$tmp/old"
out=$(answer rsa-sha2-512,rsa-sha2-256 "$tmp/old")
case $out in
  *"Host key verification failed"*) ok "a known_hosts with GitHub's old RSA key fails the host check" ;;
  *) bad "a known_hosts with GitHub's old RSA key did not fail the host check: $out" ;;
esac

if [ "$FAILS" -ne 0 ]; then echo "github-host-keys-live.sh: $FAILS FAILED"; exit 1; fi
echo "github-host-keys-live.sh: all passed"

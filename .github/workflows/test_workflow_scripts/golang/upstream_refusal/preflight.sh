#!/usr/bin/env bash
# The handshake hold needs these kernel modules; on a runner without them the
# lane cannot test anything, and that is the runner's problem, not keploy's.
# Ubuntu ships some of them only in linux-modules-extra; that is installed
# when a module is missing. Nothing is loaded here: the kernel loads them on
# demand when keploy uses them, which is what production relies on.
set -euo pipefail
mods=(nfnetlink_queue nft_queue nft_reject_inet nf_tables inet_diag tcp_diag)
have() { [ -d "/sys/module/$1" ] || modinfo "$1" >/dev/null 2>&1; }
missing=()
for m in "${mods[@]}"; do have "$m" || missing+=("$m"); done
if [ "${#missing[@]}" -gt 0 ] && command -v apt-get >/dev/null; then
  echo "missing ${missing[*]}; installing linux-modules-extra-$(uname -r)"
  sudo timeout 600 apt-get -y update -o Acquire::Retries=3 >/dev/null || true
  sudo timeout 600 apt-get -y install -o Acquire::Retries=3 "linux-modules-extra-$(uname -r)" >/dev/null || true
  missing=()
  for m in "${mods[@]}"; do have "$m" || missing+=("$m"); done
fi
if [ "${#missing[@]}" -gt 0 ]; then
  echo "::error::runner infrastructure: kernel $(uname -r) on $(uname -m) has no ${missing[*]}; the handshake hold cannot be tested here"
  exit 1
fi
echo "kernel $(uname -r) on $(uname -m): ${mods[*]} available"

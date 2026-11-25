#!/usr/bin/env bash
# go-test-shard.sh [--stdin] <shard_index> <total_shards>
#
# Deterministically partitions `go list ./...` (or newline-separated packages
# from stdin with --stdin) into <total_shards> disjoint shards, spreading the
# known slowest packages across shards via serpentine assignment so every
# shard stays balanced and every current and future Go package is covered by
# exactly one shard.
set -euo pipefail

from_stdin=0
if [ "${1:-}" = "--stdin" ]; then
  from_stdin=1
  shift
fi

if [ "$#" -ne 2 ]; then
  echo "usage: $0 [--stdin] <shard_index> <total_shards>" >&2
  exit 2
fi

shard="$1"
total="$2"

if ! [[ "$shard" =~ ^[0-9]+$ ]] || ! [[ "$total" =~ ^[0-9]+$ ]]; then
  echo "error: shard_index ($shard) and total_shards ($total) must be non-negative integers" >&2
  exit 2
fi

if [ "$total" -lt 1 ] || [ "$shard" -ge "$total" ]; then
  echo "error: require 0 <= shard_index ($shard) < total_shards ($total)" >&2
  exit 2
fi

# Slowest unit-test packages ordered longest-first (measured on ubuntu-latest
# Go-Test run 45588244174). Present packages are spread across shards in
# serpentine order (0, 1, ..., N-1, N-1, ..., 1, 0, ...) before remaining
# packages are assigned round-robin.
heavy_pkgs=(
  "go.keploy.io/server/v3/pkg/client/app"
  "go.keploy.io/server/v3/pkg/service/record"
  "go.keploy.io/server/v3/pkg/matcher/learn"
  "go.keploy.io/server/v3/pkg/agent/proxy"
  "go.keploy.io/server/v3/pkg/service/replay"
  "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
  "go.keploy.io/server/v3/pkg/platform/http"
  "go.keploy.io/server/v3/utils"
  "go.keploy.io/server/v3/pkg"
  "go.keploy.io/server/v3/pkg/service/mock"
  "go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
  "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
  "go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
)

if [ "$from_stdin" -eq 1 ]; then
  raw_pkgs=$(cat)
else
  raw_pkgs=$(go list ./...)
fi
mapfile -t all_pkgs < <(printf '%s\n' "$raw_pkgs" | awk 'NF { print $1 }' | LC_ALL=C sort -u)

if [ "${#all_pkgs[@]}" -eq 0 ]; then
  echo "error: package list is empty" >&2
  exit 1
fi

declare -A present=()
for p in "${all_pkgs[@]}"; do
  present["$p"]=1
done

declare -A assigned_heavy=()
heavy_pos=0
for p in "${heavy_pkgs[@]}"; do
  if [ -n "${present[$p]+x}" ]; then
    assigned_heavy["$p"]=1
    cycle=$((heavy_pos / total))
    rem=$((heavy_pos % total))
    if [ $((cycle % 2)) -eq 0 ]; then
      target="$rem"
    else
      target=$((total - 1 - rem))
    fi
    if [ "$target" -eq "$shard" ]; then
      printf '%s\n' "$p"
    fi
    heavy_pos=$((heavy_pos + 1))
  fi
done

rest_pos=0
for p in "${all_pkgs[@]}"; do
  if [ -n "${assigned_heavy[$p]+x}" ]; then
    continue
  fi
  if [ $((rest_pos % total)) -eq "$shard" ]; then
    printf '%s\n' "$p"
  fi
  rest_pos=$((rest_pos + 1))
done

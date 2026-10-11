#!/usr/bin/env bash
# run.sh — end-to-end guard for concurrent keploy runs sharing one system trust
# store.
#
# Every native keploy agent on a Linux machine installs its run CA into the
# same store: an anchor file plus a bundle the distro's tool regenerates. This
# runs the real install and teardown from several processes at once inside a
# container that has the distro's real trust store, and checks trust the way
# applications do — with curl and with openssl s_client, given nothing but the
# system store.
#
# For each distro (see Dockerfile) the scenario in harness/main.go asserts:
#   [1]-[3] a second agent starting, running and stopping leaves the first
#           agent's CA trusted;
#   [4]     an agent that is killed leaves its anchor behind, and the next
#           agent to start sweeps it — and only it;
#   [5]-[6] six agents starting together, then stopping together, each end up
#           trusted and each remove exactly their own CA;
#   [7]     once every agent has stopped, nothing of keploy is left in the store;
#   [8]     the store's outputs are exactly what they were before the first agent
#           started, and the distro's own tool, run afterwards, succeeds and
#           changes none of them.
# After every step it also asserts that the bundle still holds each certificate
# the distro shipped: the distros' tools, run two at a time, lose the machine's
# own roots, and that breaks TLS for every process on it. And from [1] to [7]
# the first agent is probed without a pause, with both clients: while the
# others start and stop around it, it must never be refused once.
#
# The containers need no privileges and no network: each one is its own
# machine, with its own trust store, talking to itself over loopback.
#
# Usage: ./run.sh                       every distro in the Dockerfile
#        DISTROS="fedora44 debian12" ./run.sh
#
# GOLANG_IMAGE and the Dockerfile's per-distro *_IMAGE variables (FEDORA44_IMAGE,
# DEBIAN12_IMAGE, ...) override the base images, for a machine that has them
# cached under other names. HARNESS_VERBOSE=1 prints the agents' debug logs,
# which name every anchor written, kept and swept.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../../.." && pwd)"

read -r -a DISTROS <<<"${DISTROS:-debian12 debian13 ubuntu2204 fedora44 rhel8 rhel9 rhel10 al2023}"

build_args=()
for arg in GOLANG_IMAGE DEBIAN12_IMAGE DEBIAN13_IMAGE UBUNTU2204_IMAGE FEDORA44_IMAGE RHEL8_IMAGE RHEL9_IMAGE RHEL10_IMAGE AL2023_IMAGE; do
    if [ -n "${!arg:-}" ]; then
        build_args+=(--build-arg "${arg}=${!arg}")
    fi
done

for distro in "${DISTROS[@]}"; do
    echo "=== building the ${distro} image ==="
    # ${arr[@]+...}: an empty array is an unbound variable to bash before 4.4.
    docker build -f "${HERE}/Dockerfile" --target "${distro}" ${build_args[@]+"${build_args[@]}"} \
        -t "kpl-ca-concurrent-agents:${distro}" "${ROOT}"
done

failed=()
for distro in "${DISTROS[@]}"; do
    echo ""
    echo "################ ${distro} ################"
    if ! docker run --rm --network none -e HARNESS_VERBOSE="${HARNESS_VERBOSE:-0}" \
        "kpl-ca-concurrent-agents:${distro}"; then
        failed+=("${distro}")
    fi
done

echo ""
if [ "${#failed[@]}" -ne 0 ]; then
    echo "[RESULT] FAIL (${failed[*]})"
    exit 1
fi
echo "[RESULT] PASS (${DISTROS[*]})"

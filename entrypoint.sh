#!/bin/sh
# keploy-agent dual-mode entrypoint.
#
# Branch on container UID so the same image works for both:
#
#   1. docker-compose (root):  `sudo docker run …` → container starts as UID 0.
#      We have CAP_SYS_ADMIN via the (effective) root caps, so we can mount
#      debugfs ourselves if the host hasn't pre-mounted it.
#
#   2. k8s PSA-restricted (non-root):  securityContext.runAsUser=65532 +
#      capabilities.add=[BPF,PERFMON,NET_ADMIN,SYS_PTRACE,SYS_RESOURCE].
#      Bounding-set caps become effective on exec via the setcap'd binary
#      (see Dockerfile). We do NOT have CAP_SYS_ADMIN, so we cannot mount
#      debugfs — but the keploy-node-setup DaemonSet's host-bootstrap
#      init container ensures debugfs is mounted host-wide before this
#      pod starts, and we mount it into the container via hostPath.
#
# No `sudo` anywhere. The previous version `exec sudo -E "$@"` even when
# the container was already root, which is redundant for case 1 and
# crashes outright for case 2 (no NOPASSWD, no tty). Use exec so keploy
# replaces this shell PID and receives SIGTERM directly from the runtime.

set -e

# Best-effort debugfs mount, root path only. Non-root caller skips this;
# debugfs is expected to be mounted by the host / hostPath. If neither
# holds (rare misconfig), keploy will log a clear error when it tries to
# attach kprobes — fail-loud is better than silently mounting from a
# non-root context that would EPERM anyway.
if [ "$(id -u)" = "0" ]; then
    if ! mountpoint -q /sys/kernel/debug 2>/dev/null; then
        mount -t debugfs debugfs /sys/kernel/debug 2>/dev/null || true
    fi
fi

exec "$@"

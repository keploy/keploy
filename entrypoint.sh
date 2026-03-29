#!/bin/sh

# Mount debugfs if not already mounted
if ! mountpoint -q /sys/kernel/debug 2>/dev/null; then
  sudo mount -t debugfs debugfs /sys/kernel/debug 2>/dev/null || true
fi

# Mount tracefs if not already available (fallback for newer kernels
# where tracefs is separate from debugfs)
if [ ! -d /sys/kernel/debug/tracing ] && [ ! -d /sys/kernel/tracing ]; then
  sudo mount -t tracefs tracefs /sys/kernel/tracing 2>/dev/null || true
fi

# Use exec to replace the shell process with keploy
# This ensures keploy receives signals (SIGTERM) directly from Docker
exec sudo -E "$@"

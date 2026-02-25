#!/bin/sh

if ! mountpoint -q /sys/kernel/debug; then
  sudo mount -t debugfs debugfs /sys/kernel/debug
fi

# Use exec to replace the shell process with keploy
# This ensures keploy receives signals (SIGTERM) directly from Docker
exec sudo -E "$@"

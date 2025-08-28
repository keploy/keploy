#!/bin/sh

# Ignore SIGHUP inside the container so TTY hangups (e.g., Ctrl-C on the client)
# don't terminate the app. This disposition persists across exec.
trap '' HUP

# Mount debugfs if not already mounted (requires root; container runs as root).
if ! mountpoint -q /sys/kernel/debug; then
  mount -t debugfs debugfs /sys/kernel/debug || true
fi

# Replace the shell with the target process, preserving env and TTY.
exec "$@"

#!/bin/sh

if ! mountpoint -q /sys/kernel/debug; then
  sudo mount -t debugfs debugfs /sys/kernel/debug
fi

sudo -E "$@"

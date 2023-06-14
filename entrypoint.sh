#!/bin/sh
sudo mount -t debugfs debugfs /sys/kernel/debug
sudo -E "$@"

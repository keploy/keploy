#!/bin/sh

# Handle SIGINT and SIGTERM signals, forwarding them to the sleep process
trap 'echo "Init Container received SIGTERM or SIGINT, exiting..."; exit' SIGINT SIGTERM

# Start sleep infinity to keep the container running in the background
sleep infinity &

# Wait for the background process and forward any signals
wait $!

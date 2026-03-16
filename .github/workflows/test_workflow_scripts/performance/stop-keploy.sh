#!/bin/bash

# Stop Keploy gracefully
echo "🛑 Stopping Keploy..."

if [ -f keploy.pid ]; then
  PID=$(cat keploy.pid)
  echo "Stopping Keploy process: $PID"
  sudo kill -- "$PID" || true
  sleep 2
  echo "✅ Keploy stopped"
else
  echo "⚠️  No keploy.pid file found"
fi

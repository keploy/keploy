#!/bin/bash

# ... your existing code ...

# Start the application in background
echo "Starting the application..."
./main &
APP_PID=$!

# Wait a moment for the app to start
sleep 5

# Check if the app is running
if ! kill -0 $APP_PID 2>/dev/null; then
    echo "Error: Application failed to start"
    exit 1
fi

echo "Application started with PID: $APP_PID"

# Run Keploy tests
echo "Starting Keploy record..."
if [ -n "$RECORD_BIN" ] && [ -f "$RECORD_BIN" ]; then
    # Run record mode
    sudo -E env PATH="$PATH" "$RECORD_BIN" record --cwd "$(pwd)" --command "./main" --delay 10
    RECORD_EXIT_CODE=$?
    echo "Keploy record finished with exit code: $RECORD_EXIT_CODE"
else
    echo "Record binary not found, skipping record mode"
fi

# Run replay mode if binary exists
if [ -n "$REPLAY_BIN" ] && [ -f "$REPLAY_BIN" ]; then
    echo "Starting Keploy replay..."
    sudo -E env PATH="$PATH" "$REPLAY_BIN" test --cwd "$(pwd)" --command "./main" --delay 10 --apiTimeout 30
    REPLAY_EXIT_CODE=$?
    echo "Keploy replay finished with exit code: $REPLAY_EXIT_CODE"
else
    echo "Replay binary not found, skipping replay mode"
fi

# Cleanup: stop the application
echo "Stopping the application..."
kill $APP_PID 2>/dev/null
wait $APP_PID 2>/dev/null
echo "Application stopped"

# Exit with appropriate code
if [ $RECORD_EXIT_CODE -ne 0 ] || [ $REPLAY_EXIT_CODE -ne 0 ]; then
    exit 1
else
    exit 0
fi
#!/bin/bash

# Setup sudo access, a common requirement in CI environments.
echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

# Function to generate API traffic for the go-joke-app
generate_traffic() {
  # Wait for the Go application to start on port 8080
  echo "Waiting for app to start..."
  for i in {1..30}; do
    # Use curl to check for readiness, which is more portable than nc
    if curl -s "http://localhost:8080/joke" >/dev/null; then
      echo "✅ App started and responded."
      break
    fi
    echo "Waiting... ($i/30)"
    sleep 1
  done
  if [ "$i" -eq 30 ]; then
    echo "❌ Application did not start in 30 seconds."
    # Stop the container on failure to allow the main script to exit
    docker stop go-joke-app-record || true
    exit 1
  fi

  echo "Generating API calls..."
  curl -X GET http://localhost:8080/joke
  curl -X GET http://localhost:8080/joke
  echo "✅ Traffic generation complete."

  # Wait for Keploy to process the captured traffic
  echo "Waiting 10 seconds for Keploy to record..."
  sleep 10
  
  # Stop the container, which will cause the Keploy record session to end.
  echo "Stopping the application container to end recording..."
  if docker stop go-joke-app-record; then
    echo "✅ Container stopped successfully."
  else
    echo "⚠️  Could not stop container. It may have already exited."
  fi
}

# Function to check test results from a log file
check_test_results() {
    local log_file=$1
    local stage=$2 # "record" or "test"

    if [ "$stage" = "record" ]; then
        if grep "Keploy has captured test cases" "$log_file"; then
            echo "✅ Keploy captured test cases successfully."
        else
            echo "❌ Failed to capture test cases."
            cat "$log_file"
            exit 1
        fi
    fi

    if grep "WARNING: DATA RACE" "$log_file"; then
        echo "❌ Race condition detected in $log_file, stopping pipeline..."
        cat "$log_file"
        exit 1
    fi

    if grep -E "Testrun failed for testcase|error while running the app|FATAL" "$log_file"; then
        echo "❌ Critical error detected in $log_file"
        cat "$log_file"
        exit 1
    fi

    if [ "$stage" = "test" ]; then
        echo "🟢 Checking test reports..."
        local latest_run_dir
        latest_run_dir=$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1 || true)
        if [ -z "${latest_run_dir:-}" ]; then
            echo "❌ Test report directory not found! Failing test."
            cat "$log_file"
            exit 1
        fi

        # Check for any failed tests in the YAML reports
        if grep -r 'status: FAILED' "$latest_run_dir"; then
            echo "❌ Found FAILED status in test reports."
            cat "$log_file"
            grep -r 'status: FAILED' "$latest_run_dir"
            exit 1
        else
            echo "✅ All test reports show PASSED."
        fi
    fi
    # If no failures, print the log for context
    cat "$log_file"
}

# --- Main Execution ---

# Build the application Docker image
echo "🟢 Building Docker image for the application..."
docker build -t go-joke-app .
if [ $? -ne 0 ]; then
    echo "❌ Failed to build the Docker image."
    exit 1
fi
echo "✅ Docker image built successfully."

# Setup Keploy configuration
echo "🟢 Setting up Keploy..."
rm -rf keploy*
sudo -E env PATH="$PATH" $RECORD_BIN config --generate
if [ $? -ne 0 ]; then
    echo "❌ Failed to generate Keploy config."
    exit 1
fi
echo "✅ Keploy setup complete."

# 1️⃣ Record test cases
echo "🟢 Starting to record test cases..."
generate_traffic & # Run traffic generation in the background
# Keploy will wrap the docker run command
sudo -E env PATH="$PATH" $RECORD_BIN record -c "docker run --rm -p 8080:8080 --name go-joke-app-record --network keploy-network go-joke-app" --containerName "go-joke-app-record" --generateGithubActions=false &> record.log
check_test_results "record.log" "record"
sleep 5 # Give a moment for processes to terminate cleanly
wait

# 2️⃣ Test with captured mocks
echo "🟢 Starting to test with captured mocks..."
# Keploy will run the docker command for testing
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "docker run --rm -p 8080:8080 --name go-joke-app-test --network keploy-network go-joke-app" --containerName "go-joke-app-test" --delay 10 --generateGithubActions=false --disableMockUpload &> test.log
check_test_results "test.log" "test"

# Final cleanup and success message
echo "🟢 Cleaning up temporary log files..."
rm -f record.log test.log
echo "✅ All tests completed successfully."
exit 0

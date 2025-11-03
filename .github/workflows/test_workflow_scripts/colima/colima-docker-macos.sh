#!/usr/bin/env bash

# Colima support test for macOS - Tests Keploy with Colima Docker daemon
set -euo pipefail

echo "Starting Colima support test..."
echo "This test validates Keploy works with Colima without manual DOCKER_HOST configuration"

# Create Keploy network
docker network inspect keploy-network >/dev/null 2>&1 || docker network create keploy-network

# Start MongoDB
echo "Starting MongoDB..."
docker run -d --name mongoDb \
  --network keploy-network \
  -p 27017:27017 \
  mongo

sleep 5
docker logs mongoDb

echo "Building test app and pulling Keploy image in parallel..."
docker pull ghcr.io/keploy/keploy:latest &
PULL_PID=$!

docker buildx build \
  --load \
  --tag gin-mongo:colima \
  --cache-from type=gha \
  --cache-to type=gha,mode=max \
  .

wait $PULL_PID
echo "Build and pull completed"

# Remove any preexisting keploy tests and mocks
rm -rf keploy/

# Record with Keploy (Iteration 1)
echo "=== Recording Iteration 1 ==="
docker run --name keploy-record-1 \
  --privileged --pid=host -p 16789:16789 \
  -v "$(pwd):$(pwd)" -w "$(pwd)" \
  -v /sys/fs/cgroup:/sys/fs/cgroup \
  -v /sys/kernel/debug:/sys/kernel/debug \
  -v /sys/fs/bpf:/sys/fs/bpf \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/keploy/keploy:latest \
  record -c "docker run -p 8080:8080 --name ginApp-1 --network keploy-network --rm gin-mongo:colima" \
  --container-name ginApp-1 \
  --keploy-container keploy-record-1 \
  --record-timer 20s \
  --in-ci &

KEPLOY_PID=$!

# Wait for app to be ready
echo "Waiting for app to start..."
for i in {1..30}; do
  if docker exec keploy-record-1 curl -s http://ginApp-1:8080/CJBKJd92 &>/dev/null; then
    echo "App is ready!"
    break
  fi
  if [ $i -eq 30 ]; then
    echo "App failed to start"
    docker logs ginApp-1 || true
    docker logs keploy-record-1 || true
    exit 1
  fi
  sleep 1
done

# Generate traffic
echo "Generating traffic..."
docker exec keploy-record-1 curl -X POST http://ginApp-1:8080/url \
  -H "Content-Type: application/json" \
  -d '{"url":"https://google.com"}'

sleep 2

docker exec keploy-record-1 curl -X POST http://ginApp-1:8080/url \
  -H "Content-Type: application/json" \
  -d '{"url":"https://github.com"}'

sleep 2

docker exec keploy-record-1 curl -X GET http://ginApp-1:8080/CJBKJd92

sleep 5

# Stop Keploy gracefully
echo "Stopping Keploy..."
kill -SIGINT $KEPLOY_PID || true
sleep 15
kill -9 $KEPLOY_PID 2>/dev/null || true

docker stop ginApp-1 2>/dev/null || true

# Verify testcases created
sleep 2
if ! ls ./keploy/test-set-0/tests/test-*.yaml 1> /dev/null 2>&1; then
  echo "Iteration 1 failed: No testcases found"
  ls -la ./keploy/ || true
  docker logs keploy-record-1 || true
  exit 1
fi
echo "Iteration 1: Testcases created"

# Record with Keploy (Iteration 2)
echo "=== Recording Iteration 2 ==="
docker run --name keploy-record-2 \
  --privileged --pid=host -p 16789:16789 \
  -v "$(pwd):$(pwd)" -w "$(pwd)" \
  -v /sys/fs/cgroup:/sys/fs/cgroup \
  -v /sys/kernel/debug:/sys/kernel/debug \
  -v /sys/fs/bpf:/sys/fs/bpf \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/keploy/keploy:latest \
  record -c "docker run -p 8080:8080 --name ginApp-2 --network keploy-network --rm gin-mongo:colima" \
  --container-name ginApp-2 \
  --keploy-container keploy-record-2 \
  --record-timer 20s \
  --in-ci &

KEPLOY_PID=$!

echo "Waiting for app to start..."
for i in {1..30}; do
  if docker exec keploy-record-2 curl -s http://ginApp-2:8080/CJBKJd92 &>/dev/null; then
    echo "App is ready!"
    break
  fi
  if [ $i -eq 30 ]; then
    echo "App failed to start"
    docker logs ginApp-2 || true
    docker logs keploy-record-2 || true
    exit 1
  fi
  sleep 1
done

docker exec keploy-record-2 curl -X POST http://ginApp-2:8080/url \
  -H "Content-Type: application/json" \
  -d '{"url":"https://facebook.com"}'

sleep 2

docker exec keploy-record-2 curl -X GET http://ginApp-2:8080/CJBKJd92

sleep 5

echo "Stopping Keploy..."
kill -SIGINT $KEPLOY_PID || true
sleep 15
kill -9 $KEPLOY_PID 2>/dev/null || true

docker stop ginApp-2 2>/dev/null || true

sleep 2
if ! ls ./keploy/test-set-1/tests/test-*.yaml 1> /dev/null 2>&1; then
  echo "Iteration 2 failed: No testcases found"
  ls -la ./keploy/ || true
  docker logs keploy-record-2 || true
  exit 1
fi
echo "Iteration 2: Testcases created"

# Verify testcases were recorded
if [ ! -d "./keploy" ]; then
  echo "Error: keploy directory not created"
  exit 1
fi

if ! ls ./keploy/test-set-*/tests/test-*.yaml 1> /dev/null 2>&1; then
  echo "Error: No testcases found"
  ls -la ./keploy/ || true
  exit 1
fi

echo "Testcases recorded successfully:"
find ./keploy -name "*.yaml" -type f

# Stop MongoDB before test mode - validates mocking
echo "Stopping MongoDB - Keploy should use mocks..."
docker stop mongoDb || true
docker rm mongoDb || true

# Test with Keploy
echo "=== Testing with Keploy ==="
docker run --name keploy-test \
  --privileged --pid=host -p 16789:16789 \
  -v "$(pwd):$(pwd)" -w "$(pwd)" \
  -v /sys/fs/cgroup:/sys/fs/cgroup \
  -v /sys/kernel/debug:/sys/kernel/debug \
  -v /sys/fs/bpf:/sys/fs/bpf \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/keploy/keploy:latest \
  test -c "docker run -p 8080:8080 --name ginApp-test --network keploy-network --rm gin-mongo:colima" \
  --container-name ginApp-test \
  --keploy-container keploy-test \
  --delay 10 \
  --in-ci

# Verify test reports
if ! find ./keploy/reports -name "*-report.yaml" -type f | grep -q .; then
    echo "Error: No test report found"
    ls -laR ./keploy/reports/ || true
    exit 1
fi

echo "Test reports generated:"
find ./keploy/reports -name "*-report.yaml" -type f

# Check test status
all_passed=true
for report_file in ./keploy/reports/test-run-*/*-report.yaml; do
  if [ -f "$report_file" ]; then
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    test_name=$(basename "$(dirname "$report_file")")/$(basename "$report_file")
    echo "Test status for $test_name: $test_status"
    if [ "$test_status" != "PASSED" ]; then
      all_passed=false
      echo "Failed test details:"
      cat "$report_file"
    fi
  fi
done

if [ "$all_passed" = false ]; then
  echo "Some tests failed"
  exit 1
fi

echo "All tests passed - Colima support verified!"

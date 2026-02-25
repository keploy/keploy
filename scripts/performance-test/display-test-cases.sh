#!/bin/bash

# Display Keploy recorded test cases
echo "📝 Keploy recorded test cases:"

if [ -d "keploy-tests" ]; then
  TEST_COUNT=$(find keploy-tests -name "*.yaml" -type f | wc -l)
  echo "$TEST_COUNT test cases found"
  
  if [ -d "keploy-tests/keploy/test-set-0/tests/" ]; then
    echo ""
    echo "Test directory contents:"
    ls -la keploy-tests/keploy/test-set-0/tests/
  else
    echo "⚠️  No tests directory found at keploy-tests/keploy/test-set-0/tests/"
  fi
else
  echo "⚠️  No keploy-tests directory found"
fi

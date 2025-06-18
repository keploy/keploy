#!/bin/bash

echo "Testing Keploy input validation fix..."
echo "======================================"

# Test 1: Test the config command with invalid input
echo "Test 1: Testing config command with invalid input"
echo "This should show the retry mechanism and graceful exit"

# Create a test directory
mkdir -p /tmp/keploy-test
cd /tmp/keploy-test

# Create a fake keploy.yml to trigger the confirmation prompt
echo "fake config" > keploy.yml

echo ""
echo "Now testing with invalid input (invalid, wrong, maybe, then y):"
echo "You should see error messages for invalid inputs and then success with 'y'"
echo ""

# Test the input validation by providing invalid input
echo -e "invalid\nwrong\nmaybe\ny" | ../../keploy config --generate --path /tmp/keploy-test

echo ""
echo "Test completed!"
echo "The fix should have:"
echo "1. Asked for confirmation (y/n)"
echo "2. Rejected invalid inputs with clear error messages"
echo "3. Allowed up to 3 attempts"
echo "4. Accepted 'y' on the 4th attempt"
echo "5. Exited gracefully without hanging" 
#!/bin/bash

# In this script, latest version of keploy is used for recording, build version of keploy is used for testing

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh


delete_if_exists() {
    local path=$1
    if [ -e "$path" ]; then
        sudo rm -rf "$path"
    fi
}


check_test_status() {
    local path=$1
    local fixed_index=$2 # Boolean to determine if index should be fixed to 0
    local overallStatus=1 # true
    local idx=0 # Initialize index

    for dir in $test_sets; do
        if [ "$fixed_index" -eq 1 ]; then
            local report_file="$path/keploy/reports/test-run-0/$dir-report.yaml"
        else
            local report_file="$path/keploy/reports/test-run-$idx/$dir-report.yaml"
            idx=$((idx + 1))
        fi
        
        local test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
        
        if [ "$test_status" != "PASSED" ]; then
            overallStatus=0 # false
        fi
    done
    echo $overallStatus
}


# Checkout a different branch
git fetch origin
git checkout native-linux

# Check and remove keploy config file if exists
delete_if_exists "./keploy.yml"

# Generate the keploy-config file
kTestBuild config --generate

# # Update the global noise to ts.
# config_file="./keploy.yml"
# sed -i 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

# Build the binary
go build -o ginApp

#### Recording Phase of test-bench ####
pre_rec="."

# Delete the reports directory if it exists
delete_if_exists "$pre_rec/keploy/reports"

# Get all directories except the 'reports' directory
test_sets=$(find "$pre_rec/keploy/" -mindepth 1 -maxdepth 1 -type d ! -name "reports" -exec basename {} \;)

# Loop over each directory stored in 'test_sets'
for dir in $test_sets; do
    echo "Recording and replaying for (test-set): $dir"
    sudo -E env PATH=$PATH kRecordHosted record -c "sudo -E env PATH=$PATH kTestBuild test -c 'go run main.go handler.go' --proxyPort 56789 --dnsPort 46789  --delay=10 --testsets $dir --enableTesting --generateGithubActions=false" --path "./test-bench/" --proxyPort=36789 --dnsPort 26789 --enableTesting --generateGithubActions=false 
    # Wait for 1 second before new test-set
    sleep 1
done

sleep 2


# Check whether the original tests passed or failed
overallStatus=$(check_test_status "." 0)
echo "Overall TestRun status for pre-recorded testscase ran via test-bench: $overallStatus"
if [ "$overallStatus" -eq 0 ]; then
    exit 1
fi


#### Testing Phase of test-bench ####
test_bench_rec="./test-bench"

## Test assertion
pilot -test-assert -preRecPath . -testBenchPath $test_bench_rec
exit_status=$?
if [ $exit_status -ne 0 ]; then
    echo "Test assertion failed with exit status $exit_status."
    exit 1
fi

echo "Tests are asserted successfully."

## Mock assertion preparation

pilot -mock-assert -preRecPath . -testBenchPath $test_bench_rec
exit_status=$?
if [ $exit_status -ne 0 ]; then
    echo "Mock assertion preparation failed with exit status $exit_status."
    exit 1
fi

echo "Mock assertion prepared successfully."

## Now run the tests both for pre-recorded test cases and test-bench recorded test cases to compare the mocks (mock assertion)
delete_if_exists "$pre_rec/keploy/reports"

## Run tests for pre-recorded test cases
sudo -E env PATH=$PATH kTestBuild test -c "./ginApp" --delay=7 --generateGithubActions=false

sleep 2

overallStatus=$(check_test_status "." 1)
echo "Overall TestRun status for pre-recorded testscase (after mock assertion): $overallStatus"
if [ "$overallStatus" -eq 0 ]; then
    echo "Newly recorded mocks are not consistent with the pre-recorded mocks."
    exit 1
fi
echo "New mocks are consistent with the pre-recorded mocks."


## Run tests for test-bench-recorded test cases
sudo -E env PATH=$PATH kTestBuild test -c "./ginApp" --path "$test_bench_rec" --delay=7 --generateGithubActions=false

sleep 2

overallStatus=$(check_test_status "." 1)
echo "Overall TestRun status for test-bench-recorded testscase (after mock assertion): $overallStatus"
if [ "$overallStatus" -eq 0 ]; then
    echo "Old recorded mocks are not consistent with the test-bench-recorded mocks."
    exit 1
fi
echo "Old mocks are consistent with the test-bench-recorded mocks."

# Delete the tests and mocks generated via test-bench.
delete_if_exists "$test_bench_rec"

echo "Tests and mocks are consistent for this application."
exit 0

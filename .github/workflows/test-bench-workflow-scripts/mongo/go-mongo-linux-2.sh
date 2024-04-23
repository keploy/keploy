#!/bin/bash

# In this script, latest version of keploy is used for testing, build version of keploy is used for recording

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

download_and_setup() {
    local url=$1
    local target_bin=$2
    curl --silent --location "$url" | tar xz -C /tmp
    sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/$target_bin
}

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

# Get the hosted version of keploy
curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz" | tar xz -C /tmp
sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/kTestHosted

# Check and remove keploy config file if exists
delete_if_exists "./keploy.yml"

# Generate the keploy-config file
kRecordBuild config --generate

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
    sudo -E env PATH=$PATH kRecordBuild record -c "sudo -E env PATH=$PATH kTestHosted test -c ./ginApp --proxyPort 56789 --dnsPort 46789  --delay=10 --testsets $dir --enableTesting --generateGithubActions=false" --path "./test-bench/" --proxyPort=36789 --dnsPort 26789 --enableTesting --generateGithubActions=false 
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

## Get the pilot for tests and mocks assertion
curl --silent -o pilot --location "https://github.com/keploy/pilot/releases/latest/download/pilot_linux_amd64" && 
sudo chmod a+x pilot && sudo mkdir -p /usr/local/bin && sudo mv pilot /usr/local/bin

## Test assertion
pilot -test-assert -preRecPath . -testBenchPath ./test-bench
exit_status=$?
if [ $exit_status -ne 0 ]; then
    echo "Test assertion failed with exit status $exit_status."
    exit 1
fi

echo "Tests are asserted successfully."

## Mock assertion preparation
pilot -mock-assert -preRecPath . -testBenchPath ./test-bench
exit_status=$?
if [ $exit_status -ne 0 ]; then
    echo "Mock assertion preparation failed with exit status $exit_status."
    exit 1
fi

echo "Mock assertion prepared successfully."

## Now run the tests both for pre-recorded test cases and test-bench recorded test cases to compare the mocks (mock assertion)
delete_if_exists "$pre_rec/keploy/reports"

## Run tests for pre-recorded test cases
sudo -E env PATH=$PATH kTestHosted test -c "./ginApp" --delay=7 --generateGithubActions=false

sleep 2

overallStatus=$(check_test_status "." 1)
echo "Overall TestRun status for pre-recorded testscase (after mock assertion): $overallStatus"
if [ "$overallStatus" -eq 0 ]; then
    echo "Newly recorded mocks are not consistent with the pre-recorded mocks."
    exit 1
fi
echo "New mocks are consistent with the pre-recorded mocks."

test_bench_rec="./test-bench"

## Run tests for test-bench-recorded test cases
sudo -E env PATH=$PATH kTestHosted test -c "./ginApp" --path "$test_bench_rec" --delay=7 --generateGithubActions=false

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
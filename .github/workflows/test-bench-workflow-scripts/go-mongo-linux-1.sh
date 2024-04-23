
#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Checkout a different branch
git fetch origin
git checkout native-linux

# Get the hosted version of keploy
curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz" | tar xz -C /tmp
sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/kRecordHosted

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

# Generate the keploy-config file.
kTestBuild config --generate

# # Update the global noise to ts.
# config_file="./keploy.yml"
# sed -i 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

# Build the binary.
go build -o ginApp

#### Recording Phase of test-bench ####

# Pre-recorded path (for tests and mocks)
pre_rec="."

## Delete the reports directory if it exists in pre_rec
if [ -d "$pre_rec/keploy/reports" ]; then
    sudo rm -rf "$pre_rec/keploy/reports"
fi

# Get all directories except the 'reports' directory
test_sets=$(find "$pre_rec/keploy/" -mindepth 1 -maxdepth 1 -type d ! -name "reports" -exec basename {} \;)

## Loop over each directory stored in 'test_sets'
for dir in $test_sets; do
    echo "Recording and replaying for (test-set): $dir"
    sudo -E env PATH=$PATH kRecordHosted record -c "sudo -E env PATH=$PATH kTestBuild test -c ./ginApp --proxyPort 56789 --dnsPort 46789  --delay=10 --testsets $dir --enableTesting --generateGithubActions=false" --path "./test-bench/" --proxyPort=36789 --dnsPort 26789 --enableTesting --generateGithubActions=false 
    
    # Wait for 1 second before new test-set
    sleep 1
done

## Check whether the original tests passed or failed

sleep 2

# Initialize the index
idx=0

overallStatus=1 #(true)

# Iterate over each directory in test_sets
for dir in $test_sets; do

    # Construct the path to the report file
    report_file="$pre_rec/keploy/reports/test-run-$idx/$dir-report.yaml"
    
    # Extract the status from the report file
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    
    # Check if the test status is not PASSED
    if [ "$test_status" != "PASSED" ]; then
        # Set overallStatus to false if any test fails
        overallStatus=0 #(false)
    fi

    idx=$((idx + 1))
done

# Output the final status
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

# Capture the exit status of the pilot command
exit_status=$?

# Check if the command was successful
if [ $exit_status -eq 0 ]; then
    echo "Tests are asserted successfully."
else
    echo "Test assertion failed with exit status $exit_status."
    exit 1
fi

## Mock assertion preparation
pilot -mock-assert -preRecPath . -testBenchPath ./test-bench

# Capture the exit status of the pilot command
exit_status=$?

# Check if the command was successful
if [ $exit_status -eq 0 ]; then
    echo "Mock assertion prepared successfully."
else
    echo "Mock assertion preparation failed with exit status $exit_status."
    exit 1
fi

## Now run the tests both for pre-recorded test cases and test-bench recorded test cases to compare the mocks (mock assertion)

# Delete the previously generated reports for pre-recorded test cases
if [ -d "$pre_rec/keploy/reports" ]; then
   sudo rm -rf "$pre_rec/keploy/reports"
fi


## Run tests for pre-recorded test cases
sudo -E env PATH=$PATH kTestBuild test -c "./ginApp" --delay=7 --generateGithubActions=false

sleep 2

# Get the status of pre-recorded test cases after preparation of mock assertion
overallStatus=1 #(true)

# Iterate over each directory in test_sets
for dir in $test_sets; do
    # Construct the path to the report file
    report_file="$pre_rec/keploy/reports/test-run-0/$dir-report.yaml"
    
    # Extract the status from the report file
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    
    # Check if the test status is not PASSED
    if [ "$test_status" != "PASSED" ]; then
        # Set overallStatus to false if any test fails
        overallStatus=0 #(false)
    fi
done

# Output the final status
echo "Overall TestRun status for pre-recorded testscase (after mock assertion): $overallStatus"
if [ "$overallStatus" -eq 0 ]; then
    echo "Newly recorded mocks are not consistent with the pre-recorded mocks."
    exit 1
fi

echo "New mocks are consistent with the pre-recorded mocks."

## Run tests for test-bench-recorded test cases
sudo -E env PATH=$PATH kTestBuild test -c "./ginApp" --path "./test-bench" --delay=7 --generateGithubActions=false

test_bench_rec="./test-bench"

sleep 2

# Get the status of pre-recorded test cases after preparation of mock assertion
overallStatus=1 #(true)

# Iterate over each directory in test_sets
for dir in $test_sets; do
    # Construct the path to the report file
    report_file="$test_bench_rec/keploy/reports/test-run-0/$dir-report.yaml"
    
    # Extract the status from the report file
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    
    # Check if the test status is not PASSED
    if [ "$test_status" != "PASSED" ]; then
        # Set overallStatus to false if any test fails
        overallStatus=0 #(false)
    fi
done

# Output the final status
echo "Overall TestRun status for test_bench_rec testscase (after mock assertion): $overallStatus"
if [ "$overallStatus" -eq 0 ]; then
    echo "Old recorded mocks are not consistent with the test-bench-recorded mocks."
    exit 1
fi

echo "Old mocks are consistent with the test-bench-recorded mocks."


# Delete the tests and mocks generated via test-bench.
if [ -d "$test_bench_rec" ]; then
    sudo rm -rf "$test_bench_rec"
fi

echo "Tests and mocks are consistent for this application."
exit 0



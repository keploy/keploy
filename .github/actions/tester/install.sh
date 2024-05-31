#Print the current github workspace
echo Github_workspace "${GITHUB_WORKSPACE}"

# Add fake installation-id for the workflow.
source ${GITHUB_WORKSPACE}/.github/workflows/test_workflow_scripts/test-iid.sh

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


# Go to the working directory
echo "Working directory: ${WORKDIR}"
cd "${WORKDIR}"

# Make a directory to dump coverage
mkdir coverage-reports

# Set GOCOVERDIR to the coverage directory
export GOCOVERDIR="./coverage-reports"

#### Recording Phase of test-bench ####
pre_rec="${KEPLOY_PATH}"

# Delete the reports directory if it exists
delete_if_exists "$pre_rec/keploy/reports"

# Get all directories except the 'reports' directory and maintain order
test_sets=$(find "$pre_rec/keploy/" -mindepth 1 -maxdepth 1 -type d ! -name "reports" | sort | xargs -n 1 basename)


# Check the exit status of the find command
if [ $? -ne 0 ]; then
    echo "Error: No such file or directory."
    echo "::set-output name=script_output::failure"
    exit 1
fi

# Count the number of directories found
num_test_sets=$(echo "$test_sets" | wc -l)

# Check if the number of directories is zero
if [ "$num_test_sets" -eq 0 ]; then
    echo "No test sets found."
    echo "::set-output name=script_output::failure"
    exit 1
fi

# Loop over each directory stored in 'test_sets'
for dir in $test_sets; do
    echo "Recording and replaying for (test-set): $dir"
# MODE (0, recordHosted,testBuild) , (1, recordBuild, testHosted)
    if [ "$MODE" -eq 0 ]; then
        echo "Latest version of keploy is being used for recording, Build version of keploy is being used for testing" 
    else
        echo "Build version of keploy is being used for recording, Latest version of keploy is being used for testing" 
    fi
   
    sudo -E env PATH=$PATH ${KEPLOY_RECORD_BIN} record -c "sudo -E env PATH=$PATH ${KEPLOY_TEST_BIN} test -c '${COMMAND}' --proxyPort 56789 --dnsPort 46789  --delay=${DELAY} --testsets $dir --configPath '${CONFIG_PATH}' --path '$pre_rec' --enableTesting --generateGithubActions=false" --path "./test-bench/" --proxyPort=36789 --dnsPort 26789 --configPath "${CONFIG_PATH}" --enableTesting --generateGithubActions=false 
    # Wait for 1 second before new test-set
    sleep 1
done

sleep 2

# Check whether the original tests passed or failed
overallStatus=$(check_test_status "$pre_rec" 0)
echo "Overall TestRun status for pre-recorded testscase ran via test-bench: $overallStatus"
if [ "$overallStatus" -eq 0 ]; then
    echo "Pre-recorded testcases failed. Exiting..."
    delete_if_exists "$pre_rec/keploy/reports"
    echo "::set-output name=script_output::failure"
    exit 1
fi

echo "Successfully recorded tests and mocks via test-bench 🎉"

#### Testing Phase of test-bench ####
test_bench_rec="./test-bench"

## Test assertion
pilot -test-assert -preRecPath $pre_rec -testBenchPath $test_bench_rec
exit_status=$?
if [ $exit_status -ne 0 ]; then
    echo "Test assertion failed with exit status $exit_status."
    echo "::set-output name=script_output::failure"
    exit 1
fi

echo "Tests are asserted successfully 🎉"


## Mock assertion preparation

pilot -mock-assert -preRecPath $pre_rec -testBenchPath $test_bench_rec
exit_status=$?
if [ $exit_status -ne 0 ]; then
    echo "Mock assertion preparation failed with exit status $exit_status."
    echo "::set-output name=script_output::failure"
    exit 1
fi

echo "Mock assertion prepared successfully 🎉"

## Now run the tests both for pre-recorded test cases and test-bench recorded test cases to compare the mocks (mock assertion)
delete_if_exists "$pre_rec/keploy/reports"

## Run tests for pre-recorded test cases
sudo -E env PATH=$PATH keployR test -c "${COMMAND}" --delay ${DELAY} --path "$pre_rec" --generateGithubActions=false

sleep 2

overallStatus=$(check_test_status "$pre_rec" 1)
echo "Overall TestRun status for pre-recorded testscase (after mock assertion): $overallStatus"
if [ "$overallStatus" -eq 0 ]; then
    echo "Newly recorded mocks are not consistent with the pre-recorded mocks."
    echo "::set-output name=script_output::failure"
    exit 1
fi
echo "New mocks are consistent with the pre-recorded mocks 🎉"


## Run tests for test-bench-recorded test cases
sudo -E env PATH=$PATH keployR test -c "${COMMAND}" --delay ${DELAY} --path "$test_bench_rec" --generateGithubActions=false

sleep 2

overallStatus=$(check_test_status "$test_bench_rec" 1)
echo "Overall TestRun status for test-bench-recorded testscase (after mock assertion): $overallStatus"
if [ "$overallStatus" -eq 0 ]; then
    echo "Old recorded mocks are not consistent with the test-bench-recorded mocks."
    delete_if_exists "$test_bench_rec"
    echo "::set-output name=script_output::failure"
    exit 1
fi
echo "Old mocks are consistent with the test-bench-recorded mocks 🎉"

# Delete the tests and mocks generated via test-bench.
delete_if_exists "$test_bench_rec"

echo "Tests and mocks are consistent for this application 🎉"
echo "::set-output name=script_output::success"

exit 0
# Performance Test Scripts

This directory contains modular scripts used by the GitHub Actions workflow for Keploy performance testing.

## Scripts

### `start-petclinic-with-keploy.sh`
Starts Spring PetClinic wrapped with Keploy in record mode.
- Creates keploy-tests directory
- Locates the PetClinic JAR file
- Starts Keploy with PetClinic as the target application
- Waits for the application to be healthy (up to 60 seconds)
- Saves the Keploy PID for later cleanup

### `create-k6-script.sh`
Generates the k6 load test script (`load-test.js`).
- Configures constant-arrival-rate executor for 100 RPS
- Sets up multiple PetClinic endpoints to test
- Defines performance thresholds and metrics

### `stop-keploy.sh`
Gracefully stops the Keploy process.
- Reads the PID from keploy.pid
- Sends kill signal to stop Keploy
- Used in the workflow's cleanup step (runs always)

### `validate-rps.sh`
Validates that the performance test met the RPS threshold.
- Extracts actual RPS from k6 output log
- Displays performance metrics (percentiles)
- Validates RPS >= 100
- Exits with appropriate status code (0 for pass, 1 for fail)

### `display-test-cases.sh`
Displays information about Keploy recorded test cases.
- Counts the number of YAML test files
- Lists the test directory contents
- Used for debugging and verification

## Usage

These scripts are called by `.github/workflows/keploy-performance-test.yml`. They should be made executable before use:

```bash
chmod +x scripts/performance-test/*.sh
```

## Requirements

- Bash shell
- sudo access (for Keploy)
- curl (for health checks)
- bc (for floating-point comparison)
- grep with Perl regex support (for parsing k6 output)

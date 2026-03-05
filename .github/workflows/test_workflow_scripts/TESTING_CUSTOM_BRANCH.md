# Testing Performance with Custom Keploy Branch

This guide explains how to test the performance test workflow with a different Keploy repository or branch.

## Quick Start

### Option 1: Using GitHub Actions UI (Recommended)

1. Go to the **Actions** tab in your repository
2. Select **Keploy Performance Test** workflow
3. Click **Run workflow** button
4. Fill in the inputs:
   - **keploy_repo**: Repository to test (e.g., `keploy/enterprise`)
   - **keploy_branch**: Branch to test (e.g., `fix/kafka-issue-1672`)
5. Click **Run workflow**

### Option 2: Using GitHub CLI

```bash
# Test with enterprise repo and specific branch
gh workflow run keploy-performance-test.yml \
  -f keploy_repo=keploy/enterprise \
  -f keploy_branch=fix/kafka-issue-1672

# Test with main keploy repo and specific branch
gh workflow run keploy-performance-test.yml \
  -f keploy_repo=keploy/keploy \
  -f keploy_branch=feature/new-feature
```

## How It Works

The workflow now:

1. **Checks out the current repo** (for test scripts) → `keploy/` directory
2. **Checks out the Keploy version to test** → `keploy-to-test/` directory
   - Uses `keploy_repo` input (default: `keploy/keploy`)
   - Uses `keploy_branch` input (default: current branch)
3. **Builds Keploy** from `keploy-to-test/`
4. **Copies the binary** to `keploy/` directory
5. **Runs performance tests** using the custom Keploy build

## Examples

### Test Enterprise Repo Issue #1672

```bash
gh workflow run keploy-performance-test.yml \
  -f keploy_repo=keploy/enterprise \
  -f keploy_branch=main
```

### Test a PR Branch

```bash
gh workflow run keploy-performance-test.yml \
  -f keploy_repo=keploy/keploy \
  -f keploy_branch=pr/1234
```

### Test Your Fork

```bash
gh workflow run keploy-performance-test.yml \
  -f keploy_repo=yourusername/keploy \
  -f keploy_branch=my-feature
```

## Default Behavior

If you don't specify inputs:
- **On manual trigger**: Uses `keploy/keploy` repo and `main` branch
- **On PR**: Uses the PR's branch automatically

## Troubleshooting

### Build Fails

If the build fails, check:
- The repository exists and is accessible
- The branch name is correct
- The repo has a `main.go` file in the root
- Go dependencies are valid

### Different Project Structure

If testing with `keploy/enterprise` or another repo with different structure:
- Ensure the binary is built correctly
- Check if the main file is in a different location
- Modify the build command in the workflow if needed

For enterprise repo, you might need:
```yaml
- name: Build Keploy
  working-directory: keploy-to-test
  run: |
    # For enterprise repo
    cd cmd/enterprise
    go build -o ../../keploy main.go
```

## Performance Thresholds

The tests use these thresholds (configured in workflow):
- **P50**: < 5ms
- **P90**: < 15ms
- **P99**: < 70ms
- **RPS**: >= 100 (±1% tolerance)

Tests run 3 times, pipeline fails only if 2+ runs show regression.

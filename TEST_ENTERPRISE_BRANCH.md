# Quick Guide: Test Enterprise Branch with Performance Tests

## To test the enterprise repo (issue #1672 or any branch):

### Using GitHub Actions UI:

1. Go to: **Actions** → **Keploy Performance Test** → **Run workflow**
2. Enter:
   - **keploy_repo**: `keploy/enterprise`
   - **keploy_branch**: `main` (or your branch name)
3. Click **Run workflow**

### Using GitHub CLI:

```bash
# Test enterprise main branch
gh workflow run keploy-performance-test.yml \
  -f keploy_repo=keploy/enterprise \
  -f keploy_branch=main

# Test specific enterprise branch
gh workflow run keploy-performance-test.yml \
  -f keploy_repo=keploy/enterprise \
  -f keploy_branch=fix/your-branch-name
```

## What happens:

1. Workflow checks out your test scripts (from current repo)
2. Checks out the enterprise repo/branch you specified
3. Builds Keploy from enterprise repo (handles `cmd/enterprise/main.go` structure)
4. Runs performance tests with the enterprise build
5. Validates against thresholds: P50<5ms, P90<15ms, P99<70ms

## Results:

- Tests run 3 times
- Pipeline fails only if 2+ runs show regression
- Results uploaded as artifacts
- PR gets automatic comment with results table

## Fixed Issues:

✅ P99 threshold now correctly enforced (195ms will fail, not pass)
✅ Metrics extracted with proper units
✅ k6 exit code doesn't block validation
✅ Works with both keploy/keploy and keploy/enterprise repos

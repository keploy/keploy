# Quick Guide: Test Enterprise Branch with Performance Tests

## How It Works

```
┌─────────────────────────────────────────────────────────────┐
│ Trigger Type                                                 │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  PR (automatic)          →  Tests current PR branch         │
│                                                              │
│  Manual (no inputs)      →  Tests current branch            │
│                                                              │
│  Manual (with inputs)    →  Tests specified repo/branch     │
│                              (e.g., keploy/enterprise)       │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

## Default Behavior

### On Pull Requests (Automatic):
- Tests the PR branch from the current repository
- No manual input needed
- Uses the code from the PR

### On Manual Trigger (workflow_dispatch):
- **Without inputs**: Tests current branch
- **With inputs**: Tests the specified repo/branch

## To test the enterprise repo (issue #1672 or any branch):

### Using GitHub Actions UI (Step-by-Step):

1. **Go to your repository on GitHub** (e.g., `github.com/keploy/keploy`)

2. **Click the "Actions" tab** at the top of the page

3. **Find "Keploy Performance Test"** in the left sidebar (list of workflows)

4. **Click "Run workflow"** button (on the right side, it's a dropdown button)

5. **Fill in the form that appears:**
   ```
   Use workflow from: [Branch: main ▼]  ← Select branch to run FROM
   
   keploy_repo: [keploy/enterprise    ]  ← Type the repo to TEST
   
   keploy_branch: [main               ]  ← Type the branch to TEST
   ```

6. **Click the green "Run workflow" button** at the bottom of the form

### Example Values:

To test enterprise repo:
```
keploy_repo:    keploy/enterprise
keploy_branch:  main
```

To test a specific enterprise branch:
```
keploy_repo:    keploy/enterprise
keploy_branch:  fix/kafka-issue-1672
```

To test your fork:
```
keploy_repo:    yourusername/keploy
keploy_branch:  my-feature-branch
```

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

## Troubleshooting:

**Q: I don't see the "Run workflow" button**
- Make sure you're on the "Actions" tab
- Make sure you're looking at the workflow list (left sidebar)
- Click on "Keploy Performance Test" workflow name
- The button appears on the right side above the workflow runs list

**Q: The form doesn't show up**
- Make sure you clicked the "Run workflow" dropdown button (not just the text)
- You need write access to the repository to manually trigger workflows

**Q: Build fails with "Could not find main.go"**
- Check that the repository and branch exist
- Verify the repo has either `main.go` or `cmd/enterprise/main.go`

## Fixed Issues:

✅ P99 threshold now correctly enforced (195ms will fail, not pass)
✅ Metrics extracted with proper units
✅ k6 exit code doesn't block validation
✅ Works with both keploy/keploy and keploy/enterprise repos
✅ No duplicate checkouts on PR runs

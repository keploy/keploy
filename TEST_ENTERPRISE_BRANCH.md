# Quick Guide: Test Enterprise Branch with Performance Tests

## How It Works

```
┌─────────────────────────────────────────────────────────────┐
│ Trigger Type                                                 │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  PR (automatic)          →  Tests the PR branch             │
│                              (developer's actual changes)    │
│                                                              │
│  Manual (no inputs)      →  Tests current branch            │
│                                                              │
│  Manual (with inputs)    →  Tests specified repo/branch     │
│                              (e.g., keploy/enterprise)       │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

## Default Behavior

### On Pull Requests (Automatic) - IDEAL FOR DEVELOPERS:
- Tests the PR branch (the developer's actual code changes)
- No manual input needed
- Results posted as comment on the PR
- This is how developers get automatic performance feedback

### On Manual Trigger (workflow_dispatch) - FOR TESTING OTHER BRANCHES:
- **Without inputs**: Tests current branch
- **With inputs**: Tests the specified repo/branch
- Useful for testing branches before creating a PR
- Useful for testing other repos (like enterprise)

## To test the enterprise repo (issue #1672 or any branch):

### Using GitHub Actions UI (Step-by-Step):

1. **Go to your repository on GitHub** (e.g., `github.com/keploy/keploy`)

2. **Click the "Actions" tab** at the top of the page

3. **In the LEFT SIDEBAR**, click on **"Keploy Performance Test"** 
   - You should see it in the list under "All workflows"
   - It's in the left panel, NOT the main area showing workflow runs

4. **After clicking the workflow name**, you'll see a blue "Run workflow" button appear on the RIGHT side
   - It's above the list of workflow runs
   - Next to the "Filter workflow runs" search box

5. **Click "Run workflow"** - a dropdown form will appear

6. **Fill in the form:**
   ```
   Use workflow from: [Branch: main ▼]  ← Select branch to run FROM
   
   keploy_repo: [keploy/enterprise    ]  ← Type the repo to TEST
   
   keploy_branch: [main               ]  ← Type the branch to TEST
   ```

7. **Click the green "Run workflow" button** at the bottom of the dropdown

### Visual Guide:

```
┌─────────────────────────────────────────────────────────────┐
│ Actions Tab                                                  │
├──────────────┬──────────────────────────────────────────────┤
│              │  [Run workflow ▼]  ← Click this button       │
│ LEFT SIDEBAR │                                               │
│              │  32 workflow runs                             │
│ All workflows│                                               │
│              │  ✓ Add k6 performance testing...              │
│ > Docker     │  ✓ Add k6 performance testing...              │
│ > Release    │  ✓ Add k6 performance testing...              │
│ > Keploy ←───┼─ Click this first!                           │
│   Performance│                                               │
│   Test       │                                               │
│              │                                               │
└──────────────┴──────────────────────────────────────────────┘
```

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
- You're looking at the workflow runs list (main area)
- You need to click on "Keploy Performance Test" in the LEFT SIDEBAR first
- After clicking the workflow name, the "Run workflow" button appears on the right
- The button is blue and says "Run workflow" with a dropdown arrow

**Q: I don't see "Keploy Performance Test" in the sidebar**
- Make sure the workflow file is committed to the repository
- Check that you're on the correct repository (keploy/keploy)
- Try refreshing the page

**Q: The form doesn't show up**
- Make sure you clicked the "Run workflow" dropdown button (not just the text)
- You need write access to the repository to manually trigger workflows
- If you don't have write access, ask a maintainer to run it for you

**Q: Build fails with "Could not find main.go"**
- Check that the repository and branch exist
- Verify the repo has either `main.go` or `cmd/enterprise/main.go`

## Fixed Issues:

✅ P99 threshold now correctly enforced (195ms will fail, not pass)
✅ Metrics extracted with proper units
✅ k6 exit code doesn't block validation
✅ Works with both keploy/keploy and keploy/enterprise repos
✅ No duplicate checkouts on PR runs

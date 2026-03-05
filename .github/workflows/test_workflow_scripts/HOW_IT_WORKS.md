# How Performance Testing Works

## Overview

The performance test workflow automatically tests code changes to ensure they don't cause performance regressions.

## Workflow Behavior

### 1. On Pull Request (Automatic)

When someone creates a PR:

```
┌─────────────────────────────────────────────────────────────┐
│ Developer creates PR with code changes                       │
│         ↓                                                    │
│ Workflow automatically triggers                              │
│         ↓                                                    │
│ Tests THE PR BRANCH (their actual changes)                   │
│         ↓                                                    │
│ Runs performance tests (3 times)                             │
│         ↓                                                    │
│ Posts results as comment on the PR                           │
└─────────────────────────────────────────────────────────────┘
```

**Example:**
- Alice creates PR #123 with changes to improve caching
- Workflow runs automatically
- Tests Alice's branch with her caching changes
- Posts performance results on PR #123
- If P99 > 70ms in 2+ runs → PR fails ❌
- If P99 < 70ms in 2+ runs → PR passes ✅

### 2. Manual Trigger (Testing Other Branches)

To test a specific branch without creating a PR:

```
┌─────────────────────────────────────────────────────────────┐
│ Go to Actions → Keploy Performance Test → Run workflow      │
│         ↓                                                    │
│ Fill in inputs:                                              │
│   - keploy_repo: keploy/enterprise                           │
│   - keploy_branch: fix/some-issue                            │
│         ↓                                                    │
│ Tests that specific repo/branch                              │
│         ↓                                                    │
│ Results uploaded as artifacts                                │
└─────────────────────────────────────────────────────────────┘
```

**Example:**
- You want to test the `tls-refactor` branch
- Go to Actions → Run workflow
- Enter: `keploy_repo: keploy/keploy`, `keploy_branch: tls-refactor`
- Tests that branch
- Results available in artifacts

## What Gets Tested

The workflow:
1. Builds Keploy from the specified branch
2. Starts Spring PetClinic application with Keploy
3. Runs k6 load tests (100 RPS for 30 seconds)
4. Measures performance metrics (P50, P90, P99, RPS)
5. Validates against thresholds

## Performance Thresholds

```
P50 (median):       < 5ms
P90 (90th %ile):    < 15ms
P99 (99th %ile):    < 70ms
RPS (requests/sec): >= 100 (±1% tolerance)
```

## Pass/Fail Logic

Tests run **3 times** to account for variance:

- **PASS**: 0 or 1 runs fail (allows for occasional variance)
- **FAIL**: 2 or 3 runs fail (consistent regression)

**Example Scenarios:**

| Run 1 | Run 2 | Run 3 | Result | Reason |
|-------|-------|-------|--------|--------|
| ✅ PASS | ✅ PASS | ✅ PASS | ✅ PASS | All runs passed |
| ❌ FAIL | ✅ PASS | ✅ PASS | ✅ PASS | Only 1 failure (acceptable) |
| ❌ FAIL | ❌ FAIL | ✅ PASS | ❌ FAIL | 2 failures (regression) |
| ❌ FAIL | ❌ FAIL | ❌ FAIL | ❌ FAIL | All failed (major regression) |

## How Code Changes Affect Performance

### Scenario 1: Developer Adds Caching

```go
// Before (no caching)
func GetUser(id string) User {
    return db.Query("SELECT * FROM users WHERE id = ?", id)
}

// After (with caching)
func GetUser(id string) User {
    if cached := cache.Get(id); cached != nil {
        return cached
    }
    user := db.Query("SELECT * FROM users WHERE id = ?", id)
    cache.Set(id, user)
    return user
}
```

**Expected Result:**
- P50, P90, P99 should decrease (faster responses)
- RPS might increase (can handle more requests)
- Performance test: ✅ PASS

### Scenario 2: Developer Adds Heavy Processing

```go
// Before (simple)
func ProcessRequest(data string) {
    return data
}

// After (heavy processing)
func ProcessRequest(data string) {
    // Expensive operation
    for i := 0; i < 1000000; i++ {
        hash := sha256.Sum256([]byte(data + string(i)))
        data = string(hash[:])
    }
    return data
}
```

**Expected Result:**
- P50, P90, P99 will increase (slower responses)
- RPS might decrease (can handle fewer requests)
- Performance test: ❌ FAIL (if P99 > 70ms)

## Viewing Results

### On PR (Automatic)

Results appear as a comment:

```markdown
🚀 Keploy Performance Test Results

| Run | P50 | P90 | P99 | RPS | Status |
|-----|-----|-----|-----|-----|--------|
| 1   | 2.88ms | 8.95ms | 65.23ms | 100.12 | ✅ PASS |
| 2   | 2.91ms | 9.01ms | 68.45ms | 99.98 | ✅ PASS |
| 3   | 2.85ms | 8.88ms | 64.89ms | 100.05 | ✅ PASS |

✅ Result: PASSED - Only 0 out of 3 runs failed
```

### Manual Trigger

Results available in:
- Artifacts: Download `performance-results-{run_number}.zip`
- Logs: View in the workflow run details

## Summary

**For PR authors:**
- Your code is automatically tested for performance
- Results appear as a comment on your PR
- Fix any regressions before merging

**For reviewers:**
- Check the performance test results in PR comments
- Investigate any failures or significant changes
- Consider if performance trade-offs are acceptable

**For manual testing:**
- Use workflow inputs to test any branch
- Useful for testing before creating a PR
- Results available in artifacts

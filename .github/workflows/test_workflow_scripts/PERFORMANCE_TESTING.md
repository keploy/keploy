# Keploy Performance Testing

## Overview

The Keploy performance testing pipeline implements a robust multi-run validation strategy that runs tests 3 times and only fails if 2 or more runs show regression. This approach eliminates false positives from transient issues while catching real performance problems.

## Key Features

### Multi-Run Validation
- Tests run **3 times** by default
- Pipeline fails only if **2 or more runs** show regression
- Helps eliminate false positives from transient issues

### Comprehensive Metrics
The tests track multiple percentiles to catch different types of performance issues:

- **P50 (Median)**: < 50ms - Represents typical user experience
- **P90**: < 100ms - Catches issues affecting 10% of requests
- **P99**: < 500ms - Identifies tail latency problems
- **Error Rate**: < 1% - Ensures reliability
- **RPS**: >= 100 - Throughput validation

### Why These Percentiles Matter

- **P50 (median)**: 50% of requests are faster than this value and 50% are slower — represents typical user experience.
- **P90**: 90% of requests are faster than this value — highlights issues affecting a significant minority of users.
- **P99**: 99% of requests are faster than this value — surfaces rare but severe tail latency problems.

By tracking all three, we understand both typical and worst-case performance and can detect regressions across the full latency distribution.

## Configuration

### Environment Variables

You can customize the test behavior using environment variables:

```yaml
env:
  NUM_RUNS: 3                    # Number of test runs
  REQUIRED_FAILURES: 2           # Failures needed to fail pipeline
  P50_THRESHOLD: 50              # P50 threshold in milliseconds
  P90_THRESHOLD: 100             # P90 threshold in milliseconds
  P99_THRESHOLD: 500             # P99 threshold in milliseconds
  ERROR_RATE_THRESHOLD: 0.01     # Error rate threshold (1%)
  RPS_THRESHOLD: 100             # Minimum RPS required
```

### Adjusting Thresholds

Edit the workflow file `.github/workflows/keploy-performance-test.yml`:

```yaml
- name: Run k6 performance tests (3 runs with validation)
  env:
    P50_THRESHOLD: 30    # Stricter threshold
    P90_THRESHOLD: 80
    P99_THRESHOLD: 400
```

## How It Works

### Test Execution Flow

1. **Run 1**: Execute k6 load test, extract metrics, check thresholds
2. **Wait 10s**: Allow system to stabilize
3. **Run 2**: Execute k6 load test, extract metrics, check thresholds
4. **Wait 10s**: Allow system to stabilize
5. **Run 3**: Execute k6 load test, extract metrics, check thresholds
6. **Aggregate**: Calculate statistics and determine pass/fail

### Decision Logic

```
Run Results          Failed Runs    Required    Pipeline Result
─────────────────────────────────────────────────────────────────
PASS, PASS, PASS         0/3           2        ✅ PASS
PASS, PASS, FAIL         1/3           2        ✅ PASS
PASS, FAIL, PASS         1/3           2        ✅ PASS
FAIL, PASS, PASS         1/3           2        ✅ PASS
PASS, FAIL, FAIL         2/3           2        ❌ FAIL
FAIL, PASS, FAIL         2/3           2        ❌ FAIL
FAIL, FAIL, PASS         2/3           2        ❌ FAIL
FAIL, FAIL, FAIL         3/3           2        ❌ FAIL
```

### Threshold Evaluation

Each run is evaluated against all thresholds:

```
Metric          Threshold    Impact
─────────────────────────────────────
P50             < 50ms       Run fails if exceeded
P90             < 100ms      Run fails if exceeded
P99             < 500ms      Run fails if exceeded
Error Rate      < 1%         Run fails if exceeded
RPS             >= 100       Run fails if not met
```

If **ANY** threshold fails, the entire run is marked as FAILED.

## Example Output

### Successful Run

```
=========================================
Run 1 of 3
=========================================

Checking thresholds for Run 1...
  ✓ P50 passed: 23.45ms < 50ms
  ✓ P90 passed: 67.89ms < 100ms
  ✓ P99 passed: 234.56ms < 500ms
  ✓ Error rate passed: 0.12% < 1%
  ✓ RPS passed: 145.67 >= 100
Run 1: PASSED

[... runs 2 and 3 ...]

=========================================
SUMMARY REPORT
=========================================

Individual Run Results:
  Run 1: PASSED
    P50: 23.45ms
    P90: 67.89ms
    P99: 234.56ms
    Error Rate: 0.12%
    RPS: 145.67
  Run 2: PASSED
    P50: 24.12ms
    P90: 69.34ms
    P99: 241.23ms
    Error Rate: 0.15%
    RPS: 143.21
  Run 3: PASSED
    P50: 22.98ms
    P90: 66.45ms
    P99: 229.87ms
    Error Rate: 0.09%
    RPS: 147.89

Aggregate Statistics:
  Average P50: 23.52ms
  Average P90: 67.89ms
  Average P99: 235.22ms
  Average Error Rate: 0.12%
  Average RPS: 145.59

FINAL RESULT
=========================================
Failed runs: 0 / 3
Required failures to fail pipeline: 2

✅ PIPELINE PASSED
Only 0 out of 3 runs failed.
This is below the threshold of 2 failures.
```

### Failed Run (Regression Detected)

```
=========================================
SUMMARY REPORT
=========================================

Individual Run Results:
  Run 1: FAILED
    P50: 78.45ms
    P90: 156.89ms
    P99: 678.56ms
    Error Rate: 0.45%
    RPS: 87.23
  Run 2: FAILED
    P50: 82.12ms
    P90: 162.34ms
    P99: 701.23ms
    Error Rate: 0.52%
    RPS: 84.56
  Run 3: PASSED
    P50: 45.98ms
    P90: 89.45ms
    P99: 389.87ms
    Error Rate: 0.19%
    RPS: 112.34

FINAL RESULT
=========================================
Failed runs: 2 / 3
Required failures to fail pipeline: 2

❌ PIPELINE FAILED
Performance regression detected in 2 out of 3 runs.
This exceeds the threshold of 2 failures.
```

## CI/CD Integration

### GitHub Actions

The workflow runs automatically on:
- Pull requests to `main` branch
- Manual trigger via `workflow_dispatch`

### PR Comments

For pull requests, the workflow automatically posts a comment with:
- Summary table of all 3 runs
- Metrics for each run (P50, P90, P99, Error Rate, RPS)
- Pass/Fail status for each run
- Overall result

Example PR comment:

```markdown
## 🚀 Keploy Performance Test Results

**Multi-Run Validation:** Tests run 3 times, pipeline fails only if 2+ runs show regression.

| Run | P50 | P90 | P99 | Error Rate | RPS | Status |
|-----|-----|-----|-----|------------|-----|--------|
| 1 | 23.45ms | 67.89ms | 234.56ms | 0.12% | 145.67 | ✅ PASS |
| 2 | 24.12ms | 69.34ms | 241.23ms | 0.15% | 143.21 | ✅ PASS |
| 3 | 22.98ms | 66.45ms | 229.87ms | 0.09% | 147.89 | ✅ PASS |

**Thresholds:** P50 < 50ms, P90 < 100ms, P99 < 500ms, Error Rate < 1%, RPS >= 100

✅ **Result:** PASSED - Only 0 out of 3 runs failed (threshold: 2)

*P50, P90, and P99 percentiles naturally filter out outliers*
```

## Best Practices

### When to Adjust Thresholds

- **Too many false positives**: Increase thresholds or required failures
- **Missing real regressions**: Decrease thresholds or required failures
- **Different test scenarios**: Use scenario-specific thresholds

### Interpreting Failures

If 2+ runs fail:
1. Check if it's a consistent pattern across all metrics
2. Review recent code changes
3. Check system resources in CI environment
4. Compare with historical results
5. Run tests locally to reproduce

### Optimizing Test Duration

Current test takes ~60 seconds per run (~3 minutes total for 3 runs):
- Adjust `duration` in k6 script for faster/slower tests
- Adjust `rate` for different load levels
- Balance between test thoroughness and CI time

## Troubleshooting

### Tests timing out
- Increase test duration in k6 script
- Reduce target RPS
- Check application startup time

### High error rates
- Verify application is healthy before tests
- Check dependencies (MySQL, etc.)
- Review application logs

### Inconsistent results
- Increase number of runs (NUM_RUNS=5)
- Add longer stabilization period between runs
- Check for resource contention in CI

## Local Testing

To run the performance tests locally:

```bash
# Navigate to keploy directory
cd keploy

# Make script executable
chmod +x .github/workflows/test_workflow_scripts/run-perf-test-with-validation.sh

# Run with default settings
./.github/workflows/test_workflow_scripts/run-perf-test-with-validation.sh

# Run with custom settings
NUM_RUNS=5 REQUIRED_FAILURES=3 P50_THRESHOLD=30 \
  ./.github/workflows/test_workflow_scripts/run-perf-test-with-validation.sh
```

## References

- [k6 Documentation](https://k6.io/docs/)
- [Performance Testing Best Practices](https://k6.io/docs/testing-guides/test-types/)
- [Understanding Percentiles](https://www.dynatrace.com/news/blog/why-averages-suck-and-percentiles-are-great/)

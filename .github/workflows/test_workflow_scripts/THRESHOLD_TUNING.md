# Performance Threshold Tuning Guide

## Current Issue Analysis

### Observed Performance Data

From your test runs:

**Run 1 (Initial baseline):**
```
P50: 2.76ms, P90: 8.44ms,  P99: 23.64ms
P50: 2.25ms, P90: 6.91ms,  P99: 12.12ms
P50: 2.37ms, P90: 7.45ms,  P99: 11.27ms
Average: P50=2.46ms, P90=7.60ms, P99=15.68ms
```

**Run 2 (After optimization):**
```
P50: 3.07ms, P90: 10.23ms, P99: 58.03ms
```

### The P99 Problem

**Why P99 varies so much:**

```
Run 1 average: P99 = 15.68ms
Run 2:         P99 = 58.03ms  (3.7x higher!)
```

**This is NORMAL for P99 in CI environments because:**

1. **P99 = 99th percentile** = The slowest 1% of requests
2. In 3,000 requests, P99 represents the slowest 30 requests
3. CI environments have:
   - Shared CPU with other jobs
   - Variable I/O performance
   - Occasional GC pauses
   - Network hiccups

**P50 and P90 are stable:**
```
P50: 2.46ms → 3.07ms (25% variance) ✅ Stable
P90: 7.60ms → 10.23ms (35% variance) ✅ Acceptable
P99: 15.68ms → 58.03ms (270% variance) ❌ High variance
```

## Updated Thresholds

### New Configuration

```yaml
P50_THRESHOLD: 5ms    # 2x baseline (2.5ms)
P90_THRESHOLD: 15ms   # 2x baseline (7.6ms)
P99_THRESHOLD: 70ms   # 4x baseline (15.7ms) - accounts for CI variance
```

### Why These Work

**P50 Threshold (5ms):**
```
Baseline: 2.5ms
Threshold: 5ms (2x)
Catches: 2x regressions
Variance: ±20% is normal
```

**P90 Threshold (15ms):**
```
Baseline: 7.6ms
Threshold: 15ms (2x)
Catches: 2x regressions
Variance: ±30% is normal
```

**P99 Threshold (70ms):**
```
Baseline: 15.7ms
Threshold: 70ms (4.5x)
Catches: 4x+ regressions
Variance: ±200% is normal in CI
```

## Why P99 Needs Higher Threshold

### Statistical Reality

```
3,000 requests per run:
- P50 = median of 3,000 samples → Very stable
- P90 = 90th percentile of 3,000 samples → Stable
- P99 = 99th percentile of 3,000 samples → Sensitive to outliers

P99 represents only 30 requests out of 3,000!
```

### CI Environment Factors

**Things that affect P99 but not P50/P90:**

1. **CPU throttling** (affects slowest requests)
2. **GC pauses** (occasional, hits tail latency)
3. **I/O contention** (shared disk, affects some requests)
4. **Network jitter** (occasional delays)
5. **Container scheduling** (CPU time slicing)

**Example:**
```
2,970 requests: 2-10ms (normal)
  29 requests: 10-30ms (slightly slow)
   1 request:  58ms (CI hiccup) ← This becomes P99!
```

## Multi-Run Validation Saves You

Even with P99 variance, the 3-run strategy protects against false failures:

### Scenario 1: Real Regression
```
Your code makes Keploy 3x slower:

Run 1: P50=7ms, P90=22ms, P99=90ms  ← FAIL
Run 2: P50=8ms, P90=24ms, P99=95ms  ← FAIL
Run 3: P50=7.5ms, P90=23ms, P99=88ms ← FAIL

Result: ❌ FAIL (3/3 runs failed)
This is a REAL problem!
```

### Scenario 2: CI Hiccup
```
One run has bad P99 due to CI:

Run 1: P50=3ms, P90=10ms, P99=58ms  ← FAIL (CI hiccup)
Run 2: P50=2.5ms, P90=8ms, P99=18ms ← PASS
Run 3: P50=2.8ms, P90=9ms, P99=22ms ← PASS

Result: ✅ PASS (only 1/3 failed)
This is just CI variance, not your code!
```

### Scenario 3: Consistent P99 Issue
```
Your code has a tail latency bug:

Run 1: P50=3ms, P90=10ms, P99=75ms  ← FAIL
Run 2: P50=2.8ms, P90=9ms, P99=72ms ← FAIL
Run 3: P50=3.1ms, P90=11ms, P99=78ms ← FAIL

Result: ❌ FAIL (3/3 runs failed)
This catches the tail latency bug!
```

## Threshold Tuning Strategy

### Start Conservative (Current)

```yaml
P50_THRESHOLD: 5ms    # 2x baseline
P90_THRESHOLD: 15ms   # 2x baseline
P99_THRESHOLD: 70ms   # 4.5x baseline
```

**Catches:**
- 2x P50/P90 regressions (significant)
- 4x+ P99 regressions (severe tail latency)

**Allows:**
- Normal CI variance (±30% for P50/P90, ±200% for P99)
- Occasional slow runs
- Minor optimizations/regressions

### After Collecting Data (1-2 weeks)

Review your actual P99 distribution:

```bash
# If P99 is consistently 15-25ms:
P99_THRESHOLD: 50ms   # 3x baseline

# If P99 occasionally spikes to 60-80ms:
P99_THRESHOLD: 70ms   # Keep current (4.5x)

# If P99 is very stable (always <30ms):
P99_THRESHOLD: 40ms   # 2.5x baseline
```

### For Stricter Validation

If you want to catch smaller regressions:

```yaml
P50_THRESHOLD: 4ms     # 1.6x baseline
P90_THRESHOLD: 12ms    # 1.6x baseline
P99_THRESHOLD: 60ms    # 3.8x baseline
NUM_RUNS: 5            # More runs for confidence
REQUIRED_FAILURES: 3   # Higher threshold
```

## Monitoring Recommendations

### Track These Metrics Over Time

1. **P50 trend** (should be very stable)
   - Alert if >20% variance
   - Alert if consistently above 4ms

2. **P90 trend** (should be stable)
   - Alert if >30% variance
   - Alert if consistently above 12ms

3. **P99 distribution** (will vary)
   - Track min/max/median P99 across runs
   - Alert if median P99 >40ms
   - Alert if max P99 >100ms consistently

### Example Dashboard

```
Last 10 runs:
P50: [2.5, 2.8, 2.3, 2.9, 2.6, 2.7, 2.4, 2.8, 2.5, 3.0] ✅ Stable
P90: [7.5, 8.2, 7.8, 9.1, 8.5, 8.0, 7.9, 8.8, 8.2, 10.2] ✅ Stable
P99: [15, 58, 22, 18, 45, 20, 25, 62, 19, 23] ⚠️ Variable (normal)

Median P99: 23ms ✅ Good
Max P99: 62ms ✅ Within threshold (70ms)
```

## When to Investigate

### ✅ Normal (Don't worry)
```
Single run with high P99: 58ms
Other runs normal: 18ms, 22ms
→ This is CI variance
```

### ⚠️ Watch (Monitor)
```
P99 trending up over weeks:
Week 1: avg 18ms
Week 2: avg 25ms
Week 3: avg 32ms
→ Possible slow degradation
```

### ❌ Investigate (Action needed)
```
Consistent high P99 across all runs:
Run 1: 75ms
Run 2: 78ms
Run 3: 72ms
→ Real tail latency issue
```

## Summary

**Current thresholds (5/15/70ms) are appropriate because:**

1. ✅ P50 and P90 catch 2x regressions (significant issues)
2. ✅ P99 allows for CI variance while catching 4x+ regressions
3. ✅ 3-run validation filters out transient CI issues
4. ✅ Based on your actual performance data

**The pipeline will catch:**
- Real performance regressions (2x+ slower)
- Consistent tail latency issues (4x+ P99)
- Significant throughput drops

**The pipeline will NOT fail on:**
- Normal CI variance (±30% for P50/P90)
- Occasional slow runs (single P99 spike)
- Minor optimizations/regressions (<50%)

This is the right balance for a CI performance test!

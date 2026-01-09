# How Keploy Uses Keploy for Replay & Re-record Testing

## Overview

Modern API-driven systems change fast.  
In Keploy itself, even a small code change can affect **multiple APIs, response structures, or database behavior**. Traditional unit tests often fail to capture these changes accurately and require **manual updates**, which slows down development.

To solve this, **Keploy uses Keploy to test Keploy**.

In our CI pipeline, we automatically:

- Replay real API traffic as tests
- Detect behavior changes
- Re-record updated API behavior when needed
- Stabilize tests
- Enforce coverage on new code

This flow runs the API server with its **real runtime dependencies (MongoDB)** to validate behavior under production-like conditions.

All of this runs **inside GitHub Actions on every Pull Request**.

---

## What Keploy Tests with Keploy

We use Keploy to test our **core API server**.

Specifically, we test:

- REST APIs exposed by the `api-server`
- End-to-end request → database → response flows
- API behavior against MongoDB-backed data
- Regression across API contracts when code changes

The API server is built as a binary inside CI and tested in a **realistic environment** with:

- A live MongoDB service
- Real API request/response snapshots
- Previously recorded test sets (`atg-flow`)

This ensures we are testing **real system behavior**, not mocked logic.

---

## Dependencies Used in This Flow

This replay & re-record pipeline runs the API server with its **actual runtime dependencies**, not mocks.

### Primary Dependency

- **MongoDB**
  - Acts as the primary data store for the API server
  - Runs as a Docker service inside CI
  - Reset and restored to ensure consistent test results

### Why MongoDB Matters Here

- API behavior is tightly coupled with database state
- Many API responses depend on:
  - Stored documents
  - Generated identifiers
  - Business rules enforced at the database layer
- Replaying and re-recording API traffic without MongoDB would not reflect real behavior

Using MongoDB ensures:

- Accurate request → database → response validation
- Deterministic test replays
- Reliable re-recording when APIs change

---

## Why Keploy Uses Keploy

### The Actual Problem

1. **API behavior changes frequently**
   - Response fields evolve
   - Error messages change
   - Business logic updates break existing tests

2. **Manual test maintenance does not scale**
   - Updating snapshots by hand is slow
   - Engineers forget to update tests
   - Tests become flaky or outdated

3. **Traditional tests miss real integrations**
   - Unit tests don’t capture DB + API behavior together
   - Mocked tests pass while production breaks

---

### How Keploy Solves This

Keploy introduces **record → replay → re-record** testing:

#### 1. Replay First (Fail Fast)

- Existing API test cases are replayed on every PR  
- If behavior is unchanged → tests pass immediately

#### 2. Re-record on Failure (Auto-healing)

- If APIs change, Keploy automatically:
  - Re-runs the server
  - Re-records real API traffic
  - Captures the new correct behavior

#### 3. Stabilize with Templatization

- Dynamic values (timestamps, UUIDs, tokens) are converted into templates
- This prevents flaky test failures

#### 4. Verify Before Committing

- New test cases are verified twice:
  - Before templatization
  - After templatization
- Only stable tests are accepted

#### 5. Enforce Coverage on New Code

- Coverage is calculated only for **changed lines**
- PRs fail if coverage drops below the threshold

This allows Keploy to:

- Keep tests always in sync with code
- Eliminate manual snapshot updates
- Catch real regressions early

---

## Where Keploy Uses Keploy

We use Keploy **directly inside our CI pipeline**.

---

### CI Environment

- Platform: **GitHub Actions**
- Trigger: Pull Requests to `main`
- Runtime:
  - Linux self-hosted runner
  - MongoDB service via Docker
- Application under test:
  - `api-server` (built from the PR code)

---

### How It Runs in CI

1. CI builds the API server from the PR  
2. MongoDB starts in a clean state as a Docker service  
3. Keploy replays existing API test cases  
4. On failure:
   - MongoDB state is reset
   - APIs are re-recorded
   - Tests are stabilized and verified  
5. Updated test cases are committed back to the PR  
6. Coverage is enforced before merge  

This ensures **every PR**:

- Is validated against real API behavior
- Has stable, up-to-date tests
- Meets coverage standards

---

### One-line Summary

> Keploy tests Keploy by replaying real API traffic in CI, automatically re-recording changes, stabilizing test cases, and enforcing coverage — all without manual test maintenance.

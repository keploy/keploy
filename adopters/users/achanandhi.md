# How Keploy Uses Keploy for E2E Testing

## Overview

End-to-end (E2E) testing ensures that the API server works correctly as a **complete system**, not just as isolated units.

In Keploy, this pipeline validates that every Pull Request:

- Does not break existing APIs
- Works correctly with real database state
- Preserves expected request–response behavior

For this, **Keploy uses Keploy itself** to run automated E2E API test suites in CI.

This workflow runs on **every PR** and acts as a strict regression gate.

---

## What Keploy Tests with Keploy

In this E2E pipeline, Keploy tests the **running API server** as a black box.

Specifically, it validates:

- Public and internal REST APIs
- Complete request → database → response flows
- API behavior against realistic MongoDB data
- Backward compatibility of existing API contracts

The API server is:

- Built from the PR code
- Started as a real running process
- Connected to a MongoDB instance populated from a known dump

This ensures tests reflect **real-world usage**, not mocked behavior.

---

## Why Keploy Uses Keploy

### The Actual Problem

1. **Unit tests are not enough**
   - APIs may pass unit tests but fail with real data
   - Integration issues often appear only at runtime

2. **Manual E2E testing doesn’t scale**
   - Running Postman collections manually is slow
   - Coverage is inconsistent and error-prone

3. **API regressions are easy to miss**
   - Small changes can silently break critical flows
   - Issues often surface only after deployment

---

### How Keploy Solves This

Keploy enables **automated E2E API testing** by replaying real API flows.

In this pipeline:

- API test suites are **predefined and managed centrally**
- Tests are executed automatically against a live server
- Real HTTP requests are sent to the API
- Responses are validated against expected behavior
- Any mismatch immediately fails the PR

Keploy runs strictly in **test mode** here — no recording or re-recording — making this pipeline a **pure validation gate**.

---

## Where We Use Keploy

We use Keploy **inside GitHub Actions** as part of our Pull Request checks.

---

### CI Execution Flow

1. GitHub Actions starts a Linux runner  
2. MongoDB is launched and populated from a known dump  
3. The API server is started from PR code  
4. Keploy fetches E2E test suites from the cloud  
5. Test suites are executed against the live API  
6. The pipeline fails immediately if any test fails  

This guarantees that **every PR is validated against real API behavior before merge**.

---

### One-line Summary

> Keploy uses Keploy to run automated end-to-end API test suites in CI, ensuring every Pull Request preserves real API behavior and prevents regressions.

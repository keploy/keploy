# Keploy Mock Record & Replay

A comprehensive guide for using Keploy's mock recording and replay functionality for Go integration tests.

## Overview

Keploy provides powerful commands to record and replay real environment mocks for your e2e/integration tests:

- **`keploy sandbox record`** - Records external service calls during test execution
- **`keploy sandbox test`** - Replays tests using recorded mocks (no external service calls needed)

This significantly reduces test execution time and eliminates dependencies on external services during testing.

---

## Quick Start

### Recording Mocks

```bash
keploy sandbox record -c "go test -v" --location="./sandboxes" --name="main_test"
```

### Replaying Tests with Mocks

```bash
keploy sandbox test -c "go test -v" --location="./sandboxes" --name="main_test" --local
```

---

## Local Setup

### Execution Mode

For Go projects, all tests run **synchronously**:
- Packages are tested one by one
- Test files within packages run sequentially
- Individual tests execute in order

This ensures proper mock segregation for different tests.

### Mock Segregation

To properly segregate mocks for different tests, **checkpoint** API calls to the Keploy agent are required.

> **Note:** These modifications can be automated using the Keploy MCP server with AI assistance. If not configured, all mocks will be saved under a single file (the first checkpoint provided in the `keploy sandbox record` command).

**AI-assisted refactoring will:**
- Consolidate all `init()` functions in each package into a single `init()` function with the Keploy checkpoint API call at the beginning (since Go doesn't guarantee init function execution order, this ensures the checkpoint is always called first)
- Add checkpoint API calls at the start of each test so Keploy can save mocks separately per test

**Checkpoint API Endpoint:**
```
POST /sandbox/checkpoint
{
  "location": "./pkg/api",
  "name": "test_service"
}
```


### Mock Storage Structure

After running `keploy sandbox record -c "go test -v"`, mocks are saved locally with the `.sb.yaml` extension. A `.gitignore` entry is automatically created/updated to exclude these files from version control.

**Storage behavior:**
- Mocks are saved according to the checkpoint provided in the command
- If no checkpoint is specified, all mocks are saved under the initial checkpoint file

**File Extension:** `.sb.yaml` is used to differentiate sandbox mocks from other Keploy-generated test mock files.

---

## Cloud Integration

### Configuration

Local mocks (.sb.yaml) are automatically added to `.gitignore` during record mode.

**Test Commands:**

| Command | Description |
|---------|-------------|
| `keploy sandbox test ... --local` | Uses existing local mocks for testing |
| `keploy sandbox test ... --tag "v1.1.0"` | Creates a new tag, uploads mocks to registry, then runs tests |
| `keploy sandbox test ... --cloud` | Fetches and uses existing mocks from registry (requires config reference) |

**Registry Reference Configuration:**

The reference is stored in the root `keploy.yaml` config file under the `sandbox` section:

```yaml
sandbox:
  ref: allen/order-svc:v3.3
```

**Reference Format:** `<company>/<service>:<tag>`
- `allen` - Company name
- `order-svc` - Service name (root directory app name)
- `v3.3` - Tag name

### Tag Permissions

| User Type | Can Create New Tags | Can Update Existing Tags |
|-----------|---------------------|--------------------------|
| Non-CI    | ✅ Yes              | ❌ No                    |
| CI        | ✅ Yes              | ✅ Yes                   |

---

## CI/CD Pipeline Integration

### Adding New E2E Tests

Since mocks are not committed to the repository, users have two options when adding new e2e tests:

#### Option 1: Merge Failing PR
1. Mocks are missing/outdated, so tests fail
2. PR is merged (with known failure reason)
3. New mocks are generated in main branch

#### Option 2: Create Local Tag First
1. Create a local tag and upload mocks to registry
2. Ensure tests pass before creating tag (to avoid unnecessary tag proliferation)
3. Submit PR with passing tests
4. On merge to main, tag is incremented (and overridden if necessary)

### Pipeline Workflows

#### On Pull Request

Runs: `keploy sandbox test -c "go test -v" --cloud`

```
┌─────────────────────────────────────────────────────┐
│  1. Fetch mocks from tag specified in config.yaml  │
│  2. Run tests using fetched mocks                  │
│  3. Report test results                            │
└─────────────────────────────────────────────────────┘
```

#### On Push to Main

Runs: `keploy sandbox record` → `keploy sandbox test`

```
┌─────────────────────────────────────────────────────┐
│  1. Wait for other pipelines to complete           │
│  2. Run e2e tests against actual environment       │
│  3. Record latest mocks                            │
│  4. Replay with incremented tag (v1.0.0 → v1.0.1)  │
│  5. Update/generate mocks for new tag              │
│  6. Commit version bump                            │
└─────────────────────────────────────────────────────┘
```

---

## Important Notes

### Version Bumping
- Tag increments automatically (e.g., `v1.0.0` → `v1.0.1`)
- Bumping logic can be customized as needed

### Mock References
- Local mocks are **not** committed to the repository
- Only tag references (pointing to cloud mocks) are stored in config

### Known Caveat
If following Option 1 (merging failing PRs), the generated mocks in main may not make tests pass initially. Monitor and address these cases accordingly.

---

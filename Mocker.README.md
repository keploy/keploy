# Keploy Sandbox Record & Replay

A comprehensive guide for using Keploy's sandbox recording and replay functionality for Go integration tests.

## Overview

Keploy provides powerful commands to record real environment interactions and replay them during testing, eliminating the need for external service calls and significantly reducing test execution time.

- **`keploy sandbox record`** - Records external service calls during test execution
- **`keploy sandbox replay`** - Replays tests using recorded sandbox (no external service calls needed)

---

## Quick Start

### Recording Sandboxes

```bash
keploy sandbox record -c "go test -v" --tag "v1.0.1" --location="./sandboxes" --name="main_test"
```

> **Note:** The `--tag` flag is required.

### Replaying Tests with Sandboxes

```bash
keploy sandbox replay -c "go test -v" --location="./sandboxes" --name="main_test"
```

---

## Local Setup

### Execution Mode

For Go projects, all tests run **synchronously**:
- Packages are tested one by one
- Test files within packages run sequentially
- Individual tests execute in order

This ensures proper sandbox segregation for different tests.

### Sandbox Segregation

To properly segregate sandbox for different tests, **scope** API calls to the Keploy agent are required.

> **Note:** These modifications can be automated using the Keploy MCP server with AI assistance. If not configured, all sandbox will be saved under a single file (the first scope provided in the `keploy sandbox record` command).

**AI-assisted refactoring will:**
- Consolidate all `init()` functions in each package into a single `init()` function with the Keploy scope API call at the beginning (since Go doesn't guarantee init function execution order, this ensures the scope is always set first)
- Add scope API calls at the start of each test so Keploy can save sandbox separately per test

**Scope API Endpoint:**
```
POST /sandbox/scope
{
  "location": "./pkg/api",
  "name": "test_service"
}
```


### Sandbox Storage Structure

After running `keploy sandbox record -c "go test -v" --tag v1.0.1`, sandboxes are saved locally with the `.sb.yaml` extension. A `.gitignore` entry is automatically created/updated to exclude these files from version control.

**Storage behavior:**
- Sandboxes are saved according to the scope provided in the command
- If no scope is specified, all sandboxes are saved under the initial scope file
- Since initial scope is not required, if none is provided, all sandboxes will be saved under a single file named `keploy.sb.yaml` in the current directory

**File Extension:** `.sb.yaml` is used to differentiate sandbox mocks from other Keploy-generated test mock files.

---

## Cloud Integration

### Configuration

Local sandboxes (.sb.yaml) are automatically added to `.gitignore` during record mode.

**Test Commands:**

| Command | Description |
|---------|-------------|
| `keploy sandbox replay --local` | Uses existing local sandboxes for testing |
| `keploy sandbox replay` | Smart sync with registry (see below) |

**`keploy sandbox replay` Behavior:**

1. **Checks for reference tag** in config file (fails if not present)
2. **Verifies tag exists** in registry:
   - **If tag doesn't exist:** After successful replay, uploads sandbox to registry
   - **If tag exists:**
     - *No local sandbox:* Fetches all sandbox files from registry, saves them according to their paths, then runs tests
     - *Local sandbox exists:* Compares file hashes — if hash matches, uses local; if different, fetches from registry and overrides local, then runs tests

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

> **About Tag Updates:** Non-CI users cannot update existing tags in the registry. If a sandbox file already exists and needs updating, they must create a new tag and upload to that tag instead. CI users can update existing tags, which is necessary for CI/CD pipelines to refresh sandboxes when test cases or code changes.

---

## CI/CD Pipeline Integration

### Adding New E2E Tests

Since sandboxes are not committed to the repository, users have two options when adding new e2e tests:

#### Option 1: Merge Failing PR
1. Sandboxes are missing/outdated, so tests fail
2. PR is merged (with known failure reason)
3. New sandboxes are generated in main branch

#### Option 2: Create Local Tag First
1. Create a local tag and upload sandboxes to registry
2. Ensure tests pass before creating tag (to avoid unnecessary tag proliferation)
3. Submit PR with passing tests
4. On merge to main, tag is incremented (and overridden if necessary)

### Pipeline Workflows

#### On Pull Request

Runs: `keploy sandbox replay -c "go test -v" --cloud`

```
┌───────────────────────────────────────────────────────┐
│  1. Fetch sandboxes from tag specified in config.yaml │
│  2. Run tests using fetched sandboxes                 │
│  3. Report test results                               │
└───────────────────────────────────────────────────────┘
```

#### On Push to Main

Runs: `keploy sandbox record` → `keploy sandbox replay`

```
┌───────────────────────────────────────────────────────────┐
│  1. Wait for other pipelines to complete                  │
│  2. Run e2e tests against actual environment              │
│  3. Record latest sandboxes                               │
│  4. Replay with incremented tag (v1.0.0 → v1.0.1)         │
│  5. Update/generate sandboxes for new tag                 │
│  6. Commit version bump                                   │
└───────────────────────────────────────────────────────────┘
```

---

## Important Notes

### Version Bumping
- Tag increments automatically (e.g., `v1.0.0` → `v1.0.1`)
- Bumping logic can be customized as needed

### Sandbox References
- Local sandboxes are **not** committed to the repository
- Only tag references (pointing to cloud sandboxes) are stored in config

### Known Caveat
If following Option 1 (merging failing PRs), the generated sandboxes in main may not make tests pass initially. Monitor and address these cases accordingly.

---

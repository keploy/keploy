# Keploy Mock Record & Replay

A comprehensive guide for using Keploy's mock recording and replay functionality for Go integration tests.

## Overview

Keploy provides powerful commands to record and replay mocks for your e2e/integration tests:

- **`keploy mock record`** - Records external service calls during test execution
- **`keploy mock test`** - Replays tests using recorded mocks (no external service calls needed)

This significantly reduces test execution time and eliminates dependencies on external services during testing.

---

## Quick Start

### Recording Mocks

```bash
keploy mock record -c "go test -v"
```

### Replaying Tests with Mocks

```bash
keploy mock test -c "go test -v"
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

To properly segregate mocks for different tests, two API calls to the Keploy agent are required:

1. **In `init()`** - Tells Keploy which package to save mocks for
2. **At the start of each test** - Associates the mock with the specific test name

> **Note:** These modifications can be automated using the Keploy MCP server with AI assistance.

### Mock Storage Structure

After running `keploy mock record -c "go test -v"`, mocks are saved locally:

```
kmocks/
├── TestIntegrationCreateTodo.yaml      # Mocks for CreateTodo test
├── TestIntegrationGetAllTodos.yaml     # Mocks for GetAllTodos test
├── TestIntegrationGetTodoByID.yaml     # Mocks for GetTodoByID test
└── mocks.yaml                          # Initial mocks for all init functions
```

Each package with tests will have its own `kmocks` folder.

---

## Cloud Integration

### Configuration

When running with a tag, mocks are uploaded to the cloud registry. Local mocks are automatically added to `.gitignore`.

**config.yaml:**

```yaml
metadata:
  name: "integration-suite-mocks"
  labels:
    env: staging
    runner: playwright

mockRegistry:
  tag: ["v1.0.0", "staging"]
  app: page-service
  kind: mock-set
```

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
2. Ensure tests pass before creating tag (to avoid un-necessay tag spread)
3. Submit PR with passing tests
4. On merge to main, tag is incremented (and overridden if necessary)

### Pipeline Workflows

#### On Pull Request

Runs: `keploy mock test`

```
┌─────────────────────────────────────────────────────┐
│  1. Fetch mocks from tag specified in config.yaml  │
│  2. Run tests using fetched mocks                  │
│  3. Report test results                            │
└─────────────────────────────────────────────────────┘
```

#### On Push to Main

Runs: `keploy mock record` → `keploy mock test`

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

## Example Workflow

```bash
# 1. Record mocks locally during development
keploy mock record -c "go test -v ./..."

# 2. Verify tests pass with recorded mocks
keploy mock test -c "go test -v ./..."

# 3. Run tests in CI using cloud mocks
keploy mock test -c "go test -v ./..." --tag v1.0.0
```

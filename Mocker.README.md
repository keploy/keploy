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

Local sandboxes (`.sb.yaml`) are automatically added to `.gitignore` during record mode.

Cloud smart-sync is driven by the **registry reference** (`sandbox.ref`) configured in `keploy.yaml` / `keploy.yml`.

> If you want **purely local replay** (no registry lookup / downloads), use `--local`.

### Test Commands

| Command | Description |
|---------|-------------|
| `keploy sandbox replay --local` | Uses existing local sandboxes for testing (no cloud sync) |
| `keploy sandbox replay` | Smart sync with registry (see below; requires `sandbox.ref`) |

### `keploy sandbox replay` Behavior (summary)

`keploy sandbox record` is local capture only; registry creation for missing refs happens in replay after a successful local run.

1. **Checks for `sandbox.ref`** in config file (fails if not present)
2. **Verifies ref exists** in registry:
- **If ref doesn't exist in manifest:** after successful replay, uploads sandbox to registry
- **If ref exists in manifest:**
  - *No local sandbox:* fetches sandbox files from registry, saves them according to their paths, then runs tests
  - *Local sandbox exists:* compares file hashes — if hash matches, uses local; if different, fetches from registry and overrides local, then runs tests


Keploy uses:

* MongoDB to store a **tiny manifest** (fast lookup + verification data)
* Azure Blob Storage to store the **heavy artifact zip** (MBs)

### What is stored where?

**MongoDB (Manifest)**

* Stored **only in MongoDB**
* **Not persisted on disk**
* Contains:

  * `ref` metadata (company, service/app, tag)
  * List of files with their relative paths + content hashes (fingerprints)
  * Any other minimal metadata needed to verify local state quickly

**Azure Blob Storage (artifact.zip)**

* Stores the zipped sandbox files (the “heavy” payload)
* Downloaded only when local files are missing or dirty

---

## `keploy sandbox replay` Smart Sync Behavior

### Step 1 — Start replay

You run:

```bash
keploy sandbox replay
```

Keploy interprets this as:
“I want to run tests using the sandbox reference in my config.”

**First action:** Keploy reads `sandbox.ref` from `keploy.yaml` / `keploy.yml`.

* If missing → **replay fails**
* If present → continue

---

### Step 2 — Ask MongoDB for the manifest of `sandbox.ref`

Keploy queries MongoDB:

“Do you have a manifest for `sandbox.ref`?”

Two paths:

---

## Path A — Upload Flow (ref does not exist in mongodb)

This means: **this ref is new** and must be created.

1. **Run tests locally**

   * Keploy treats your current local sandbox files as the “source of truth.”

2. **Did tests pass?**

   * **No** → stop (don’t upload broken mocks)
   * **Yes** → proceed

3. **Compute hashes**

   * Keploy hashes each sandbox file content.
   * Any small change → hash changes.

4. **Pack the box (zip)**

   * Keploy zips sandbox files into `artifact.zip`
   * **Maintains directory structure**
   * **Does NOT include test source files** (`*_test.go`), only sandbox mocks

Example (included vs excluded):

```
pkg1/
  main_test.go          (exclude)
  main_test1.yaml       (include)
  main_test2.yaml       (include)

pkg2/folder1/folder2/
  api_test.go           (exclude)
  api_test1.yaml        (include)
  api_test2.yaml        (include)
```

5. **Upload artifact.zip to Azure Blob Storage**

   * This is the heavy upload step.

6. **Save manifest to MongoDB**

   * Store manifest + company name + app/service name + tag
   * ✅ **Crucial order:** MongoDB is updated **only after** Azure upload succeeds.

---

## Path B — Sync Flow (ref exists in cloud)

MongoDB says:
“Yes — here is the manifest for `sandbox.ref`.”

### Step 1 — Download the manifest (tiny)

Keploy downloads just the manifest (KBs), which is fast.

### Step 2 — Verify local files first (no zip download yet)

For each file listed in the manifest:

* Keploy checks if the file exists locally at that path.
* If it exists, Keploy hashes it and compares with the manifest hash.

Three scenarios:

**Scenario 1: Match**

* File exists
* Local hash == manifest hash
  ✅ Keep local file. No download needed.

**Scenario 2: Mismatch (dirty)**

* File exists
* Local hash != manifest hash
  ⚠️ Mark for download.

**Scenario 3: Missing**

* File does not exist
  ⚠️ Mark for download.

### Step 3 — Verdict

✅ If **all** files match:

* Keploy runs tests immediately (fast path, no blob download)

⚠️ If **any** file is missing/dirty:

1. Download `artifact.zip` from Azure Blob Storage
2. Unzip it **preserving directory structure**
3. Overwrite local sandbox files so they match cloud exactly
4. Run tests

---

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

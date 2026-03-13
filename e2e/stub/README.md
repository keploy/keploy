# Keploy Stub E2E Tests

This directory contains end-to-end tests for the Keploy `stub` command functionality, which allows recording and replaying HTTP mocks while running external test frameworks.

## Directory Structure

```
e2e/stub/
├── README.md              # This file
├── fixtures/              # Shared test fixtures
│   ├── mock-server/       # Go HTTP server (external dependency)
│   └── test-client/       # Go test client
├── go/                    # Go E2E tests
│   └── stub_test.go
├── playwright/            # Playwright E2E tests
│   ├── package.json
│   ├── playwright.config.ts
│   ├── server.js
│   └── tests/
│       └── api.spec.ts
└── pytest/                # Pytest E2E tests
    ├── pyproject.toml
    ├── requirements.txt
    ├── server.py
    └── tests/
        ├── conftest.py
        └── test_api.py
```

## Prerequisites

- Go 1.21+
- Node.js 20+
- Python 3.9+
- Keploy binary (built from source)

## Quick Start

### Build Keploy

```bash
cd /path/to/keploy
go build -o keploy .
```

### Running Tests Manually

#### 1. Go E2E Tests

```bash
# Build fixtures
cd e2e/stub/fixtures/mock-server && go build -o mock-server .
cd e2e/stub/fixtures/test-client && go build -o test-client .

# Start mock server
./e2e/stub/fixtures/mock-server/mock-server &

# Run tests
cd e2e/stub/go
KEPLOY_E2E_TEST=true go test -v ./...
```

#### 2. Playwright E2E Tests

```bash
cd e2e/stub/playwright

# Install dependencies
npm install
npx playwright install

# Start server
node server.js &

# Run tests without Keploy (baseline)
npx playwright test

# Record mocks with Keploy
keploy stub record -c "npx playwright test" -p ./stubs --name my-test

# Replay mocks with Keploy (can run without server)
keploy stub replay -c "npx playwright test" -p ./stubs --name my-test
```

#### 3. Pytest E2E Tests

```bash
cd e2e/stub/pytest

# Install dependencies
pip install -r requirements.txt

# Start server
python server.py &

# Run tests without Keploy (baseline)
pytest tests/ -v

# Record mocks with Keploy
keploy stub record -c "pytest tests/" -p ./stubs --name my-test

# Replay mocks with Keploy (can run without server)
keploy stub replay -c "pytest tests/" -p ./stubs --name my-test
```

## Usage Examples

### Recording Mocks

```bash
# Record all HTTP calls made during Playwright tests
keploy stub record \
  -c "npx playwright test" \
  -p ./stubs \
  --name playwright-api-tests \
  --record-timer 60s

# Record with specific ports
keploy stub record \
  -c "pytest tests/" \
  -p ./stubs \
  --name pytest-tests \
  --proxy-port 16789 \
  --dns-port 26789
```

### Replaying Mocks

```bash
# Replay mocks (latest recorded set)
keploy stub replay \
  -c "npx playwright test" \
  -p ./stubs

# Replay specific mock set
keploy stub replay \
  -c "pytest tests/" \
  -p ./stubs \
  --name pytest-tests

# Replay with fallback to real services if mock not found
keploy stub replay \
  -c "npm test" \
  -p ./stubs \
  --fallback-on-miss
```

## GitHub Actions

The E2E tests are automatically run in CI via `.github/workflows/stub_e2e.yml`. The workflow:

1. Builds the Keploy binary
2. Runs Go E2E tests
3. Runs Playwright E2E tests (with record/replay cycle)
4. Runs Pytest E2E tests (with record/replay cycle)
5. Runs integration tests

### Triggering CI

The workflow runs on:
- Push to `main` branch (when stub-related files change)
- Pull requests (when stub-related files change)
- Manual trigger via `workflow_dispatch`

## Test Coverage

### Go Tests (`e2e/stub/go/stub_test.go`)
- `TestStubRecordReplay`: Complete record and replay cycle
- `TestStubRecordAutoName`: Auto-generated stub names
- `TestStubReplayFallbackOnMiss`: Fallback functionality

### Playwright Tests (`e2e/stub/playwright/tests/api.spec.ts`)
- Health check endpoint
- Users CRUD operations
- Products API
- Echo endpoint (request/response verification)
- Search API with query parameters
- Time API
- Sequential workflows

### Pytest Tests (`e2e/stub/pytest/tests/test_api.py`)
- Health check endpoint
- Users CRUD operations
- Products API
- Echo endpoint with various HTTP methods
- Search API
- Time API
- Orders API
- Complete workflows
- Edge cases (empty queries, special characters, large payloads)

## Mock Server Endpoints

All mock servers (Go, Node.js, Python) provide the same API:

| Endpoint | Methods | Description |
|----------|---------|-------------|
| `/health` | GET | Health check |
| `/api/users` | GET, POST | List/create users |
| `/api/users/:id` | GET | Get user by ID |
| `/api/products` | GET | List products |
| `/api/products/:id` | GET | Get product by ID |
| `/api/echo` | ALL | Echo request details |
| `/api/time` | GET | Current timestamp |
| `/api/search` | GET | Search users/products |
| `/api/orders` | GET, POST | List/create orders (Pytest only) |

## Troubleshooting

### Mock server not starting
```bash
# Check if port is in use
lsof -i :9000
# Kill existing process
pkill -f mock-server
```

### Keploy proxy port conflict
```bash
# Use different ports
keploy stub record -c "npm test" --proxy-port 16790 --dns-port 26790
```

### Tests timeout during recording
```bash
# Increase record timer
keploy stub record -c "pytest tests/" --record-timer 120s
```

### No mocks recorded
- Ensure the mock server is running and accessible
- Check that tests are actually making HTTP requests
- Verify proxy port is not blocked by firewall

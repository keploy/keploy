# Mock Replay Service

This package provides the core functionality for replaying recorded mocks during application testing.

## Overview

The `mockreplay` service loads recorded mocks and intercepts outgoing calls during test execution, returning recorded responses instead of making real external calls. This enables isolated, deterministic testing without external dependencies.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                         mockreplay Service                          │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  ┌─────────────┐    ┌──────────────┐    ┌────────────────────────┐  │
│  │  Service    │    │  replayer    │    │ mockdb.GetMocks()      │  │
│  │ (interface) │───>│(implementation)──>│  (YAML parser)         │  │
│  └─────────────┘    └──────────────┘    └────────────────────────┘  │
│         │                  │                                       │
│         │                  │                                       │
│         ▼                  ▼                                       │
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │                     replay.Runtime                           │  │
│  │        (shared config + instrumentation)                    │  │
│  └─────────────────────────────────────────────────────────────┘  │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

## Files

| File | Purpose |
|------|---------|
| `service.go` | Interface definitions (`Service`, `Runtime`) |
| `replay.go` | Service wrapper and entrypoint |
| `mock_replay.go` | Mock replay flow and helpers |

## Usage

```go
import (
    "go.keploy.io/server/v3/pkg/service/mockreplay"
    "go.keploy.io/server/v3/pkg/models"
)

// Create service
replayer := mockreplay.New(logger, cfg, replayRuntime)

// Replay mocks during testing
result, err := replayer.Replay(ctx, models.ReplayOptions{
    Command:        "go test ./...",
    MockName:       "mock-123",
    FallBackOnMiss: false,
})

// Check results
if result.Success {
    fmt.Printf("All %d mocks replayed successfully\n", result.MocksReplayed)
} else {
    fmt.Printf("Warning: %d mocks missed\n", result.MocksMissed)
}
```

## Interfaces

### Service

```go
type Service interface {
    Replay(ctx context.Context, opts models.ReplayOptions) (*models.ReplayResult, error)
}
```

### Runtime

```go
type Runtime interface {
    Logger() *zap.Logger
    Config() *config.Config
    Instrumentation() replay.Instrumentation
    MockDB() replay.MockDB
}
```

## Flow

```
┌─────────────────────────────────────────────────────────────────────┐
│                         Replay Flow                                  │
└─────────────────────────────────────────────────────────────────────┘

1. Load mocks from the mock DB by mock name
   └── Defaults to the latest mock set if name is omitted
   
2. Setup instrumentation (shared replay runtime)
   
3. Set mocks in agent for matching
   └── Agent stores mocks for request matching
   
4. Enable mock outgoing mode
   └── Agent intercepts outgoing calls
   
5. Run application command
   └── App makes outgoing calls → Agent intercepts → Returns mock response
   
6. Collect consumed mocks
   └── Track which mocks were used vs missed
   
7. Return ReplayResult
   └── {success, mocksReplayed, mocksMissed, appExitCode, output}
```

## Mock Matching

When the application makes an outgoing call during replay:

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│ Application │────>│   Agent     │────>│   Mock      │
│   Call      │     │ (eBPF)      │     │   Store     │
└─────────────┘     └──────┬──────┘     └──────┬──────┘
                           │                   │
                           │ Match request     │
                           │<──────────────────│
                           │                   │
                    ┌──────┴──────┐            │
                    │   Match?    │            │
                    └──────┬──────┘            │
                     Yes   │   No              │
                    ┌──────┴──────┐            │
                    ▼             ▼            │
              Return mock    FallBackOnMiss?   │
              response       ├─ Yes: Real call │
                            └─ No: Error      │
```

## Mock File Format

The service loads mocks from YAML files:

```yaml
version: api.keploy.io/v1beta1
kind: Http
name: mock-0
spec:
  metadata:
    host: api.stripe.com
  req:
    method: POST
    url: /v1/charges
    header:
      Content-Type: application/json
    body: '{"amount": 1000}'
  res:
    status_code: 200
    header:
      Content-Type: application/json
    body: '{"id": "ch_123", "status": "succeeded"}'
---
version: api.keploy.io/v1beta1
kind: Postgres
name: mock-1
spec:
  postgres_requests:
    - query: "SELECT * FROM users WHERE id = $1"
  postgres_responses:
    - rows: [{"id": 1, "name": "John"}]
```

## Configuration

| Option | Description | Default |
|--------|-------------|---------|
| `FallBackOnMiss` | Make real calls when no mock matches | `false` |
| `Timeout` | Maximum replay duration | `5m` |
| `cfg.BypassRules` | Rules for skipping interception | `[]` |

## Result Interpretation

| Scenario | Success | Description |
|----------|---------|-------------|
| All mocks matched | `true` | All outgoing calls found matching mocks |
| Some mocks missed | `false` | Some calls had no matching mock |
| App crashed | `false` | Application exited with non-zero code |
| Timeout | `false` | Replay exceeded timeout duration |

## Error Handling

```go
result, err := replayer.Replay(ctx, opts)
if err != nil {
    // Setup or execution error
    log.Error("Replay failed", err)
    return
}

if !result.Success {
    if result.MocksMissed > 0 {
        // Some mocks didn't match - may need to re-record
        log.Warn("Mocks missed", result.MocksMissed)
    }
    if result.AppExitCode != 0 {
        // Application crashed or test failed
        log.Warn("App exit code", result.AppExitCode)
    }
}
```

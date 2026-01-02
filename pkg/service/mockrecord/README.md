# Mock Recording Service

This package provides the core functionality for recording outgoing calls from applications.

## Overview

The `mockrecord` service captures all external dependencies (HTTP APIs, databases, message queues, etc.) while an application runs, creating mock files that can be replayed during testing.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                         mockrecord Service                          │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  ┌─────────────┐    ┌──────────────┐    ┌──────────────────────┐  │
│  │  Service    │    │  recorder    │    │  ExtractMetadata()   │  │
│  │ (interface) │───>│(implementation)──>│  (metadata.go)       │  │
│  └─────────────┘    └──────────────┘    └──────────────────────┘  │
│         │                  │                       │               │
│         │                  │                       │               │
│         ▼                  ▼                       ▼               │
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │                     AgentService                             │  │
│  │         (eBPF-based network interception)                   │  │
│  └─────────────────────────────────────────────────────────────┘  │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

## Files

| File | Purpose |
|------|---------|
| `service.go` | Interface definitions (`Service`, `AgentService`, `MockDB`) |
| `record.go` | Recording implementation |
| `metadata.go` | Metadata extraction for contextual naming |

## Usage

```go
import (
    "go.keploy.io/server/v3/pkg/service/mockrecord"
    "go.keploy.io/server/v3/pkg/models"
)

// Create service
recorder := mockrecord.New(logger, cfg, agentService, mockDB)

// Record outgoing calls
result, err := recorder.Record(ctx, models.RecordOptions{
    Command:  "go run main.go",
    Path:     "./keploy",
    Duration: 60 * time.Second,
})

// Access results
fmt.Printf("Recorded %d mocks\n", result.MockCount)
fmt.Printf("Protocols: %v\n", result.Metadata.Protocols)
fmt.Printf("File: %s\n", result.MockFilePath)
```

## Interfaces

### Service

```go
type Service interface {
    Record(ctx context.Context, opts models.RecordOptions) (*models.RecordResult, error)
}
```

### AgentService

```go
type AgentService interface {
    Setup(ctx context.Context, startCh chan int) error
    GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error)
    StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
}
```

## Metadata Extraction

The `ExtractMetadata` function analyzes recorded mocks to extract:

- **Protocols**: HTTP, gRPC, PostgreSQL, MySQL, Redis, MongoDB, Generic
- **Endpoints**: Host, path, method for each captured call
- **Service Name**: Inferred from the application command

This metadata is used for:
1. LLM-powered contextual file naming
2. Summary display to users
3. Debugging and analysis

### Protocol Detection

| Protocol | Detection Method |
|----------|------------------|
| HTTP | `mock.Kind == models.HTTP` |
| gRPC | `mock.Kind == models.GRPC_EXPORT` |
| PostgreSQL | `mock.Kind == models.Postgres` |
| MySQL | `mock.Kind == models.MySQL` |
| Redis | `mock.Kind == models.REDIS` |
| MongoDB | `mock.Kind == models.Mongo` |
| Generic | `mock.Kind == models.GENERIC` |

## Flow

```
1. Setup agent (eBPF hooks)
2. Start outgoing capture channel
3. Run application command in background
4. Collect mocks from channel (concurrent goroutine)
5. Wait for:
   - Command completion, OR
   - Duration timeout, OR
   - Context cancellation
6. Store mocks via agent
7. Extract metadata
8. Return RecordResult
```

## Configuration

Recording behavior is influenced by:

| Config | Description |
|--------|-------------|
| `cfg.Path` | Default storage path |
| `cfg.BypassRules` | Rules for skipping certain calls |
| `cfg.Test.MongoPassword` | MongoDB authentication |
| `cfg.Test.Delay` | SQL query delay simulation |

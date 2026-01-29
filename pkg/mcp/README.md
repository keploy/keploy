# Keploy MCP Server

This package implements an MCP (Model Context Protocol) server that exposes Keploy's mock recording and replay capabilities as tools for AI assistants.

## Table of Contents

- [Overview](#overview)
- [Architecture](#architecture)
  - [High-Level Design (HLD)](#high-level-design-hld)
  - [Low-Level Design (LLD)](#low-level-design-lld)
- [User Flow](#user-flow)
- [Backend Flow](#backend-flow)
- [API Reference](#api-reference)
  - [keploy_list_mocks](#tool-keploy_list_mocks)
  - [keploy_mock_record](#tool-keploy_mock_record)
  - [keploy_mock_test](#tool-keploy_mock_test)
  - [Error Handling](#error-handling)
- [Configuration](#configuration)
- [Examples](#examples)

---

## Overview

The Keploy MCP Server enables AI coding assistants (like GitHub Copilot, Claude, Cursor) to interact with Keploy's mocking capabilities through the Model Context Protocol. This allows developers to:

1. **List mocks** - View all available recorded mock sets
2. **Record mocks** - Capture outgoing calls (HTTP, databases, gRPC, etc.) while running an application
3. **Replay mocks** - Test applications in isolation by replaying recorded mocks
4. **Smart naming** - Use LLM callbacks to generate contextual names for mock files

### Key Features

- **Protocol Support**: HTTP, gRPC, PostgreSQL, MySQL, Redis, MongoDB
- **Stdio Transport**: Compatible with VS Code, Claude Desktop, and other MCP clients
- **LLM-Powered Naming**: Contextual file naming with deterministic fallback
- **Zero Configuration**: Works out of the box with default settings
- **Auto-Discovery**: Automatically uses the latest mock set if not specified

---

## Architecture

### High-Level Design (HLD)

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              AI Assistant                                    │
│                    (VS Code, Claude Desktop, Cursor)                        │
└─────────────────────────────────┬───────────────────────────────────────────┘
                                  │ MCP Protocol (JSON-RPC over stdio)
                                  ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Keploy MCP Server                                  │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  ┌────────────────┐  │
│  │ keploy_list_ │  │ keploy_mock_ │  │ keploy_mock_ │  │ LLM Callback   │  │
│  │ mocks        │  │ record       │  │ test         │  │ (CreateMessage)│  │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘  └────────────────┘  │
└─────────┼─────────────────┼─────────────────┼───────────────────────────────┘
          │                 │                 │
          ▼                 ▼                 ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                         Keploy Core Services                                 │
│  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────────────┐  │
│  │ mockrecord       │  │ mockreplay       │  │   models                 │  │
│  │ Service          │  │ Service          │  │   (shared types)         │  │
│  └────────┬─────────┘  └────────┬─────────┘  └──────────────────────────┘  │
└───────────┼─────────────────────┼───────────────────────────────────────────┘
            │                     │
            ▼                     ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Keploy Agent                                       │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │                    eBPF-based Network Interception                    │  │
│  │         (Captures/Replays outgoing calls transparently)               │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
            │                     │
            ▼                     ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                        External Dependencies                                 │
│  ┌────────┐  ┌────────┐  ┌────────┐  ┌────────┐  ┌────────┐  ┌────────┐   │
│  │  HTTP  │  │  gRPC  │  │Postgres│  │ MySQL  │  │ Redis  │  │MongoDB │   │
│  │  APIs  │  │Services│  │   DB   │  │   DB   │  │ Cache  │  │   DB   │   │
│  └────────┘  └────────┘  └────────┘  └────────┘  └────────┘  └────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Component Responsibilities

| Component | Responsibility |
|-----------|----------------|
| **MCP Server** | JSON-RPC communication, tool registration, session management |
| **keploy_list_mocks** | List available mock sets from the keploy directory |
| **mockrecord Service** | Recording logic, metadata extraction, mock file generation |
| **mockreplay Service** | Mock loading, replay orchestration, result reporting |
| **models** | Shared types: `RecordOptions`, `ReplayOptions`, `MockMetadata`, etc. |
| **Agent** | eBPF-based network interception, transparent proxy |
| **LLM Callback** | CreateMessage API for contextual naming |

---

### Low-Level Design (LLD)

#### Package Structure

```
pkg/
├── mcp/                          # MCP Server package
│   ├── server.go                 # Server lifecycle, transport setup
│   ├── tools.go                  # Tool handlers (record, replay)
│   ├── naming.go                 # LLM callback, fallback naming
│   └── README.md                 # This file
│
├── service/
│   ├── mockrecord/               # Mock recording service
│   │   ├── service.go            # Interface definitions
│   │   ├── record.go             # Recording implementation
│   │   └── metadata.go           # Metadata extraction
│   │
│   └── mockreplay/               # Mock replay service
│       ├── service.go            # Interface definitions
│       └── replay.go             # Replay implementation
│
├── models/
│   ├── mockrecord.go             # RecordOptions, RecordResult, MockMetadata
│   ├── mockreplay.go             # ReplayOptions, ReplayResult
│   └── mock.go                   # Mock, MockSpec (existing)
│
cli/
└── mcp.go                        # CLI entry point (keploy mcp serve)
```

#### Class Diagram

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                                  pkg/mcp                                     │
├─────────────────────────────────────────────────────────────────────────────┤
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │ Server                                                               │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │ - server: *sdkmcp.Server                                            │   │
│  │ - mockRecorder: mockrecord.Service                                  │   │
│  │ - mockReplayer: mockreplay.Service                                  │   │
│  │ - logger: *zap.Logger                                               │   │
│  │ - activeSession: *sdkmcp.ServerSession                              │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │ + NewServer(opts *ServerOptions) *Server                            │   │
│  │ + Run(ctx context.Context) error                                    │   │
│  │ - registerTools()                                                    │   │
│  │ - handleMockRecord(ctx, req, input) (*Result, Output, error)        │   │
│  │ - handleMockReplay(ctx, req, input) (*Result, Output, error)        │   │
│  │ - generateContextualName(ctx, meta) (string, error)                 │   │
│  │ - fallbackName(meta) string                                         │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│                            pkg/service/mockrecord                            │
├─────────────────────────────────────────────────────────────────────────────┤
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │ <<interface>> Service                                                │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │ + Record(ctx, opts RecordOptions) (*RecordResult, error)            │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                    △                                        │
│                                    │ implements                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │ recorder                                                             │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │ - logger: *zap.Logger                                               │   │
│  │ - cfg: *config.Config                                               │   │
│  │ - runner: RecordRunner                                             │   │
│  │ - mockDB: MockDB                                                    │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │ + Record(ctx, opts) (*RecordResult, error)                          │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │ ExtractMetadata(mocks []*Mock, command string) *MockMetadata        │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│                            pkg/service/mockreplay                            │
├─────────────────────────────────────────────────────────────────────────────┤
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │ <<interface>> Service                                                │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │ + Replay(ctx, opts ReplayOptions) (*ReplayResult, error)            │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                    △                                        │
│                                    │ implements                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │ replayer                                                             │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │ - logger: *zap.Logger                                               │   │
│  │ - cfg: *config.Config                                               │   │
│  │ - runtime: Runtime                                                │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │ + Replay(ctx, opts) (*ReplayResult, error)                          │   │
│  │ - mockReplay(ctx, opts) (*ReplayResult, error)                      │   │
│  │ - prepareMockReplayConfig(opts) (string, string, func(), error)     │   │
│  │ - runMockReplay(ctx, cmd, type, mocks, opts) (*ReplayResult, error) │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│                               pkg/models                                     │
├─────────────────────────────────────────────────────────────────────────────┤
│  RecordOptions {Command, Path, ProxyPort, DNSPort}                        │
│  RecordResult {MockFilePath, Metadata, MockCount, Mocks}                   │
│  MockMetadata {Protocols, Endpoints, ServiceName, Timestamp}               │
│  EndpointInfo {Protocol, Host, Path, Method}                               │
│  ReplayOptions {Command, MockFilePath, ProxyPort, DNSPort, Timeout, ...}   │
│  ReplayResult {Success, MocksReplayed, MocksMissed, AppExitCode, ...}      │
└─────────────────────────────────────────────────────────────────────────────┘
```

#### Sequence Diagram: Mock Recording

```
┌─────────┐     ┌─────────┐     ┌─────────┐     ┌─────────┐     ┌─────────┐     ┌─────────┐
│   AI    │     │   MCP   │     │ mock    │     │  Agent  │     │  Your   │     │External │
│Assistant│     │ Server  │     │ record  │     │(eBPF)   │     │  App    │     │Services │
└────┬────┘     └────┬────┘     └────┬────┘     └────┬────┘     └────┬────┘     └────┬────┘
     │               │               │               │               │               │
     │ CallTool      │               │               │               │               │
     │(keploy_mock_  │               │               │               │               │
     │ record)       │               │               │               │               │
     │──────────────>│               │               │               │               │
     │               │               │               │               │               │
     │               │ Record(opts)  │               │               │               │
     │               │──────────────>│               │               │               │
     │               │               │               │               │               │
     │               │               │ Setup()       │               │               │
     │               │               │──────────────>│               │               │
     │               │               │               │               │               │
     │               │               │ GetOutgoing() │               │               │
     │               │               │──────────────>│               │               │
     │               │               │               │               │               │
     │               │               │ runCommand()  │               │               │
     │               │               │─────────────────────────────>│               │
     │               │               │               │               │               │
     │               │               │               │               │ HTTP/DB calls │
     │               │               │               │               │──────────────>│
     │               │               │               │               │               │
     │               │               │               │ Intercept &   │<──────────────│
     │               │               │               │ Record        │               │
     │               │               │               │<─ ─ ─ ─ ─ ─ ─ │               │
     │               │               │               │               │               │
     │               │               │ Mock stream   │               │               │
     │               │               │<──────────────│               │               │
     │               │               │               │               │               │
     │               │               │ ExtractMetadata()             │               │
     │               │               │───────┐       │               │               │
     │               │               │       │       │               │               │
     │               │               │<──────┘       │               │               │
     │               │               │               │               │               │
     │               │ RecordResult  │               │               │               │
     │               │<──────────────│               │               │               │
     │               │               │               │               │               │
     │               │ generateContextualName()      │               │               │
     │               │───────┐       │               │               │               │
     │               │       │ (LLM  │               │               │               │
     │               │       │Callback)              │               │               │
     │               │<──────┘       │               │               │               │
     │               │               │               │               │               │
     │ CallToolResult│               │               │               │               │
     │ {mockFilePath,│               │               │               │               │
     │  protocols,   │               │               │               │               │
     │  mockCount}   │               │               │               │               │
     │<──────────────│               │               │               │               │
     │               │               │               │               │               │
```

#### Sequence Diagram: Mock Replay

```
┌─────────┐     ┌─────────┐     ┌─────────┐     ┌─────────┐     ┌─────────┐
│   AI    │     │   MCP   │     │ mock    │     │  Agent  │     │  Your   │
│Assistant│     │ Server  │     │ replay  │     │(eBPF)   │     │  App    │
└────┬────┘     └────┬────┘     └────┬────┘     └────┬────┘     └────┬────┘
     │               │               │               │               │
     │ CallTool      │               │               │               │
     │(keploy_mock_  │               │               │               │
     │ test)         │               │               │               │
     │──────────────>│               │               │               │
     │               │               │               │               │
     │               │ Replay(opts)  │               │               │
     │               │──────────────>│               │               │
     │               │               │               │               │
     │               │               │loadMocksFromPath()            │
     │               │               │───────┐       │               │
     │               │               │       │       │               │
     │               │               │<──────┘       │               │
     │               │               │               │               │
     │               │               │ Setup()       │               │
     │               │               │──────────────>│               │
     │               │               │               │               │
     │               │               │ SetMocks()    │               │
     │               │               │──────────────>│               │
     │               │               │               │               │
     │               │               │ MockOutgoing()│               │
     │               │               │──────────────>│               │
     │               │               │               │               │
     │               │               │     Run()     │               │
     │               │               │─────────────────────────────>│
     │               │               │               │               │
     │               │               │               │ App makes     │
     │               │               │               │ outgoing call │
     │               │               │               │<──────────────│
     │               │               │               │               │
     │               │               │               │ Return mock   │
     │               │               │               │ response      │
     │               │               │               │──────────────>│
     │               │               │               │               │
     │               │               │GetConsumedMocks()             │
     │               │               │──────────────>│               │
     │               │               │               │               │
     │               │ ReplayResult  │               │               │
     │               │<──────────────│               │               │
     │               │               │               │               │
     │ CallToolResult│               │               │               │
     │ {success,     │               │               │               │
     │  mocksReplayed│               │               │               │
     │  mocksMissed} │               │               │               │
     │<──────────────│               │               │               │
     │               │               │               │               │
```

---

## User Flow

### 1. Setup (One-time)

Configure your AI assistant to use the Keploy MCP server:

**Claude Desktop** (`~/.config/claude/claude_desktop_config.json`):
```json
{
  "mcpServers": {
    "keploy": {
      "command": "keploy",
      "args": ["mcp", "serve"]
    }
  }
}
```

**VS Code** (`.vscode/settings.json` or User Settings):
```json
{
  "mcp.servers": {
    "keploy": {
      "command": "keploy",
      "args": ["mcp", "serve"]
    }
  }
}
```

### 2. Recording Mocks

Ask your AI assistant to record mocks:

```
User: "Record the external API calls from my Go service"

AI: I'll record the outgoing calls from your application.
    [Calls keploy_mock_record with command="go run main.go"]

AI: Successfully recorded 5 mocks:
    - 3 HTTP calls to api.stripe.com
    - 2 PostgreSQL queries
    Mock file saved to: ./keploy/user-service-stripe-postgres/mocks.yaml
```

### 3. Listing Available Mocks

Ask your AI assistant to show available mock sets:

```
User: "What mocks do I have recorded?"

AI: [Calls keploy_list_mocks]

AI: Found 2 mock sets:
    1. user-service-stripe-postgres (latest)
    2. order-api-redis
    
    You can use any of these with keploy_mock_test.
```

### 4. Replaying Mocks (Testing)

Ask your AI assistant to test with recorded mocks:

```
User: "Test my service using the recorded mocks"

AI: I'll replay the recorded mocks while running your application.
    [Calls keploy_mock_test with command="go test ./..."]
    (Automatically uses the latest mock set: user-service-stripe-postgres)

AI: Test completed successfully:
    - 5/5 mocks replayed
    - 0 mocks missed
    - Application exited with code 0
```

Or specify a particular mock set:

```
User: "Test using the order-api-redis mocks"

AI: [Calls keploy_mock_test with mockName="order-api-redis"]

AI: Test passed! Replayed 3 mock(s), app exited successfully.
```

### 5. Iterative Development Flow

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Development Workflow                          │
└─────────────────────────────────────────────────────────────────────┘

    ┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐
    │  Write   │     │  Record  │     │   Run    │     │  Commit  │
    │   Code   │────>│  Mocks   │────>│  Tests   │────>│  Code    │
    └──────────┘     └──────────┘     └──────────┘     └──────────┘
         │                                  │               │
         │                                  │               │
         │         ┌────────────────────────┘               │
         │         │ If tests fail                          │
         │         ▼                                        │
         │    ┌──────────┐                                  │
         └────│  Debug   │                                  │
              │  & Fix   │                                  │
              └──────────┘                                  │
                   │                                        │
                   │ Re-record if external APIs changed     │
                   └────────────────────────────────────────┘
```

---

## Backend Flow

### Mock Recording Flow

```
1. MCP Server receives CallTool(keploy_mock_record)
   └── Input: {command: "go run main.go", path: "./keploy"}

2. Parse and validate input

3. Create mockrecord.Service.Record()
   ├── Setup agent (eBPF hooks)
   ├── Start outgoing capture channel
   ├── Execute application command
   ├── Collect mocks from channel (goroutine)
   └── Wait for command completion or timeout

4. Extract metadata from recorded mocks
   ├── Identify protocols (HTTP, Postgres, etc.)
   ├── Extract endpoints (host, path, method)
   └── Infer service name from command

5. Generate contextual name
   ├── Try: LLM callback (CreateMessage API)
   │   └── Prompt with metadata summary
   └── Fallback: Deterministic name
       └── Pattern: {service}-{protocol}-{timestamp}

6. Rename mock file to contextual name

7. Return result to MCP client
   └── {success, mockFilePath, protocols, mockCount, message}
```

### Mock Replay Flow

```
1. MCP Server receives CallTool(keploy_mock_test)
   └── Input: {command: "go run main.go", mockFilePath: "./keploy/mocks"}

2. Parse and validate input

3. Create mockreplay.Service.Replay()
   ├── Load mocks from YAML file
   ├── Setup agent (eBPF hooks)
   ├── Set mocks in agent for matching
   ├── Enable mock outgoing mode
   └── Execute application command

4. Agent intercepts outgoing calls
   ├── Match against loaded mocks
   ├── Return recorded response if match
   └── Track consumed mocks

5. Collect results
   ├── Get consumed mocks from agent
   ├── Calculate: replayed vs missed
   └── Capture app exit code

6. Return result to MCP client
   └── {success, mocksReplayed, mocksMissed, appExitCode, message}
```

### LLM Callback Flow (Naming)

```
1. Build metadata summary
   ├── Service name (from command)
   ├── Protocols list
   └── Endpoints summary (top 5)

2. Create prompt for LLM
   └── "Generate a short, descriptive filename..."

3. Call session.CreateMessage()
   ├── MaxTokens: 50
   ├── SpeedPriority: 1.0 (fast response)
   └── SystemPrompt: "Respond with just filename..."

4. Parse response
   ├── Extract text content
   └── Sanitize filename (remove special chars)

5. On failure: Use fallback
   └── Pattern: {service}-{protocol}-{YYYYMMDDHHmmss}
```

---

## API Reference

### Tool: keploy_list_mocks

Lists all available mock sets that have been recorded.

**Input Parameters:**

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `path` | string | No | Path to search for mock files (default: `./keploy`) |

**Output:**

| Field | Type | Description |
|-------|------|-------------|
| `success` | boolean | Whether the operation succeeded |
| `mockSets` | string[] | List of available mock set names |
| `count` | integer | Number of mock sets found |
| `path` | string | Path where mocks were searched |
| `message` | string | Human-readable status message |

**Example:**
```json
// Input
{
  "path": "./keploy"
}

// Output
{
  "success": true,
  "mockSets": ["user-service-stripe-postgres", "order-api-redis"],
  "count": 2,
  "path": "./keploy",
  "message": "Found 2 mock set(s). The latest is 'user-service-stripe-postgres'."
}
```

---

### Tool: keploy_mock_record

Records outgoing calls from your application.

**Input Parameters:**

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `command` | string | **Yes** | Application command to run (e.g., `go run main.go`, `npm start`) |
| `path` | string | No | Path to store mock files (default: `./keploy`) |

**Output:**

| Field | Type | Description |
|-------|------|-------------|
| `success` | boolean | Whether recording succeeded |
| `mockFilePath` | string | Path to the generated mock file |
| `mockCount` | integer | Number of mocks recorded |
| `protocols` | string[] | List of protocols detected (HTTP, Postgres, Redis, etc.) |
| `message` | string | Human-readable status message |
| `configuration` | object | Configuration used for recording |

**Example:**
```json
// Input
{
  "command": "npm start",
  "path": "./keploy"
}

// Output
{
  "success": true,
  "mockFilePath": "./keploy/order-service-postgres-redis/mocks.yaml",
  "mockCount": 12,
  "protocols": ["HTTP", "Postgres", "Redis"],
  "message": "Successfully recorded 12 mock(s) to './keploy/order-service-postgres-redis'. Detected protocols: [HTTP Postgres Redis]",
  "configuration": {
    "command": "npm start",
    "path": "./keploy"
  }
}
```

---

### Tool: keploy_mock_test

Replays recorded mocks while running your application tests.

**Input Parameters:**

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `command` | string | **Yes** | Test command to run (e.g., `go test -v`, `npm test`) |
| `mockName` | string | No | Name of the mock set to replay. Use `keploy_list_mocks` to see available mocks. If not provided, the latest mock set will be used. |
| `fallBackOnMiss` | boolean | No | Fall back to real calls when no mock matches (default: `false`) |

**Output:**

| Field | Type | Description |
|-------|------|-------------|
| `success` | boolean | Whether replay succeeded (all mocks matched) |
| `mocksReplayed` | integer | Number of mocks successfully replayed |
| `mocksMissed` | integer | Number of unmatched calls |
| `appExitCode` | integer | Application exit code |
| `message` | string | Human-readable status message |
| `configuration` | object | Configuration used for replay |

**Example:**
```json
// Input
{
  "command": "go test ./...",
  "mockName": "user-service-stripe",
  "fallBackOnMiss": false
}

// Output
{
  "success": true,
  "mocksReplayed": 8,
  "mocksMissed": 0,
  "appExitCode": 0,
  "message": "Test passed! Replayed 8 mock(s), app exited successfully",
  "configuration": {
    "command": "go test ./...",
    "mockName": "user-service-stripe",
    "fallBackOnMiss": false
  }
}
```

---

### Error Handling

All tools return structured error responses with helpful messages:

```json
// Missing required parameter
{
  "success": false,
  "message": "Error: 'command' is required. Please provide the application command to run (e.g., 'go run main.go', 'npm start')."
}

// No mocks found
{
  "success": false,
  "message": "No mock sets found. Use keploy_mock_record to create mocks first."
}

// Service unavailable
{
  "success": false,
  "message": "Error: Mock recorder service is not available."
}
```

---

## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `KEPLOY_PATH` | Default path for mock storage | `./keploy` |
| `KEPLOY_PROXY_PORT` | Proxy port for interception | `16789` |
| `KEPLOY_DNS_PORT` | DNS port for interception | `26789` |

### Config File (keploy.yml)

```yaml
path: ./keploy
test:
  delay: 5
  mongoPassword: ""
bypassRules: []
```

---

## Examples

### Example 1: Recording a Node.js API

```
User: Record the external calls from my Express API

AI: [Calls keploy_mock_record]
    Command: npm start

AI: Recording started. Please make some API requests to your service...

AI: Recorded 15 mocks:
    - 10 HTTP calls (api.openai.com, stripe.com)
    - 5 MongoDB queries
    
    Mock file: ./keploy/express-api-openai-mongo/mocks.yaml
```

### Example 2: Listing and Selecting Mocks

```
User: What mock sets do I have?

AI: [Calls keploy_list_mocks]

AI: Found 3 mock sets:
    1. express-api-openai-mongo (latest)
    2. user-service-stripe
    3. payment-api-postgres
    
    Use keploy_mock_test with any of these names.
```

### Example 3: Testing with Mocks

```
User: Run my tests with the recorded mocks

AI: [Calls keploy_mock_test]
    Command: npm test
    (Using latest mock set: express-api-openai-mongo)

AI: Test Results:
    ✓ All 15 mocks replayed successfully
    ✓ Application tests passed (exit code 0)
    
    Your tests ran in complete isolation from external services!
```

### Example 4: Testing with Specific Mock Set

```
User: Test using the payment-api-postgres mocks

AI: [Calls keploy_mock_test with mockName="payment-api-postgres"]

AI: Test passed! Replayed 8 mock(s), app exited successfully.
```

### Example 5: Debugging Mock Misses

```
User: Test with mocks, show any misses

AI: [Calls keploy_mock_test with fallBackOnMiss: true]

AI: Test completed with warnings:
    - 13/15 mocks replayed
    - 2 mocks missed (fell back to real calls):
      1. POST /api/v1/new-endpoint (no mock found)
      2. GET /api/users?new_param=true (mock mismatch)
    
    You may need to re-record mocks for the new endpoints.
```

---

## Troubleshooting

### Common Issues

1. **"Mock recorder service is not available"**
   - Ensure Keploy agent is properly installed
   - Check if running on a supported platform (Linux with eBPF)

2. **"No mocks were recorded"**
   - Verify the application makes outgoing calls
   - Check if duration is sufficient
   - Ensure the command actually runs the application

3. **"LLM callback failed"**
   - This is expected if the AI client doesn't support sampling
   - Falls back to deterministic naming automatically

4. **"Mock mismatch during replay"**
   - Request parameters may have changed
   - Consider re-recording mocks
   - Use `fallBackOnMiss: true` to identify changed endpoints

---

## Contributing

See the main [CONTRIBUTING.md](../../CONTRIBUTING.md) for guidelines.

### Running Tests

```bash
# Build the MCP package
go build ./pkg/mcp/

# Run tests (when available)
go test ./pkg/mcp/...
```

---

## License

Apache 2.0 - See [LICENSE](../../LICENSE)

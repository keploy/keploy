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

1. **Unified Manager** - Single tool (`keploy_manager`) for all Keploy operations
2. **List mocks** - View all available recorded mock sets
3. **Record mocks** - Capture outgoing calls (HTTP, databases, gRPC, etc.) while running an application
4. **Replay mocks** - Test applications in isolation by replaying recorded mocks
5. **CI/CD Pipelines** - Generate CI/CD pipeline configurations for automated mock testing
6. **Smart naming** - Use LLM callbacks to generate contextual names for mock files

### Key Features

- **Unified Tool**: `keploy_manager` provides a single entry point for all operations
- **Stateless Design**: Each tool invocation is independent - the `action` parameter determines behavior
- **Protocol Support**: HTTP, gRPC, PostgreSQL, MySQL, Redis, MongoDB
- **Stdio Transport**: Compatible with VS Code, Claude Desktop, and other MCP clients
- **LLM-Powered Naming**: Contextual file naming with deterministic fallback
- **MCP Elicitation**: Interactive configuration gathering for pipeline generation
- **CI/CD Auto-Detection**: Automatically detects GitHub Actions, GitLab CI, Jenkins, CircleCI, Azure Pipelines, Bitbucket Pipelines
- **Zero Configuration**: Works out of the box with default settings
- **Auto-Discovery**: Automatically uses the latest mock set if not specified

### Important: Tool Behavior vs AI Intelligence

**The tools are stateless** - they don't "remember" previous actions or automatically decide what to do next:

- ✅ **AI Assistant decides**: Based on conversation context, the AI chooses which action to call
- ✅ **You control via `action` parameter**: Every call to `keploy_manager` must specify the action
- ❌ **Tool does NOT auto-detect**: The tool won't check if mocks exist and skip to testing
- ❌ **Tool does NOT auto-progress**: Record doesn't automatically lead to test

**Example:**
```
User: "Test my app"
→ AI calls keploy_list_mocks (sees mocks exist)
→ AI decides to call keploy_manager with action="keploy_mock_test"
→ Tool executes the test (because action parameter said so)
```

The intelligence comes from the AI assistant interpreting your intent, not from the tool itself.

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
│  │ keploy_      │  │ keploy_mock_ │  │ keploy_mock_ │  │ keploy_list_   │  │
│  │ manager      │  │ record       │  │ test         │  │ mocks          │  │
│  │ (unified)    │  │              │  │              │  │                │  │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘  └────────────────┘  │
│         │                 │                 │                               │
│         ▼                 │                 │          ┌────────────────┐  │
│  ┌──────────────┐         │                 │          │ LLM Callback   │  │
│  │ Pipeline     │         │                 │          │ (Elicitation & │  │
│  │ Generator    │         │                 │          │  Sampling)     │  │
│  └──────────────┘         │                 │          └────────────────┘  │
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
| **keploy_manager** | Unified tool for mock recording, testing, and CI/CD pipeline generation |
| **keploy_list_mocks** | List available mock sets from the keploy directory |
| **mockrecord Service** | Recording logic, metadata extraction, mock file generation |
| **mockreplay Service** | Mock loading, replay orchestration, result reporting |
| **Pipeline Generator** | CI/CD pipeline generation for 6 platforms |
| **models** | Shared types: `RecordOptions`, `ReplayOptions`, `MockMetadata`, etc. |
| **Agent** | eBPF-based network interception, transparent proxy |
| **LLM Callback** | CreateMessage API for contextual naming |
| **MCP Elicitation** | Interactive configuration gathering (Form Mode) |
| **MCP Sampling** | LLM-powered CI/CD detection and pipeline generation |

---

### Low-Level Design (LLD)

#### Package Structure

```
pkg/
├── mcp/                          # MCP Server package
│   ├── server.go                 # Server lifecycle, transport setup
│   ├── tools.go                  # Tool handlers (record, replay, list)
│   ├── types.go                  # Type definitions and constants
│   ├── manager.go                # Unified keploy_manager handler
│   ├── pipeline.go               # Pipeline generation with MCP Elicitation
│   ├── templates.go              # CI/CD pipeline templates (6 platforms)
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
│  │ - handleManager(ctx, req, input) (*Result, Output, error)           │   │
│  │ - handleMockRecord(ctx, req, input) (*Result, Output, error)        │   │
│  │ - handleMockReplay(ctx, req, input) (*Result, Output, error)        │   │
│  │ - handlePipeline(ctx, input) (*PipelineOutput, error)               │   │
│  │ - elicitAppCommand(ctx) (string, error)                             │   │
│  │ - elicitCICDPlatform(ctx) (string, error)                           │   │
│  │ - detectCICDPlatform(ctx) (string, error)                           │   │
│  │ - generateContextualName(ctx, meta) (string, error)                 │   │
│  │ - fallbackName(meta) string                                         │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│                              pkg/mcp/types                                   │
├─────────────────────────────────────────────────────────────────────────────┤
│  ManagerInput {Action, Command, Path, MockName, FallBackOnMiss, ...}       │
│  ManagerOutput {Success, Action, Message, RecordResult, TestResult, ...}   │
│  PipelineInput {AppCommand, DefaultBranch, MockPath, CICDTool}             │
│  PipelineOutput {Success, CICDTool, FilePath, Content, Message, ...}       │
│  PipelineConfig {AppCommand, DefaultBranch, MockPath, CICDTool}            │
│  CICDFiles {GitHubWorkflows, GitLabCI, Jenkinsfile, CircleCI, ...}         │
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
    [AI first calls keploy_list_mocks to check available mocks]
    [Then calls keploy_manager with action="keploy_mock_test", command="go test ./..."]
    (Automatically uses the latest mock set: user-service-stripe-postgres)

AI: Test completed successfully:
    - 5/5 mocks replayed
    - 0 mocks missed
    - Application exited with code 0
```

**Note:** The AI assistant may automatically check for existing mocks and choose the test action. However, the tool itself requires you to explicitly specify `action="keploy_mock_test"` - it's the AI that makes this intelligent decision based on context.

Or specify a particular mock set:

```
User: "Test using the order-api-redis mocks"

AI: [Calls keploy_manager with action="keploy_mock_test", mockName="order-api-redis"]

AI: Test passed! Replayed 3 mock(s), app exited successfully.
```

### 5. Generating CI/CD Pipelines

Ask your AI assistant to create a CI/CD pipeline:

```
User: "Create a GitHub Actions workflow for Keploy mock testing"

AI: [Calls keploy_manager with action="pipeline"]
    (Auto-detects GitHub Actions from .github/workflows/ directory)
    (Uses MCP Elicitation to ask for app command if not provided)

AI: Please provide your application command (e.g., 'go run main.go', 'npm start'):

User: "npm start"

AI: Successfully created GitHub Actions pipeline!
    - File: .github/workflows/keploy-mock-test.yml
    - Triggers on: PRs and merges to main
    - Mock path: ./keploy
```

Supported CI/CD platforms:
- **GitHub Actions** (`github-actions`)
- **GitLab CI/CD** (`gitlab-ci`)
- **Jenkins** (`jenkins`)
- **CircleCI** (`circleci`)
- **Azure Pipelines** (`azure-pipelines`)
- **Bitbucket Pipelines** (`bitbucket-pipelines`)

### 6. Iterative Development Flow

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Development Workflow                          │
└─────────────────────────────────────────────────────────────────────┘

    ┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐
    │  Write   │     │  Record  │     │   Run    │     │  Setup   │
    │   Code   │────>│  Mocks   │────>│  Tests   │────>│  CI/CD   │
    └──────────┘     └──────────┘     └──────────┘     └──────────┘
         │                                  │               │
         │                                  │               ▼
         │         ┌────────────────────────┘          ┌──────────┐
         │         │ If tests fail                     │  Commit  │
         │         ▼                                   │  Code    │
         │    ┌──────────┐                             └──────────┘
         └────│  Debug   │                                  │
              │  & Fix   │                                  │
              └──────────┘                                  │
                   │                                        │
                   │ Re-record if external APIs changed     │
                   └────────────────────────────────────────┘

    ┌────────────────────────────────────────────────────────────────┐
    │                    CI/CD Pipeline (Automated)                  │
    │  ┌─────────┐    ┌─────────────┐    ┌────────────────────┐     │
    │  │  PR     │───>│ Run Keploy  │───>│ Report Test Results│     │
    │  │ Created │    │ Mock Tests  │    │ (Pass/Fail)        │     │
    │  └─────────┘    └─────────────┘    └────────────────────┘     │
    └────────────────────────────────────────────────────────────────┘
```

---

## Backend Flow

### Mock Recording Flow

```
1. MCP Server receives CallTool(keploy_manager) with action="keploy_mock_record"
   └── Input: {action: "keploy_mock_record", command: "go run main.go", path: "./keploy"}

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

### Pipeline Generation Flow (MCP Elicitation & Sampling)

```
1. MCP Server receives CallTool(keploy_manager) with action="pipeline"
   └── Input: {action: "pipeline", appCommand?: "", cicdTool?: ""}

2. Check if appCommand is provided
   ├── If missing: Use MCP Elicitation (Form Mode)
   │   └── session.CreateMessage() to request app command from user
   └── User provides command interactively

3. Detect CI/CD platform
   ├── Scan project for CI/CD config files:
   │   ├── .github/workflows/ → GitHub Actions
   │   ├── .gitlab-ci.yml → GitLab CI
   │   ├── Jenkinsfile → Jenkins
   │   ├── .circleci/config.yml → CircleCI
   │   ├── azure-pipelines.yml → Azure Pipelines
   │   └── bitbucket-pipelines.yml → Bitbucket Pipelines
   ├── If multiple found: Use MCP Sampling for intelligent selection
   ├── If none found: Use MCP Elicitation to ask user
   └── Default: GitHub Actions

4. Generate pipeline content
   ├── Select template based on CI/CD platform
   ├── Substitute configuration values:
   │   ├── appCommand
   │   ├── defaultBranch (default: main)
   │   └── mockPath (default: ./keploy)
   └── Generate platform-specific YAML/Groovy

5. Write pipeline file
   ├── Create directory if needed (e.g., .github/workflows/)
   └── Write file with appropriate permissions

6. Return result to MCP client
   └── {success, cicdTool, filePath, content, message, configuration}
```

---

## API Reference

### Tool: keploy_manager (Recommended)

Unified tool for mock recording, testing, and CI/CD pipeline generation. This is the **recommended** tool for all Keploy operations.

**Input Parameters:**

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `action` | string | **Yes** | Action to perform: `keploy_mock_record`, `keploy_mock_test`, or `pipeline` |
| `command` | string | Conditional | Application/test command (required for `keploy_mock_record` and `keploy_mock_test`) |
| `path` | string | No | Path for mock storage (default: `./keploy`) |
| `mockName` | string | No | Mock set name (for `keploy_mock_test` action) |
| `fallBackOnMiss` | boolean | No | Fall back to real calls (for `keploy_mock_test`, default: `false`) |
| `appCommand` | string | No | App command for pipeline (for `pipeline` action, will elicit if missing) |
| `defaultBranch` | string | No | Branch for CI triggers (for `pipeline`, default: `main`) |
| `mockPath` | string | No | Mock path for pipeline (for `pipeline`, default: `./keploy`) |
| `cicdTool` | string | No | CI/CD platform (for `pipeline`, auto-detected if not provided) |

**Supported CI/CD Platforms (`cicdTool`):**
- `github-actions` - GitHub Actions
- `gitlab-ci` - GitLab CI/CD
- `jenkins` - Jenkins
- `circleci` - CircleCI
- `azure-pipelines` - Azure Pipelines
- `bitbucket-pipelines` - Bitbucket Pipelines

**Output (varies by action):**

| Field | Type | Description |
|-------|------|-------------|
| `success` | boolean | Whether the operation succeeded |
| `action` | string | Action that was performed |
| `message` | string | Human-readable status message |
| `recordResult` | object | Result of mock recording (if action was `keploy_mock_record`) |
| `testResult` | object | Result of mock testing (if action was `keploy_mock_test`) |
| `pipelineResult` | object | Result of pipeline generation (if action was `pipeline`) |

**Examples:**

```json
// Record mocks
{
  "action": "keploy_mock_record",
  "command": "go run main.go",
  "path": "./keploy"
}

// Test with mocks
{
  "action": "keploy_mock_test",
  "command": "go test ./...",
  "mockName": "user-service-postgres"
}

// Generate CI/CD pipeline
{
  "action": "pipeline",
  "appCommand": "npm start",
  "cicdTool": "github-actions",
  "defaultBranch": "main"
}
```

**Pipeline Output Example:**
```json
{
  "success": true,
  "action": "pipeline",
  "message": "Successfully created GitHub Actions pipeline at '.github/workflows/keploy-mock-test.yml'.",
  "pipelineResult": {
    "success": true,
    "cicdTool": "github-actions",
    "filePath": ".github/workflows/keploy-mock-test.yml",
    "configuration": {
      "appCommand": "npm start",
      "defaultBranch": "main",
      "mockPath": "./keploy",
      "cicdTool": "github-actions"
    }
  }
}
```

---

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

AI: [Calls keploy_manager with action="keploy_mock_test", fallBackOnMiss=true]

AI: Test completed with warnings:
    - 13/15 mocks replayed
    - 2 mocks missed (fell back to real calls):
      1. POST /api/v1/new-endpoint (no mock found)
      2. GET /api/users?new_param=true (mock mismatch)
    
    You may need to re-record mocks for the new endpoints.
```

### Example 6: Using keploy_manager (Unified Tool)

```
User: Record mocks from my Go service

AI: [Calls keploy_manager with action="keploy_mock_record", command="go run main.go"]

AI: Successfully recorded 8 mocks:
    - 5 HTTP calls to api.stripe.com
    - 3 PostgreSQL queries
    Mock file saved to: ./keploy/payment-service-stripe/mocks.yaml
```

### Example 7: Generating CI/CD Pipeline (Auto-detect)

```
User: Create a CI/CD pipeline for Keploy testing

AI: [Calls keploy_manager with action="pipeline"]
    (Detected .github/workflows/ directory → GitHub Actions)

AI: What command runs your application? (e.g., 'go run main.go', 'npm start')

User: python app.py

AI: Successfully created GitHub Actions pipeline!
    
    File: .github/workflows/keploy-mock-test.yml
    Configuration:
    - App command: python app.py
    - Branch: main
    - Mock path: ./keploy
    
    The pipeline will run Keploy mock tests on PRs and merges to main.
```

### Example 8: Generating Pipeline for Specific Platform

```
User: Create a GitLab CI pipeline for my Node.js app

AI: [Calls keploy_manager with action="pipeline", cicdTool="gitlab-ci", appCommand="npm start"]

AI: Successfully created GitLab CI/CD pipeline!
    
    File: .gitlab-ci.yml
    Configuration:
    - App command: npm start
    - Branch: main
    - Mock path: ./keploy
    
    The pipeline will run Keploy mock tests on MRs and merges to main.
```

### Example 9: Full Workflow with Pipeline

```
User: I want to set up complete Keploy testing for my project

AI: I'll help you set up Keploy mock testing with CI/CD integration.

    Step 1: Recording mocks...
    [Calls keploy_manager action="keploy_mock_record"]
    ✓ Recorded 12 mocks (HTTP, Postgres)
    
    Step 2: Verifying mock replay...
    [Calls keploy_manager action="keploy_mock_test"]
    ✓ All 12 mocks replayed successfully
    
    Step 3: Creating CI/CD pipeline...
    [Calls keploy_manager action="pipeline"]
    ✓ Created .github/workflows/keploy-mock-test.yml
    
    Your Keploy testing is now set up! The CI pipeline will run
    mock tests on every PR and merge to main.
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

5. **"Pipeline generation requires appCommand"**
   - Provide the `appCommand` parameter when calling `keploy_manager` with `action="pipeline"`
   - If using MCP Elicitation, ensure your MCP client supports the CreateMessage API

6. **"CI/CD platform not detected"**
   - Ensure your project has CI/CD configuration files in the expected locations
   - Alternatively, specify `cicdTool` explicitly (e.g., `"cicdTool": "github-actions"`)

7. **"Failed to write pipeline file"**
   - Check file system permissions
   - Ensure the target directory is writable
   - The pipeline content is still returned in the response for manual creation

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

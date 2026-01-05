# Keploy MCP Architecture

This document provides detailed High-Level Design (HLD) and Low-Level Design (LLD) for the Keploy MCP server.

## Table of Contents

- [High-Level Design](#high-level-design)
  - [System Overview](#system-overview)
  - [Component Architecture](#component-architecture)
  - [Data Flow](#data-flow)
- [Low-Level Design](#low-level-design)
  - [Package Structure](#package-structure)
  - [Class Diagrams](#class-diagrams)
  - [Sequence Diagrams](#sequence-diagrams)
  - [State Machines](#state-machines)
- [Design Decisions](#design-decisions)

---

## High-Level Design

### System Overview

The Keploy MCP server bridges AI coding assistants with Keploy's mocking capabilities using the Model Context Protocol standard.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           EXTERNAL LAYER                                     │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                      AI Assistant (MCP Client)                       │   │
│  │              VS Code · Claude Desktop · Cursor · etc.               │   │
│  └───────────────────────────────┬─────────────────────────────────────┘   │
└──────────────────────────────────┼──────────────────────────────────────────┘
                                   │
                                   │ MCP Protocol
                                   │ (JSON-RPC 2.0 over stdio)
                                   │
┌──────────────────────────────────┼──────────────────────────────────────────┐
│                           │MCP LAYER│                                        │
│  ┌───────────────────────────────┴─────────────────────────────────────┐   │
│  │                        Keploy MCP Server                             │   │
│  │  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────────┐  │   │
│  │  │ Tool Handler:   │  │ Tool Handler:   │  │ LLM Callback:       │  │   │
│  │  │ keploy_mock_    │  │ keploy_mock_    │  │ generateContextual  │  │   │
│  │  │ record          │  │ test            │  │ Name()              │  │   │
│  │  └────────┬────────┘  └────────┬────────┘  └──────────┬──────────┘  │   │
│  └───────────┼────────────────────┼─────────────────────┼──────────────┘   │
└──────────────┼────────────────────┼─────────────────────┼───────────────────┘
               │                    │                     │
┌──────────────┼────────────────────┼─────────────────────┼───────────────────┐
│              │        SERVICE LAYER                     │                    │
│  ┌───────────▼────────────────────▼─────────────────────┼──────────────┐   │
│  │  ┌─────────────────┐  ┌─────────────────┐            │              │   │
│  │  │  mockrecord     │  │  mockreplay     │            │              │   │
│  │  │  Service        │  │  Service        │            │              │   │
│  │  │                 │  │                 │            │              │   │
│  │  │ • Record()      │  │ • Replay()      │            │              │   │
│  │  │ • ExtractMeta() │  │ • LoadMocks()   │            │              │   │
│  │  └────────┬────────┘  └────────┬────────┘            │              │   │
│  └───────────┼────────────────────┼─────────────────────┼──────────────┘   │
└──────────────┼────────────────────┼─────────────────────┼───────────────────┘
               │                    │                     │
┌──────────────┼────────────────────┼─────────────────────┼───────────────────┐
│              │       AGENT LAYER  │                     │                    │
│  ┌───────────▼────────────────────▼─────────────────────┘                   │
│  │                                                                          │
│  │                         Keploy Agent                                     │
│  │  ┌──────────────────────────────────────────────────────────────────┐  │
│  │  │                  eBPF Network Interception                        │  │
│  │  │  • Hook system calls (connect, send, recv)                       │  │
│  │  │  • Transparent proxy for outgoing traffic                         │  │
│  │  │  • Protocol detection and parsing                                 │  │
│  │  │  • Mock matching and response injection                           │  │
│  │  └──────────────────────────────────────────────────────────────────┘  │
│  │                                                                          │
│  └──────────────────────────────────────────────────────────────────────────┘
└─────────────────────────────────────────────────────────────────────────────┘
               │
┌──────────────┼──────────────────────────────────────────────────────────────┐
│              │          EXTERNAL SERVICES                                    │
│  ┌───────────▼────────────────────────────────────────────────────────┐    │
│  │  ┌────────┐  ┌────────┐  ┌────────┐  ┌────────┐  ┌────────┐       │    │
│  │  │  HTTP  │  │  gRPC  │  │Postgres│  │ MySQL  │  │ Redis  │  ...  │    │
│  │  │  APIs  │  │Services│  │   DB   │  │   DB   │  │ Cache  │       │    │
│  │  └────────┘  └────────┘  └────────┘  └────────┘  └────────┘       │    │
│  └────────────────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Component Architecture

#### 1. MCP Server (`pkg/mcp`)

**Responsibilities:**
- Handle MCP protocol communication (JSON-RPC over stdio)
- Register and dispatch tool calls
- Manage client sessions
- Coordinate LLM callbacks for naming

**Key Components:**
| Component | File | Purpose |
|-----------|------|---------|
| Server | `server.go` | Lifecycle management, transport setup |
| Tool Handlers | `tools.go` | Input parsing, output formatting |
| Naming | `naming.go` | LLM callback, fallback naming |

#### 2. Mock Recording Service (`pkg/service/mockrecord`)

**Responsibilities:**
- Execute application commands
- Capture outgoing calls via agent
- Extract metadata for naming
- Generate mock files

**Key Components:**
| Component | File | Purpose |
|-----------|------|---------|
| Service Interface | `service.go` | Contract definition |
| Recorder | `record.go` | Recording orchestration |
| Metadata | `metadata.go` | Protocol/endpoint extraction |

#### 3. Mock Replay Service (`pkg/service/mockreplay`)

**Responsibilities:**
- Load mocks from YAML files
- Configure agent for mock mode
- Execute application with mocking
- Report replay results

**Key Components:**
| Component | File | Purpose |
|-----------|------|---------|
| Service Interface | `service.go` | Contract definition |
| Replayer | `replay.go` | Replay orchestration |

#### 4. Shared Models (`pkg/models`)

**Types:**
| Type | File | Purpose |
|------|------|---------|
| `RecordOptions` | `mockrecord.go` | Recording configuration |
| `RecordResult` | `mockrecord.go` | Recording output |
| `MockMetadata` | `mockrecord.go` | Extracted mock info |
| `EndpointInfo` | `mockrecord.go` | Endpoint details |
| `ReplayOptions` | `mockreplay.go` | Replay configuration |
| `ReplayResult` | `mockreplay.go` | Replay output |

### Data Flow

#### Recording Flow

```
1. AI Assistant sends CallTool(keploy_mock_record, {command, path, duration})
                    │
                    ▼
2. MCP Server parses input → MockRecordInput struct
                    │
                    ▼
3. MCP Server calls mockrecord.Service.Record(RecordOptions)
                    │
                    ▼
4. recorder.Record():
   a. Setup agent (eBPF hooks)
   b. Start mock capture goroutine
   c. Execute application command
   d. Wait for completion/timeout
   e. Extract metadata from mocks
                    │
                    ▼
5. MCP Server generates contextual name:
   a. Try LLM callback (session.CreateMessage)
   b. On failure: Use fallback naming
   c. Rename mock file
                    │
                    ▼
6. MCP Server returns CallToolResult to AI Assistant
```

#### Replay Flow

```
1. AI Assistant sends CallTool(keploy_mock_test, {command, mockFilePath})
                    │
                    ▼
2. MCP Server parses input → MockReplayInput struct
                    │
                    ▼
3. MCP Server calls mockreplay.Service.Replay(ReplayOptions)
                    │
                    ▼
4. replayer.Replay():
   a. Load mocks from YAML file
   b. Setup agent (eBPF hooks)
   c. Set mocks in agent for matching
   d. Enable mock outgoing mode
   e. Execute application command
   f. Collect consumed mock stats
                    │
                    ▼
5. MCP Server returns CallToolResult to AI Assistant
```

---

## Low-Level Design

### Package Structure

```
go.keploy.io/server/v3/
│
├── cli/
│   └── mcp.go                    # CLI entry point
│       ├── MCP()                 # Parent command
│       ├── MCPServe()            # serve subcommand
│       ├── recordService         # record.Service for mockrecord
│       └── replayAgentAdapter    # Agent → mockreplay.AgentService
│
├── pkg/
│   ├── mcp/
│   │   ├── server.go             # MCP server
│   │   │   ├── Server struct
│   │   │   ├── NewServer()
│   │   │   ├── Run()
│   │   │   ├── registerTools()
│   │   │   └── getActiveSession()
│   │   │
│   │   ├── tools.go              # Tool handlers
│   │   │   ├── MockRecordInput/Output
│   │   │   ├── MockReplayInput/Output
│   │   │   ├── handleMockRecord()
│   │   │   └── handleMockReplay()
│   │   │
│   │   └── naming.go             # LLM naming
│   │       ├── generateContextualName()
│   │       ├── buildMetadataSummary()
│   │       ├── fallbackName()
│   │       └── sanitizeFilename()
│   │
│   ├── service/
│   │   ├── mockrecord/
│   │   │   ├── service.go        # Interfaces
│   │   │   │   ├── Service interface
│   │   │   │   └── RecordService interface
│   │   │   │
│   │   │   ├── record.go         # Implementation
│   │   │   │   ├── recorder struct
│   │   │   │   ├── New()
│   │   │   │   └── Record()
│   │   │   │
│   │   │   └── metadata.go       # Metadata extraction
│   │   │       ├── ExtractMetadata()
│   │   │       ├── extractServiceName()
│   │   │       └── extractHostAndPath()
│   │   │
│   │   └── mockreplay/
│   │       ├── service.go        # Interfaces
│   │       │   ├── Service interface
│   │       │   ├── AgentService interface
│   │       │   └── MockDB interface
│   │       │
│   │       └── replay.go         # Implementation
│   │           ├── replayer struct
│   │           ├── New()
│   │           ├── Replay()
│   │           ├── loadMocksFromFile()
│   │           └── runCommand()
│   │
│   └── models/
│       ├── mockrecord.go         # Recording types
│       │   ├── RecordOptions
│       │   ├── RecordResult
│       │   ├── MockMetadata
│       │   └── EndpointInfo
│       │
│       └── mockreplay.go         # Replay types
│           ├── ReplayOptions
│           └── ReplayResult
│
└── docs/mcp/                     # Documentation
    ├── README.md
    ├── USER_GUIDE.md
    ├── ARCHITECTURE.md
    └── API_REFERENCE.md
```

### Class Diagrams

#### MCP Server Package

```
┌─────────────────────────────────────────────────────────────────────────┐
│                              pkg/mcp                                     │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ┌───────────────────────────────────────────────────────────────────┐ │
│  │ Server                                                             │ │
│  ├───────────────────────────────────────────────────────────────────┤ │
│  │ - server: *sdkmcp.Server                                          │ │
│  │ - mockRecorder: mockrecord.Service                                │ │
│  │ - mockReplayer: mockreplay.Service                                │ │
│  │ - logger: *zap.Logger                                             │ │
│  │ - mu: sync.RWMutex                                                │ │
│  │ - activeSession: *sdkmcp.ServerSession                            │ │
│  ├───────────────────────────────────────────────────────────────────┤ │
│  │ + NewServer(opts *ServerOptions) *Server                          │ │
│  │ + Run(ctx context.Context) error                                  │ │
│  │ - registerTools()                                                 │ │
│  │ - getActiveSession() *sdkmcp.ServerSession                        │ │
│  │ - handleMockRecord(ctx, req, in) (*Result, Output, error)         │ │
│  │ - handleMockReplay(ctx, req, in) (*Result, Output, error)         │ │
│  │ - generateContextualName(ctx, meta) (string, error)               │ │
│  │ - buildMetadataSummary(meta) metadataSummary                      │ │
│  │ - fallbackName(meta) string                                       │ │
│  │ - sanitizeFilename(name) string                                   │ │
│  └───────────────────────────────────────────────────────────────────┘ │
│                                                                         │
│  ┌───────────────────────────────────────────────────────────────────┐ │
│  │ ServerOptions                                                      │ │
│  ├───────────────────────────────────────────────────────────────────┤ │
│  │ + Logger: *zap.Logger                                             │ │
│  │ + MockRecorder: mockrecord.Service                                │ │
│  │ + MockReplayer: mockreplay.Service                                │ │
│  └───────────────────────────────────────────────────────────────────┘ │
│                                                                         │
│  ┌────────────────────────┐  ┌────────────────────────┐               │
│  │ MockRecordInput        │  │ MockRecordOutput       │               │
│  ├────────────────────────┤  ├────────────────────────┤               │
│  │ + Command: string      │  │ + Success: bool        │               │
│  │ + Path: string         │  │ + MockFilePath: string │               │
│  │ + Duration: string     │  │ + MockCount: int       │               │
│  └────────────────────────┘  │ + Protocols: []string  │               │
│                              │ + Message: string      │               │
│  ┌────────────────────────┐  └────────────────────────┘               │
│  │ MockReplayInput        │  ┌────────────────────────┐               │
│  ├────────────────────────┤  │ MockReplayOutput       │               │
│  │ + Command: string      │  ├────────────────────────┤               │
│  │ + MockFilePath: string │  │ + Success: bool        │               │
│  │ + FallBackOnMiss: bool │  │ + MocksReplayed: int   │               │
│  └────────────────────────┘  │ + MocksMissed: int     │               │
│                              │ + AppExitCode: int     │               │
│                              │ + Message: string      │               │
│                              └────────────────────────┘               │
└─────────────────────────────────────────────────────────────────────────┘
```

#### Service Interfaces

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        pkg/service/mockrecord                            │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ┌───────────────────────────────────────────────────────────────────┐ │
│  │ <<interface>> Service                                              │ │
│  ├───────────────────────────────────────────────────────────────────┤ │
│  │ + Record(ctx, opts RecordOptions) (*RecordResult, error)          │ │
│  └───────────────────────────────────────────────────────────────────┘ │
│                               △                                         │
│                               │ implements                              │
│  ┌───────────────────────────────────────────────────────────────────┐ │
│  │ recorder                                                           │ │
│  ├───────────────────────────────────────────────────────────────────┤ │
│  │ - logger: *zap.Logger                                             │ │
│  │ - cfg: *config.Config                                             │ │
│  │ - record: RecordService                                           │ │
│  └───────────────────────────────────────────────────────────────────┘ │
│                                                                         │
│  ┌───────────────────────────────────────────────────────────────────┐ │
│  │ <<interface>> RecordService                                        │ │
│  ├───────────────────────────────────────────────────────────────────┤ │
│  │ + RecordMocks(ctx, opts RecordOptions) (*RecordResult, error)     │ │
│  └───────────────────────────────────────────────────────────────────┘ │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│                        pkg/service/mockreplay                            │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ┌───────────────────────────────────────────────────────────────────┐ │
│  │ <<interface>> Service                                              │ │
│  ├───────────────────────────────────────────────────────────────────┤ │
│  │ + Replay(ctx, opts ReplayOptions) (*ReplayResult, error)          │ │
│  └───────────────────────────────────────────────────────────────────┘ │
│                               △                                         │
│                               │ implements                              │
│  ┌───────────────────────────────────────────────────────────────────┐ │
│  │ replayer                                                           │ │
│  ├───────────────────────────────────────────────────────────────────┤ │
│  │ - logger: *zap.Logger                                             │ │
│  │ - cfg: *config.Config                                             │ │
│  │ - agent: AgentService                                             │ │
│  │ - mockDB: MockDB                                                  │ │
│  └───────────────────────────────────────────────────────────────────┘ │
│                                                                         │
│  ┌───────────────────────────────────────────────────────────────────┐ │
│  │ <<interface>> AgentService                                         │ │
│  ├───────────────────────────────────────────────────────────────────┤ │
│  │ + Setup(ctx, startCh chan int) error                              │ │
│  │ + MockOutgoing(ctx, opts OutgoingOptions) error                   │ │
│  │ + SetMocks(ctx, filtered, unFiltered []*Mock) error               │ │
│  │ + GetConsumedMocks(ctx) ([]MockState, error)                      │ │
│  └───────────────────────────────────────────────────────────────────┘ │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

### Sequence Diagrams

#### Recording Sequence

```
┌─────────┐   ┌─────────┐   ┌─────────┐   ┌─────────┐   ┌─────────┐   ┌─────────┐
│   AI    │   │   MCP   │   │ mock    │   │  Agent  │   │  App    │   │External │
│ Client  │   │ Server  │   │ record  │   │ (eBPF)  │   │Process  │   │Services │
└────┬────┘   └────┬────┘   └────┬────┘   └────┬────┘   └────┬────┘   └────┬────┘
     │             │             │             │             │             │
     │ CallTool    │             │             │             │             │
     │(record)     │             │             │             │             │
     │────────────>│             │             │             │             │
     │             │             │             │             │             │
     │             │ Record()    │             │             │             │
     │             │────────────>│             │             │             │
     │             │             │             │             │             │
     │             │             │ Setup()     │             │             │
     │             │             │────────────>│             │             │
     │             │             │             │             │             │
     │             │             │ GetOutgoing │             │             │
     │             │             │────────────>│             │             │
     │             │             │             │             │             │
     │             │             │     <-chan Mock           │             │
     │             │             │<────────────│             │             │
     │             │             │             │             │             │
     │             │             │ exec.Command│             │             │
     │             │             │─────────────────────────>│             │
     │             │             │             │             │             │
     │             │             │             │             │ HTTP/DB    │
     │             │             │             │             │────────────>│
     │             │             │             │             │             │
     │             │             │             │ intercept   │<────────────│
     │             │             │             │<─ ─ ─ ─ ─ ─ │             │
     │             │             │             │             │             │
     │             │             │   Mock      │             │             │
     │             │             │<────────────│             │             │
     │             │             │             │             │             │
     │             │             │ (collect    │             │             │
     │             │             │  mocks)     │             │             │
     │             │             │             │             │             │
     │             │             │ cmd.Wait()  │             │             │
     │             │             │─────────────────────────>│             │
     │             │             │             │             │             │
     │             │             │ ExtractMetadata()        │             │
     │             │             │───────┐     │             │             │
     │             │             │       │     │             │             │
     │             │             │<──────┘     │             │             │
     │             │             │             │             │             │
     │             │ RecordResult│             │             │             │
     │             │<────────────│             │             │             │
     │             │             │             │             │             │
     │             │generateContextualName()   │             │             │
     │             │───────┐     │             │             │             │
     │<─ ─ ─ ─ ─ ─ ┤ LLM   │     │             │             │             │
     │ ─ ─ ─ ─ ─ ─>│callback     │             │             │             │
     │             │<──────┘     │             │             │             │
     │             │             │             │             │             │
     │ CallTool    │             │             │             │             │
     │ Result      │             │             │             │             │
     │<────────────│             │             │             │             │
     │             │             │             │             │             │
```

#### Replay Sequence

```
┌─────────┐   ┌─────────┐   ┌─────────┐   ┌─────────┐   ┌─────────┐
│   AI    │   │   MCP   │   │ mock    │   │  Agent  │   │  App    │
│ Client  │   │ Server  │   │ replay  │   │ (eBPF)  │   │Process  │
└────┬────┘   └────┬────┘   └────┬────┘   └────┬────┘   └────┬────┘
     │             │             │             │             │
     │ CallTool    │             │             │             │
     │(test)       │             │             │             │
     │────────────>│             │             │             │
     │             │             │             │             │
     │             │ Replay()    │             │             │
     │             │────────────>│             │             │
     │             │             │             │             │
     │             │             │loadMocksFromFile()       │
     │             │             │───────┐     │             │
     │             │             │       │     │             │
     │             │             │<──────┘     │             │
     │             │             │             │             │
     │             │             │ Setup()     │             │
     │             │             │────────────>│             │
     │             │             │             │             │
     │             │             │ SetMocks()  │             │
     │             │             │────────────>│             │
     │             │             │             │             │
     │             │             │MockOutgoing │             │
     │             │             │────────────>│             │
     │             │             │             │             │
     │             │             │ exec.Command│             │
     │             │             │─────────────────────────>│
     │             │             │             │             │
     │             │             │             │ outgoing    │
     │             │             │             │ call        │
     │             │             │             │<────────────│
     │             │             │             │             │
     │             │             │             │ match mock  │
     │             │             │             │───────┐     │
     │             │             │             │       │     │
     │             │             │             │<──────┘     │
     │             │             │             │             │
     │             │             │             │ return mock │
     │             │             │             │ response    │
     │             │             │             │────────────>│
     │             │             │             │             │
     │             │             │ cmd.Wait()  │             │
     │             │             │─────────────────────────>│
     │             │             │             │             │
     │             │             │GetConsumed  │             │
     │             │             │Mocks()      │             │
     │             │             │────────────>│             │
     │             │             │             │             │
     │             │ ReplayResult│             │             │
     │             │<────────────│             │             │
     │             │             │             │             │
     │ CallTool    │             │             │             │
     │ Result      │             │             │             │
     │<────────────│             │             │             │
     │             │             │             │             │
```

### State Machines

#### Recording State Machine

```
                    ┌─────────────┐
                    │    IDLE     │
                    └──────┬──────┘
                           │ Record() called
                           ▼
                    ┌─────────────┐
                    │   SETUP     │
                    │  (Agent)    │
                    └──────┬──────┘
                           │ Setup successful
                           ▼
              ┌────────────────────────┐
              │      RECORDING         │
              │ (Capturing mocks)      │
              └────────────┬───────────┘
                           │
          ┌────────────────┼────────────────┐
          │                │                │
    Timeout        Command exits      Context cancelled
          │                │                │
          ▼                ▼                ▼
    ┌─────────────────────────────────────────────┐
    │              FINALIZING                      │
    │  (Store mocks, extract metadata)            │
    └──────────────────────┬──────────────────────┘
                           │
                           ▼
                    ┌─────────────┐
                    │  COMPLETE   │
                    │(Return result)
                    └─────────────┘
```

#### Replay State Machine

```
                    ┌─────────────┐
                    │    IDLE     │
                    └──────┬──────┘
                           │ Replay() called
                           ▼
                    ┌─────────────┐
                    │   LOADING   │
                    │  (Mocks)    │
                    └──────┬──────┘
                           │ Mocks loaded
                           ▼
                    ┌─────────────┐
                    │   SETUP     │
                    │  (Agent)    │
                    └──────┬──────┘
                           │ Setup successful
                           ▼
              ┌────────────────────────┐
              │      REPLAYING         │
              │ (Running with mocks)   │
              └────────────┬───────────┘
                           │
          ┌────────────────┼────────────────┐
          │                │                │
    Timeout        Command exits      Context cancelled
          │                │                │
          ▼                ▼                ▼
    ┌─────────────────────────────────────────────┐
    │              COLLECTING                      │
    │  (Get consumed mocks, calculate stats)      │
    └──────────────────────┬──────────────────────┘
                           │
                           ▼
                    ┌─────────────┐
                    │  COMPLETE   │
                    │(Return result)
                    └─────────────┘
```

---

## Design Decisions

### 1. Separation of MCP and Core Services

**Decision**: Keep MCP server separate from mockrecord/mockreplay services.

**Rationale**:
- Services are reusable (CLI can use them directly)
- MCP layer is a thin integration layer
- Easier to test services in isolation
- Follows single responsibility principle

### 2. Types in Models Package

**Decision**: Move `RecordOptions`, `ReplayOptions`, etc. to `pkg/models`.

**Rationale**:
- Consistent with existing Keploy patterns
- Types are shared across packages
- Avoids circular dependencies
- Services define behavior, models define data

### 3. LLM Callback with Fallback

**Decision**: Use LLM callback for naming with deterministic fallback.

**Rationale**:
- Better UX when LLM available (contextual names)
- Always works even without LLM support
- Fallback is predictable and debuggable
- No blocking on LLM failures

### 4. Interface-Based Recording Abstraction

**Decision**: Use a `RecordService` abstraction for mockrecord and `AgentService` for mockreplay.

**Rationale**:
- Different operations needed (GetOutgoing vs SetMocks)
- Enables testing with mock implementations
- Decouples services from agent implementation
- Adapter pattern for existing agent.Service (replay)

### 5. Stdio Transport Only

**Decision**: Use stdio transport for MCP communication.

**Rationale**:
- Standard for CLI-based MCP servers
- Works with all major AI assistants
- Simple configuration
- No network setup required

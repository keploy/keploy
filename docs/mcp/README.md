# Keploy MCP (Model Context Protocol) Documentation

This directory contains documentation for the Keploy MCP server feature, which enables AI assistants to interact with Keploy's mock recording and replay capabilities.

## Quick Links

| Document | Description |
|----------|-------------|
| [User Guide](./USER_GUIDE.md) | End-user documentation for using MCP with AI assistants |
| [Architecture](./ARCHITECTURE.md) | High-level and low-level design documentation |
| [API Reference](./API_REFERENCE.md) | Complete tool specifications and examples |
| [Contributing](./CONTRIBUTING.md) | Developer guide for contributing to MCP |

## What is MCP?

The **Model Context Protocol (MCP)** is an open standard that enables AI assistants to interact with external tools and services. Keploy's MCP server exposes mock recording and replay as tools that AI coding assistants can use.

### Supported AI Assistants

- **GitHub Copilot** (VS Code)
- **Claude Desktop**
- **Cursor**
- **Any MCP-compatible client**

## Quick Start

### 1. Install Keploy

```bash
curl -O https://raw.githubusercontent.com/keploy/keploy/main/keploy.sh && source keploy.sh
```

### 2. Configure Your AI Assistant

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

**VS Code** (Settings → MCP Servers):
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

### 3. Start Using

Ask your AI assistant:
- *"Record the external API calls from my Node.js app"*
- *"Test my service using the recorded mocks"*
- *"What mocks are available in my project?"*

## Architecture Overview

```
┌─────────────────────┐
│    AI Assistant     │
│  (Copilot, Claude)  │
└──────────┬──────────┘
           │ MCP Protocol (JSON-RPC/stdio)
           ▼
┌──────────────────────┐
│  Keploy MCP Server   │
│  ┌────────────────┐  │
│  │ keploy_mock_   │  │
│  │ record         │  │
│  ├────────────────┤  │
│  │ keploy_mock_   │  │
│  │ test           │  │
│  └────────────────┘  │
└──────────┬───────────┘
           │
           ▼
┌──────────────────────┐
│   Keploy Agent       │
│   (eBPF-based)       │
└──────────┬───────────┘
           │
           ▼
┌──────────────────────┐
│ External Services    │
│ (APIs, DBs, etc.)    │
└──────────────────────┘
```

## Tools Available

### `keploy_mock_record`

Records outgoing calls from your application.

```
Input:
  - command: "npm start" (required)
  - path: "./keploy" (optional)
  - duration: "60s" (optional)

Output:
  - mockFilePath: Path to generated mocks
  - mockCount: Number of mocks recorded
  - protocols: List of detected protocols
```

### `keploy_mock_test`

Replays recorded mocks during testing.

```
Input:
  - command: "npm test" (required)
  - mockFilePath: "./keploy/mocks" (required)
  - fallBackOnMiss: false (optional)

Output:
  - success: Whether all mocks matched
  - mocksReplayed: Count of replayed mocks
  - mocksMissed: Count of unmatched calls
```

## Directory Structure

```
docs/mcp/
├── README.md           # This file
├── USER_GUIDE.md       # End-user documentation
├── ARCHITECTURE.md     # HLD/LLD documentation
├── API_REFERENCE.md    # Tool specifications
└── CONTRIBUTING.md     # Developer guide

pkg/mcp/                # MCP server implementation
├── README.md           # Package documentation
├── server.go           # Server lifecycle
├── tools.go            # Tool handlers
└── naming.go           # LLM callback naming

pkg/service/mockrecord/ # Recording service
├── README.md           # Service documentation
├── service.go          # Interface definitions
├── record.go           # Implementation
└── metadata.go         # Metadata extraction

pkg/service/mockreplay/ # Replay service
├── README.md           # Service documentation
├── service.go          # Interface definitions
└── replay.go           # Implementation

pkg/models/             # Shared types
├── mockrecord.go       # Recording types
└── mockreplay.go       # Replay types
```

## Need Help?

- **Issues**: [GitHub Issues](https://github.com/keploy/keploy/issues)
- **Discussions**: [GitHub Discussions](https://github.com/keploy/keploy/discussions)
- **Slack**: [Keploy Community](https://join.slack.com/t/keploy/shared_invite/...)
- **Docs**: [keploy.io/docs](https://keploy.io/docs)

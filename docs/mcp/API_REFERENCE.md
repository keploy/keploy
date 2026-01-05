# Keploy MCP API Reference

Complete reference documentation for the Keploy MCP server tools.

## Table of Contents

- [Protocol Overview](#protocol-overview)
- [Tools](#tools)
  - [keploy_list_mocks](#keploy_list_mocks)
  - [keploy_mock_record](#keploy_mock_record)
  - [keploy_mock_test](#keploy_mock_test)
- [Error Handling](#error-handling)
- [JSON Schema](#json-schema)

---

## Protocol Overview

### Transport

The Keploy MCP server uses **stdio transport**:
- Input: JSON-RPC messages on stdin
- Output: JSON-RPC messages on stdout
- Logging: stderr

### Protocol Version

- MCP Protocol: `2024-11-05`
- JSON-RPC: `2.0`

### Server Capabilities

```json
{
  "capabilities": {
    "tools": {
      "listChanged": true
    }
  },
  "serverInfo": {
    "name": "keploy-mock",
    "version": "v1.0.0"
  }
}
```

---

## Tools

### keploy_list_mocks

Lists all available mock sets in your workspace.

#### Description

```
List available mock sets in the Keploy directory. This helps you discover what 
mocks have been recorded and can be used with keploy_mock_test. Returns mock 
sets sorted by recency (latest first).
```

#### Input Schema

```json
{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to search for mock files (default: ./keploy)"
    }
  },
  "required": []
}
```

#### Input Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `path` | string | No | `./keploy` | Directory to search for mock sets |

#### Output Schema

```json
{
  "type": "object",
  "properties": {
    "success": {
      "type": "boolean",
      "description": "Whether listing was successful"
    },
    "mockSets": {
      "type": "array",
      "items": { "type": "string" },
      "description": "List of available mock set names (sorted by recency)"
    },
    "count": {
      "type": "integer",
      "description": "Number of mock sets found"
    },
    "path": {
      "type": "string",
      "description": "Path where mocks were searched"
    },
    "message": {
      "type": "string",
      "description": "Human-readable status message"
    }
  }
}
```

#### Output Fields

| Field | Type | Description |
|-------|------|-------------|
| `success` | boolean | `true` if listing completed successfully |
| `mockSets` | string[] | Names of available mock sets (latest first) |
| `count` | integer | Total number of mock sets found |
| `path` | string | Directory that was searched |
| `message` | string | Human-readable summary |

#### Example Request

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "keploy_list_mocks",
    "arguments": {}
  }
}
```

#### Example Response (Success)

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"success\":true,\"mockSets\":[\"order-service-stripe-postgres\",\"user-service-http\",\"payment-api\"],\"count\":3,\"path\":\"./keploy\",\"message\":\"Found 3 mock set(s). The latest is 'order-service-stripe-postgres'.\"}"
      }
    ]
  }
}
```

#### Example Response (No Mocks)

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"success\":true,\"mockSets\":[],\"count\":0,\"path\":\"./keploy\",\"message\":\"No mock sets found. Use keploy_mock_record to create mocks first.\"}"
      }
    ]
  }
}
```

---

### keploy_mock_record

Records outgoing calls from your application, capturing HTTP APIs, database queries, and other external dependencies.

#### Description

```
Record outgoing calls (HTTP APIs, databases, message queues, etc.) from your 
application. This captures all external dependencies while running your 
application command, creating mock files that can be replayed during testing.
```

#### Input Schema

```json
{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "Application command to run (e.g. 'go run main.go' or 'npm start')"
    },
    "path": {
      "type": "string",
      "description": "Path to store mock files (default: ./keploy)"
    }
  },
  "required": ["command"]
}
```

#### Input Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `command` | string | **Yes** | - | Application command to execute |
| `path` | string | No | `./keploy` | Directory to store mock files |

#### Output Schema

```json
{
  "type": "object",
  "properties": {
    "success": {
      "type": "boolean",
      "description": "Whether recording was successful"
    },
    "mockFilePath": {
      "type": "string",
      "description": "Path to the generated mock file"
    },
    "mockCount": {
      "type": "integer",
      "description": "Number of mocks recorded"
    },
    "protocols": {
      "type": "array",
      "items": { "type": "string" },
      "description": "List of protocols detected in recorded mocks"
    },
    "message": {
      "type": "string",
      "description": "Human-readable status message"
    },
    "configuration": {
      "type": "object",
      "description": "Configuration that was used for recording",
      "properties": {
        "command": { "type": "string" },
        "path": { "type": "string" }
      }
    }
  }
}
```

#### Output Fields

| Field | Type | Description |
|-------|------|-------------|
| `success` | boolean | `true` if recording completed successfully |
| `mockFilePath` | string | Full path to generated mock file |
| `mockCount` | integer | Total number of mocks captured |
| `protocols` | string[] | Detected protocols (HTTP, Postgres, etc.) |
| `message` | string | Human-readable summary |
| `configuration` | object | Configuration used for recording |

#### Example Request

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "keploy_mock_record",
    "arguments": {
      "command": "npm start",
      "path": "./keploy/api-tests"
    }
  }
}
```

#### Example Response (Success)

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"success\":true,\"mockFilePath\":\"./keploy/api-tests/order-service-stripe-postgres/mocks.yaml\",\"mockCount\":12,\"protocols\":[\"HTTP\",\"Postgres\"],\"message\":\"Recorded 12 mocks (HTTP, Postgres)\"}"
      }
    ]
  }
}
```

#### Example Response (No Mocks)

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"success\":true,\"mockFilePath\":\"./keploy/mock-1704153600/mocks.yaml\",\"mockCount\":0,\"protocols\":[],\"message\":\"Recording completed but no mocks were captured\"}"
      }
    ]
  }
}
```

#### Example Response (Error)

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"success\":false,\"mockFilePath\":\"\",\"mockCount\":0,\"protocols\":null,\"message\":\"Recording failed: failed to setup agent: permission denied\"}"
      }
    ]
  }
}
```

---

### keploy_mock_test

Replays recorded mocks while running your application, enabling isolated testing.

#### Description

```
Replay recorded mocks while running your application. This intercepts outgoing 
calls and returns the recorded responses, enabling isolated testing without 
external dependencies.
```

#### Input Schema

```json
{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "Test command to run (e.g. 'go test -v' or 'npm test')"
    },
    "mockName": {
      "type": "string",
      "description": "Name of the mock set to replay. Use keploy_list_mocks to see available mocks. If not provided, the latest mock set will be used."
    },
    "fallBackOnMiss": {
      "type": "boolean",
      "description": "Whether to fall back to real calls when no mock matches (default: false)"
    }
  },
  "required": ["command"]
}
```

#### Input Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `command` | string | **Yes** | - | Application/test command to execute |
| `mockName` | string | No | (latest) | Name of mock set to replay (use `keploy_list_mocks` to discover) |
| `fallBackOnMiss` | boolean | No | `false` | Allow real calls on mock miss |

#### Mock Name Discovery

If `mockName` is not provided, the latest recorded mock set is used automatically.
Use `keploy_list_mocks` to discover available mock sets and their names.

#### Output Schema

```json
{
  "type": "object",
  "properties": {
    "success": {
      "type": "boolean",
      "description": "Whether replay was successful (all mocks matched)"
    },
    "mocksReplayed": {
      "type": "integer",
      "description": "Number of mocks that were replayed"
    },
    "mocksMissed": {
      "type": "integer",
      "description": "Number of unmatched calls"
    },
    "appExitCode": {
      "type": "integer",
      "description": "Application exit code"
    },
    "message": {
      "type": "string",
      "description": "Human-readable status message"
    },
    "configuration": {
      "type": "object",
      "description": "Configuration that was used for replay",
      "properties": {
        "command": { "type": "string" },
        "mockName": { "type": "string" },
        "fallBackOnMiss": { "type": "boolean" }
      }
    }
  }
}
```

#### Output Fields

| Field | Type | Description |
|-------|------|-------------|
| `success` | boolean | `true` if all mocks matched and app succeeded |
| `mocksReplayed` | integer | Number of mock responses returned |
| `mocksMissed` | integer | Number of calls without matching mock |
| `appExitCode` | integer | Application process exit code |
| `message` | string | Human-readable summary |
| `configuration` | object | Configuration used for replay |

#### Success Criteria

`success` is `true` when:
- `mocksMissed == 0` (all calls had matching mocks)
- `appExitCode == 0` (application exited normally)

#### Example Request

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "keploy_mock_test",
    "arguments": {
      "command": "go test ./...",
      "mockName": "user-service-stripe",
      "fallBackOnMiss": false
    }
  }
}
```

#### Example Request (Auto-select Latest Mock)

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "keploy_mock_test",
    "arguments": {
      "command": "npm test"
    }
  }
}
```

#### Example Response (Success)

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"success\":true,\"mocksReplayed\":8,\"mocksMissed\":0,\"appExitCode\":0,\"message\":\"Replayed 8 mocks\"}"
      }
    ]
  }
}
```

#### Example Response (Mock Miss)

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"success\":false,\"mocksReplayed\":6,\"mocksMissed\":2,\"appExitCode\":0,\"message\":\"Replayed 6 mocks, 2 mocks missed\"}"
      }
    ]
  }
}
```

#### Example Response (Test Failure)

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"success\":false,\"mocksReplayed\":8,\"mocksMissed\":0,\"appExitCode\":1,\"message\":\"Replayed 8 mocks, app exited with code 1\"}"
      }
    ]
  }
}
```

---

## Error Handling

### Tool-Level Errors

Tool errors are returned in the output with `success: false`:

```json
{
  "success": false,
  "message": "Recording failed: <error description>"
}
```

### Common Errors

| Error | Cause | Solution |
|-------|-------|----------|
| `Command is required` | Missing command parameter | Provide `command` argument |
| `Mock recorder service is not available` | Agent not initialized | Check Keploy installation |
| `Mock replayer service is not available` | Agent not initialized | Check Keploy installation |
| `Failed to setup agent: permission denied` | Need elevated permissions | Run with sudo |
| `Failed to load mocks` | Mock file/set not found | Use `keploy_list_mocks` to check available mocks |
| `No mock sets found` | No recordings exist | Run `keploy_mock_record` first |

### Protocol Errors

MCP protocol errors use standard JSON-RPC error codes:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "error": {
    "code": -32602,
    "message": "Invalid params"
  }
}
```

---

## JSON Schema

### Complete Tool Definitions

```json
{
  "tools": [
    {
      "name": "keploy_list_mocks",
      "description": "List available mock sets in the Keploy directory. This helps you discover what mocks have been recorded and can be used with keploy_mock_test. Returns mock sets sorted by recency (latest first).",
      "inputSchema": {
        "type": "object",
        "properties": {
          "path": {
            "type": "string",
            "description": "Path to search for mock files (default: ./keploy)"
          }
        },
        "required": []
      }
    },
    {
      "name": "keploy_mock_record",
      "description": "Record outgoing calls (HTTP APIs, databases, message queues, etc.) from your application. This captures all external dependencies while running your application command, creating mock files that can be replayed during testing.",
      "inputSchema": {
        "type": "object",
        "properties": {
          "command": {
            "type": "string",
            "description": "Application command to run (e.g. 'go run main.go' or 'npm start')"
          },
          "path": {
            "type": "string",
            "description": "Path to store mock files (default: ./keploy)"
          }
        },
        "required": ["command"]
      }
    },
    {
      "name": "keploy_mock_test",
      "description": "Replay recorded mocks while running your application. This intercepts outgoing calls and returns the recorded responses, enabling isolated testing without external dependencies.",
      "inputSchema": {
        "type": "object",
        "properties": {
          "command": {
            "type": "string",
            "description": "Test command to run (e.g. 'go test -v' or 'npm test')"
          },
          "mockName": {
            "type": "string",
            "description": "Name of the mock set to replay. Use keploy_list_mocks to see available mocks. If not provided, the latest mock set will be used."
          },
          "fallBackOnMiss": {
            "type": "boolean",
            "description": "Whether to fall back to real calls when no mock matches (default: false)"
          }
        },
        "required": ["command"]
      }
    }
  ]
}
```

---

## Protocol Examples

### Initialize Handshake

**Client → Server:**
```json
{
  "jsonrpc": "2.0",
  "id": 0,
  "method": "initialize",
  "params": {
    "protocolVersion": "2024-11-05",
    "capabilities": {},
    "clientInfo": {
      "name": "vscode-copilot",
      "version": "1.0.0"
    }
  }
}
```

**Server → Client:**
```json
{
  "jsonrpc": "2.0",
  "id": 0,
  "result": {
    "protocolVersion": "2024-11-05",
    "capabilities": {
      "tools": {}
    },
    "serverInfo": {
      "name": "keploy-mock",
      "version": "v1.0.0"
    }
  }
}
```

### List Tools

**Client → Server:**
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/list"
}
```

**Server → Client:**
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "tools": [
      {
        "name": "keploy_list_mocks",
        "description": "List available mock sets...",
        "inputSchema": { ... }
      },
      {
        "name": "keploy_mock_record",
        "description": "Record outgoing calls...",
        "inputSchema": { ... }
      },
      {
        "name": "keploy_mock_test",
        "description": "Replay recorded mocks...",
        "inputSchema": { ... }
      }
    ]
  }
}
```

### Call Tool

**Client → Server:**
```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "keploy_mock_record",
    "arguments": {
      "command": "go run main.go"
    }
  }
}
```

**Server → Client:**
```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"success\":true,\"mockFilePath\":\"./keploy/my-app-http/mocks.yaml\",\"mockCount\":5,\"protocols\":[\"HTTP\"],\"message\":\"Recorded 5 mocks (HTTP)\"}"
      }
    ]
  }
}
```

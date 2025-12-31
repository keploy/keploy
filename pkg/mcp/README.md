# Keploy MCP Integration - Mock Recording & Replay

This document describes the Model Context Protocol (MCP) integration for Keploy's mock recording and replay functionality, enabling AI agents to interact with Keploy using natural language prompts.

## Overview

The MCP integration allows AI agents to:
- **Record mocks** by capturing outgoing network calls during test execution
- **Replay tests** using recorded mocks for environment isolation
- **Generate tests** with automated record/replay workflows
- **List mocks** with contextual naming based on API descriptions

## Quick Start

### 1. Start the MCP Server

```bash
# Start with HTTP transport (default)
keploy mcp serve

# Start on a custom port
keploy mcp serve --port 8090

# Start with stdio transport (for direct AI agent integration)
keploy mcp serve --transport stdio
```
 C:\Users\nehap\keploy\keploy.exe
### 2. List Available Tools

```bash
keploy mcp tools
```

Output:
```
Available MCP Tools:
====================

📦 keploy_mock_record
   Record mocks by capturing outgoing network calls during test execution.

📦 keploy_mock_test
   Run tests using recorded mocks for environment isolation.

📦 keploy_generate_tests
   Generate tests using Keploy's mocking feature with full record/test cycle.

📦 keploy_list_mocks
   List all recorded mock files with their contextual names.

📦 keploy_recording_status
   Get the status of the current or last recording session.

📦 keploy_test_status
   Get the status of the current or last test session.
```

### 3. Invoke Tools Directly

```bash
# Record mocks
keploy mcp invoke keploy_mock_record --params '{"testCommand": "go test ./..."}'

# Test with mocks
keploy mcp invoke keploy_mock_test --params '{"testCommand": "go test ./...", "testSetID": "test-set-0"}'

# Generate tests with full workflow
keploy mcp invoke keploy_generate_tests --params '{"testCommand": "npm test", "apiDescription": "User Authentication API"}'
```

## CLI Commands for Mock Recording & Replay

### Record Mocks

```bash
# Basic recording
keploy mock record -c "go test ./..."

# With npm
keploy mock record -c "npm test"

# With pytest
keploy mock record -c "pytest tests/"
```

### Test with Mocks

```bash
# Basic test
keploy mock test -c "go test ./..."

# Test specific test set
keploy mock test --test-set test-set-0 -c "npm test"

# With isolation validation
keploy mock test --validate-isolation -c "pytest tests/"
```

## MCP Protocol Integration

### HTTP Transport

The MCP server exposes the following HTTP endpoints:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/tools/list` | GET | List all available tools |
| `/tools/call` | POST | Invoke a tool with parameters |
| `/health` | GET | Health check endpoint |

#### Example: List Tools via HTTP

```bash
curl http://localhost:8090/tools/list
```

#### Example: Invoke Tool via HTTP

```bash
curl -X POST http://localhost:8090/tools/call \
  -H "Content-Type: application/json" \
  -d '{
    "name": "keploy_mock_record",
    "arguments": {
      "testCommand": "go test ./...",
      "apiDescription": "User API"
    }
  }'
```

### stdio Transport

For direct integration with AI agents, use the stdio transport:

```bash
keploy mcp serve --transport stdio
```

The server communicates using JSON-RPC 2.0 protocol over stdin/stdout.

## MCP Tool Reference

### keploy_mock_record

Record mocks by wrapping your test command and capturing all external dependencies.

**Parameters:**
| Name | Type | Required | Description |
|------|------|----------|-------------|
| `testCommand` | string | Yes | The test command to execute |
| `testSetName` | string | No | Custom name for the test set |
| `contextDescription` | string | No | Description for contextual mock naming |
| `metadata` | object | No | Additional metadata for mocks |

**Example:**
```json
{
  "testCommand": "go test ./...",
  "contextDescription": "User authentication endpoints",
  "testSetName": "auth-api-tests"
}
```

### keploy_mock_test

Run tests using previously recorded mocks to ensure environment isolation.

**Parameters:**
| Name | Type | Required | Description |
|------|------|----------|-------------|
| `testCommand` | string | Yes | The test command to execute |
| `testSetID` | string | No | Specific test set to use |
| `validateIsolation` | boolean | No | Validate no real calls were made (default: true) |

**Example:**
```json
{
  "testCommand": "npm test",
  "testSetID": "test-set-0",
  "validateIsolation": true
}
```

### keploy_generate_tests

High-level command that orchestrates the full record/replay cycle.

**Parameters:**
| Name | Type | Required | Description |
|------|------|----------|-------------|
| `testCommand` | string | Yes | The test command to execute |
| `apiDescription` | string | No | Description for intelligent mock naming |
| `autoReplay` | boolean | No | Run replay after recording (default: true) |

**Example:**
```json
{
  "testCommand": "pytest tests/",
  "apiDescription": "Payment gateway integration",
  "autoReplay": true
}
```

## Contextual Mock Naming

The MCP integration includes intelligent contextual naming for mock files based on:

1. **Mock Kind**: HTTP, Postgres, MySQL, MongoDB, Redis, gRPC, Generic
2. **HTTP Method**: GET → fetch, POST → create, PUT → update, DELETE → delete
3. **Endpoint Resource**: Extracted from URL path
4. **API Description**: Provided context for meaningful names
5. **Unique Hash**: Short hash for uniqueness

### Examples

| Mock Type | Generated Name |
|-----------|----------------|
| HTTP GET /api/v1/users | `http-fetch-users-a1b2c3d4` |
| HTTP POST /api/v1/orders | `http-create-orders-e5f6g7h8` |
| Postgres SELECT query | `postgres-query-i9j0k1l2` |
| MongoDB users collection | `mongo-users-m3n4o5p6` |
| Redis SET command | `redis-set-q7r8s9t0` |

## Workflow Phases

The MCP workflow orchestrator supports the following phases:

1. **Idle**: No workflow in progress
2. **Recording**: Capturing outgoing network calls
3. **Processing**: Applying contextual naming to mocks
4. **Replaying**: Running tests with recorded mocks
5. **Completed**: Workflow finished successfully
6. **Failed**: Workflow encountered an error

## AI Agent Integration Example

Here's how an AI agent can use natural language to trigger the workflow:

**User Prompt:** "Generate tests using Keploy mocking feature for my user API"

**AI Agent Action:**
```json
{
  "tool": "keploy_generate_tests",
  "arguments": {
    "testCommand": "go test ./...",
    "apiDescription": "User API",
    "autoReplay": true
  }
}
```

**Expected Flow:**
1. Agent invokes `keploy_generate_tests`
2. Keploy starts recording network calls
3. Test command executes, capturing external dependencies
4. Mocks are saved with contextual names like `http-fetch-users-abc123`
5. Replay phase runs tests with recorded mocks
6. Agent receives workflow result with stats and validation

## Configuration

The MCP server uses the standard Keploy configuration file (`keploy.yml`):

```yaml
# keploy.yml
path: "./keploy"
command: ""
record:
  filters: []
  metadata: ""
test:
  delay: 5
  mocking: true
```

## Troubleshooting

### Common Issues

1. **MCP server not responding**
   - Check if the server is running: `curl http://localhost:8090/health`
   - Verify the port is not in use

2. **Mocks not being recorded**
   - Ensure the test command makes network calls
   - Check bypass rules in configuration

3. **Replay failing**
   - Verify mocks exist in the test set
   - Check for mock format compatibility

### Debug Mode

Enable debug logging:

```bash
keploy mcp serve --debug
```

## API Reference

See the [MCP Protocol Specification](https://modelcontextprotocol.io/) for detailed protocol documentation.

## Contributing

Contributions to the MCP integration are welcome! Please see [CONTRIBUTING.md](../CONTRIBUTING.md) for guidelines.

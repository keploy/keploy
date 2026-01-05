# Keploy MCP User Guide

This guide explains how to use the Keploy MCP server with your AI coding assistant to record and replay mocks.

## Table of Contents

- [Introduction](#introduction)
- [Setup](#setup)
- [Available Tools](#available-tools)
- [Listing Mocks](#listing-mocks)
- [Recording Mocks](#recording-mocks)
- [Replaying Mocks](#replaying-mocks)
- [Example Workflows](#example-workflows)
- [Best Practices](#best-practices)
- [Troubleshooting](#troubleshooting)

---

## Introduction

The Keploy MCP server allows AI assistants to help you:

1. **List mocks** - Discover available recorded mock sets
2. **Record mocks** - Capture all external calls (APIs, databases, etc.) your app makes
3. **Replay mocks** - Run tests without needing real external services
4. **Smart naming** - Auto-generate descriptive names for your mock files

### What Can Be Recorded?

| Protocol | Examples |
|----------|----------|
| HTTP/HTTPS | REST APIs, GraphQL, webhooks |
| gRPC | Microservice calls |
| PostgreSQL | Database queries |
| MySQL | Database queries |
| MongoDB | Document operations |
| Redis | Cache operations |
| Generic TCP | Any TCP-based protocol |

---

## Setup

### Prerequisites

- **Keploy installed**: [Installation Guide](https://keploy.io/docs/server/installation/)
- **Linux** (or WSL on Windows) - Required for eBPF-based interception
- **MCP-compatible AI assistant**: VS Code with Copilot, Claude Desktop, Cursor

### Configuration

#### VS Code (GitHub Copilot / Copilot Chat)

Add to your VS Code settings (`settings.json`):

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

Or through the UI:
1. Open Settings (Cmd/Ctrl + ,)
2. Search for "MCP"
3. Add a new server with command `keploy` and args `["mcp", "serve"]`

#### Claude Desktop

Edit `~/.config/claude/claude_desktop_config.json` (Linux/Mac) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

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

#### Cursor

Add to Cursor settings:

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

### Verify Setup

After configuring, restart your AI assistant and ask:

> "What Keploy tools are available?"

You should see three tools listed:
- `keploy_list_mocks` - List available mock sets
- `keploy_mock_record` - Record mocks
- `keploy_mock_test` - Replay mocks during testing

---

## Available Tools

The Keploy MCP server provides three tools:

### 1. `keploy_list_mocks`

**Purpose**: Discover available recorded mock sets before testing.

**Parameters**:
| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `path` | string | No | `./keploy` | Path to search for mock files |

**Output**:
```json
{
  "success": true,
  "mockSets": ["my-app-http-stripe", "payment-feature-postgres"],
  "count": 2,
  "path": "./keploy",
  "message": "Found 2 mock set(s). The latest is 'my-app-http-stripe'."
}
```

### 2. `keploy_mock_record`

**Purpose**: Record outgoing calls from your application.

**Parameters**:
| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `command` | string | **Yes** | - | Application command (e.g., `go run main.go`, `npm start`) |
| `path` | string | No | `./keploy` | Path to store mock files |
| `duration` | string | No | `60s` | Recording duration (e.g., `60s`, `5m`, `2h`) |

**Output**:
```json
{
  "success": true,
  "mockFilePath": "./keploy/my-app-http-stripe/mocks.yaml",
  "mockCount": 12,
  "protocols": ["HTTP", "PostgreSQL"],
  "configuration": {
    "command": "npm start",
    "path": "./keploy",
    "duration": "60s"
  },
  "message": "Successfully recorded 12 mock(s) to './keploy/my-app-http-stripe/mocks.yaml'."
}
```

### 3. `keploy_mock_test`

**Purpose**: Replay recorded mocks during testing.

**Parameters**:
| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `command` | string | **Yes** | - | Test command (e.g., `go test -v`, `npm test`) |
| `mockName` | string | No | Latest mock | Name of mock set to use (from `keploy_list_mocks`) |
| `fallBackOnMiss` | boolean | No | `false` | Make real calls when no mock matches |

**Output**:
```json
{
  "success": true,
  "mocksReplayed": 12,
  "mocksMissed": 0,
  "appExitCode": 0,
  "configuration": {
    "command": "npm test",
    "mockName": "my-app-http-stripe",
    "fallBackOnMiss": false
  },
  "message": "Test passed! Replayed 12 mock(s), app exited successfully"
}
```

---

## Listing Mocks

Before replaying mocks, discover what's available:

### Basic Listing

Ask your AI assistant:

> "List available Keploy mocks"

or

> "What mock sets do I have recorded?"

The AI will use `keploy_list_mocks` and show you something like:

```
Found 3 mock set(s):
1. payment-feature-stripe (latest)
2. user-service-postgres
3. notification-email

Use any of these with keploy_mock_test by specifying the mockName parameter.
```

---

## Recording Mocks

### Basic Recording

Ask your AI assistant:

> "Record the external API calls from my application using `npm start`"

The AI will:
1. Start your app with Keploy intercepting outgoing calls
2. Capture all external requests and responses
3. Save them to a mock file
4. Report what was captured

### Recording Options

| Option | What to say | Example |
|--------|-------------|---------|
| Custom command | "Record mocks while running `<command>`" | "Record mocks while running `python app.py`" |
| Custom path | "Save mocks to `<path>`" | "Save mocks to `./tests/mocks`" |
| Duration | "Record for `<time>`" | "Record for 2 minutes" |

### During Recording

While recording:
- Make API requests to your app (if it's a server)
- Let your app make external calls (if it's a script/job)
- The AI will capture everything

### Recording Output

After recording, you'll see:

```
✓ Recorded 12 mocks:
  - 8 HTTP calls (api.stripe.com, api.sendgrid.com)
  - 4 PostgreSQL queries
  
Mock file saved to: ./keploy/order-service-stripe-postgres/mocks.yaml

Configuration used:
  Command: npm start
  Path: ./keploy
  Duration: 60s
```

---

## Replaying Mocks

### Recommended Workflow

The AI assistant follows this workflow:

1. **First**: Use `keploy_list_mocks` to show available mocks
2. **Then**: Use `keploy_mock_test` with the appropriate mock set
3. **Report**: Show results with configuration details

### Basic Replay

Ask your AI assistant:

> "Test my application with the recorded mocks"

or

> "Run `npm test` with mocks from `./keploy/order-service`"

The AI will:
1. List available mocks (if no specific mock specified)
2. Load the recorded mocks
3. Run your test command
4. Intercept outgoing calls and return mock responses
5. Report results with configuration used

### Replay Options

| Option | What to say | Example |
|--------|-------------|---------|
| Specific mocks | "Use mocks named `<name>`" | "Use mocks named `payment-stripe`" |
| Allow fallback | "Fall back to real calls if no mock matches" | "Test with mocks but allow real calls on miss" |
| Latest mocks | "Use the latest recorded mocks" | "Run tests with the latest mocks" |

### Replay Output

After replay:

```
✓ Test completed:
  - 12/12 mocks replayed
  - 0 mocks missed
  - Application exit code: 0
  
Configuration used:
  Command: npm test
  Mock Set: order-service-stripe-postgres
  Fallback on Miss: false
  
All external calls were successfully mocked!
```

### Handling Mock Misses

If mocks don't match:

```
⚠ Test completed with warnings:
  - 10/12 mocks replayed
  - 2 mocks missed:
    1. POST /api/v2/charges (new endpoint)
    2. GET /users?include_inactive=true (changed parameters)
    
Consider re-recording mocks to capture the new behavior.
```

---

## Example Workflows

### Workflow 1: New Feature Development

```
You: "I'm adding a new payment feature. Help me record the Stripe API calls."

AI: I'll first check if you have existing mocks.
    [Uses keploy_list_mocks]
    
    You have 2 existing mock sets. Now I'll record new mocks.
    [Starts recording with your dev server]

You: [Make test payments through your app]

AI: Recorded 5 Stripe API calls. Mock file saved to:
    ./keploy/payment-feature-stripe/mocks.yaml
    
    Configuration used:
    - Command: npm run dev
    - Duration: 60s
    - Detected: HTTP protocol

You: "Now run my tests with those mocks"

AI: [Uses keploy_list_mocks then keploy_mock_test]
    ✓ All tests passed with mocked Stripe calls
    
    Results:
    - Mocks replayed: 5
    - Mocks missed: 0
    - Exit code: 0
```

### Workflow 2: Discovering and Using Existing Mocks

```
You: "What mocks do I have available?"

AI: [Uses keploy_list_mocks]
    
    Found 4 mock sets:
    1. payment-feature-stripe (latest)
    2. user-auth-postgres
    3. notification-sendgrid
    4. integration-full
    
    Would you like to run tests with any of these?

You: "Run tests with the user-auth-postgres mocks"

AI: [Uses keploy_mock_test with mockName: "user-auth-postgres"]
    
    ✓ Test completed successfully!
    - Mocks replayed: 23
    - Exit code: 0
```

### Workflow 3: CI/CD Integration

```
You: "How can I use these mocks in CI?"

AI: You can use Keploy directly in CI without the MCP server:

    # In your CI workflow
    keploy mock test -c "npm test" --mock-name payment-feature-stripe

    # If you omit --mock-name, Keploy replays the latest mock set in ./keploy
    
    Or keep using the MCP server locally for interactive development,
    and use CLI commands in CI.
```

### Workflow 4: Debugging Test Failures

```
You: "My tests are failing with mock misses. What's happening?"

AI: Let me first check what mocks you have.
    [Uses keploy_list_mocks]
    
    Found mock set: user-service-v2
    
    Now let me run with fallback enabled to see what's missing.
    [Uses keploy_mock_test with fallBackOnMiss: true]
    
    I found 2 calls that didn't match existing mocks:
    
    1. POST /api/users - The request body changed
       Old: {"name": "John"}
       New: {"name": "John", "role": "admin"}
    
    2. GET /api/config - New endpoint not previously recorded
    
    Would you like me to re-record mocks to capture these changes?
```

### Workflow 5: Database Testing

```
You: "Record my app's PostgreSQL queries"

AI: I'll record database calls along with any HTTP calls.
    [Uses keploy_mock_record]

You: [Run your app with database operations]

AI: Recorded 15 mocks:
    - 12 PostgreSQL queries (users, orders, products tables)
    - 3 HTTP calls to external APIs
    
    Configuration:
    - Command: python manage.py test
    - Duration: 120s
    - Protocols: PostgreSQL, HTTP
    
    Your tests can now run without needing a real database!
```

---

## Best Practices

### 1. Organize Mocks by Feature

```
./keploy/
├── user-auth/              # Authentication feature mocks
├── payment-processing/     # Payment feature mocks
├── notification-service/   # Notification mocks
└── integration-tests/      # Full integration test mocks
```

### 2. Use `keploy_list_mocks` Before Testing

Always check available mocks before running tests:

> "List my available mocks, then run tests with the payment mocks"

### 3. Re-record After API Changes

When external APIs change their responses:

> "Re-record mocks for the payment feature"

### 4. Use Descriptive Commands

Bad:
> "Record mocks"

Good:
> "Record HTTP calls to Stripe API while running `npm run test:payments` for 2 minutes"

### 5. Review Tool Output

The MCP tools return detailed configuration information. Review it to ensure:
- Correct command was used
- Expected duration
- Right mock set selected

### 6. Keep Mocks in Version Control

```bash
git add ./keploy/
git commit -m "Add payment feature mocks"
```

---

## Troubleshooting

### "MCP server not available"

**Cause**: Keploy MCP server isn't running or configured.

**Solution**:
1. Verify Keploy is installed: `keploy --version`
2. Check your AI assistant configuration
3. Restart your AI assistant
4. Try running manually: `keploy mcp serve`

### "No mocks were recorded"

**Cause**: App didn't make external calls, or calls weren't intercepted.

**Solutions**:
- Verify your app makes outgoing calls during the recording period
- Ensure you're running on Linux (or WSL)
- Check if the duration is sufficient
- Try: "Record for 5 minutes" instead of default 60 seconds

### "Mock replayer service is not available"

**Cause**: The mock replay service isn't properly initialized.

**Solution**:
- Ensure you have mocks recorded first
- Check the ./keploy directory exists
- Try re-recording mocks

### "No mock sets found"

**Cause**: No mocks have been recorded yet.

**Solution**:
- Use `keploy_mock_record` to create mocks first
- Check the path parameter is correct
- Ensure recording completed successfully

### "Mock mismatch during replay"

**Cause**: Request parameters changed since recording.

**Solutions**:
- Re-record mocks to capture current behavior
- Enable fallback: "Test with mocks but allow fallback to real calls"
- Check what changed by reviewing the tool output

### "Permission denied"

**Cause**: Keploy needs elevated permissions for eBPF.

**Solution**:
```bash
sudo keploy mcp serve
```

Or configure your AI assistant to use sudo.

### "command is required" Error

**Cause**: The command parameter wasn't provided to record/test.

**Solution**: Always specify the command explicitly:
> "Record mocks while running `go run main.go`"

### Tool Returns Error with Configuration

The MCP tools always return the configuration used, even on error. This helps debug:

```json
{
  "success": false,
  "configuration": {
    "command": "npm start",
    "path": "./keploy",
    "duration": "60s"
  },
  "message": "Recording failed: permission denied"
}
```

---

## FAQ

### Q: What are the three MCP tools?

**A**: 
1. `keploy_list_mocks` - List available recorded mock sets
2. `keploy_mock_record` - Record outgoing calls from your app
3. `keploy_mock_test` - Replay mocks during testing

### Q: Can I use recorded mocks in CI/CD?

**A**: Yes! Mocks are stored as YAML files. Use `keploy mock test -c "<command>"` in CI without the MCP server.

### Q: How does the AI know which mock to use?

**A**: The AI uses `keploy_list_mocks` first to discover available mocks, then uses `keploy_mock_test` with the appropriate `mockName`. If no name is specified, it uses the latest mock set.

### Q: How are mocks matched?

**A**: Keploy matches based on:
- HTTP: Method, URL, headers, body
- gRPC: Service, method, request message
- Database: Query pattern, parameters

### Q: Can I edit recorded mocks?

**A**: Yes! Mock files are human-readable YAML. You can:
- Modify response data
- Add/remove headers
- Change status codes

### Q: What's the default recording duration?

**A**: 60 seconds. You can change it with: "Record for 5 minutes"

### Q: What if my mock name has spaces?

**A**: Use quotes when asking the AI: "Use mocks named `my feature test`"

### Q: Can I record WebSocket connections?

**A**: Currently, WebSocket support is limited. HTTP/gRPC/database protocols are fully supported.

### Q: What does `fallBackOnMiss` do?

**A**: When enabled, if a request doesn't match any recorded mock, Keploy makes the real external call instead of failing. Useful for debugging what changed.

---

## Getting Help

- **Documentation**: [keploy.io/docs](https://keploy.io/docs)
- **GitHub Issues**: [Report a bug](https://github.com/keploy/keploy/issues)
- **Community Slack**: [Join Keploy Slack](https://join.slack.com/t/keploy/shared_invite/)
- **Discussions**: [GitHub Discussions](https://github.com/keploy/keploy/discussions)

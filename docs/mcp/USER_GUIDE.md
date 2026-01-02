# Keploy MCP User Guide

This guide explains how to use the Keploy MCP server with your AI coding assistant to record and replay mocks.

## Table of Contents

- [Introduction](#introduction)
- [Setup](#setup)
- [Recording Mocks](#recording-mocks)
- [Replaying Mocks](#replaying-mocks)
- [Example Workflows](#example-workflows)
- [Best Practices](#best-practices)
- [Troubleshooting](#troubleshooting)

---

## Introduction

The Keploy MCP server allows AI assistants to help you:

1. **Record mocks** - Capture all external calls (APIs, databases, etc.) your app makes
2. **Replay mocks** - Run tests without needing real external services
3. **Smart naming** - Auto-generate descriptive names for your mock files

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

You should see `keploy_mock_record` and `keploy_mock_test` listed.

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
```

---

## Replaying Mocks

### Basic Replay

Ask your AI assistant:

> "Test my application with the recorded mocks"

or

> "Run `npm test` with mocks from `./keploy/order-service`"

The AI will:
1. Load the recorded mocks
2. Run your test command
3. Intercept outgoing calls and return mock responses
4. Report results

### Replay Options

| Option | What to say | Example |
|--------|-------------|---------|
| Specific mocks | "Use mocks from `<path>`" | "Use mocks from `./keploy/user-service`" |
| Allow fallback | "Fall back to real calls if no mock matches" | "Test with mocks but allow real calls on miss" |

### Replay Output

After replay:

```
✓ Test completed:
  - 12/12 mocks replayed
  - 0 mocks missed
  - Application exit code: 0
  
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

AI: I'll record the external calls while you test your feature.
    [Starts recording with your dev server]

You: [Make test payments through your app]

AI: Recorded 5 Stripe API calls. Mock file saved to:
    ./keploy/payment-feature-stripe/mocks.yaml

You: "Now run my tests with those mocks"

AI: [Runs tests with mocks]
    ✓ All tests passed with mocked Stripe calls
```

### Workflow 2: CI/CD Integration

```
You: "How can I use these mocks in CI?"

AI: You can use Keploy directly in CI without the MCP server:

    # In your CI workflow
    keploy mock test -c "npm test" --mockPath ./keploy/payment-feature-stripe
    
    Or keep using the MCP server locally for interactive development,
    and use CLI commands in CI.
```

### Workflow 3: Debugging Test Failures

```
You: "My tests are failing with mock misses. What's happening?"

AI: Let me run with detailed output and fallback enabled.
    [Runs with fallBackOnMiss: true]
    
    I found 2 calls that didn't match existing mocks:
    
    1. POST /api/users - The request body changed
       Old: {"name": "John"}
       New: {"name": "John", "role": "admin"}
    
    2. GET /api/config - New endpoint not previously recorded
    
    Would you like me to re-record mocks to capture these changes?
```

### Workflow 4: Database Testing

```
You: "Record my app's PostgreSQL queries"

AI: I'll record database calls along with any HTTP calls.
    [Starts recording]

You: [Run your app with database operations]

AI: Recorded 15 mocks:
    - 12 PostgreSQL queries (users, orders, products tables)
    - 3 HTTP calls to external APIs
    
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

### 2. Re-record After API Changes

When external APIs change their responses:

> "Re-record mocks for the payment feature"

### 3. Use Descriptive Commands

Bad:
> "Record mocks"

Good:
> "Record HTTP calls to Stripe API while running `npm run test:payments`"

### 4. Review Recorded Mocks

After recording, review the generated YAML:

> "Show me what was recorded in the latest mock file"

### 5. Keep Mocks in Version Control

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

### "No mocks were recorded"

**Cause**: App didn't make external calls, or calls weren't intercepted.

**Solutions**:
- Verify your app makes outgoing calls during the recording period
- Ensure you're running on Linux (or WSL)
- Check if the duration is sufficient
- Try: "Record for 5 minutes" instead of default 60 seconds

### "Mock mismatch during replay"

**Cause**: Request parameters changed since recording.

**Solutions**:
- Re-record mocks to capture current behavior
- Enable fallback: "Test with mocks but allow fallback to real calls"
- Check what changed: "What mocks missed during the last test?"

### "Permission denied"

**Cause**: Keploy needs elevated permissions for eBPF.

**Solution**:
```bash
sudo keploy mcp serve
```

Or configure your AI assistant to use sudo.

### "LLM callback failed"

**Cause**: AI assistant doesn't support the CreateMessage API.

**Impact**: None - falls back to deterministic naming like `my-app-http-20240102150405`

**Note**: This is expected behavior, not an error.

---

## FAQ

### Q: Can I use recorded mocks in CI/CD?

**A**: Yes! Mocks are stored as YAML files. Use `keploy mock test -c "<command>"` in CI without the MCP server.

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

### Q: What's the file size limit?

**A**: There's no hard limit, but very large responses (>10MB) may slow down recording/replay.

### Q: Can I record WebSocket connections?

**A**: Currently, WebSocket support is limited. HTTP/gRPC/database protocols are fully supported.

---

## Getting Help

- **Documentation**: [keploy.io/docs](https://keploy.io/docs)
- **GitHub Issues**: [Report a bug](https://github.com/keploy/keploy/issues)
- **Community Slack**: [Join Keploy Slack](https://join.slack.com/t/keploy/shared_invite/)
- **Discussions**: [GitHub Discussions](https://github.com/keploy/keploy/discussions)

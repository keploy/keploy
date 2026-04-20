# Keploy MCP Server

An [MCP (Model Context Protocol)](https://modelcontextprotocol.io/) server for [Keploy](https://keploy.io/). This server acts as a bridge between AI agents/tools and your Keploy-managed projects, allowing AI to access test statistics and other project metadata programmatically.

## Features

- **Get Test Statistics**: Retrieve the total number of recorded test cases in a Keploy project.

## Installation

1. **Clone the repository:**
   ```bash
   git clone <repository-url>
   cd keploy/mcp-server
   ```

2. **Install dependencies:**
   ```bash
   npm install
   ```

3. **Build the server:**
   ```bash
   npm run build
   ```

## Configuration

To use this server with an MCP-compatible client (like Claude Desktop, Cursor, or other AI agents), add the following configuration to your MCP config file (e.g., `~/.config/Claude/mcp_config.json` or project specific config).

```json
{
  "mcpServers": {
    "keploy": {
      "command": "node",
      "args": ["/absolute/path/to/keploy/mcp-server/dist/src/index.js"],
      "env": {
        "NODE_ENV": "production"
      }
    }
  }
}
```

*Note: Replace `/absolute/path/to/...` with the actual absolute path to the `dist/src/index.js` file on your system.*

## Usage

### Tools

#### `get_test_statistics`
Returns statistics about the test cases in a given project.

**Arguments:**
- `projectPath` (string, required): The absolute path to the project root containing the `keploy` directory.

**Example Request:**
```json
{
  "name": "get_test_statistics",
  "arguments": {
    "projectPath": "/home/user/my-node-app"
  }
}
```

**Example Response:**
```json
{
  "totalTests": 15,
  "passedTests": 0,
  "failedTests": 0,
  "testSets": 2,
  "lastRun": "2024-02-06T12:00:00.000Z"
}
```

## Development

- **Run in watch mode:**
  ```bash
  npm run dev
  ```

- **Run verification script:**
  (Requires a sample project at a known path)
  ```bash
  npx tsx verify-client.ts
  ```

## License

[License Name]

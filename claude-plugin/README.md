# Keploy plugin for Claude Code

Connects Claude Code to the [Keploy](https://keploy.io) MCP server so Claude can
generate, mock and run API tests against your application.

Keploy captures real API, database and Kafka traffic with eBPF and replays it as
deterministic tests with auto-generated mocks.

## What Claude can do with it

- Register an app and record real traffic under the Keploy eBPF agent
- Generate chained integration test suites from that traffic, or from OpenAPI
  specs, curl commands and Postman collections
- Run suites against virtualized dependencies: Postgres, MongoDB, Kafka, RabbitMQ
- Report statement, branch, schema and business-flow coverage
- Flag uncovered endpoints and schema drift
- Scaffold a GitHub Actions workflow

## Install

Install from the Claude plugin directory, then set your Keploy API key when
prompted. Generate a key at https://app.keploy.io/settings/api-keys — it is shown
once, so copy it immediately.

## Configuration

| Setting   | Required | Description                                  |
| --------- | -------- | -------------------------------------------- |
| `api_key` | yes      | Keploy API key, starting with `kep_`          |

The key is injected into the MCP request headers at runtime through
`${user_config.api_key}`. No credential is stored in this repository.

## MCP server

- Endpoint: `https://api.keploy.io/client/v1/mcp`
- Transport: streamable HTTP
- Auth header: `X-API-Key`
- Official MCP registry entry: `io.github.keploy/mcp`

## Layout

```
claude-plugin/
├── .claude-plugin/plugin.json   plugin manifest
├── .mcp.json                    MCP server definition
├── skills/setup/SKILL.md        guides Claude through connecting the server
└── README.md
```

## Links

- Docs: https://keploy.io/docs/running-keploy/agent-test-generation/
- Keploy: https://github.com/keploy/keploy

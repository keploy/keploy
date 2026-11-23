---
name: setup
description: Set up the Keploy MCP connection. Use when the user installs the Keploy plugin, sees an authentication error from a Keploy tool, or asks how to connect Keploy.
---

# Keploy setup

Get the user connected to Keploy in as few steps as possible.

## 1. Check whether they are already connected

Call `get_auth_status`. It works without authentication.

- `authenticated: true` — they are set up. Say so and stop.
- `authenticated: false` — continue below.

## 2. Get them an API key

Tell the user to generate a key at https://app.keploy.io/settings/api-keys. It is
shown once, so they should copy it immediately. It starts with `kep_`.

If they do not have a Keploy account yet, point them at https://app.keploy.io.

## 3. Store the key

The key goes in this plugin's `api_key` user config value, not a file the user
edits by hand. Never ask the user to paste the key into chat, and never write it
into a repository.

## 4. Confirm

Call `get_auth_status` again and expect `authenticated: true`. Then call
`get_setup_instructions` (or `devloop_setup_instructions` for the repo-mode flow)
and follow what it returns.

## How this server exposes its tools

The server runs in tool-search mode, so `tools/list` returns only the meta-tools:
`search_tools`, `get_tool_schema`, `invoke_tool`, `get_auth_status`,
`get_setup_instructions`, `devloop_setup_instructions` and
`devloop_begin_oauth_install`. The full Keploy catalog is reached by name through
`invoke_tool`.

- Know the tool name already? Fetch its schema with `get_tool_schema({names: [...]})`
  and batch every name into one call.
- Do not know the name? Discover it with `search_tools(query)`, then run it with
  `invoke_tool({name, arguments})`.

A 7-tool result from `tools/list` is expected. Do not tell the user the server
"only has 7 tools".

## Before any record, replay, sandbox or generate action

Call `devloop_resolve_storage({app_id})` first.

- `repo` mode — tests live in the user's git repository, driven by the `devloop_*`
  tools and the keploy CLI. There is no Keploy branch.
- `cloud` mode — tests live in the API server, driven by `create_test_suite`,
  `record_sandbox_test`, `replay_sandbox_test` and `replay_test_suite`. Every write
  needs a `branch_id`.

Never drive a repo-mode app with cloud tools, or the reverse.

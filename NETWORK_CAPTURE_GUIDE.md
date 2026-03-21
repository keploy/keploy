# Keploy Network Capture Guide

## Overview

Keploy Network Capture (`.kpcap` files) records raw network packets flowing through the Keploy proxy during `record` or `test` mode. Combined with debug logs and mocks, this creates a complete reproduction package for diagnosing issues.

**Use case**: When a customer reports a problem with record/replay, they share:
1. The `.kpcap` capture file
2. Debug logs
3. Mock files
4. Keploy configuration

The Keploy team can then replay the exact network traffic to reproduce the issue, fix it, and validate the fix.

---

## Quick Start

### Capturing Network Traffic

Network capture is automatically enabled when using the `--debug` flag:

```bash
# Record mode with capture
keploy record -c "your-app-command" --debug

# Test mode with capture
keploy test -c "your-app-command" --debug
```

The capture file is saved to `keploy/debug/capture_<mode>_<timestamp>.kpcap`.

### Analyzing a Capture File

```bash
keploy debug analyze keploy/debug/capture_record_20240101_120000.kpcap
```

Output:
```
═══════════════════════════════════════════════════
  Keploy Network Capture Analysis Report
═══════════════════════════════════════════════════

  File:       capture_record_20240101_120000.kpcap
  Mode:       record
  Created:    2024-01-01T12:00:00Z
  OS/Arch:    linux/amd64
  Duration:   2m30s
  Total Data: 1.2 MB
  Connections: 15

─── Protocol Breakdown ───────────────────────────
  HTTP:        8 connections
  MySQL:       4 connections
  Redis:       2 connections
  Generic:     1 connections

─── Connection Details ───────────────────────────
  [1] Connection #1
       Protocol:  HTTP
       Source:    127.0.0.1:54321
       Dest:      10.0.0.1:80
       Duration:  150ms
       Packets:   4
       Client→:   512 B
       ←Server:   1.2 KB
  ...
═══════════════════════════════════════════════════
```

### Creating a Debug Bundle

Package everything needed for issue reproduction:

```bash
keploy debug bundle \
  --capture keploy/debug/capture_record_20240101_120000.kpcap \
  --mocks keploy/mocks \
  --tests keploy/tests \
  --log keploy-debug.log \
  --config keploy.yml \
  --notes "MySQL mock matching fails on test-set-0:test-1"
```

This creates a `keploy-debug-bundle_debug_<timestamp>.tar.gz` file.

### Extracting a Debug Bundle

```bash
keploy debug extract keploy-debug-bundle_debug_20240101_120000.tar.gz --dir ./debug-output
```

### Validating a Capture File

```bash
keploy debug validate capture.kpcap

# JSON output for automation
keploy debug validate capture.kpcap --json
```

### Replaying Captured Traffic

Replay traffic against a running proxy to reproduce issues:

```bash
# Start your app with keploy in test mode first
keploy test -c "your-app-command"

# In another terminal, replay the capture
keploy debug replay --proxy localhost:16789 capture.kpcap

# JSON output for CI
keploy debug replay --proxy localhost:16789 capture.kpcap --json
```

---

## Configuration

### Via `keploy.yml`

```yaml
# Enable network capture (auto-enabled with --debug)
capture:
  enabled: true
  path: "keploy/debug"     # directory for .kpcap files
  bundle: true              # reserved for future use; currently has no effect
```

### Via CLI Flags

```bash
# Explicitly enable capture without full debug mode
keploy record -c "./app" --debug
```

---

## Supported Protocols

Network capture records raw bytes for ALL protocols supported by Keploy:

| Protocol | Detection | Capture |
|----------|-----------|---------|
| HTTP/1.x | Method prefix (`GET`, `POST`, etc.) | Full request/response headers + body |
| HTTP/2   | Connection preface | Binary frames |
| gRPC     | HTTP/2 with content-type detection | Binary frames |
| MySQL    | Port 3306 or wire protocol | Wire protocol packets |
| PostgreSQL | Wire protocol prefix bytes | Wire protocol packets |
| MongoDB  | Wire protocol header | OP_MSG/OP_QUERY packets |
| Redis    | RESP protocol prefix | RESP commands + responses |
| Kafka    | Wire protocol header | Request/response messages |
| DNS      | UDP/TCP DNS queries | Query + response packets |
| Generic  | Fallback for unknown protocols | Raw binary data |

### TLS/SSL Connections

TLS connections are captured **after** TLS termination at the proxy level, so the captured data contains decrypted application-layer bytes. The `isTLS` flag is purely informational metadata — during replay, TLS connections are replayed normally (as plaintext) just like non-TLS connections.

---

## File Format

### `.kpcap` (Keploy Packet Capture)

Binary format with the following structure:

```
┌──────────────────────────────┐
│ File Header                  │
│   Magic: "KPCAP\x00\x01\x00"│
│   Version: uint16            │
│   Mode: uint8                │
│   CreatedAt: int64           │
│   MetadataLen: uint32        │
│   Metadata: JSON             │
├──────────────────────────────┤
│ Packet 1                     │
│   Timestamp: int64           │
│   ConnectionID: uint64       │
│   Type: uint8                │
│   Direction: uint8           │
│   Protocol: uint8            │
│   Flags: uint8               │
│   SrcAddr: length-prefixed   │
│   DstAddr: length-prefixed   │
│   Payload: length-prefixed   │
├──────────────────────────────┤
│ Packet 2...N                 │
└──────────────────────────────┘
```

### Packet Types

| Type | Name | Description |
|------|------|-------------|
| 0 | DATA | Actual payload data |
| 1 | CONN_OPEN | New connection established |
| 2 | CONN_CLOSE | Connection closed |
| 3 | PROTOCOL | Protocol detected |
| 4 | ERROR | Error during handling |
| 5 | DNS | DNS query/response |

### Data Directions

| Direction | Name | Description |
|-----------|------|-------------|
| 0 | client→proxy | App sending to proxy |
| 1 | proxy→dest | Proxy forwarding to external service |
| 2 | dest→proxy | External service responding |
| 3 | proxy→client | Proxy sending back to app |

---

## Debug Bundle Contents

A debug bundle (`.tar.gz`) contains:

```
keploy-debug-bundle/
├── manifest.json          # Bundle metadata
├── capture/
│   └── capture_*.kpcap    # Network capture file
├── mocks/
│   ├── mock-1.yaml        # Recorded mocks
│   └── mock-2.yaml
├── tests/
│   ├── test-1.yaml        # Test cases
│   └── test-2.yaml
├── logs/
│   └── debug.log          # Debug log file
└── keploy.yml             # Configuration
```

### `manifest.json`

```json
{
  "version": "1",
  "created_at": "2024-01-01T12:00:00Z",
  "mode": "record",
  "app_name": "my-app",
  "capture_file": "capture/capture_record_20240101_120000.kpcap",
  "mock_dir": "mocks",
  "test_dir": "tests",
  "log_file": "logs/debug.log",
  "config_file": "keploy.yml",
  "notes": "MySQL mock matching fails on test-set-0:test-1"
}
```

---

## Workflow: Reproducing a Customer Issue

### For the Customer

1. Reproduce the issue with `--debug`:
   ```bash
   keploy record -c "./app" --debug
   # or
   keploy test -c "./app" --debug
   ```

2. Create a debug bundle:
   ```bash
   keploy debug bundle --notes "Description of the issue"
   ```

3. Share the `.tar.gz` bundle file with the Keploy team.

### For the Keploy Team

1. Extract the bundle:
   ```bash
   keploy debug extract customer-bundle.tar.gz --dir ./debug
   ```

2. Analyze the capture:
   ```bash
   keploy debug analyze debug/keploy-debug-bundle/capture/*.kpcap
   ```

3. Review connection details, error events, and protocol breakdown.

4. Fix the issue in the Keploy codebase.

5. Validate the fix by replaying:
   ```bash
   # Start the fixed keploy with the customer's mocks
   cp -r debug/keploy-debug-bundle/mocks ./keploy/
   keploy test -c "./app" --debug

   # Replay the original capture
   keploy debug replay --proxy localhost:16789 debug/keploy-debug-bundle/capture/*.kpcap
   ```

6. Verify all connections match (no mismatches in the replay summary).

---

## Limits and Safety

| Limit | Value | Purpose |
|-------|-------|---------|
| Max payload per packet | 16 MB | Prevent OOM from large payloads |
| Max packets per file | 10,000,000 | Prevent runaway captures |
| Max metadata size | 1 MB | Prevent corrupt file OOM |
| Max file in bundle | 100 MB | Prevent huge bundles |
| Address length | 65535 bytes | Sanity check for corrupt files |

---

## Troubleshooting

### Capture file is empty or missing
- Ensure `--debug` flag is used
- Check that the proxy is actually intercepting traffic (look for "Proxy started" log)
- Verify the capture path in config is writable

### Replay shows all connections as "skipped"
- Only connections with zero data packets are skipped
- Check if the capture file contains actual traffic (use `keploy debug validate`)
- Check if the proxy address is correct

### Bundle creation fails
- Ensure the capture file path is valid
- Check disk space for the bundle output
- Verify mock/test directories exist

### Validation reports errors
- The capture file may be truncated (keploy was killed during capture)
- Try analyzing the file anyway — partial captures are still useful

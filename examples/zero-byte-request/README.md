# Zero-byte Request Reproduction

This example demonstrates how to trigger the Keploy tracker warning:

```
Malformed request ExpectedRecvBytes=0 ActualRecvBytes=0
```

The warning originates from `pkg/core/hooks/conn/tracker.go` when the tracker attempts to validate a request/response pair but finds the request byte counts are zero. This scenario can occur if a response is written on a connection before any request bytes are observed (or if eBPF missed ingress bytes).

## How It Works

`main.go` starts a raw TCP server on `:8087` and immediately sends an HTTP/1.1 response without reading from the client socket. It then sleeps for 3 seconds and closes the connection. The Keploy tracker logic (on Linux with eBPF hooks active) will see an egress chunk first, mark internal flags, and later try to verify the preceding request sizes—which are zero—leading to the warning.

`client.go` is an optional helper that connects and stays idle so the server's response arrives first. After a delay it sends a late GET request, further exaggerating the mismatch.

## Steps to Reproduce

1. Build/run Keploy in record mode so that network traffic for this process is captured.
2. In one terminal run:

```bash
go run ./examples/zero-byte-request/main.go
```

3. In another terminal, either:
   - Use netcat:

     ```bash
     nc localhost 8087
     # do nothing for >3s
     ```

   - Or run the provided client:

     ```bash
     go run ./examples/zero-byte-request/client
     ```

4. Observe Keploy logs; you should see the warning line similar to:

```
{"level":"warn","msg":"Malformed request","ExpectedRecvBytes":0,"ActualRecvBytes":0}
```

(Exact JSON/fields depend on the configured zap logger.)

## Notes

- Multiple simultaneous idle connections amplify the effect.
- Adjust sleep durations in `main.go` and `client.go` if needed.
- If the warning does not appear, ensure Keploy's eBPF hooks are attached to the running process and that you're on Linux.

## Cleanup

Terminate the server with Ctrl+C. Close any client/netcat sessions.

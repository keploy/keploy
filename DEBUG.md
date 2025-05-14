# 🐰 Keploy Debugging Guide: 

## 1) Capturing Stack Traces with `SIGQUIT`

If Keploy appears stuck or unresponsive, and you want a quick insight into what it's doing (e.g., which goroutines are running), you can send a `SIGQUIT` signal. This is a fast alternative to setting up `pprof` and will make the Go runtime dump a full stack trace to `stderr`.

---

## 🔧 **When Keploy is Running Natively (on the Host)**

### 1. **Find the PID of the Keploy process**

You can use the proxy port Keploy listens on (`16789` by default) to locate the process:

```bash
sudo lsof -i:16789
```
Example output:

```bash
keploy  12345 root  ... TCP *:16789 ...
```

In this case, 12345 is the PID of the keploy process.

### 2. Send a SIGQUIT signal to the Keploy process
```bash
sudo kill -SIGQUIT 12345
```

The Go runtime will then print a full goroutine stack trace to the terminal or to wherever your logs are configured to go.



## 🐳 When Keploy is Running in Docker (with --pid=host)

If Keploy is running in a Docker container with the --pid=host flag, docker kill --signal=SIGQUIT won't work because Keploy is not PID 1 inside the container. You'll need to send the signal to the actual host PID.

### 1. Find the host PID of the /app/keploy process

Use docker top to inspect the container's process tree:

```bash 
docker top keploy-v2
```

Look for a line where the command starts with /app/keploy:
```bash
UID   PID      PPID   C   STIME   TTY   TIME     CMD
root  341003   ...    ... ...     ...   ...      /app/keploy record -c ...
```
In this case, 341003 is the host PID of the running Go binary inside the container.

### 2. Send SIGQUIT to that PID from the host

```bash
sudo kill -SIGQUIT 341003
```

This will cause the Go runtime to emit a full stack trace from the running keploy process.

You should see a stack trace similar to:
```bash
SIGQUIT: quit
goroutine 1 [running]:
main.main()
    /app/main.go:42 +0x123
...
```

⸻

🧠 Notes
	•	This is a quick way to inspect a hung or slow keploy run without setting up full profiling.
	•	Make sure the Go binary is not built with -ldflags="-s -w" — that strips debug symbols, making stack traces useless.
	•	Do not intercept SIGQUIT in your code using signal.Notify(..., syscall.SIGQUIT) — it prevents the Go runtime from printing the trace.
	•	When using --pid=host, ensure you signal the actual host PID, not via docker kill.

⸻
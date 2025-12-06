# Critical Bug: Resource Leak in StopProxyServer and handleConnection

## Summary
The `StopProxyServer` function in `pkg/agent/proxy/proxy.go` has early returns that skip critical cleanup operations, causing resource leaks including mutex deadlocks, unclosed listeners, DNS servers, and error channels.

## Location
**File:** `pkg/agent/proxy/proxy.go`  
**Lines:** 664-696 (StopProxyServer) and 365-394 (handleConnection defer)

## Severity
**CRITICAL** - GSOC Level Bug
- Resource leaks (listeners, DNS servers, channels)
- Mutex deadlock risk
- Goroutine leaks
- Port binding issues

## The Bug

### Bug 1: StopProxyServer Early Returns (Lines 664-696)

```go
func (p *Proxy) StopProxyServer(ctx context.Context) {
	<-ctx.Done()

	p.logger.Info("stopping proxy server...")

	p.connMutex.Lock()
	for _, clientConn := range p.clientConnections {
		err := clientConn.Close()
		if err != nil {
			return  // ❌ BUG: Early return skips all cleanup below!
		}
	}
	p.connMutex.Unlock()  // This may never execute!

	if p.Listener != nil {
		err := p.Listener.Close()  // This may never execute!
		// ...
	}

	err := p.stopDNSServers(ctx)  // This may never execute!
	if err != nil {
		utils.LogError(p.logger, err, "failed to stop the dns servers")
		return  // ❌ BUG: Another early return!
	}

	p.CloseErrorChannel()  // This may never execute!
}
```

**Problem:** Two early returns skip critical cleanup:
1. **Line 673:** If closing any connection fails, function returns immediately, skipping:
   - Mutex unlock (line 676) → **DEADLOCK RISK**
   - Listener close (line 678-683) → **PORT LEAK**
   - DNS server stop (line 686-690) → **RESOURCE LEAK**
   - Error channel close (line 693) → **GOROUTINE LEAK**

2. **Line 689:** If stopping DNS servers fails, function returns, skipping:
   - Error channel close (line 693) → **GOROUTINE LEAK**

### Bug 2: handleConnection Defer Early Returns (Lines 365-394)

```go
defer func() {
	parserCtxCancel()

	if srcConn != nil {
		err := srcConn.Close()
		if err != nil {
			// ... error handling ...
			return  // ❌ BUG: Skips dstConn.Close() and parserErrGrp.Wait()!
		}
	}

	if dstConn != nil {
		err = dstConn.Close()
		if err != nil {
			// ... error handling ...
			return  // ❌ BUG: Skips parserErrGrp.Wait()!
		}
	}

	err = parserErrGrp.Wait()  // This may never execute!
	// ...
}()
```

**Problem:** Early returns in defer skip cleanup:
1. **Line 374:** If `srcConn.Close()` fails, skips:
   - `dstConn.Close()` → **CONNECTION LEAK**
   - `parserErrGrp.Wait()` → **GOROUTINE LEAK**

2. **Line 386:** If `dstConn.Close()` fails, skips:
   - `parserErrGrp.Wait()` → **GOROUTINE LEAK**

## Impact

### Resource Leaks
- **Mutex Deadlock:** Mutex remains locked if connection close fails
- **Port Leak:** Listener not closed, port remains bound
- **DNS Server Leak:** DNS servers continue running
- **Channel Leak:** Error channel not closed, goroutines waiting on it leak
- **Connection Leak:** Destination connections not closed
- **Goroutine Leak:** Error groups not waited, goroutines leak

### Production Impact
- Server becomes unresponsive (mutex deadlock)
- Ports cannot be reused (listener leak)
- Memory leaks from leaked goroutines
- DNS resolution issues (DNS servers not stopped)
- System resource exhaustion over time

## How to Reproduce

### Option 1: Run the Test
```bash
cd pkg/agent/proxy
go test -v -run TestStopProxyServer_ResourceLeak
```

### Option 2: Manual Reproduction
1. Start proxy server
2. Create connections that will fail to close (e.g., already closed)
3. Trigger `StopProxyServer`
4. Observe:
   - Mutex remains locked (deadlock on next operation)
   - Port still bound (netstat shows listener)
   - DNS servers still running
   - Error channel not closed

## Expected Behavior
All cleanup operations should execute regardless of individual errors. Errors should be logged but not cause early returns that skip subsequent cleanup.

## Suggested Fix

### Fix 1: StopProxyServer
```go
func (p *Proxy) StopProxyServer(ctx context.Context) {
	<-ctx.Done()

	p.logger.Info("stopping proxy server...")

	p.connMutex.Lock()
	for _, clientConn := range p.clientConnections {
		err := clientConn.Close()
		if err != nil {
			utils.LogError(p.logger, err, "failed to close client connection")
			// Continue closing other connections
		}
	}
	p.connMutex.Unlock()

	if p.Listener != nil {
		err := p.Listener.Close()
		if err != nil {
			utils.LogError(p.logger, err, "failed to stop proxy server")
		}
	}

	err := p.stopDNSServers(ctx)
	if err != nil {
		utils.LogError(p.logger, err, "failed to stop the dns servers")
		// Continue with cleanup
	}

	p.CloseErrorChannel()

	p.logger.Info("proxy stopped...")
}
```

### Fix 2: handleConnection Defer
```go
defer func() {
	parserCtxCancel()

	if srcConn != nil {
		err := srcConn.Close()
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			utils.LogError(p.logger, err, "failed to close the source connection", zap.Any("clientConnID", clientConnID))
		}
		// Continue with cleanup
	}

	if dstConn != nil {
		err := dstConn.Close()
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			utils.LogError(p.logger, err, "failed to close the destination connection")
		}
		// Continue with cleanup
	}

	err := parserErrGrp.Wait()
	if err != nil {
		utils.LogError(p.logger, err, "failed to handle the parser cleanUp")
	}
}()
```

## Why This is a GSOC-Level Bug

1. **Resource Management:** Critical resource leaks that affect system stability
2. **Deadlock Risk:** Mutex not unlocked can cause complete system hang
3. **Production Impact:** Causes server unavailability and resource exhaustion
4. **Common Pattern:** Early returns in cleanup code is a frequent mistake
5. **Easy to Reproduce:** Simple test case demonstrates the issue
6. **High Priority:** Affects core proxy functionality

## Related Issues
This pattern of early returns in cleanup code may exist elsewhere. Search for:
- `defer` functions with early `return` statements
- Cleanup functions with early returns on errors
- Mutex unlocks after early returns

## Test Cases
The test file `proxy_resource_leak_test.go` demonstrates:
1. Resource leaks in StopProxyServer
2. Defer early return issues in handleConnection
3. Impact of skipped cleanup operations


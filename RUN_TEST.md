# How to Run the Resource Leak Test

## From the repository root:

```bash
cd ~/keploy
go test -v ./pkg/agent/proxy -run TestStopProxyServer_ResourceLeak
```

## Or navigate to the directory first:

```bash
cd ~/keploy/pkg/agent/proxy
go test -v -run TestStopProxyServer_ResourceLeak
```

## To run all tests in the proxy package:

```bash
cd ~/keploy
go test -v ./pkg/agent/proxy
```

## Expected Output:

The test will demonstrate the resource leak bug by showing:
- How early returns skip critical cleanup
- The impact on mutex unlocking
- Resource leaks in listeners, DNS servers, and channels


// Package recorder — forwarder.go
//
// This file previously contained the channel-based bidirectional forwarder.
// It has been replaced by TeeForwardConn (pkg/agent/proxy/orchestrator/)
// which uses a pre-allocated ring buffer instead of per-Read heap allocations.
// This eliminates GC pressure from the forwarding path and delivers
// latency identical to bare io.Copy (12-13ms P50 vs 33ms with channels).
//
// The old StartForwarding / forwardDirection code is no longer used.
// This file is kept as a tombstone to explain the architectural change.
package recorder

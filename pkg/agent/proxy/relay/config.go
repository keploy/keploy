package relay

import (
	"context"
	"crypto/tls"
	"net"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.uber.org/zap"
)

// Default values for Config fields. Exposed so callers can reference
// them when they want to scale relative to defaults.
const (
	// DefaultPerConnCap is the default soft cap on parser-owned
	// buffered bytes per connection, in bytes. Sized to comfortably
	// hold a single large query response (postgres SELECT against a
	// table whose rows carry 96 KB+ blobs returns ~10 MB per query;
	// 8 MiB tripped per_conn_cap drops on real /batch workloads — see
	// keploy/integrations#188). 64 MiB gives ~6× headroom over the
	// pathological large-blob case while keeping per-connection
	// memory bounded.
	DefaultPerConnCap int64 = 64 * 1024 * 1024 // 64 MiB

	// DefaultTeeChanBuf is the default capacity of the internal tee
	// channel. The staging channel (and the FakeConn-facing out channel)
	// hold one Chunk per slot; with DefaultForwardBuf=32 KiB this means
	// the channel cap × 32 KiB bounds the in-flight bytes the parser
	// can lag behind by before pushes start dropping with reason
	// "channel_full". 64 was too small for postgres queries returning
	// large blobs (e.g. 100 rows × 96 KB = ~10 MB per query maps to
	// ~300 chunks; the 64-slot channel filled almost immediately and
	// the recorder lost ~95% of the response, marking the mock
	// incomplete — see keploy/integrations#188 for the concurrent
	// simple-Query repro). Bumped to 1024 (≈32 MiB max staging per
	// direction) so the parser has enough room to absorb a realistic
	// large-result-set response without dropping. PerConnCap remains
	// the byte-budget enforcer for memory bounds.
	DefaultTeeChanBuf = 1024

	// DefaultForwardBuf is the size of the per-iteration Read/Write
	// scratch buffer used by the forwarder goroutines.
	DefaultForwardBuf = 32 * 1024 // 32 KiB
)

// TLSUpgradeFn performs a TLS handshake on a real net.Conn and returns
// the upgraded connection. The relay calls this once for the
// destination side and once for the client side in response to a
// [directive.KindUpgradeTLS] directive. The returned net.Conn replaces
// the relay's pointer to the original, so subsequent forwarder reads
// and writes operate on the TLS-wrapped stream.
//
// isClient=true indicates keploy is the TLS client for this side
// (i.e. handshaking against the real destination server). isClient=false
// indicates keploy is the TLS server (i.e. presenting the MITM cert to
// the real client).
//
// cfg is the *tls.Config chosen by the parser via [directive.UpgradeTLS].
// Implementations may ignore cfg on the client side and instead invoke
// a helper such as pkg/agent/proxy/tls.HandleTLSConnection which
// synthesises a cert per-ClientHello.
type TLSUpgradeFn func(ctx context.Context, conn net.Conn, isClient bool, cfg *tls.Config) (net.Conn, error)

// Config tunes a Relay. All fields are optional. Zero values resolve
// to the documented defaults at [New] time.
type Config struct {
	// Logger receives diagnostic messages. Nil is safe; a no-op
	// logger is substituted.
	Logger *zap.Logger

	// PerConnCap is the soft cap on parser-owned buffered bytes.
	// When the number of bytes currently sitting in the tee channel
	// (i.e. read from the socket but not yet consumed by the parser)
	// plus the incoming chunk size would exceed this, the tee is
	// dropped and OnMarkMockIncomplete is called with "per_conn_cap".
	// The forward itself still proceeds.
	//
	// Zero (or negative) resolves to DefaultPerConnCap.
	PerConnCap int64

	// TeeChanBuf is the capacity of the internal tee channel. When
	// the channel is full the tee is dropped with reason
	// "channel_full". Zero resolves to DefaultTeeChanBuf.
	TeeChanBuf int

	// ForwardBuf is the size of the per-iteration scratch buffer
	// used by forwarder Reads. Zero resolves to DefaultForwardBuf.
	ForwardBuf int

	// MemoryGuardCheck is polled on every chunk. When it returns
	// true the tee is dropped with reason "memory_pressure" — the
	// forward itself is not affected. Nil resolves to
	// [memoryguard.IsRecordingPaused].
	MemoryGuardCheck func() bool

	// TLSUpgradeFn performs TLS handshakes in response to
	// KindUpgradeTLS directives. If nil, a KindUpgradeTLS directive
	// is acked with OK=false and a wrapped ErrNoTLSUpgrader.
	TLSUpgradeFn TLSUpgradeFn

	// BumpActivity is invoked after every successful forward. The
	// supervisor's activity watchdog uses this to distinguish "parser
	// hung with traffic still flowing" from "whole connection is
	// idle". Nil is safe.
	BumpActivity func()

	// OnMarkMockIncomplete is invoked whenever the relay drops a
	// tee chunk (memoryguard, cap, channel full) or processes a
	// KindAbortMock directive. The reason string is the same value
	// the supervisor will record in telemetry. Nil is safe.
	OnMarkMockIncomplete func(reason string)

	// OnClientChunkTeed is invoked after each successful tee of a
	// client-to-dest chunk into the parser's FakeConn. Callers wire
	// this to the supervisor's MarkPendingWork so the activity
	// watchdog can distinguish "parser has no work" (connection is
	// idle between requests) from "parser has a request in flight
	// but isn't emitting a mock" (hang candidate). Nil is safe.
	OnClientChunkTeed func()
}

// withDefaults returns a copy of cfg with zero-valued optional fields
// replaced by documented defaults. It never mutates the caller's Config.
func (c Config) withDefaults() Config {
	out := c
	if out.Logger == nil {
		out.Logger = zap.NewNop()
	}
	if out.PerConnCap <= 0 {
		out.PerConnCap = DefaultPerConnCap
	}
	if out.TeeChanBuf <= 0 {
		out.TeeChanBuf = DefaultTeeChanBuf
	}
	if out.ForwardBuf <= 0 {
		out.ForwardBuf = DefaultForwardBuf
	}
	if out.MemoryGuardCheck == nil {
		out.MemoryGuardCheck = memoryguard.IsRecordingPaused
	}
	if out.BumpActivity == nil {
		out.BumpActivity = func() {}
	}
	if out.OnMarkMockIncomplete == nil {
		out.OnMarkMockIncomplete = func(string) {}
	}
	return out
}

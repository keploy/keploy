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
	// buffered bytes per connection, in bytes.
	DefaultPerConnCap int64 = 8 * 1024 * 1024 // 8 MiB

	// DefaultTeeChanBuf is the default capacity of the internal tee
	// channel. A value of 64 balances burst tolerance (a single
	// parser's read stall is absorbed) against memory overhead.
	DefaultTeeChanBuf = 64

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

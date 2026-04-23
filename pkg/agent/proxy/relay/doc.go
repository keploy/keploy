// Package relay is the sole owner and sole writer of the real client and
// destination TCP sockets during record mode.
//
// Role: for each accepted connection the proxy constructs a Relay bound to
// the two real net.Conns. The Relay spawns a pair of forwarder goroutines
// that read from one socket, stamp the read instant as ReadAt, write to
// the opposite socket, stamp the write instant as WrittenAt, and tee a
// copy of the resulting [fakeconn.Chunk] into the parser-facing FakeConn.
// Parsers consume those read-only FakeConns; they never see the real
// net.Conns and therefore cannot close, shutdown, or racily write to
// peer sockets. See ../fakeconn for the consumer contract and ../directive
// for the control channel the parser uses to ask the relay for TLS
// upgrades or to mark a mock as dropped.
//
// Real traffic is never blocked by parser backpressure, channel capacity,
// or memory-guard pressure: the tee is bounded and non-blocking. Any
// chunk the relay fails to tee is counted as a drop and reported to the
// supervised Session via the OnMarkMockIncomplete callback; the forward
// itself always completes. This enforces Invariant I1 ("transparent
// forwarding") from PLAN.md at the repository root.
//
// Lifecycle ownership: callers (proxy.go) create the Relay, pass in the
// real net.Conns, and call [Relay.Run]. The Relay does NOT close the
// real sockets on parser misbehavior or on its own Run returning — it
// only reads and writes them. Callers close them at connection end.
//
// This package is Phase 2 scaffolding per PLAN.md and is not yet wired
// into handleConnection; it compiles and is fully unit-tested in
// isolation so that the wiring PR is small and reviewable.
package relay

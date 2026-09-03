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
// real sockets on parser misbehavior or on its own Run returning —
// callers close them at connection end.
//
// One qualification: the relay does half-close them. When one direction
// reaches a clean EOF it calls CloseWrite on the conn that direction was
// writing to, forwarding the peer's FIN so a client that did
// shutdown(SHUT_WR) still receives its reply (see
// [Config.HalfCloseGrace]). That shuts only the write half and leaves
// the conn readable and closable, so full ownership still sits with the
// caller — but "only reads and writes them" is no longer the whole
// story.
//
// This package is wired into the record path via the V2 dispatcher
// (see ../proxy_v2.go::recordViaSupervisor). V2-capable parsers
// (those implementing integrations.IntegrationsV2 with IsV2()==true)
// run inside a supervisor attached to a Relay; legacy parsers
// continue on the pre-V2 path unchanged. The KEPLOY_NEW_RELAY=off
// env var forces legacy routing for all parsers as a rollback knob.
//
// Note that the legacy path handles half-close differently rather than
// identically: it waits for both directions instead of tearing the
// second down, so it does not LOSE the reply — but it never forwards
// the FIN either, so an EOF-delimited peer never learns the request
// ended and the connection hangs until external teardown. Rolling back
// to it trades one half-close defect for another.
package relay

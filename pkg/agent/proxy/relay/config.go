package relay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"time"

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

	// DefaultConsumerStallGrace is how long the tee waits for a parser that
	// has stopped draining before giving up on the chunks it still holds.
	//
	// It bounds STALLED time, not elapsed time: the wait ends the moment the
	// parser takes anything at all, so an arbitrarily slow parser still
	// receives every chunk. It only elapses when the parser frees nothing at
	// all for this long, which is taken as "gone" — the whole remaining
	// queue is then abandoned at once and reported, rather than the wait
	// being re-entered per chunk.
	//
	// Exposed as [Config.ConsumerStallGrace] rather than fixed in the tee so
	// the owner picks the policy, the way net/http's Server.Shutdown takes
	// its deadline from the caller's context.
	DefaultConsumerStallGrace = 2 * time.Second

	// DefaultForwardBuf is the size of the per-iteration Read/Write
	// scratch buffer used by the forwarder goroutines.
	DefaultForwardBuf = 32 * 1024 // 32 KiB

	// DefaultHalfCloseGrace bounds how long the surviving direction may
	// sit IDLE after its peer half-closed. See [Config.HalfCloseGrace].
	//
	// Because the bound is on idle time and not on total time, it only
	// has to cover the gap before a peer STARTS answering, never the
	// length of the answer: a response that streams for ten minutes
	// re-arms the window on every chunk. 10s is generous for
	// "connection accepted, request complete, first byte not yet sent"
	// while keeping the worst case — a peer that goes silent and never
	// closes — short enough that a busy proxy is not holding file
	// descriptors and tee buffers for half a minute per connection.
	DefaultHalfCloseGrace = 10 * time.Second

	// DefaultClientHoldCap bounds how many client bytes a
	// [Config.HoldClientWrites] hold may accumulate before the relay
	// gives up on the hold and releases it — see [Config.ClientHoldCap]
	// for why the breach is handled by releasing rather than by
	// dropping the connection.
	//
	// The window a hold covers is one client message: the parser is
	// deciding, off the first chunk, whether this connection upgrades
	// to TLS. MySQL's SSLRequest is 36 bytes and its plaintext
	// HandshakeResponse is a few hundred, so 256 KiB is roughly three
	// orders of magnitude of headroom over the real traffic while
	// still bounding what one connection can pin in memory.
	DefaultClientHoldCap int64 = 256 * 1024 // 256 KiB
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

	// ConsumerStallGrace bounds how long the tee waits on a parser that has
	// stopped draining before abandoning the chunks it still holds. Zero
	// resolves to [DefaultConsumerStallGrace]. See that constant for why the
	// bound is on stalled time rather than total teardown time.
	//
	// The default suits every workload we have measured: the bound is only
	// consulted AFTER close(), on a connection whose parser has stopped
	// draining a full out channel, so waiting longer rarely recovers
	// anything — the parser is usually not coming back. It is a field
	// rather than a package constant so the owner picks the policy, in the
	// spirit of net/http's Server.Shutdown taking its deadline from the
	// caller: tests shorten it, and operators can override it through
	// config.Record.RecordBuffer.ConsumerStallGrace (keploy.yml), the
	// hidden --consumer-stall-grace flag, or
	// KEPLOY_RECORD_CONSUMER_STALL_GRACE. Values GREATER THAN ZERO are
	// clamped to a safe range by clampConsumerStallGrace in
	// pkg/agent/proxy/proxy.go; zero and negative are passed through
	// untouched so the default above applies.
	ConsumerStallGrace time.Duration

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

	// ClientTLSFirst swaps the order of the two handshakes performed for a
	// KindUpgradeTLS directive: client side first, destination side second.
	//
	// Default (false) is destination-first, which is what every release before
	// upstream verification existed did, and it stays the default so the
	// overwhelmingly common path keeps its exact wire timing.
	//
	// It is set only when record.upstreamTls.verify is on, and only because of
	// what the two handshakes KNOW. keploy learns the hostname the application
	// intended — its SNI — by terminating the CLIENT side; on the destination
	// side all it has is the IP:port eBPF reported. Verifying a hostname-
	// addressed upstream against that IP fails against a DNS-SAN-only
	// certificate, the supervisor falls through to raw passthrough, and the
	// mock is dropped with only a Debug log. Running the client handshake
	// first makes the real ServerName available to the destination dial.
	//
	// Safe in both orders: neither handshake depends on the other. Keploy is
	// the TLS server for one and the TLS client for the other, over two
	// separate sockets, and Step 1 has already exchanged whatever preamble
	// the protocol needs before the client sends its ClientHello (Postgres'
	// 'S'; MySQL sends its ClientHello straight after SSLRequest). Both conns
	// are still published atomically after BOTH succeed, so the failure of
	// either leaves the relay's published pointers exactly as it found them.
	//
	// It does change one thing, and only inside the opt-in: what a dest-side
	// handshake FAILURE costs. Destination-first, keploy has not touched the
	// client socket yet, so the supervisor's FallthroughToPassthrough can
	// still relay the client's withheld ClientHello to the real server and
	// the application's connection survives with its mock dropped.
	// Client-first, keploy has already terminated TLS with the application,
	// so there is no cleartext stream left to pass through and the connection
	// is closed instead. That is the honest outcome of asking keploy to
	// authenticate an upstream that does not authenticate: it is LOUD rather
	// than a silently missing mock, it matches what the application's own
	// strict TLS config would have done, and it is reachable only when the
	// operator has explicitly set record.upstreamTls.verify. With the flag at
	// its default the field is false and none of this applies.
	ClientTLSFirst bool

	// RealCertHook, if non-nil, is invoked after the V2 relay path
	// completes the upstream TLS handshake (handleUpgradeTLS), with
	// the source-port-derived connID and the DER bytes of the real
	// upstream leaf certificate. Wired by the agent to
	// cbshim.RegisterReal so the channel-binding shim can pair this
	// real cert with the MITM cert minted by CertForClient.
	//
	// Symmetric to the dialPostgresSSLUpstream → cb.RegisterReal call
	// for the legacy direct-dial postgres path; this is the
	// equivalent for the V3 parser path that drives upstream TLS
	// through the supervisor relay instead.
	RealCertHook func(connID string, realCertDER []byte, sigAlgo x509.SignatureAlgorithm)

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

	// OnCaptureDesync is invoked at most once per direction, the first time
	// a chunk is dropped on that tee. Nil is safe.
	//
	// It is NOT a louder OnMarkMockIncomplete. That one is per-mock and is
	// cleared by Session.MarkMockComplete after each cycle; this one reports
	// that the connection's byte stream now has a hole, which no later mock
	// recovers from. Downstream parsers frame by length prefix, so after a
	// hole the next header is read mid-body and every subsequent frame on
	// the connection is garbage — the connection keeps carrying user traffic
	// perfectly while producing zero further mocks, and the test cases
	// recorded against it replay as match_phase=no_mocks.
	//
	// The expected implementation is to record the start of the hole and,
	// when the connection ends, mark that whole span so the test cases
	// overlapping it are suppressed instead of shipped mock-less. That is
	// the half that closes the failure by construction; retirement (below)
	// is the best-effort half that gets capture going again.
	OnCaptureDesync func(reason string)

	// OnClientChunkTeed is invoked after each successful tee of a
	// client-to-dest chunk into the parser's FakeConn. Callers wire
	// this to the supervisor's MarkPendingWork so the activity
	// watchdog can distinguish "parser has no work" (connection is
	// idle between requests) from "parser has a request in flight
	// but isn't emitting a mock" (hang candidate). Nil is safe.
	OnClientChunkTeed func()

	// PreDispatchPause, when true, makes [Relay.Run] install a pause
	// barrier BEFORE spawning the forwarder goroutines and switches
	// the forwarders into pre-dispatch mode for the duration of the
	// pause. Pre-dispatch mode differs from the standard pause in two
	// ways:
	//
	//  1. The pre-Read pause check is bypassed so the very first Read
	//     on each direction proceeds. Without this, the forwarders
	//     would park immediately at the top of their loop and the
	//     parser would never see the first chunk on its FakeConn
	//     (the tee is downstream of the Read), deadlocking on the
	//     parser's first read.
	//
	//  2. The post-Read pause check tees the chunk to the parser's
	//     FakeConn AND stashes the payload, instead of stashing only.
	//     The parser sees the bytes while the real-peer Write is
	//     deferred; the parser can inspect, decide, and release the
	//     pause via [directive.ResumePreDispatch] (which drains the
	//     stash to the real peer) or escalate to a full TLS upgrade
	//     via [directive.UpgradeTLS] (which consumes the stash
	//     directly).
	//
	// The exact use case is the Postgres SSL preamble race documented
	// in keploy/enterprise#2012: the V2 forwarder reads + writes
	// continuously, so by the time the parser sees SSLRequest on its
	// FakeConn the server may already have replied with 'S' and the
	// client may already have started its TLS ClientHello. Holding
	// writes on the destination→client direction until the parser has
	// inspected the first chunk closes the race deterministically.
	//
	// Default false preserves today's behaviour for parsers that have
	// not opted in. Most V2 parsers (http, mysql, mongo) do not need
	// pre-dispatch pause and would deadlock if they didn't issue a
	// ResumePreDispatch on entry — recordViaSupervisor only sets
	// this when the parser implements an opt-in capability method.
	PreDispatchPause bool

	// HalfCloseGrace bounds how long the relay keeps copying the
	// surviving direction after one side has half-closed (sent FIN
	// while still able to receive). Zero resolves to
	// [DefaultHalfCloseGrace]; negative disables half-close entirely and
	// restores the original behaviour of tearing both directions down
	// on the first EOF.
	//
	// A clean EOF from one side means "I have finished writing", not
	// "this connection is over". A client that does shutdown(SHUT_WR)
	// and then reads the reply — Node's socket.end(data), Python's
	// sock.shutdown(SHUT_WR), any request/EOF/response protocol — loses
	// that reply if the relay closes the other direction, and the
	// application sees its connection end before the answer arrives.
	//
	// The bound exists because the opposite risk is real too: a peer
	// that answers with neither data nor a FIN would leave the
	// surviving forwarder parked in Read forever. That is the ~60s hang
	// the unconditional teardown originally prevented, and this keeps
	// that protection while giving a well-behaved peer room to answer.
	//
	// It measures IDLE time, not total time. A total bound would cut a
	// slow response off mid-body — and silently, since the caller closes
	// the client socket once Run returns, so an EOF-delimited protocol
	// reads a truncated body as a complete one. Every forwarded chunk
	// re-arms the window.
	//
	// Operators reach it exactly like its siblings in
	// config.RecordBuffer: record.recordBuffer.halfCloseGrace in
	// keploy.yml or the hidden --half-close-grace flag.
	HalfCloseGrace time.Duration

	// HoldClientWrites, when true, stops the client→destination
	// forwarder from writing to the real destination. Bytes are still
	// read and still teed to the parser's FakeConn; only the
	// destination Write is deferred. The hold stays up until the
	// parser ends it with [directive.ReleaseClient] (which flushes
	// everything held, in read order) or [directive.UpgradeTLS] (which
	// flushes [directive.UpgradeTLSParams.ClientFlushBytes] and leaves
	// the rest to the client-side handshake).
	//
	// This exists because [PreDispatchPause] cannot express MySQL's
	// CLIENT_SSL upgrade, for two reasons:
	//
	//  1. It is all-or-nothing in TIME. The pre-dispatch pause parks
	//     the forwarders at the first chunk and ends at dispatch; the
	//     MySQL decision point is later, after the server's greeting
	//     has already round-tripped through both directions.
	//
	//  2. It is all-or-nothing in BYTES. Its TLS flush is takeStashed,
	//     the whole stash. MySQL needs a split: the 36-byte SSLRequest
	//     must reach the server (or it never switches to TLS) and the
	//     ClientHello immediately behind it must not (keploy terminates
	//     that handshake itself). Those two routinely arrive in a
	//     single read, so the split has to be by byte count.
	//
	// Without a hold, the C2D forwarder keeps running while the parser
	// decides and forwards whatever the client wrote next. That is not
	// a lost recording, it is a corrupted connection: the real server
	// parses the ClientHello as MySQL protocol, and keploy's own
	// destination handshake then runs against a desynchronised socket.
	//
	// The hold is directional. Destination→client forwarding is
	// untouched, which is what lets a server-speaks-first protocol
	// deliver its greeting while the client's reply is held.
	//
	// Default false. A parser that does not arm this sees today's
	// behaviour exactly; a parser that arms it and then never sends a
	// releasing directive wedges the connection until teardown, so the
	// dispatcher only sets it for parsers that opt in via a capability
	// method.
	HoldClientWrites bool

	// ClientHoldCap bounds the bytes a [HoldClientWrites] hold may
	// accumulate. Zero resolves to [DefaultClientHoldCap]; negative
	// disables the bound.
	//
	// On breach the relay releases the hold — flushing what it held
	// and resuming normal forwarding — rather than dropping the
	// connection, and reports "client_hold_cap" through
	// [OnMarkMockIncomplete].
	//
	// Releasing is the right failure mode because of what a breach
	// actually means. The hold covers one decision by the parser, over
	// a message measured in hundreds of bytes; reaching 256 KiB means
	// the parser is not going to answer. The client is by then blocked
	// waiting for a server reply that cannot come, because its request
	// is sitting in our stash. Flushing unwedges it: in the common
	// no-TLS case the session simply proceeds, and the only casualty
	// is the recording. Dropping the connection would take the user's
	// application down to protect a mock, which inverts the priority
	// the relay holds everywhere else — the forward path is not
	// allowed to lose to the capture path.
	ClientHoldCap int64

	// ParserCanResyncAfterGap tells the tees whether the parser on the other
	// end can re-anchor its framer after a HOLE in capture. Set by the
	// dispatcher from the parser's optional
	// [integrations.GapResyncCapable] capability.
	//
	// The default is FALSE, and that is the load-bearing choice.
	//
	// When a tee desyncs — a chunk lost to [DropPerConnCap] or
	// [DropMemoryPressure] — the connection's captured byte stream has a
	// hole in it. The tee already says so in its own words: it logs "capture
	// desynced; this connection can no longer be recorded" and fires
	// [Config.OnCaptureDesync] so the owner can suppress the test cases
	// recorded over the hole. Until now it then kept pushing anyway, and a
	// length-prefix framer reading from the wrong offset does not merely
	// produce nothing: postgres read four bytes of misread row data as a
	// uint32 length and attempted a multi-gigabyte allocation.
	//
	// So false stops the feed for every parser that has not claimed
	// otherwise. Yes, that changes today's behaviour for every non-mongo
	// parser — deliberately. What it gives up is the theoretical chance that
	// a parser recovers by luck; what those parsers actually produce after a
	// hole is garbage frames, and the honest accounting is that this
	// connection's recording ended when the tee said it did. Defaulting the
	// other way would mean a parser has to know about this mechanism to be
	// protected by it, which inverts the safety: a third-party parser that
	// never heard of the capability is precisely the one least likely to
	// have a resync path.
	//
	// True is for parsers that genuinely re-anchor and therefore NEED the
	// post-hole bytes to do it — mongo/v2 detects the SeqNo discontinuity
	// and content-scans for the next validated message header. Cutting its
	// feed would strand it desynced forever, which is a regression, not a
	// fix.
	//
	// Either way the FORWARD path is untouched: the relay writes every byte
	// to the real peer before it ever offers the chunk to a tee.
	ParserCanResyncAfterGap bool
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
	// == 0, not <= 0: a negative HalfCloseGrace is the documented way to
	// opt out of half-close, so it must survive default resolution.
	if out.HalfCloseGrace == 0 {
		out.HalfCloseGrace = DefaultHalfCloseGrace

	// == 0, not <= 0: a negative ClientHoldCap is the documented way to
	// disable the bound, so it must survive default resolution.
	if out.ClientHoldCap == 0 {
		out.ClientHoldCap = DefaultClientHoldCap
	}
	if out.ConsumerStallGrace <= 0 {
		out.ConsumerStallGrace = DefaultConsumerStallGrace
	}
	// The two client-direction brakes are alternatives, not layers, and
	// the conflict is resolved HERE rather than in run() because run()
	// installs the pre-dispatch pause barrier before it could act on
	// the flag — clearing preDispatchActive afterwards leaves that
	// barrier up with nothing to release it, and both forwarders park
	// forever on a connection whose greeting never reaches the client.
	//
	// The hold wins. Pre-dispatch routes the client's first chunk
	// through the pause stash, which bypasses the hold's byte
	// accounting entirely (ClientHoldCap silently stops being
	// enforced), and its resume handler acks OK while leaving the hold
	// armed. See [HoldClientWrites].
	if out.HoldClientWrites && out.PreDispatchPause {
		out.PreDispatchPause = false
		out.Logger.Error("relay: HoldClientWrites and PreDispatchPause are mutually exclusive; ignoring PreDispatchPause",
			zap.String("next_step", "a parser should implement WantsClientWriteHold or WantsPreDispatchPause, never both — the hold subsumes pre-dispatch for the client direction"),
		)
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

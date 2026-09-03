package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// sniffResult is the message published on the sniffCh channel by each
// sniffAndRelayLoop goroutine when it has a verdict on its side.
type sniffResult struct {
	side     string
	isTLS    bool
	buffered []byte
	err      error
}

// opportunisticTLSIntercept handles a connection in "passthrough by
// default, MITM if TLS shows up" mode. Concretely:
//
//  1. Dials the upstream so app and upstream have a live TCP path.
//  2. Spawns two goroutines that read chunks from each side and
//     forward them verbatim to the other side. Each chunk is also
//     checked against the TLS handshake pattern.
//  3. The instant a chunk on the CLIENT side starts with a TLS
//     ClientHello (0x16 0x03 …), the goroutines stop. The proxy
//     then takes over both sockets:
//     - terminates TLS with the client using keploy's MITM cert
//     (HandleTLSConnection — KeyLogWriter is wired there)
//     - opens a fresh tls.Client to the upstream socket (which is
//     in "expecting ClientHello" state since we did NOT forward
//     the client's ClientHello upstream)
//     - relays cleartext both ways with no parser dispatch
//  4. If neither side produces a TLS ClientHello within
//     opportunisticPeekMaxBytes / opportunisticPeekTimeout, the
//     connection falls through to a pure plaintext relay until EOF.
//
// Why bidirectional: server-first TLS protocols (MySQL) have a
// multi-roundtrip pre-TLS dance — server greeting, client capability
// flags with the SSL bit set, then the client's ClientHello. The
// dst-side relay forwards the greeting; the src-side relay forwards
// the capability response; the next chunk on src is the ClientHello
// which the src-side peeker catches. Without bidirectional relay
// during pre-TLS we would never reach the point where TLS starts.
//
// Caveats (documented in detail on the config field):
//   - cert pinning rejects keploy's MITM cert
//   - SCRAM-*-PLUS and other channel-binding mechanisms break
//   - apps without keploy's CA installed fail handshake
//
// opts carries the session's resolved upstream-TLS trust material
// (see Proxy.applyUpstreamTLSOptions); it is threaded down to
// hijackAndMITM, which owns the only upstream tls.Config on this path.
func (p *Proxy) opportunisticTLSIntercept(ctx context.Context, srcConn net.Conn, dstAddr string, backdate time.Time, opts models.OutgoingOptions) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, opportunisticDialTimeout)
	defer dialCancel()
	var dialer net.Dialer
	dstConn, err := dialer.DialContext(dialCtx, "tcp", dstAddr)
	if err != nil {
		return fmt.Errorf("dial upstream %s: %w", dstAddr, err)
	}

	sniffCh := make(chan sniffResult, 2)
	relayCtx, cancelRelay := context.WithCancel(ctx)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		p.sniffAndRelayLoop(relayCtx, "src", srcConn, dstConn, sniffCh)
	}()
	go func() {
		defer wg.Done()
		p.sniffAndRelayLoop(relayCtx, "dst", dstConn, srcConn, sniffCh)
	}()

	cleanup := func() {
		cancelRelay()
		// Unblock any goroutine currently sleeping inside Read() by
		// setting an already-expired deadline. Without this, a goroutine
		// blocked in from.Read() can't observe ctx cancellation until the
		// opportunisticPeekChunkTimeout (5 s) fires — adding a 5 s stall
		// to every TLS connection before hijackAndMITM can start.
		//
		// Cutting a Read short loses nothing: the bytes stay in the
		// kernel receive buffer, and the deadline clear below is what
		// lets the next reader still get them.
		_ = srcConn.SetReadDeadline(time.Now())
		_ = dstConn.SetReadDeadline(time.Now())
		wg.Wait()
		// Clear deadlines so hijackAndMITM / continuePlainRelay can use
		// the connections normally.
		_ = srcConn.SetReadDeadline(time.Time{})
		_ = dstConn.SetReadDeadline(time.Time{})
	}

	for {
		select {
		case <-ctx.Done():
			cleanup()
			_ = srcConn.Close()
			_ = dstConn.Close()
			return ctx.Err()

		case res := <-sniffCh:
			if res.isTLS {
				// Hijack: kill the relay loops, then drive both TLS
				// handshakes ourselves. Relay loops have already
				// withheld the buffered ClientHello from the upstream.
				cleanup()
				return p.hijackAndMITM(ctx, srcConn, dstConn, res.buffered, dstAddr, backdate, opts)
			}

			// One side reported "not TLS" — either it hit the byte
			// budget without seeing a handshake, or it errored out.
			// Drain the OTHER side too so both goroutines exit; then
			// either fall through to a pure relay (budget-exhaustion
			// case) or close on the error path.
			//
			// cancelRelay runs BEFORE waitForOther, not after: waitForOther
			// blocks until the peer's sniffAndRelayLoop goroutine exits,
			// and that goroutine only checks ctx.Done() between reads —
			// on every ordinary read timeout it just continues. If we
			// waited first and cancelled after, a peer with nothing left
			// to send would never be told to stop and waitForOther would
			// hang forever (#4398).
			//
			// Poke an expired read deadline, same as cleanup(), so a peer
			// parked in Read wakes now instead of serving out the rest of
			// its opportunisticPeekChunkTimeout. Otherwise every closing
			// plaintext connection holds two goroutines and two sockets
			// for up to 5 s more.
			//
			// The clear afterwards is not decoration — it is what makes
			// the poke safe. An expired deadline is sticky: the goroutine
			// that already reported clears only its OWN side (:221) before
			// returning, so without the clear here continuePlainRelay
			// inherits an expired deadline and dies on its first read with
			// i/o timeout, relaying nothing. cleanup() clears for the same
			// reason.
			//
			// No data is lost by cutting a Read short. An expired deadline
			// makes Read return (0, timeout) even with bytes already in
			// the socket buffer, but those bytes stay in the kernel buffer
			// and the next read — continuePlainRelay's io.Copy — gets them
			// intact.
			//
			// The poke is best-effort, not a guarantee: a peer sitting
			// between its loop-top ctx check and its own SetReadDeadline
			// will overwrite this deadline with a fresh one and park
			// anyway. It also only reaches a peer blocked in Read — one
			// blocked in to.Write() is bounded by that call's own write
			// deadline instead. Either way the wait is capped at one
			// opportunisticPeekChunkTimeout rather than forever.
			cancelRelay()
			_ = srcConn.SetReadDeadline(time.Now())
			_ = dstConn.SetReadDeadline(time.Now())
			other := waitForOther(ctx, sniffCh, &wg)
			_ = srcConn.SetReadDeadline(time.Time{})
			_ = dstConn.SetReadDeadline(time.Time{})

			// The peer caught a ClientHello while we were taking the
			// non-TLS branch on this side. Those bytes were withheld from
			// the upstream so they could be replayed, and they exist
			// nowhere else — dropping them leaves the upstream waiting
			// for a handshake it will never see while the app waits for a
			// ServerHello that can never come.
			//
			// Forward them and carry on as a plain relay rather than
			// hijacking. Interception was already abandoned on this
			// connection: only the src side sets isTLS, so res must be
			// the dst result, and the res.err == nil guard narrows that
			// to dst budget exhaustion — the upstream sent a full
			// opportunisticPeekMaxBytes of pre-TLS bytes before the
			// ClientHello landed. Real STARTTLS preambles are orders of
			// magnitude under 64 KiB, so this is a defensive path, and
			// handing an unexercised corner to hijackAndMITM would risk
			// more than passing the handshake through costs. The app and
			// the upstream then negotiate TLS directly; keploy simply
			// does not intercept this one.
			if res.err == nil && other.isTLS && len(other.buffered) > 0 {
				if werr := replayWithheldHandshake(dstConn, other.buffered); werr != nil {
					_ = srcConn.Close()
					_ = dstConn.Close()
					return firstNonShutdownErr(werr, nil)
				}
				return p.continuePlainRelay(ctx, srcConn, dstConn)
			}

			if res.err == nil && other.err == nil {
				// Budget hit on both sides without TLS — the
				// goroutines have already been forwarding bytes
				// during their peek window. After they exit, finish
				// the relay until either side closes.
				return p.continuePlainRelay(ctx, srcConn, dstConn)
			}

			// One or both sides errored — close and return the most
			// informative error. Closed-network errors are expected
			// at end-of-conversation; demote them to nil.
			_ = srcConn.Close()
			_ = dstConn.Close()
			return firstNonShutdownErr(res.err, other.err)
		}
	}
}

const (
	// opportunisticPeekMaxBytes caps how many pre-TLS bytes we'll
	// relay verbatim before giving up on TLS detection. Large
	// enough to absorb realistic STARTTLS preambles (MySQL server
	// greeting up to a few hundred bytes, SMTP capability lists)
	// without letting a hostile or buggy client pin keploy memory
	// open by sending forever without a handshake.
	opportunisticPeekMaxBytes = 64 * 1024

	// opportunisticPeekChunkTimeout bounds each individual read's
	// blocking time so the goroutine wakes up periodically and can
	// observe ctx cancellation. Too short and slow networks miss
	// chunks; too long and shutdown drags. 5 s is comfortable for
	// LAN/loopback and tolerable on WAN.
	opportunisticPeekChunkTimeout = 5 * time.Second

	// opportunisticDialTimeout caps the upstream dial. Same value
	// as the synchronous-dial path uses elsewhere in the proxy.
	opportunisticDialTimeout = 10 * time.Second
)

// sniffAndRelayLoop reads chunks from `from` and forwards each one
// to `to` while peeking the first 5 bytes for the TLS handshake
// pattern. Only the src side meaningfully detects TLS — the dst side
// just relays server-first pre-TLS bytes and reports non-TLS / error
// results. Exits cleanly on ctx cancellation, a relay error, or once
// the byte budget is exhausted.
func (p *Proxy) sniffAndRelayLoop(ctx context.Context, side string, from, to net.Conn, sniffCh chan<- sniffResult) {
	buf := make([]byte, 8192)
	relayed := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = from.SetReadDeadline(time.Now().Add(opportunisticPeekChunkTimeout))
		n, err := from.Read(buf)
		_ = from.SetReadDeadline(time.Time{})

		if err != nil {
			if isTimeoutErr(err) {
				if ctx.Err() == nil {
					// Idle on this side; just loop to re-check ctx.
					continue
				}
				// Cancelled while parked in Read. Being told to stop is
				// not a failure of this side, so exit without publishing.
				//
				// Publishing here would be read by waitForOther as "the
				// peer errored", which flips a clean budget-exhaustion
				// result onto the close-both-connections path and kills a
				// perfectly healthy plaintext relay. Whether that happened
				// was a coin flip: sniffCh is buffered, so pushSignal's
				// select had both a ready send and a ready ctx.Done() and
				// picked at random.
				return
			}
			pushSignal(sniffCh, sniffResult{side: side, isTLS: false, err: err})
			return
		}
		if n == 0 {
			continue
		}

		chunk := buf[:n]

		// TLS detection: only the client initiates a TLS handshake.
		// Limit the check to the src side so a server greeting that
		// happens to start with 0x16 0x03 (extremely unlikely) can't
		// trigger a false positive.
		if side == "src" && pTls.IsTLSHandshake(chunk) {
			persisted := make([]byte, n)
			copy(persisted, chunk)
			pushSignal(sniffCh, sniffResult{side: side, isTLS: true, buffered: persisted})
			return
		}

		// Forward to the other side verbatim, under a write deadline.
		//
		// Without one this is the #4398 leak again from the other end: a
		// peer that has stopped reading fills the send buffer, Write
		// blocks forever, and cancelRelay is invisible to a blocked
		// Write — so wg.Wait() never returns and the connection's two
		// goroutines and two sockets are pinned for the life of the
		// process. The parent's expired-deadline poke does not help
		// either; it sets a READ deadline.
		//
		// A timeout is retried rather than reported. A slow consumer is
		// not an error, and abandoning a partially written chunk would
		// punch a hole in a stream we are supposed to relay verbatim —
		// these bytes are already out of the kernel buffer, so nothing
		// else can re-send them. The deadline exists to regain control
		// every opportunisticPeekChunkTimeout, not to cap the transfer.
		//
		// Once the context is cancelled we stop retrying, which bounds
		// shutdown to one window. A peer draining too slowly to finish
		// the chunk inside that window IS cut off — Write only returns a
		// nil error once the whole slice is out — and the connection is
		// then closed rather than relayed on with a hole in it.
		written := 0
		var werr error
		for written < len(chunk) {
			_ = to.SetWriteDeadline(time.Now().Add(opportunisticPeekChunkTimeout))
			var wn int
			wn, werr = to.Write(chunk[written:])
			_ = to.SetWriteDeadline(time.Time{})
			written += wn
			if werr == nil {
				continue
			}
			if isTimeoutErr(werr) && ctx.Err() == nil {
				// Slow consumer; keep going now that ctx has been rechecked.
				werr = nil
				continue
			}
			if written == len(chunk) {
				// Everything got out despite the error; nothing was lost,
				// so don't fail a chunk that actually landed.
				werr = nil
				break
			}
			// A real write error, or cancellation with the chunk not fully
			// out. Both are reported, including the written == 0 case:
			// unlike a cut-short Read — whose bytes are still in the
			// kernel buffer for continuePlainRelay to pick up — this chunk
			// was already consumed out of `from` and lives only in buf, so
			// returning quietly would punch a hole in a stream we are
			// supposed to relay verbatim and leave the connection up to
			// carry the corruption. Closing is the honest outcome.
			break
		}
		if werr != nil {
			pushSignal(sniffCh, sniffResult{side: side, isTLS: false, err: werr})
			return
		}

		relayed += n
		if relayed >= opportunisticPeekMaxBytes {
			// Budget exhausted; sniffCh "no TLS" but keep relaying
			// until ctx is cancelled (the parent will start a clean
			// io.Copy after both goroutines exit).
			pushSignal(sniffCh, sniffResult{side: side, isTLS: false})
			return
		}
	}
}

// hijackAndMITM is invoked once the src-side sniffer has detected a
// TLS ClientHello. It owns both sockets from this point: it
// terminates TLS with the client (using the buffered ClientHello as
// the first read) and runs a fresh tls.Client handshake against the
// upstream. After both handshakes complete it relays cleartext until
// EOF on either side. There is no parser dispatch here — that's
// what differentiates this mode from the default record path.
func (p *Proxy) hijackAndMITM(ctx context.Context, srcConn, dstConn net.Conn, bufferedClientHello []byte, dstAddr string, backdate time.Time, opts models.OutgoingOptions) error {
	defer srcConn.Close()
	defer dstConn.Close()

	// Wrap srcConn so the next read returns the buffered ClientHello
	// bytes first, then the rest of the live socket. tls.Server will
	// consume the ClientHello from this wrapped reader.
	wrappedSrc := &util.Conn{
		Conn:   srcConn,
		Reader: io.MultiReader(bytes.NewReader(bufferedClientHello), srcConn),
		Logger: p.logger,
	}

	// Client-facing handshake. HandleTLSConnection's inner
	// tls.Config has KeyLogWriter wired into the package-level
	// fanout sink, so the master secret for this side is logged.
	tlsClient, _, err := pTls.HandleTLSConnection(ctx, p.logger, wrappedSrc, backdate)
	if err != nil {
		return fmt.Errorf("client-side handshake: %w", err)
	}
	defer tlsClient.Close()

	// Upstream handshake. The dst socket is in "expecting
	// ClientHello" state because we deliberately did NOT forward
	// the client's ClientHello — keploy starts a fresh TLS session
	// of its own.
	//
	// Verification is off unless the operator opted in via
	// record.upstreamTls.verify, for the same reason as every other
	// upstream dial in this package: keploy must never be stricter
	// than the app it records. An app that chose not to authenticate
	// its own upstream would have its connection broken by keploy
	// doing it on its behalf, and the failure is silent — the
	// handshake error drops this connection's recording entirely
	// while the app itself keeps working. Not a CA-bundle
	// limitation: crypto/tls uses the platform root pool when
	// RootCAs is nil.
	//
	// ServerName:
	//
	//   - Default (verification off): hostFromAddr(dstAddr), byte for byte
	//     what this site has always sent. dstAddr here is ALWAYS an
	//     `ip:port` built from destInfo, so this is an IP literal and
	//     crypto/tls omits SNI from the wire for it. Preserving that is the
	//     whole point — the app never sent an SNI to the real server on this
	//     path, and keploy must not invent one.
	//   - Verifying: the SNI the APPLICATION sent, which the client-facing
	//     handshake above has just recovered from its ClientHello, falling
	//     back to the dial address only when the app sent none. "Never
	//     empty" is not the same as "verifiable": a hostname-addressed
	//     upstream presents a DNS-SAN-only certificate, and checking it
	//     against the destination IP fails with `cannot validate certificate
	//     for <ip> because it doesn't contain any IP SANs` — which on THIS
	//     path is worse than a dropped mock, because the deferred
	//     srcConn/dstConn closes above tear down the application's own
	//     connection after it has already completed its handshake with us.
	serverName := hostFromAddr(dstAddr)
	if opts.UpstreamTLSVerify {
		serverName = resolveUpstreamServerName(capturedClientSNI(tlsClient), "", dstAddr, true)
	}
	upstreamCfg := &tls.Config{
		InsecureSkipVerify: !opts.UpstreamTLSVerify, //nolint:gosec // MITM proxy by design; opt in with record.upstreamTls.verify
		RootCAs:            opts.UpstreamTLSRootCAs,
		ServerName:         serverName,
		KeyLogWriter:       pTls.KeyLogWriter(),
	}
	tlsUpstream := tls.Client(dstConn, upstreamCfg)
	hsCtx, hsCancel := context.WithTimeout(ctx, opportunisticDialTimeout)
	defer hsCancel()
	if err := tlsUpstream.HandshakeContext(hsCtx); err != nil {
		return fmt.Errorf("upstream handshake to %s: %w", dstAddr, err)
	}
	defer tlsUpstream.Close()

	// Publish the upstream's real leaf cert into the cbshim's
	// rendezvous so SCRAM-SHA-256-PLUS clients get the real cert's
	// hash substituted in place of the MITM cert's hash. Without this
	// the opportunistic-TLS path is invisible to cbshim — only the
	// parser-directive path (relay/directive_proc.go::handleUpgradeTLS)
	// publishes today, so any postgres client whose protocol has no
	// registered parser (OSS doesn't ship PostgresV3) falls through to
	// hijackAndMITM and breaks SCRAM-PLUS. The connID matches the one
	// CertForClient uses (client's source port as decimal string) so
	// the MITM half (RegisterMITM, fired from CertForClient inside
	// HandleTLSConnection above) and the real half rendezvous on the
	// same key.
	// All cbshim logs here are per-connection diagnostics on the
	// opportunistic-TLS hot path. Kept at Debug so high-throughput
	// workloads don't flood operator-facing Info logs with cbshim
	// internals — surface --debug if you need the rendezvous trail.
	// Snapshot p.cbshim into a local once so the multiple reads below
	// (nil check + RegisterReal + CleanupConnection defer) can't tear
	// if a concurrent SetCBShim writes p.cbshim between them. The
	// shutdown path is the only writer today (it deliberately skips
	// the nil-write to avoid this race), but the snapshot also future-
	// proofs against any other call site that might mutate the field.
	cb := p.cbshim
	if cb != nil {
		state := tlsUpstream.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			// tcpAddr.Port==0 means the wrapped conn surfaced an
			// unspecified port — connID "0" would collide with every
			// other Port==0 connection in cbshim's rendezvous map.
			// Skip, matching tls.publishMITM's sourcePort==0 guard.
			if tcpAddr, ok := srcConn.RemoteAddr().(*net.TCPAddr); ok && tcpAddr.Port != 0 {
				leaf := state.PeerCertificates[0]
				connID := strconv.Itoa(tcpAddr.Port)
				if ce := p.logger.Check(zap.DebugLevel, "cbshim: opportunistic-TLS RegisterReal"); ce != nil {
					// RemoteAddr().String() allocates net.Addr's stringer
					// each call, and leaf.SignatureAlgorithm.String()
					// walks the algo enum table — neither is free on the
					// opportunistic-TLS hot path. Gated so the format
					// work only runs when debug logging is enabled.
					ce.Write(
						zap.String("connID", connID),
						zap.String("srcRemote", srcConn.RemoteAddr().String()),
						zap.String("dstAddr", dstAddr),
						zap.Int("peerCertCount", len(state.PeerCertificates)),
						zap.String("sigAlgo", leaf.SignatureAlgorithm.String()),
					)
				}
				cb.RegisterReal(connID, leaf.Raw, leaf.SignatureAlgorithm)
				// Release the cbshim's per-connection rendezvous state
				// when this hijack returns. If the MITM half (from
				// CertForClient) was already published, Publish has
				// fired and the pending entry is already cleared, so
				// this defer is a no-op. If for any reason only one
				// half arrived (e.g. CertForClient was never invoked
				// for this connID, or the connection errored before
				// rendezvous), CleanupConnection drops the half-state
				// before it leaks to process exit.
				defer cb.CleanupConnection(connID)
			} else if ce := p.logger.Check(zap.DebugLevel, "cbshim: opportunistic-TLS RegisterReal SKIPPED — srcConn.RemoteAddr is not *net.TCPAddr"); ce != nil {
				// Gated under Check so fmt.Sprintf + RemoteAddr().String()
				// don't run on the hot path when debug is off — both
				// allocate per call and this branch fires for every
				// opportunistic-TLS connection whose src isn't a
				// *net.TCPAddr (rare but not zero).
				ce.Write(
					zap.String("srcRemoteType", fmt.Sprintf("%T", srcConn.RemoteAddr())),
					zap.String("srcRemote", srcConn.RemoteAddr().String()),
				)
			}
		} else {
			p.logger.Debug("cbshim: opportunistic-TLS RegisterReal SKIPPED — no peer certs",
				zap.String("dstAddr", dstAddr))
		}
	} else {
		p.logger.Debug("cbshim: opportunistic-TLS RegisterReal SKIPPED — p.cbshim is nil",
			zap.String("dstAddr", dstAddr))
	}

	p.logger.Debug("opportunistic TLS intercept: hijacked, both sides MITM'd",
		zap.String("upstream", dstAddr),
		zap.String("upstreamProtocol", tlsUpstream.ConnectionState().NegotiatedProtocol))

	// Cleartext relay between the two MITM'd sockets. KeyLogWriter
	// already populated the fanout sink during the handshakes so
	// the keylog file the recorder is streaming carries both
	// halves' secrets; the captured pcap can be decrypted in
	// Wireshark with that keylog.
	return relayPlaintext(ctx, tlsClient, tlsUpstream)
}

// continuePlainRelay handles the "budget exhausted, no TLS"
// fall-through. The sniff goroutines have already been forwarding
// bytes during their peek window and have exited; we just need to
// keep io.Copy'ing both directions until EOF.
func (p *Proxy) continuePlainRelay(ctx context.Context, srcConn, dstConn net.Conn) error {
	defer srcConn.Close()
	defer dstConn.Close()
	return relayPlaintext(ctx, srcConn, dstConn)
}

// relayPlaintext pipes bytes between a and b in both directions
// until either side closes. Returns nil on clean shutdown; only
// surfaces errors that aren't ordinary closed-connection states.
func relayPlaintext(ctx context.Context, a, b net.Conn) error {
	g, _ := errgroup.WithContext(ctx)
	g.Go(func() error {
		_, err := io.Copy(b, a)
		_ = closeWriteIfPossible(b)
		return discardClosed(err)
	})
	g.Go(func() error {
		_, err := io.Copy(a, b)
		_ = closeWriteIfPossible(a)
		return discardClosed(err)
	})

	done := make(chan error, 1)
	go func() { done <- g.Wait() }()

	select {
	case <-ctx.Done():
		// Unblock the io.Copy goroutines immediately — without this
		// they stay blocked in Read until the remote closes, which
		// can stall a graceful proxy shutdown for the full connection
		// lifetime.
		_ = a.SetDeadline(time.Now())
		_ = b.SetDeadline(time.Now())
		<-done
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// closeWriteIfPossible signals end-of-stream on the writing half so
// the peer's read returns EOF. Falls back to a no-op when the conn
// type doesn't support half-close.
func closeWriteIfPossible(c net.Conn) error {
	// Thin alias kept for the call sites in this file; the
	// implementation lives in util because that is the only package
	// able to see through SafeConn, which wraps essentially every conn
	// a parser touches.
	return util.CloseWriteIfPossible(c)
}

// pushSignal publishes a goroutine's verdict. The send is unguarded on
// purpose: sniffCh has capacity 2, exactly two sniffAndRelayLoop
// goroutines run per connection, and every call site returns
// immediately afterwards — so at most two sends ever occur and the
// buffer can never be full.
//
// This used to select over ctx.Done() as well, described as deadlock
// protection for a cancelled goroutine. It protected against nothing
// (the send cannot block) and cost correctness: once the parent
// cancelled relayCtx, BOTH cases were runnable and Go chose uniformly
// at random, so roughly half of all post-cancellation verdicts were
// dropped. That included the isTLS verdict carrying a buffered
// ClientHello — which the loop had withheld from the upstream
// precisely so the parent could replay it — leaving the parent to
// plain-relay a connection whose handshake had been swallowed.
func pushSignal(ch chan<- sniffResult, res sniffResult) {
	ch <- res
}

// waitForOther drains the second sniff result so both goroutines
// always exit before we tear down the connection.
//
// It returns the whole sniffResult, not just its error. The peer can
// still report a ClientHello here: only the src side detects TLS, so
// when dst reports first (budget or error) the src side may be parked
// mid-handshake and publish isTLS a moment later. Returning only .err
// dropped that verdict AND the buffered ClientHello bytes the loop had
// deliberately withheld from the upstream, so the caller plain-relayed
// a stream whose handshake had been swallowed — the app then waited
// forever for a ServerHello that could never arrive.
func waitForOther(ctx context.Context, ch <-chan sniffResult, wg *sync.WaitGroup) sniffResult {
	// Wait for either the second sniffCh or the goroutines to finish.
	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()

	select {
	case <-ctx.Done():
		return sniffResult{err: ctx.Err()}
	case res := <-ch:
		<-doneCh
		return res
	case <-doneCh:
		// Drain before giving up. This is not the rare path its name
		// suggests — it is the common one, because a peer cancelled
		// while parked in Read returns without publishing at all. But
		// a peer that DID publish and then exited satisfies both this
		// case and the one above, and Go picks between ready cases at
		// random; taking this one without looking would throw away a
		// verdict that is already sitting in the buffer, including an
		// isTLS carrying a withheld ClientHello. Same defect pushSignal
		// used to have, one level up.
		select {
		case res := <-ch:
			return res
		default:
			return sniffResult{}
		}
	}
}

// replayWithheldHandshake forwards the ClientHello bytes a
// sniffAndRelayLoop deliberately withheld from the upstream, so a
// connection that stops being intercepted still carries a complete
// stream. The bytes exist nowhere else — they were consumed out of the
// client socket and held in memory — so failing to send them corrupts
// the stream rather than merely losing an optimisation.
//
// Bounded by the same per-chunk window the relay loops use. This runs
// only after the upstream has already pushed opportunisticPeekMaxBytes,
// which is exactly the profile of a peer that may have stopped reading,
// and an unbounded Write here would re-create the #4398 hang in the
// parent goroutine.
func replayWithheldHandshake(dstConn net.Conn, buffered []byte) error {
	_ = dstConn.SetWriteDeadline(time.Now().Add(opportunisticPeekChunkTimeout))
	defer func() { _ = dstConn.SetWriteDeadline(time.Time{}) }()
	if _, err := dstConn.Write(buffered); err != nil {
		return fmt.Errorf("replaying withheld ClientHello upstream: %w", err)
	}
	return nil
}

// hostFromAddr returns the host portion of "host:port" or
// "[host]:port", or the input unchanged when it isn't a host:port.
func hostFromAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// capturedClientSNI returns the ServerName the APPLICATION put in the
// ClientHello it sent to keploy, or "" if it sent none (which is what
// crypto/tls does for an IP-literal destination — RFC 6066 forbids IP
// literals in SNI).
//
// Read straight off the completed client-facing *tls.Conn rather than through
// pTls.SrcPortToDstURL: it is the same value CertForClient stored there
// moments earlier, but it needs no source-port lookup and so cannot be
// confused by a recycled port. Returns "" for anything that is not a
// *tls.Conn, so callers degrade to their existing fallback.
func capturedClientSNI(clientConn net.Conn) string {
	tc, ok := clientConn.(*tls.Conn)
	if !ok {
		return ""
	}
	return tc.ConnectionState().ServerName
}

// hostFromConn is hostFromAddr over a connection's remote address — the peer
// keploy is actually talking to. Used as the last-resort ServerName when the
// caller had no SNI to reuse and verification is on.
//
// Returns "" for a nil conn or a nil RemoteAddr rather than panicking: the
// callers feed the result straight to tls.Config.ServerName, and "" there is
// exactly the state they were already in — no worse than not calling it.
func hostFromConn(conn net.Conn) string {
	if conn == nil {
		return ""
	}
	addr := conn.RemoteAddr()
	if addr == nil {
		return ""
	}
	return hostFromAddr(addr.String())
}

// isTimeoutErr matches the deadline-exceeded error returned by
// net.Conn reads when SetReadDeadline expires.
func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// discardClosed swallows io.EOF and standard closed-connection
// errors. Callers care about real I/O failures, not normal shutdown.
func discardClosed(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	if isNetworkClosedErr(err) {
		return nil
	}
	return err
}

// firstNonShutdownErr picks the most informative error from a pair,
// preferring real failures over closed-connection ordinary EOF.
func firstNonShutdownErr(a, b error) error {
	for _, e := range []error{a, b} {
		if e == nil || errors.Is(e, io.EOF) {
			continue
		}
		if isNetworkClosedErr(e) {
			continue
		}
		return e
	}
	return nil
}

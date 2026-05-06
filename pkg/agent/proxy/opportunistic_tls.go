package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
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
func (p *Proxy) opportunisticTLSIntercept(ctx context.Context, srcConn net.Conn, dstAddr string, backdate time.Time) error {
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
				return p.hijackAndMITM(ctx, srcConn, dstConn, res.buffered, dstAddr, backdate)
			}

			// One side reported "not TLS" — either it hit the byte
			// budget without seeing a handshake, or it errored out.
			// Drain the OTHER side too so both goroutines exit; then
			// either fall through to a pure relay (budget-exhaustion
			// case) or close on the error path.
			otherErr := waitForOther(ctx, sniffCh, &wg)
			cancelRelay()

			if res.err == nil && otherErr == nil {
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
			return firstNonShutdownErr(res.err, otherErr)
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
			if isTimeoutErr(err) && ctx.Err() == nil {
				// Idle on this side; just loop to re-check ctx.
				continue
			}
			pushSignal(ctx, sniffCh, sniffResult{side: side, isTLS: false, err: err})
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
			pushSignal(ctx, sniffCh, sniffResult{side: side, isTLS: true, buffered: persisted})
			return
		}

		// Forward to the other side verbatim.
		if _, werr := to.Write(chunk); werr != nil {
			pushSignal(ctx, sniffCh, sniffResult{side: side, isTLS: false, err: werr})
			return
		}

		relayed += n
		if relayed >= opportunisticPeekMaxBytes {
			// Budget exhausted; sniffCh "no TLS" but keep relaying
			// until ctx is cancelled (the parent will start a clean
			// io.Copy after both goroutines exit).
			pushSignal(ctx, sniffCh, sniffResult{side: side, isTLS: false})
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
func (p *Proxy) hijackAndMITM(ctx context.Context, srcConn, dstConn net.Conn, bufferedClientHello []byte, dstAddr string, backdate time.Time) error {
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
	// of its own. ServerName is best-effort from the dial address;
	// InsecureSkipVerify is required because keploy is not the
	// authoritative CA for the upstream's chain.
	serverName := hostFromAddr(dstAddr)
	upstreamCfg := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // lgtm[go/disabled-tls-certificate-check] — MITM proxy by design; keploy is not the authoritative CA for the upstream chain
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
	g, gctx := errgroup.WithContext(ctx)
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
		<-done
		return ctx.Err()
	case err := <-done:
		_ = gctx
		return err
	}
}

// closeWriteIfPossible signals end-of-stream on the writing half so
// the peer's read returns EOF. Falls back to a no-op when the conn
// type doesn't support half-close.
func closeWriteIfPossible(c net.Conn) error {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

// pushSignal sends a result on the channel unless ctx is cancelled.
// The channel is buffered with capacity 2 so this never blocks in
// the normal path; the ctx.Done branch is only there so a cancelled
// goroutine doesn't deadlock if the parent has already moved on.
func pushSignal(ctx context.Context, ch chan<- sniffResult, res sniffResult) {
	select {
	case ch <- res:
	case <-ctx.Done():
	}
}

// waitForOther drains the second sniff result so both goroutines
// always exit before we tear down the connection. Returns the err
// field of the second result (nil on clean exit / budget hit).
func waitForOther(ctx context.Context, ch <-chan sniffResult, wg *sync.WaitGroup) error {
	// Wait for either the second sniffCh or the goroutines to finish.
	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case res := <-ch:
		<-doneCh
		return res.err
	case <-doneCh:
		// Both goroutines exited without a second sniffCh — possible
		// if the second one's result was dropped because the channel
		// was full. Treat as no-error; pure-relay fallback handles it.
		return nil
	}
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

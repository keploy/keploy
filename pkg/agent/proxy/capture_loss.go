package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
)

// CaptureLosses counts, by kind, the calls the proxy intercepted and could not
// record or mock since it started: a client-side TLS handshake that failed
// other than by the client giving it up, and one it abandoned silently once
// keploy's certificate was out (chargeClientHandshake), a TLS upgrade it could
// not make. Each only ever grows.
func (p *Proxy) CaptureLosses() models.CaptureLosses {
	return p.losses.Snapshot()
}

// Compile-time proof that the proxy reports its capture losses: were the
// method renamed, the agent's type assertion would stop matching and those
// losses would go unreported.
var _ agent.CaptureLossReporter = (*Proxy)(nil)

// teeDropLoss is the capture loss a relay tee's drop reason is, if any. A
// parser that stopped taking what was captured for it, whose queue was then
// abandoned (W8), and bytes dropped for want of room lose what they carried.
// A drop while the direction was paused (the parser asked for it, or a TLS
// upgrade was in flight) loses nothing the parser wanted, and a drop after a
// desync is the consequence of one already counted.
func teeDropLoss(reason string) (models.CaptureLossKind, bool) {
	switch reason {
	case relay.DropConsumerGone:
		return models.CaptureLossParserStall, true
	case relay.DropPerConnCap, relay.DropMemoryPressure:
		return models.CaptureLossDropped, true
	}
	return "", false
}

// teeDropped wraps a relay's drop hook (the session's MarkMockIncomplete) so
// that a drop that loses captured bytes is also counted as a capture loss of
// the connection to dst, which every test that ends after it is charged with
// under the proven switch. Not once the recording is stopping (stopping): a
// stop retires the parsers, and what they still had queued is abandoned as
// the stop's, not a test's loss -- as a TLS handshake a stopping agent cut
// short is not counted either.
func (p *Proxy) teeDropped(mark func(reason string), dst func() string, stopping func() bool) func(reason string) {
	return func(reason string) {
		if mark != nil {
			mark(reason)
		}
		if stopping != nil && stopping() {
			return
		}
		if kind, ok := teeDropLoss(reason); ok {
			p.losses.Add(kind, reason+" on a connection to "+dst())
		}
	}
}

// chargeClientHandshake counts a client-side TLS handshake with the proxy
// that failed (err, from pTls.HandleTLSConnection) as a capture loss when it
// lost a call: the client rejected the certificate keploy answered with (it
// does not trust keploy's CA), keploy refused its hello, or keploy could not
// make its certificate. Its call is then neither recorded nor mocked: the
// test's it happened in under the proven switch. A client that gave the
// handshake up (pTls.IsClientGiveUp) once keploy had sent its certificate
// (pTls.CertificateSent) is abandoned: it may have rejected the certificate
// without a word -- Node.js does so -- or given up (a cancel, a timeout), and
// nothing on the wire tells the two apart, so its test is not judged rather
// than failed or passed. One that gave up before keploy answered rejected
// nothing and made no call to lose, and one a stopping agent cut short is no
// loss of a test's. It returns the handshake's own error, for the caller to
// return as before.
func (p *Proxy) chargeClientHandshake(ctx context.Context, err error, dstAddr string) error {
	switch {
	case err == nil || ctx.Err() != nil:
	case !pTls.IsClientGiveUp(err):
		p.losses.Add(models.CaptureLossTLSAbort, "to "+dstAddr)
	case pTls.CertificateSent(err):
		p.losses.Add(models.CaptureLossTLSAbandoned, "to "+dstAddr)
	}
	return pTls.HandshakeCause(err)
}

// chargeSilentClient charges a client that hung up (err, pTls.IsClientGiveUp)
// on its TLS connection with keploy's proxy as soon as its handshake was done,
// having sent nothing since (pTls.SilentSinceHandshake: no data, no
// close_notify), as a handshake it abandoned (tls_handshake_abandoned): curl,
// libcurl and Node.js under TLS 1.2 reject a certificate they do not trust
// so, once the handshake is done, and a client that gave up looks the same.
// A client that sent anything after its handshake -- a call, or its
// close_notify -- is not charged, nor is one keploy's own stop cut short.
func (p *Proxy) chargeSilentClient(ctx context.Context, err error, clientTLS net.Conn, dstAddr string) {
	if ctx.Err() != nil || !pTls.IsClientGiveUp(err) || !pTls.SilentSinceHandshake(clientTLS) {
		return
	}
	p.losses.Add(models.CaptureLossTLSAbandoned, "to "+dstAddr)
}

// clientTLSHandler is pTls.HandleTLSConnection for a parser's TLS upgrader
// on the record path (util.ConnTLSUpgrader: MySQL's SSLRequest, Postgres's)
// on a connection to dstAddr: a handshake that failed is charged as the
// proxy's own client handshakes are (chargeClientHandshake), and its own
// error (pTls.HandshakeCause) is returned, which the parsers above it tell a
// client's EOF apart by identity with.
func (p *Proxy) clientTLSHandler(dstAddr string) util.ClientTLSHandlerFunc {
	return func(ctx context.Context, logger *zap.Logger, conn net.Conn, backdate time.Time) (net.Conn, bool, error) {
		c, isMTLS, err := pTls.HandleTLSConnection(ctx, logger, conn, backdate)
		if err != nil {
			err = p.chargeClientHandshake(ctx, err, dstAddr)
		}
		return c, isMTLS, err
	}
}

// relayClientHandshake is the relay's OnClientHandshakeFailed for a
// connection to dst: a client-side TLS upgrade a parser asked for (STARTTLS,
// Postgres's SSLRequest) that failed in its handshake is charged as any
// client-side handshake with the proxy (chargeClientHandshake) -- nothing for
// a client that hung up before keploy answered, abandoned once keploy's
// certificate was out, a loss for a rejection -- not as an upgrade keploy
// could not make (starttls_failed, a taint of every later test).
func (p *Proxy) relayClientHandshake(dst net.Conn) func(context.Context, error) {
	return func(ctx context.Context, err error) {
		addr := ""
		if dst != nil && dst.RemoteAddr() != nil {
			addr = dst.RemoteAddr().String()
		}
		_ = p.chargeClientHandshake(ctx, err, addr)
	}
}

// countParserPanic counts, as a capture loss, a parser that panicked while it
// served a connection to dst (util.ErrParserPanicked, which the parser's
// goroutine returns through util.RecoverInto): the call it was serving was
// neither answered from a mock nor recorded. That is keploy's failure, not a
// call the application made that has no recording (a miss), so it is not
// filed as one: the read of the run's misses leaves out a parser's error
// that is no miss. As a parser that stalled, it fails, under the proven
// switch, the test open when it happened and every later one
// (CAPTURE_INCOMPLETE). Before, the panicked goroutine sent nothing, its
// caller read its result channel's close as a clean end, and a test whose
// call was never answered could pass.
//
// dst is the destination's address, as a string or a net.Addr, read only for
// a panic.
func (p *Proxy) countParserPanic(err error, parser integrations.IntegrationType, dst any) {
	if errors.Is(err, util.ErrParserPanicked) {
		p.losses.Add(models.CaptureLossParserPanic, "the "+string(parser)+" parser, on a connection to "+fmt.Sprint(dst))
	}
}

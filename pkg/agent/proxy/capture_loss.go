package proxy

import (
	"context"
	"net"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
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

// handleClientTLS is pTls.HandleTLSConnection returning the handshake's own
// error (pTls.HandshakeCause), for a parser's TLS upgrader (the record path's
// util.ConnTLSUpgrader): the parsers above it tell a client's EOF apart by
// identity, as before.
func handleClientTLS(ctx context.Context, logger *zap.Logger, conn net.Conn, backdate time.Time) (net.Conn, bool, error) {
	c, isMTLS, err := pTls.HandleTLSConnection(ctx, logger, conn, backdate)
	return c, isMTLS, pTls.HandshakeCause(err)
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

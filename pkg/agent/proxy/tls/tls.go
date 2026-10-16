package tls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/neterr"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func IsTLSHandshake(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 && (data[2] == 0x00 || data[2] == 0x01 || data[2] == 0x02 || data[2] == 0x03)
}

// negotiateNextProtos picks the ALPN value list that the keploy TLS
// MITM server should advertise, given what the real client offered.
//
//   - http/1.1 advertised → return ["http/1.1"] (safer than h2 for HTTP apps;
//     downgrades a dual-protocol client to HTTP/1.1) UNLESS preserveH2 is set.
//   - h2 advertised but not http/1.1 → return ["h2", "http/1.1"]
//   - anything else (postgresql, mongodb, redis, no ALPN) → return nil so
//     Go's tls.Server skips ALPN negotiation and the client's offer
//     stands. Without this the handshake fails with
//     "tls: client requested unsupported application protocols" for
//     non-HTTP clients like libpq (ALPN=["postgresql"]).
//
// preserveH2: set on the REPLAY path when the target has recorded kind:Http2
// mocks (OutgoingOptions.PreferH2). A dual-protocol client (offering both h2
// and http/1.1 — every Go net/http / grpc-go / browser client) would otherwise
// be downgraded to http/1.1 by the http/1.1-first rule above; then its h2
// request could never match the recorded Http2 mock (the replay MITM can't
// dial the real upstream under the deny-egress netpolicy to discover the
// protocol). When preserveH2 is set and the client offers h2, we advertise
// ["h2","http/1.1"] so the client stays on h2 and matches the Http2 mock.
// Record and http/1.1-only recordings are unaffected (preserveH2 stays false).
func negotiateNextProtos(clientProtos []string, preserveH2 bool) []string {
	clientSupportsHTTP1 := false
	clientSupportsH2 := false
	for _, p := range clientProtos {
		switch p {
		case "http/1.1":
			clientSupportsHTTP1 = true
		case "h2":
			clientSupportsH2 = true
		}
	}
	if preserveH2 && clientSupportsH2 {
		return []string{"h2", "http/1.1"}
	}
	switch {
	case clientSupportsHTTP1:
		return []string{"http/1.1"}
	case clientSupportsH2:
		return []string{"h2", "http/1.1"}
	default:
		return nil
	}
}

// preferH2CtxKey carries the replay-side "preserve h2" hint into
// HandleTLSConnection without changing its signature (it is passed as a
// function value to the ConnTLSUpgrader, so a param change would ripple).
// The proxy sets it via WithPreferH2 on the replay handshake when the target
// has recorded kind:Http2 mocks (OutgoingOptions.PreferH2).
type preferH2CtxKey struct{}

// WithPreferH2 marks ctx so the MITM advertises h2 (not a forced http/1.1
// downgrade) when the client offers it. See negotiateNextProtos.
func WithPreferH2(ctx context.Context) context.Context {
	return context.WithValue(ctx, preferH2CtxKey{}, true)
}

func preferH2From(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(preferH2CtxKey{}).(bool)
	return v
}

// TLSLayerHolder is a connection whose close the proxy reads before it
// happens (pkg/agent/proxy's trackedConn): handed the TLS layer the proxy
// serves over it, its close reads through that layer what its socket still
// holds unread. A TLS client's close_notify, which carries no call, and data
// look alike to the socket's own count of unread bytes -- TLS 1.3 encrypts
// the alert -- and only the layer tells them apart.
type TLSLayerHolder interface {
	HoldTLSLayer(*tls.Conn)
}

// PassTLSLayer hands tlsConn to the first connection under conn that holds
// one (TLSLayerHolder), through the wrappers between (their NetConn, or
// Unwrap), if there is one. A wrapper that exposes neither passes it on
// itself by holding it (the relay's).
func PassTLSLayer(conn net.Conn, tlsConn *tls.Conn) {
	c := conn
	for i := 0; c != nil && i < 16; i++ { // a wrapper chain, never a cycle
		if h, ok := c.(TLSLayerHolder); ok {
			h.HoldTLSLayer(tlsConn)
			return
		}
		switch u := c.(type) {
		case interface{ NetConn() net.Conn }:
			c = u.NetConn()
		case interface{ Unwrap() net.Conn }:
			c = u.Unwrap()
		default:
			return
		}
	}
}

// IsClientGiveUp reports whether err, from the proxy's side of a client-side
// TLS handshake (HandleTLSConnection), is the client leaving without a word:
// it closed or reset its connection -- its own cancel, a request timeout, its
// stop -- and made no call keploy failed to capture. Every other failure lost
// its call: a certificate alert keploy read (Go, Java, rustls), one it could
// not (OpenSSL sends its TLS 1.3 alert in plaintext, which crypto/tls reads
// as "bad record MAC"), any other alert, a ClientHello keploy's server
// refused, a certificate keploy could not make. A client that rejects
// keploy's certificate by closing without an alert (Node.js) reads as a give
// up too: nothing on the wire tells the two apart.
func IsClientGiveUp(err error) bool {
	// neterr: syscall's ECONNRESET and EPIPE never match on Windows, whose
	// sockets give Winsock's codes.
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		neterr.IsConnReset(err) || neterr.IsBrokenPipe(err) || neterr.IsConnAborted(err)
}

// HandshakeError is a client-side handshake with keploy's proxy that failed
// (HandleTLSConnection's error): Err, and whether keploy had produced the
// certificate it answers the client with, so the client had it, or was being
// sent it, when the handshake failed.
type HandshakeError struct {
	Err             error
	CertificateSent bool
}

func (e *HandshakeError) Error() string { return e.Err.Error() }

func (e *HandshakeError) Unwrap() error { return e.Err }

// CertificateSent reports whether err, from HandleTLSConnection, failed once
// keploy had sent the client its certificate (HandshakeError): a client that
// hangs up then, without a word, may have rejected it -- Node.js does so --
// or given up, and nothing on the wire tells the two apart. One that hung up
// before rejected nothing.
func CertificateSent(err error) bool {
	var he *HandshakeError
	return errors.As(err, &he) && he.CertificateSent
}

// HandshakeCause is the handshake's own error under a HandshakeError: what
// a caller that tells errors apart by identity (io.EOF) compares.
func HandshakeCause(err error) error {
	var he *HandshakeError
	if errors.As(err, &he) {
		return he.Err
	}
	return err
}

// clientHandshakeReportKey carries WithClientHandshakeReport's report.
type clientHandshakeReportKey struct{}

// WithClientHandshakeReport is ctx carrying report, which HandleTLSConnection
// calls with its error when a client-side handshake made under ctx fails: an
// integration that makes its own client handshake (MySQL's, on its client's
// SSLRequest) is charged so as the proxy charges its own handshakes, without
// the error having to come back up through the integration.
func WithClientHandshakeReport(ctx context.Context, report func(error)) context.Context {
	return context.WithValue(ctx, clientHandshakeReportKey{}, report)
}

// reportClientHandshake hands err to ctx's report, if it carries one.
func reportClientHandshake(ctx context.Context, err error) {
	if report, ok := ctx.Value(clientHandshakeReportKey{}).(func(error)); ok && report != nil {
		report(err)
	}
}

func HandleTLSConnection(ctx context.Context, logger *zap.Logger, conn net.Conn, backdate time.Time) (net.Conn, bool, error) {
	preserveH2 := preferH2From(ctx)
	// 1. Load this run's signing CA (generated per run; see cagen.go). It is set
	// by SetupCAForApp before the proxy serves; getActiveCA lazily mints an
	// ephemeral one for callers that reach here without SetupCAForApp.
	ca := getActiveCA(logger)
	if ca == nil {
		err := fmt.Errorf("no MITM CA available for this run")
		utils.LogError(logger, err, "Failed to obtain the signing CA")
		reportClientHandshake(ctx, err)
		return nil, false, err
	}
	caPrivKey := ca.signer
	caCertParsed := ca.cert

	var certSent atomic.Bool
	config := &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			nextProtos := negotiateNextProtos(hello.SupportedProtos, preserveH2)
			logger.Debug("ALPN negotiation",
				zap.Strings("clientProtos", hello.SupportedProtos),
				zap.Strings("serverProtos", nextProtos))
			return &tls.Config{
				NextProtos: nextProtos,
				GetCertificate: func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
					cert, err := CertForClient(logger, clientHello, caPrivKey, caCertParsed, backdate)
					if err == nil {
						// crypto/tls sends it next, in the server's
						// first flight.
						certSent.Store(true)
					}
					return cert, err
				},
				ClientAuth: tls.RequestClientCert,
				VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
					return nil
				},
				ClientCAs:    nil,
				KeyLogWriter: KeyLogWriter(),
			}, nil
		},
	}

	// Wrap the TCP conn with TLS. Under the proven switch, over a conn that
	// hands crypto/tls a record at a time until the client's first byte
	// after the handshake, so that whether it sent any is known
	// (SilentSinceHandshake); without it, as before.
	var records *recordConn
	under := conn
	if provenswitch.On() {
		records = newRecordConn(conn)
		under = records
	}
	tlsConn := tls.Server(under, config)

	// Perform the handshake
	err := tlsConn.Handshake()
	if err == nil && records != nil {
		records.handshakeDone()
	}
	if err != nil {
		utils.LogError(logger, err, "failed to complete TLS/mTLS handshake")
		herr := &HandshakeError{Err: err, CertificateSent: certSent.Load()}
		reportClientHandshake(ctx, herr)
		return nil, false, herr
	}

	// The connection the proxy closes with care is handed the layer: its
	// close reads through it what its socket still holds (TLSLayerHolder).
	PassTLSLayer(conn, tlsConn)

	// 4. (Optional) Check what kind of connection happened
	// You can log this to verify if the client actually used mTLS or just standard TLS
	isMTLS := false
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) > 0 {
		logger.Debug("mTLS Handshake Success", zap.String("client_subject", state.PeerCertificates[0].Subject.CommonName))
		isMTLS = true
	} else {
		logger.Debug("Standard TLS Handshake Success (No Client Cert Provided)")
	}

	return tlsConn, isMTLS, nil
}

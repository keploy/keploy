package tls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"time"

	"github.com/cloudflare/cfssl/helpers"
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

func HandleTLSConnection(ctx context.Context, logger *zap.Logger, conn net.Conn, backdate time.Time) (net.Conn, bool, error) {
	preserveH2 := preferH2From(ctx)
	// 1. Load the Proxy's Signing CA (Used to generate server certs)
	caPrivKey, err := helpers.ParsePrivateKeyPEM(caPKey)
	if err != nil {
		utils.LogError(logger, err, "Failed to parse CA private key")
		return nil, false, err
	}
	caCertParsed, err := helpers.ParseCertificatePEM(caCrt)
	if err != nil {
		utils.LogError(logger, err, "Failed to parse CA certificate")
		return nil, false, err
	}

	config := &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			nextProtos := negotiateNextProtos(hello.SupportedProtos, preserveH2)
			logger.Debug("ALPN negotiation",
				zap.Strings("clientProtos", hello.SupportedProtos),
				zap.Strings("serverProtos", nextProtos))
			return &tls.Config{
				NextProtos: nextProtos,
				GetCertificate: func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
					return CertForClient(logger, clientHello, caPrivKey, caCertParsed, backdate)
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

	// Wrap the TCP conn with TLS
	tlsConn := tls.Server(conn, config)

	// Perform the handshake
	err = tlsConn.Handshake()
	if err != nil {
		utils.LogError(logger, err, "failed to complete TLS/mTLS handshake")
		return nil, false, err
	}

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

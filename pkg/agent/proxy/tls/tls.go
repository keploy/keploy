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
//   - http/1.1 advertised → return ["http/1.1"] (safer than h2 for HTTP apps)
//   - h2 advertised but not http/1.1 → return ["h2", "http/1.1"]
//   - anything else (postgresql, mongodb, redis, no ALPN) → return nil so
//     Go's tls.Server skips ALPN negotiation and the client's offer
//     stands. Without this the handshake fails with
//     "tls: client requested unsupported application protocols" for
//     non-HTTP clients like libpq (ALPN=["postgresql"]).
func negotiateNextProtos(clientProtos []string) []string {
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
	switch {
	case clientSupportsHTTP1:
		return []string{"http/1.1"}
	case clientSupportsH2:
		return []string{"h2", "http/1.1"}
	default:
		return nil
	}
}

func HandleTLSConnection(_ context.Context, logger *zap.Logger, conn net.Conn, backdate time.Time) (net.Conn, bool, error) {
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
			nextProtos := negotiateNextProtos(hello.SupportedProtos)
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
				ClientCAs: nil,
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

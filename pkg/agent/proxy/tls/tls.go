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
			// Check if client supports http/1.1
			clientSupportsHTTP1 := false
			for _, proto := range hello.SupportedProtos {
				if proto == "http/1.1" {
					clientSupportsHTTP1 = true
					break
				}
			}

			var nextProtos []string
			if clientSupportsHTTP1 {
				// Client supports HTTP/1.1, prefer it (safer for HTTP traffic)
				nextProtos = []string{"http/1.1"}
				logger.Debug("Client supports http/1.1, using http/1.1 only", zap.Strings("clientProtos", hello.SupportedProtos))
			} else {
				// Client only supports H2 (likely gRPC), must offer H2
				nextProtos = []string{"h2", "http/1.1"}
				logger.Debug("Client requires H2 (likely gRPC), offering H2", zap.Strings("clientProtos", hello.SupportedProtos))
			}

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

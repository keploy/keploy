package tls

import (
	"context"
	"crypto/tls"
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

func HandleTLSConnection(_ context.Context, logger *zap.Logger, conn net.Conn, backdate time.Time) (net.Conn, error) {
	//Load the CA certificate and private key

	caPrivKey, err := helpers.ParsePrivateKeyPEM(caPKey)
	if err != nil {
		utils.LogError(logger, err, "Failed to parse CA private key")
		return nil, err
	}
	caCertParsed, err := helpers.ParseCertificatePEM(caCrt)
	if err != nil {
		utils.LogError(logger, err, "Failed to parse CA certificate")
		return nil, err
	}

	// Create a TLS configuration with dynamic ALPN selection
	// We use GetConfigForClient to inspect what the client offers:
	// - gRPC clients typically only offer "h2" (no http/1.1), so we MUST offer h2
	// - HTTP clients offer both "h2" and "http/1.1", so we prefer http/1.1 (safer, since Keploy's HTTP parser doesn't handle H2)
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
			}, nil
		},
	}

	// Wrap the TCP conn with TLS
	tlsConn := tls.Server(conn, config)
	// Perform the handshake
	err = tlsConn.Handshake()

	if err != nil {
		utils.LogError(logger, err, "failed to complete TLS handshake with the client")
		return nil, err
	}
	// Use the tlsConn for further communication
	// For example, you can read and write data using tlsConn.Read() and tlsConn.Write()

	// Here, we simply close the conn
	return tlsConn, nil
}

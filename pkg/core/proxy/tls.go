package proxy

import (
	"crypto/tls"
	"github.com/cloudflare/cfssl/helpers"
	"go.uber.org/zap"
	"net"
)

func isTLSHandshake(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 && (data[2] == 0x00 || data[2] == 0x01 || data[2] == 0x02 || data[2] == 0x03)
}

func (ps *Proxy) handleTLSConnection(conn net.Conn) (net.Conn, error) {
	//Load the CA certificate and private key

	var err error
	caPrivKey, err = helpers.ParsePrivateKeyPEM(caPKey)
	if err != nil {
		ps.logger.Error("Failed to parse CA private key: ", zap.Error(err))
		return nil, err
	}
	caCertParsed, err = helpers.ParseCertificatePEM(caCrt)
	if err != nil {
		ps.logger.Error("Failed to parse CA certificate: ", zap.Error(err))
		return nil, err
	}

	// Create a TLS configuration
	config := &tls.Config{
		GetCertificate: certForClient,
	}

	// Wrap the TCP conn with TLS
	tlsConn := tls.Server(conn, config)
	// Perform the handshake
	err = tlsConn.Handshake()

	if err != nil {
		ps.logger.Error("failed to complete TLS handshake with the client with error: ", zap.Error(err))
		return nil, err
	}
	// Use the tlsConn for further communication
	// For example, you can read and write data using tlsConn.Read() and tlsConn.Write()

	// Here, we simply close the conn
	return tlsConn, nil
}

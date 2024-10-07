
package proxy

import (
	"crypto/tls"
	"net"

	"github.com/cloudflare/cfssl/helpers"
	"go.keploy.io/server/v2/utils"
)

func isTLSHandshake(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 && (data[2] == 0x00 || data[2] == 0x01 || data[2] == 0x02 || data[2] == 0x03)
}

func (p *Proxy) handleTLSConnection(conn net.Conn) (net.Conn, error) {
	//Load the CA certificate and private key

	var err error
	caPrivKey, err = helpers.ParsePrivateKeyPEM(caPKey)
	if err != nil {
		utils.LogError(p.logger, err, "Failed to parse CA private key")
		return nil, err
	}
	caCertParsed, err = helpers.ParseCertificatePEM(caCrt)
	if err != nil {
		utils.LogError(p.logger, err, "Failed to parse CA certificate")
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
		utils.LogError(p.logger, err, "failed to complete TLS handshake with the client")
		return nil, err
	}
	// Use the tlsConn for further communication
	// For example, you can read and write data using tlsConn.Read() and tlsConn.Write()

	// Here, we simply close the conn
	return tlsConn, nil
}

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

	// 3. Create TLS Configuration
	config := &tls.Config{
		// A. Server Identity: Present the Proxy's certificate to the client
		GetCertificate: func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if clientHello.ServerName == "" {
				clientHello.ServerName = "127.0.0.1"
			}
			return CertForClient(logger, clientHello, caPrivKey, caCertParsed, backdate)
		},

		// B. Client Identity: OPTIONAL TRUST ALL MODE
		// RequestClientCert:
		// - Request a certificate from the client.
		// - If client sends one, we accept it (and skip verification via VerifyPeerCertificate).
		// - If client sends NONE, we continue (Standard TLS).
		ClientAuth: tls.RequestClientCert,

		// Custom verification validation to skip verifying the client's certificate chain.
		// This effectively trusts ANY certificate the client sends.
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			return nil
		},

		// C. Trusted Authorities
		// Set to nil so we don't send a restrictive list of accepted CAs to the client.
		// This encourages the client to send its certificate regardless of who signed it.
		ClientCAs: nil,
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

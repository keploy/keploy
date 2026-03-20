package tls

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func IsTLSHandshake(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 && (data[2] == 0x00 || data[2] == 0x01 || data[2] == 0x02 || data[2] == 0x03)
}

// sharedTicketKey ensures all TLS configs produced by the proxy use the same
// session ticket encryption key. Without this, every config gets a random key
// and TLS session resumption NEVER works — every connection does a full
// handshake (~5-10 ms of pure CPU for key exchange).
var (
	sharedTicketKeyOnce sync.Once
	sharedTicketKey     [32]byte
	// If key generation fails, we fail closed by disabling tickets.
	sessionTicketsReady bool
	// randRead is a variable for deterministic tests.
	randRead = rand.Read
)

func getSharedTicketKey() ([32]byte, bool) {
	sharedTicketKeyOnce.Do(func() {
		n, err := randRead(sharedTicketKey[:])
		if err != nil || n != len(sharedTicketKey) {
			if err == nil {
				err = errors.New("short read from crypto/rand while generating TLS session ticket key")
			}
			utils.LogError(
				zap.L(),
				err,
				"failed to generate shared TLS session ticket key; disabling TLS session tickets",
				zap.Int("bytesRead", n),
				zap.Int("bytesExpected", len(sharedTicketKey)),
			)
			sessionTicketsReady = false
			sharedTicketKey = [32]byte{}
			return
		}
		sessionTicketsReady = true
	})
	return sharedTicketKey, sessionTicketsReady
}

func HandleTLSConnection(_ context.Context, logger *zap.Logger, conn net.Conn, backdate time.Time) (net.Conn, bool, error) {
	// Use cached/parsed-once CA credentials instead of parsing PEM every time.
	caPrivKey, caCertParsed, err := GetParsedCA()
	if err != nil {
		utils.LogError(logger, err, "Failed to get parsed CA credentials. Check file permissions on CA credentials or ensure they are properly initialized")
		return nil, false, err
	}

	ticketKey, sessionTicketsEnabled := getSharedTicketKey()

	config := &tls.Config{
		SessionTicketKey:       ticketKey,
		SessionTicketsDisabled: !sessionTicketsEnabled,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			// Check what protocols client supports
			clientSupportsHTTP1 := false
			clientSupportsPostgres := false
			for _, proto := range hello.SupportedProtos {
				if proto == "http/1.1" {
					clientSupportsHTTP1 = true
				}
				if proto == "postgresql" {
					clientSupportsPostgres = true
				}
			}

			var nextProtos []string
			if clientSupportsPostgres {
				nextProtos = []string{"postgresql"}
				logger.Debug("Client supports postgresql ALPN, using postgresql", zap.Strings("clientProtos", hello.SupportedProtos))
			} else if clientSupportsHTTP1 {
				nextProtos = []string{"http/1.1"}
				logger.Debug("Client supports http/1.1, using http/1.1 only", zap.Strings("clientProtos", hello.SupportedProtos))
			} else {
				nextProtos = []string{"h2", "http/1.1"}
				logger.Debug("Client requires H2 (likely gRPC), offering H2", zap.Strings("clientProtos", hello.SupportedProtos))
			}

			return &tls.Config{
				NextProtos:             nextProtos,
				SessionTicketKey:       ticketKey, // SAME key so session tickets work across connections
				SessionTicketsDisabled: !sessionTicketsEnabled,
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

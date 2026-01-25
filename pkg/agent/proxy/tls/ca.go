// Package tls provides functionality for handling tls connetions.
package tls

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cloudflare/cfssl/csr"
	cfsslLog "github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
	"go.keploy.io/server/v3/pkg/core/certs"
	"go.uber.org/zap"
)

// SetupCA setups custom certificate authority to handle TLS connections
// Deprecated: Use pkg/core/certs.SetupCA instead
func SetupCA(ctx context.Context, logger *zap.Logger, isDocker bool) error {
	return certs.SetupCA(ctx, logger, isDocker)
}

// SetupCaCertEnv extracts the cert to a temp file and sets the env vars.
// Deprecated: Use pkg/core/certs.SetupCaCertEnv instead
func SetupCaCertEnv(logger *zap.Logger) error {
	return certs.SetupCaCertEnv(logger)
}

// SrcPortToDstURL map is used to store the mapping between source port and DstURL for the TLS connection
var SrcPortToDstURL = sync.Map{}

var setLogLevelOnce sync.Once

func CertForClient(logger *zap.Logger, clientHello *tls.ClientHelloInfo, caPrivKey any, caCertParsed *x509.Certificate, backdate time.Time) (*tls.Certificate, error) {
	// Ensure log level is set only once

	/*
	* Since multiple goroutines can call this function concurrently, we need to ensure that the log level is set only once.
	 */
	setLogLevelOnce.Do(func() {
		// * Set the log level to error to avoid unnecessary logs. like below...

		// 2025/03/18 20:54:25 [INFO] received CSR
		// 2025/03/18 20:54:25 [INFO] generating key: ecdsa-256
		// 2025/03/18 20:54:25 [INFO] received CSR
		// 2025/03/18 20:54:25 [INFO] generating key: ecdsa-256
		// 2025/03/18 20:54:25 [INFO] encoded CSR
		// 2025/03/18 20:54:25 [INFO] encoded CSR
		// 2025/03/18 20:54:25 [INFO] signed certificate with serial number 435398774381835435678674951099961010543769077102
		cfsslLog.Level = cfsslLog.LevelError
	})

	// Generate a new server certificate and private key for the given hostname
	dstURL := clientHello.ServerName
	remoteAddr := clientHello.Conn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	SrcPortToDstURL.Store(sourcePort, dstURL)

	serverReq := &csr.CertificateRequest{
		//Make the name accordng to the ip of the request
		CN: clientHello.ServerName,
		Hosts: []string{
			clientHello.ServerName,
		},
		KeyRequest: csr.NewKeyRequest(),
	}

	serverCsr, serverKey, err := csr.ParseRequest(serverReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create server CSR: %v", err)
	}
	cryptoSigner, ok := caPrivKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("failed to typecast the caPrivKey")
	}
	signerd, err := local.NewSigner(cryptoSigner, caCertParsed, signer.DefaultSigAlgo(cryptoSigner), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %v", err)
	}

	if backdate.IsZero() {
		logger.Debug("backdate is zero, using current time")
		backdate = time.Now()
	}

	// Case: time freezing (an Ent. feature) is enabled,
	// If application time is frozen in past, and the certificate is signed today, then the certificate will be invalid.
	// This results in a certificate error during tls handshake.
	// To avoid this, we set the certificate’s validity period (NotBefore and NotAfter)
	// by referencing the testcase request time of the application (backdate) instead of the current real time.
	//
	// Note: If you have recorded test cases before April 20, 2024 (http://www.sslchecker.com/certdecoder?su=269725513dfeb137f6f29b8488f17ca9)
	// and are using time freezing, please reach out to us if you get tls handshake error.
	signReq := signer.SignRequest{
		Hosts:     serverReq.Hosts,
		Request:   string(serverCsr),
		Profile:   "web",
		NotBefore: backdate.AddDate(-1, 0, 0),
		NotAfter:  time.Now().AddDate(1, 0, 0),
	}

	serverCert, err := signerd.Sign(signReq)
	if err != nil {
		return nil, fmt.Errorf("failed to sign server certificate: %v", err)
	}

	logger.Debug("signed the certificate for a duration of 2 years", zap.String("notBefore", signReq.NotBefore.String()), zap.String("notAfter", signReq.NotAfter.String()))

	// Load the server certificate and private key
	serverTLSCert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load server certificate and key: %v", err)
	}

	return &serverTLSCert, nil
}

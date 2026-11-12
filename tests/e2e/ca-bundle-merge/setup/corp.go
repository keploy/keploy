package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// corpServerName is the DNS name (the compose service) the corporate server's
// certificate is issued for.
const corpServerName = "corp-server"

// genCorp writes a private "corporate" PKI to dir: ca/corp-ca.pem (the CA an
// operator would mount and name with SSL_CERT_FILE or SSL_CERT_DIR) and
// server.crt / server.key, a leaf for corpServerName that CA signed.
func genCorp(dir string) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "e2e corporate root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: corpServerName},
		DNSNames:     []string{corpServerName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "ca"), 0o755); err != nil {
		return err
	}
	files := map[string]*pem.Block{
		filepath.Join(dir, "ca", "corp-ca.pem"): {Type: "CERTIFICATE", Bytes: caDER},
		filepath.Join(dir, "server.crt"):        {Type: "CERTIFICATE", Bytes: leafDER},
		filepath.Join(dir, "server.key"):        {Type: "EC PRIVATE KEY", Bytes: leafKeyDER},
	}
	for path, block := range files {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// serveCorp serves HTTPS on :8443 with the leaf genCorp wrote to dir.
func serveCorp(dir string) error {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              ":8443",
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintln(w, "corp ok")
		}),
	}
	return srv.ListenAndServeTLS("", "")
}

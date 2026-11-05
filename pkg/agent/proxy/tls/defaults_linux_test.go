//go:build linux

package tls

// Linux's production defaults, with no test seam set: the upstream pool is the
// loader's (systemPoolIsFileBacked), the loader reads the default certificate
// directories (systemCABundleSearchDirs), and SSL_CERT_FILE is read through
// readRegularFile's cap and its refusal of anything but a regular file.

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// An SSL_CERT_FILE that cannot be used, a corporate CA in SSL_CERT_DIR and a
// Debian bundle, with upstreamTls.verify on: Go's pool holds the corporate CA
// alone, so on Linux keploy verifies upstreams against the loader's bundle,
// which keeps the public roots the apps trust.
func TestLoadUpstreamRootCAs_LinuxDefault_UsesTheLoadersPool(t *testing.T) {
	isolateCertEnv(t)
	ca, caKey := newTestCA(t, "debian-public-root")
	leaf := newTestLeaf(t, ca, caKey, "upstream.example")
	debianRoot := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
	corp := makeSelfSignedPEM(t, "corp-root")
	base := t.TempDir()
	corpDir := filepath.Join(base, "corp")
	if err := os.MkdirAll(corpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePEMFile(t, corpDir, "corp.pem", corp)
	debian := writePEMFile(t, base, "ca-certificates.crt", debianRoot)
	orig := systemCertPoolFn
	systemCertPoolFn = func() (*x509.CertPool, error) { return poolWith(t, corp), nil }
	t.Cleanup(func() { systemCertPoolFn = orig })
	withSystemSearch(t, []string{debian}, nil)
	t.Setenv(envSSLCertFile, filepath.Join(base, "missing.pem"))
	t.Setenv(envSSLCertDir, corpDir)
	pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
	if err != nil {
		t.Fatal(err)
	}
	if pool == nil {
		t.Fatal("nil: Go's corporate-only pool")
	}
	if _, verr := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "upstream.example"}); verr != nil {
		t.Fatalf("public upstream rejected: %v", verr)
	}
}

// SSL_CERT_FILE naming a corporate CA alone and SSL_CERT_DIR unset: the roots
// in the host's default certificate directories reach the apps' bundle, as Go
// reads them, or the apps get the corporate CA alone (keploy/k8s-proxy#375).
func TestLoadSystemCABundle_LinuxDefault_ReadsTheDefaultCertDirs(t *testing.T) {
	isolateCertEnv(t)
	if want := []string{"/etc/ssl/certs", "/etc/pki/tls/certs"}; !slices.Equal(systemCABundleSearchDirs, want) {
		t.Fatalf("Linux reads the certificate directories %q; want crypto/x509's %q", systemCABundleSearchDirs, want)
	}
	host := 0
	for _, d := range []string{"/etc/ssl/certs", "/etc/pki/tls/certs"} {
		if ents, err := os.ReadDir(d); err == nil {
			host += len(ents)
		}
	}
	if host == 0 {
		t.Skip("host has no default cert dirs")
	}
	origPaths := systemCABundleSearchPaths
	systemCABundleSearchPaths = nil
	t.Cleanup(func() { systemCABundleSearchPaths = origPaths })
	corp := makeSelfSignedPEM(t, "corp-root")
	f := writePEMFile(t, t.TempDir(), "corp.pem", corp)
	t.Setenv(envSSLCertFile, f)
	t.Setenv(envSSLCertDir, "")
	data, src := loadSystemCABundle(zap.NewNop())
	if n := bytes.Count(data, []byte("BEGIN CERTIFICATE")); n < 2 {
		t.Fatalf("bundle holds %d certificate(s) from %q: the corporate CA alone, no default-dir roots", n, src)
	}
}

// SSL_CERT_FILE goes through readRegularFile: a file over the cap is not read,
// and a FIFO is refused without blocking the agent's start.
func TestLoadSystemCABundle_SSLCertFile_IsCappedAndNeverBlocks(t *testing.T) {
	isolateCertEnv(t)
	withSystemSearch(t, nil, nil)
	dir := t.TempDir()
	over := makeSelfSignedPEM(t, "over-cap-root")
	big := append(append([]byte(nil), over...), bytes.Repeat([]byte("\n"), maxCertFile+1-len(over))...)
	p := filepath.Join(dir, "big.pem")
	if err := os.WriteFile(p, big, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envSSLCertFile, p)
	t.Setenv(envSSLCertDir, "")
	data, _ := loadSystemCABundle(zap.NewNop())
	if bytes.Contains(data, bytes.TrimSpace(over)) {
		t.Fatal("an SSL_CERT_FILE over the cap was read and used")
	}
	fifo := filepath.Join(dir, "fifo.pem")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	})
	t.Setenv(envSSLCertFile, fifo)
	done := make(chan struct{})
	go func() { loadSystemCABundle(zap.NewNop()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("SSL_CERT_FILE naming a FIFO blocked the loader")
	}
}

// Linux with no bundle file and a corporate CA in SSL_CERT_DIR (the enterprise
// agent image given a corporate CA): the embedded roots are the normal path
// there, so upstream verification logs nothing at Info or above.
func TestLoadUpstreamRootCAs_LinuxNoBundleFile_LogsNothingAtInfo(t *testing.T) {
	isolateCertEnv(t)
	withSystemSearch(t, nil, nil)
	corpDir := t.TempDir()
	writePEMFile(t, corpDir, "corp.pem", makeSelfSignedPEM(t, "corp-root"))
	t.Setenv(envSSLCertFile, "")
	t.Setenv(envSSLCertDir, corpDir)
	core, logs := observer.New(zapcore.InfoLevel)
	pool, err := LoadUpstreamRootCAs(zap.New(core), "")
	if err != nil || pool == nil {
		t.Fatalf("pool=%v err=%v", pool, err)
	}
	if logs.Len() != 0 {
		t.Fatalf("logged at Info+: %v", logs.All())
	}
}

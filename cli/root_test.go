package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	ptls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.uber.org/zap"
)

// caTrustEnv is every variable SetupCaCertEnv sets in this process. A row that
// runs it restores them (t.Setenv), so no later test, and no child it spawns,
// is pointed at a file in a temp dir that is gone.
var caTrustEnv = []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO", ptls.EnvJavaToolOptions}

// privateTempDir points os.TempDir, and so SetupCaCertEnv's file, at a
// directory of the row's own, and restores the trust variables when the row
// ends. On Windows os.TempDir reads TMP/TEMP, not TMPDIR.
func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, dir)
	}
	for _, k := range caTrustEnv {
		t.Setenv(k, "")
	}
	return dir
}

// testCACertPEM is a self-signed CA certificate, standing in for the one a
// native client fetches from its agent.
func testCACertPEM(t testing.TB) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "keploy test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// caTrustFilesIn lists the trust files SetupCaCertEnv leaves in dir.
func caTrustFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "keploy-ca-*.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestRoot_RemovesTheCATrustFileWhenTheCommandFinishes: a native run points
// the app's trust variables at a file holding the agent's CA certificate
// (ptls.SetupCaCertEnv). The app reads it for as long as the command runs, and
// nothing reads it after: it must go when the command finishes, however it
// finishes. It used to be left in the temp dir on every run, root-owned under
// sudo, one more each run.
func TestRoot_RemovesTheCATrustFileWhenTheCommandFinishes(t *testing.T) {
	for _, end := range []struct {
		name string
		end  func() error
	}{
		{"it succeeds", func() error { return nil }},
		{"it fails", func() error { return errors.New("the run failed") }},
		{"it panics", func() error { panic("the run panicked") }},
	} {
		t.Run(end.name, func(t *testing.T) {
			dir := privateTempDir(t)
			pemBytes := testCACertPEM(t)

			root := Root(context.Background(), zap.NewNop(), agentSvcFactory{}, noFlags{})
			var trustFile string
			root.AddCommand(&cobra.Command{
				Use: "run-an-app",
				RunE: func(*cobra.Command, []string) error {
					if err := ptls.SetupCaCertEnv(zap.NewNop(), pemBytes); err != nil {
						return err
					}
					trustFile = os.Getenv("SSL_CERT_FILE")
					if _, err := os.Stat(trustFile); err != nil {
						t.Errorf("the app's trust file is not there while the command runs: %v", err)
					}
					return end.end()
				},
			})
			root.SetArgs([]string{"run-an-app"})
			root.SilenceErrors, root.SilenceUsage = true, true

			func() {
				defer func() { _ = recover() }()
				_ = root.Execute()
			}()

			if trustFile == "" {
				t.Fatal("the command never set up the app's trust file")
			}
			if _, err := os.Stat(trustFile); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the app's CA trust file %s outlived the command (stat: %v)", trustFile, err)
			}
			if left := caTrustFilesIn(t, dir); len(left) != 0 {
				t.Errorf("the app's CA trust file outlived the command: %v", left)
			}
		})
	}
}

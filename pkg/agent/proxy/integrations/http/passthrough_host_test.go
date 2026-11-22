package http

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A call a bypassRule passes through is dialled to its server under the TLS
// name its client sent (SNI): the dial's error, which the decode loop logs,
// names that name whole, and the certificate's own names beside it
// ("x509: certificate is valid for example.com, not <name>"). Its error names
// each by its labels' shape beside its tag in the connection's session, and
// errors.As still finds the x509 error.
func TestAPassedThroughCallsDialErrorNamesNoHost(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	name := strings.ToLower(logleak.Canary) + ".tenant.example"
	keys := models.NewLogKeySession()
	ctx := models.WithLogKeySession(context.Background(), keys)
	client, server := net.Pipe()
	defer func() { _ = client.Close(); _ = server.Close() }()
	dst := &models.ConditionalDstCfg{Addr: strings.TrimPrefix(srv.URL, "https://"), TLSCfg: &tls.Config{ServerName: name, RootCAs: roots, MinVersion: tls.VersionTLS12}}

	_, err := passOneCallThrough(ctx, zap.NewNop(), server, dst, []byte("GET / HTTP/1.1\r\nHost: "+name+"\r\n\r\n"), http.MethodGet)
	if err == nil {
		t.Fatal("precondition: the server's certificate is not for the name, and the dial passed")
	}
	if logleak.Holds(err.Error(), logleak.Canary) || strings.Contains(err.Error(), "example.com") {
		t.Fatalf("the error, which the decode loop logs, names a host whole: %q", err)
	}
	if !strings.Contains(err.Error(), keys.HostLogName(name)) {
		t.Fatalf("the error %q does not name the TLS name by its shape and tag %q", err, keys.HostLogName(name))
	}
	var hn x509.HostnameError
	if !errors.As(err, &hn) {
		t.Fatalf("the error %v lost its x509 cause", err)
	}
}

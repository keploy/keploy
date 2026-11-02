//go:build linux

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A test's TLS call to a TLS listener of the run's own tree (an app the test
// started that serves HTTPS) reaches that listener's TLS, as it does without
// keploy, through the proxy's chain under the proven switch: the live handler
// (incoming.ServeLive) relays it unread, so the client verifies the
// listener's own certificate and the listener answers it. Before, with the
// app's calls recorded as test cases (the live handler's requests), the
// ingress took the client's ClientHello for an HTTP/1 request, answered it
// "400 Bad Request" in plain text, and the client failed with "http: server
// gave HTTP response to HTTPS client".
func TestALiveTLSCallReachesTheTreesTLSListener(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync bool
		// real: the listener is a mock.real destination too.
		real bool
	}{
		{"live", false, false},
		{"synchronous: live", true, false},
		{"mock.real", false, true},
		{"synchronous: mock.real", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "tls-pong "+r.URL.Path)
			}))
			srv.StartTLS()
			t.Cleanup(srv.Close)
			lc, _ := serveTreeCallTo(t, tc.sync, tc.real, srv.Listener.Addr().(*net.TCPAddr))

			resp, err := tlsClientOver(srv, lc.app).Get(srv.URL + "/user/1")
			if err != nil {
				t.Fatalf("the test's TLS call to the tree's TLS listener: %v", err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || string(body) != "tls-pong /user/1" {
				t.Fatalf("the listener's answer: %d %q (%v), want 200 %q", resp.StatusCode, body, err, "tls-pong /user/1")
			}
			if certs := resp.TLS.PeerCertificates; len(certs) == 0 || !certs[0].Equal(srv.Certificate()) {
				t.Fatalf("the client was not shown the listener's own certificate")
			}
			_ = lc.app.Close()
			select {
			case <-lc.served:
			case <-lc.never:
				t.Fatalf("the live call's handler never ended: %s", lc.state())
			}
		})
	}
}

// tlsClientOver is srv's own client (trusting srv's certificate, and only
// it), whose one connection is conn, the test's end of the call.
func tlsClientOver(srv *httptest.Server, conn net.Conn) *http.Client {
	tr := srv.Client().Transport.(*http.Transport).Clone()
	var dialed atomic.Bool
	tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
		if dialed.Swap(true) {
			return nil, errors.New("the test's call has one connection")
		}
		return conn, nil
	}
	tr.DisableKeepAlives = true
	return &http.Client{Transport: tr}
}

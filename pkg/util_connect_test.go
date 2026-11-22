package pkg

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// connectApp answers a CONNECT with 200 (Connection Established) and then
// relays the tunnel, as an HTTP proxy does: it sends the bytes of the
// connection it tunnels to (here the start of a TLS server's answer), ends its
// side and keeps reading its peer until its peer ends the connection, which
// ended then says (a keploy that keeps the tunnel open hangs the test there).
func connectApp(t *testing.T) (url string, ended <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		// A CONNECT's target is an authority, never a path (RFC 9110 §9.3.6).
		if r.RequestURI != "upstream.invalid:443" {
			http.Error(w, "want CONNECT upstream.invalid:443, got CONNECT "+r.RequestURI, http.StatusBadRequest)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_, _ = rw.WriteString("\x16\x03\x03\x00\x7aserver hello of the tunnelled connection")
		_ = rw.Flush()
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		_, _ = io.Copy(io.Discard, rw)
		ch <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, ch
}

// connectCase is a CONNECT as Keploy Enterprise's capture records one, to the
// app at url: a URL of the target it asks to reach (its Host) with the path
// "/", and the app's port; keploy test sends it to the app on that port.
func connectCase(t *testing.T, url string) (*models.TestCase, SimulationConfig) {
	t.Helper()
	u, err := neturl.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return &models.TestCase{Name: "connect-1", Kind: models.HTTP, AppPort: uint16(port), HTTPReq: models.HTTPReq{
		Method: "CONNECT", URL: "http://upstream.invalid:443/", Header: map[string]string{"Host": "upstream.invalid:443"},
	}}, SimulationConfig{ConfigHost: u.Hostname()}
}

// A 2xx answer to CONNECT ends at its header section: from there the
// connection is a tunnel (RFC 9110 §9.3.6), its bytes another connection's.
// keploy test's answer is the 200 with no body, and it ends the tunnel unread.
// It sends the CONNECT in authority form, the target its recording asked for.
func TestAConnectAnswerEndsAtItsHeaders(t *testing.T) {
	url, ended := connectApp(t)
	tc, cfg := connectCase(t, url)
	resp, err := SimulateHTTP(context.Background(), tc, "set", zap.NewNop(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Body != "" {
		t.Fatalf("got %d with body %q, want a 200 with none: a 400 is the CONNECT sent with the wrong target, a body the tunnel read past the answer's headers", resp.StatusCode, resp.Body)
	}
	<-ended // the app saw keploy end the tunnel; a keploy that keeps it open hangs here
}

// The streaming path ends it at its headers too.
func TestAConnectAnswerStreamsNothing(t *testing.T) {
	url, ended := connectApp(t)
	tc, cfg := connectCase(t, url)
	resp, err := SimulateHTTPStreaming(context.Background(), tc, "set", zap.NewNop(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Reader.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("got %d with %q to stream, want a 200 with nothing: a 400 is the CONNECT sent with the wrong target, bytes the tunnel read past the answer's headers", resp.StatusCode, body)
	}
	<-ended
}

// With a base path (the request sent where the run's URL names, its Host
// that URL's: TargetFromURL) a CONNECT still names the target it was recorded
// asking for, as its request target and as its Host: the base path's host is
// the proxy it goes through.
func TestAConnectToABasePathNamesItsRecordedTarget(t *testing.T) {
	url, ended := connectApp(t)
	tc := &models.TestCase{Name: "connect-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{
		Method: "CONNECT", URL: url + "/", Header: map[string]string{"Host": "upstream.invalid:443"},
	}}
	var sentTo, askedFor string
	cfg := SimulationConfig{TargetFromURL: true, SentTo: func(target *neturl.URL, host, _ string) {
		sentTo, askedFor = target.Host, host
	}}
	resp, err := SimulateHTTP(context.Background(), tc, "set", zap.NewNop(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Body != "" {
		t.Fatalf("got %d with body %q, want a 200 with none", resp.StatusCode, resp.Body)
	}
	if app := strings.TrimPrefix(url, "http://"); sentTo != app || askedFor != "upstream.invalid:443" {
		t.Fatalf("sent to %q asking for %q, want to the app at %q asking for the recorded target upstream.invalid:443", sentTo, askedFor, app)
	}
	<-ended
}

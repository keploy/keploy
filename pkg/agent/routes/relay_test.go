package routes

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// stubPodIP overrides resolvePodIP for the duration of a test so the relay
// dials the httptest server on loopback instead of scanning real interfaces.
func stubPodIP(t *testing.T, ip string) {
	t.Helper()
	orig := resolvePodIP
	resolvePodIP = func() (string, error) { return ip, nil }
	t.Cleanup(func() { resolvePodIP = orig })
}

// newRelayRouter builds a chi router with the relay handler wired at
// /agent/relay/{port}/* — the same path the production agent uses.
func newRelayRouter(t *testing.T) *chi.Mux {
	t.Helper()
	r := chi.NewRouter()
	a := &Agent{logger: zap.NewNop()}
	r.HandleFunc("/agent/relay/{port}/*", a.HandleRelay)
	return r
}

func TestHandleRelay_ForwardsRequestAndPreservesHost(t *testing.T) {
	var gotHost, gotPath, gotMethod, gotQuery, gotBody string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
		}
		w.Header().Set("X-Test-Response", "ok")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer app.Close()

	appPort := app.Listener.Addr().(*net.TCPAddr).Port
	stubPodIP(t, "127.0.0.1")

	router := newRelayRouter(t)

	body := strings.NewReader(`{"jsonrpc":"2.0"}`)
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/agent/relay/%d/client/v1/mcp?foo=bar", appPort), body)
	req.Host = "api.keploy.io"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if gotHost != "api.keploy.io" {
		t.Fatalf("Host = %q, want %q — the relay must preserve the original Host header", gotHost, "api.keploy.io")
	}
	if gotPath != "/client/v1/mcp" {
		t.Fatalf("Path = %q, want %q", gotPath, "/client/v1/mcp")
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("Method = %q, want POST", gotMethod)
	}
	if gotQuery != "foo=bar" {
		t.Fatalf("Query = %q, want %q", gotQuery, "foo=bar")
	}
	if gotBody != `{"jsonrpc":"2.0"}` {
		t.Fatalf("Body = %q, want %q", gotBody, `{"jsonrpc":"2.0"}`)
	}
	if rec.Header().Get("X-Test-Response") != "ok" {
		t.Fatal("response header X-Test-Response not proxied")
	}
}

func TestHandleRelay_InvalidPort(t *testing.T) {
	stubPodIP(t, "127.0.0.1")
	router := newRelayRouter(t)

	for _, port := range []string{"0", "99999", "abc", "-1"} {
		req := httptest.NewRequest(http.MethodGet, "/agent/relay/"+port+"/health", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("port %q: status = %d, want 400", port, rec.Code)
		}
	}
}

func TestHandleRelay_RootPath(t *testing.T) {
	var gotPath string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer app.Close()
	appPort := app.Listener.Addr().(*net.TCPAddr).Port
	stubPodIP(t, "127.0.0.1")

	router := newRelayRouter(t)
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/agent/relay/%d/", appPort), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/" {
		t.Fatalf("Path = %q, want %q", gotPath, "/")
	}
}

func TestExtractRelayPort(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/agent/relay/8080/foo", "8080"},
		{"/agent/relay/443/", "443"},
		{"/agent/relay/9090", "9090"},
		{"/agent/relay/", ""},
		{"/agent/relay", ""},
		{"/other/path", ""},
	}
	for _, tt := range tests {
		got := extractRelayPort(tt.path)
		if got != tt.want {
			t.Errorf("extractRelayPort(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// TestHandleRelay_ThroughAuthenticate drives the relay through the production
// Authenticate middleware. The relay must pass even when:
// - the request carries an Origin header (recorded from a browser call)
// - the agent has a session token and the request carries the app's own
//   Authorization, not the agent token
// Without the auth exemption, these produce 403 and 401 respectively.
func TestHandleRelay_ThroughAuthenticate(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer app.Close()
	appPort := app.Listener.Addr().(*net.TCPAddr).Port
	stubPodIP(t, "127.0.0.1")

	// Build router WITH Authenticate middleware, matching production wiring.
	agentToken := "test-agent-secret-token"
	r := chi.NewRouter()
	r.Use(Authenticate(zap.NewNop(), agentToken))
	a := &Agent{logger: zap.NewNop()}
	r.HandleFunc("/agent/relay/{port}/*", a.HandleRelay)

	tests := []struct {
		name   string
		origin string
		auth   string
		want   int
	}{
		{"Origin header from recorded browser call", "https://app.keploy.io", "", http.StatusOK},
		{"app's own Authorization (not agent token)", "", "Bearer app-jwt-token", http.StatusOK},
		{"both Origin and app Authorization", "https://app.keploy.io", "Bearer app-jwt-token", http.StatusOK},
		{"no special headers", "", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet,
				fmt.Sprintf("/agent/relay/%d/data", appPort), nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}

	// Non-relay routes must still be guarded.
	t.Run("non-relay route with Origin is still refused", func(t *testing.T) {
		r2 := chi.NewRouter()
		r2.Use(Authenticate(zap.NewNop(), agentToken))
		r2.Get("/agent/stop", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/agent/stop", nil)
		req.Header.Set("Origin", "https://evil.com")
		rec := httptest.NewRecorder()
		r2.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("non-relay with Origin: status = %d, want 403", rec.Code)
		}
	})
}

func TestExtractRelayForwardPath(t *testing.T) {
	tests := []struct {
		path string
		port string
		want string
	}{
		{"/agent/relay/8080/foo/bar", "8080", "/foo/bar"},
		{"/agent/relay/8080/", "8080", "/"},
		{"/agent/relay/8080", "8080", "/"},
	}
	for _, tt := range tests {
		got := extractRelayForwardPath(tt.path, tt.port)
		if got != tt.want {
			t.Errorf("extractRelayForwardPath(%q, %q) = %q, want %q", tt.path, tt.port, got, tt.want)
		}
	}
}

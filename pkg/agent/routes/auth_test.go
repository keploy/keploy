package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// guarded wraps a handler that records whether it was reached at all. Asserting
// on the status alone would not catch a middleware that rejects the request
// after the handler has already streamed TLS keys or stopped the session.
func guarded(t *testing.T, token string) (http.Handler, *bool) {
	t.Helper()
	reached := false
	h := Authenticate(zap.NewNop(), token)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	return h, &reached
}

func TestAuthenticate_RejectsRequestsWithoutTheRightToken(t *testing.T) {
	tests := []struct {
		name string
		auth string
	}{
		{"no Authorization header at all", ""},
		{"bearer scheme with no token", "Bearer "},
		{"wrong token", "Bearer " + strings.Repeat("f", len(testToken))},
		{"the wrong scheme", "Basic " + testToken},
		{"raw token with no scheme", testToken},
		// A prefix comparison would let a truncated token through, and a
		// HasPrefix check on the other side would accept a longer one.
		{"a prefix of the token", "Bearer " + testToken[:len(testToken)-1]},
		{"the token with something appended", "Bearer " + testToken + "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, reached := guarded(t, testToken)
			req := httptest.NewRequest(http.MethodPost, "/agent/stop", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			require.False(t, *reached, "the handler ran; /agent/stop takes no body, so reaching it is the whole side effect")
		})
	}
}

func TestAuthenticate_AcceptsTheSessionToken(t *testing.T) {
	// RFC 7235 makes the scheme name case-insensitive, and Go's own
	// http.Client is not the only thing that talks to this API.
	for _, scheme := range []string{"Bearer ", "bearer ", "BEARER "} {
		t.Run(strings.TrimSpace(scheme), func(t *testing.T) {
			h, reached := guarded(t, testToken)
			req := httptest.NewRequest(http.MethodGet, "/agent/pcap/keylog", nil)
			req.Header.Set("Authorization", scheme+testToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			require.True(t, *reached)
		})
	}
}

func TestAuthenticate_LeavesHealthReachableWithoutAToken(t *testing.T) {
	// Callers poll health to find out whether the agent is up at all, before
	// they are in a position to have agreed on anything. It takes no input and
	// returns nothing that was captured.
	h, reached := guarded(t, testToken)
	req := httptest.NewRequest(http.MethodGet, "/agent/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, *reached)
}

// TestAuthenticate_GuardsEveryRegisteredRoute walks the REAL route table
// rather than a hand-written list, so a route added to DefaultRoutes.New in
// future is covered the day it is added. A hardcoded list would have stayed
// green while a new /agent/<something> served unauthenticated.
func TestAuthenticate_GuardsEveryRegisteredRoute(t *testing.T) {
	router := chi.NewRouter()
	router.Use(Authenticate(zap.NewNop(), testToken))
	// A nil service is fine: registration only stores it, and no handler runs
	// here — every request under test is refused by the middleware first.
	DefaultRoutes{}.New(router, nil, zap.NewNop())

	var paths []string
	require.NoError(t, chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi renders a bare sub-route as a trailing slash; the request path
		// callers actually use has none.
		route = strings.TrimSuffix(route, "/")
		paths = append(paths, method+" "+route)
		return nil
	}))
	require.NotEmpty(t, paths, "walked no routes; the registration call changed shape")

	guarded := 0
	for _, mr := range paths {
		parts := strings.SplitN(mr, " ", 2)
		method, route := parts[0], parts[1]
		if route == "/agent/health" {
			continue
		}
		t.Run(mr, func(t *testing.T) {
			req := httptest.NewRequest(method, route, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusUnauthorized, rec.Code,
				"%s %s is reachable without the session token", method, route)
		})
		guarded++
	}
	require.Greater(t, guarded, 15, "far fewer routes than this agent registers; the walk is not seeing them")
}

// TestIsAuthExempt pins the allow-list itself. Adding to it is a decision to
// serve something without a credential, and it should not be possible to make
// that decision quietly.
func TestIsAuthExempt(t *testing.T) {
	require.True(t, isAuthExempt("/agent/health"))
	for _, path := range []string{
		"/agent/stop", "/agent/storemocks", "/agent/pcap/keylog",
		"/agent/pcap/traffic", "/agent/scope/begin", "/agent/mock/served",
	} {
		require.False(t, isAuthExempt(path), "%s must not be exempt", path)
	}
}

func TestAuthenticate_DoesNotExemptLookalikePaths(t *testing.T) {
	for _, path := range []string{
		"/agent/healthz",
		"/agent/health/../stop",
		"/agent/health/",
		"/AGENT/HEALTH",
	} {
		t.Run(path, func(t *testing.T) {
			h, reached := guarded(t, testToken)
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			require.False(t, *reached)
		})
	}
}

func TestAuthenticate_RefusesAnythingSentByABrowser(t *testing.T) {
	// A page cannot read a cross-origin response without CORS, but it can send
	// a simple POST — no preflight, no custom headers — and /agent/stop ignores
	// its request body entirely, so the side effect would land regardless. No
	// keploy client sends Origin, so refusing it costs nothing.
	for _, path := range []string{"/agent/stop", "/agent/health"} {
		t.Run(path, func(t *testing.T) {
			h, reached := guarded(t, testToken)
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("Origin", "https://attacker.example")
			// Even with the right token: a leaked token must not buy a browser
			// a way in, and health is exempt from the token, not from this.
			req.Header.Set("Authorization", "Bearer "+testToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusForbidden, rec.Code)
			require.False(t, *reached)
		})
	}
}

func TestAuthenticate_WithoutATokenServesAsItDidBefore(t *testing.T) {
	// An agent started by something that does not know about tokens yet — an
	// older enterprise launcher, a hand-run `keploy agent` — must keep working
	// rather than reject every request. It warns loudly at startup instead
	// (see SessionToken).
	h, reached := guarded(t, "")
	req := httptest.NewRequest(http.MethodPost, "/agent/stop", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, *reached)
}

func TestAuthenticate_WithoutATokenStillRefusesBrowsers(t *testing.T) {
	h, reached := guarded(t, "")
	req := httptest.NewRequest(http.MethodPost, "/agent/stop", nil)
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.False(t, *reached)
}

func TestTokenMatches(t *testing.T) {
	require.True(t, tokenMatches("Bearer "+testToken, testToken))
	// Authenticate short-circuits on an empty token before reaching here, so
	// this is defence in depth rather than the live guard for that case.
	require.False(t, tokenMatches("Bearer ", ""), "an empty configured token must never be matchable by a header")
	require.False(t, tokenMatches("", testToken))
	require.False(t, tokenMatches("Bearer"+testToken, testToken), "the space after the scheme is part of the grammar")
}

func TestNextStepFor_NamesTheVariableAScopeCallerIsMissing(t *testing.T) {
	// The documented pytest fixture swallows the error, so this log line is
	// the only thing that explains why per-test scoping quietly stopped.
	require.Contains(t, nextStepFor("/agent/scope/begin"), "KEPLOY_MOCK_AGENT_TOKEN")
	require.Contains(t, nextStepFor("/agent/scope/end"), "Authorization: Bearer")
	require.NotContains(t, nextStepFor("/agent/stop"), "KEPLOY_MOCK_AGENT_TOKEN",
		"a rejected /agent/stop is not a test-runner problem; pointing the operator at the scope variable would misdirect them")
}

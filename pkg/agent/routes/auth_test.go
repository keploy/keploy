package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/agent/stop", nil)
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

	paths := walkRoutes(t, router)
	// The run id a starter hands its agent (HandleClockRun) reaches it only
	// over this route: without it every agent answers 404, as one older
	// than the route, and no run's timeline is marked as its own.
	require.Contains(t, paths, "POST /agent/hooks/clock-run", "the clock-run route is not registered")

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
		"/agent/ca",
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

// TestGuard_TheOptOutIsSaidOnceAtWarn: an agent Guard serves without a token
// says so, once, at WARN, naming the variable.
func TestGuard_TheOptOutIsSaidOnceAtWarn(t *testing.T) {
	t.Setenv(token.Env, "")
	t.Setenv(token.InsecureNoAuthEnv, "true")
	core, logs := observer.New(zap.DebugLevel)
	guard, err := Guard(zap.New(core), "", true)
	require.NoError(t, err)
	require.NotNil(t, guard)
	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	require.Equal(t, zap.WarnLevel, entry.Level)
	require.Contains(t, entry.Message, token.InsecureNoAuthEnv+"=true")
	require.Contains(t, entry.Message, "to any caller")
}

// TestGuard_AnOptOutBesideATokenIsSaidToBeIgnored: an agent handed a token
// enforces it whatever the opt-out says, and says the opt-out is ignored, so
// an operator who set both does not think the agent is open.
func TestGuard_AnOptOutBesideATokenIsSaidToBeIgnored(t *testing.T) {
	for _, v := range []string{"true", "1"} {
		t.Setenv(token.Env, testToken)
		t.Setenv(token.InsecureNoAuthEnv, v)
		core, logs := observer.New(zap.DebugLevel)
		guard, err := Guard(zap.New(core), "", true)
		require.NoError(t, err)
		require.NotNil(t, guard)
		warns := logs.FilterLevelExact(zap.WarnLevel).All()
		require.Len(t, warns, 1, "%q", v)
		require.Contains(t, warns[0].Message, token.InsecureNoAuthEnv+"=\""+v+"\" is ignored")
	}
	t.Setenv(token.InsecureNoAuthEnv, "")
	core, logs := observer.New(zap.DebugLevel)
	_, err := Guard(zap.New(core), "", true)
	require.NoError(t, err)
	require.Zero(t, logs.FilterLevelExact(zap.WarnLevel).Len(), "an agent with a token and no opt-out has nothing to say about it")
}

// TestGuard_TheOptOutDoesNotCoverATokenFileItCannotRead: the opt-out is for an
// agent started with no token at all. One handed a token file it cannot read
// still refuses to start.
func TestGuard_TheOptOutDoesNotCoverATokenFileItCannotRead(t *testing.T) {
	t.Setenv(token.Env, "")
	t.Setenv(token.InsecureNoAuthEnv, "true")
	path := filepath.Join(t.TempDir(), "agent-token")
	require.NoError(t, os.WriteFile(path, []byte("garbage\n"), 0o600))
	guard, err := Guard(zap.NewNop(), path, false)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoToken)
	require.Contains(t, err.Error(), "will not start")
	require.Nil(t, guard)
}

// walkRoutes returns "METHOD /path" for every route DefaultRoutes.New
// registers on router, with chi's trailing slash for a bare sub-route removed.
func walkRoutes(t *testing.T, router chi.Router) []string {
	t.Helper()
	var paths []string
	require.NoError(t, chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		paths = append(paths, method+" "+strings.TrimSuffix(route, "/"))
		return nil
	}))
	require.NotEmpty(t, paths, "walked no routes; the registration call changed shape")
	return paths
}

// TestAuthenticate_WithoutATokenRefusesEveryRouteButHealth walks the REAL
// route table behind the guard of an agent that has no token: every route but
// /agent/health answers 401, with no header and with any bearer at all,
// empty included, and none is reached. An empty token matches nothing.
func TestAuthenticate_WithoutATokenRefusesEveryRouteButHealth(t *testing.T) {
	// Health is the one route it serves; every other path is refused before
	// reaching its handler.
	h, reached := guarded(t, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/agent/health", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, *reached)
	for _, path := range []string{"/agent/stop", "/agent/pcap/keylog", "/agent/healthz", "/agent/health/../stop"} {
		h, reached := guarded(t, "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil))
		require.Equal(t, http.StatusUnauthorized, rec.Code, path)
		require.False(t, *reached, "%s reached its handler on an agent with no token", path)
	}

	router := chi.NewRouter()
	router.Use(Authenticate(zap.NewNop(), ""))
	// A nil service is fine: no handler runs, every request is refused first.
	DefaultRoutes{}.New(router, nil, zap.NewNop())

	refused := 0
	for _, mr := range walkRoutes(t, router) {
		method, route, _ := strings.Cut(mr, " ")
		if route == "/agent/health" {
			continue
		}
		for _, auth := range []string{"", "Bearer ", "Bearer  ", "Bearer " + testToken, "bearer x"} {
			t.Run(mr+" auth="+auth, func(t *testing.T) {
				reqCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				req := httptest.NewRequestWithContext(reqCtx, method, route, nil)
				if auth != "" {
					req.Header.Set("Authorization", auth)
				}
				rec := httptest.NewRecorder()
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("%s %s reached its handler instead of returning 401 (panic: %v)", method, route, r)
						}
					}()
					router.ServeHTTP(rec, req)
				}()
				require.Equal(t, http.StatusUnauthorized, rec.Code,
					"%s %s is served by an agent with no token (Authorization %q)", method, route, auth)
			})
		}
		refused++
	}
	require.Greater(t, refused, 15, "far fewer routes than this agent registers; the walk is not seeing them")
}

// TestServeWithoutToken_TheOptOutServesWithoutATokenAndStillRefusesBrowsers:
// the guard of an agent started with the explicit opt-out serves a caller with
// no token, and still refuses what a browser sends.
func TestServeWithoutToken_TheOptOutServesWithoutATokenAndStillRefusesBrowsers(t *testing.T) {
	serve := func(origin string) (int, bool) {
		reached := false
		h := serveWithoutToken(zap.NewNop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/agent/stop", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, reached
	}
	code, reached := serve("")
	require.Equal(t, http.StatusOK, code)
	require.True(t, reached)
	code, reached = serve("https://page.example")
	require.Equal(t, http.StatusForbidden, code)
	require.False(t, reached)
}

// TestGuard_PicksTheGuardForTheTokenItHas: Guard is where an agent's guard is
// chosen. A token is enforced, opt-out or not; no token is an error unless the
// opt-out is exactly "true", and only then is the API served without one.
func TestGuard_PicksTheGuardForTheTokenItHas(t *testing.T) {
	probe := func(t *testing.T, guard func(http.Handler) http.Handler, auth string) (int, bool) {
		t.Helper()
		reached := false
		h := guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/agent/stop", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, reached
	}
	for _, tc := range []struct {
		name, env, optOut string
		wantErr           string // "" when the agent starts
		wantOpen          bool   // served with no token
	}{
		{"no token, no opt-out", "", "", "no control-plane token", false},
		{"no token, opt-out true", "", "true", "", true},
		{"no token, opt-out 1", "", "1", "is set to \"1\"", false},
		{"no token, opt-out TRUE", "", "TRUE", "is set to \"TRUE\"", false},
		{"no token, opt-out false", "", "false", "is set to \"false\"", false},
		{"token, no opt-out", testToken, "", "", false},
		{"token, opt-out true", testToken, "true", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(token.Env, tc.env)
			t.Setenv(token.InsecureNoAuthEnv, tc.optOut)
			t.Setenv("KUBERNETES_SERVICE_HOST", "")
			guard, err := Guard(zap.NewNop(), "", false)
			if tc.wantErr != "" {
				require.ErrorIs(t, err, ErrNoToken, "the refusal keeps its remedy")
				require.Contains(t, err.Error(), tc.wantErr)
				require.Nil(t, guard)
				return
			}
			require.NoError(t, err)
			code, reached := probe(t, guard, "")
			if tc.wantOpen {
				require.Equal(t, http.StatusOK, code)
				require.True(t, reached)
				return
			}
			require.Equal(t, http.StatusUnauthorized, code)
			require.False(t, reached)
			code, reached = probe(t, guard, "Bearer "+testToken)
			require.Equal(t, http.StatusOK, code)
			require.True(t, reached)
		})
	}
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
	// Defence in depth for an agent with no token: nothing matches an empty
	// one (the length check and the compare already refuse it).
	require.False(t, tokenMatches("Bearer ", ""), "an empty configured token must never be matchable by a header")
	require.False(t, tokenMatches("Bearer  ", ""), "an empty configured token must never be matchable by a header")
	require.False(t, tokenMatches("Bearer x", ""), "an empty configured token must never be matchable by a header")
	require.False(t, tokenMatches("", testToken))
	require.False(t, tokenMatches("Bearer"+testToken, testToken), "the space after the scheme is part of the grammar")
}

func TestNextStepFor_NamesTheVariableAScopeCallerIsMissing(t *testing.T) {
	// The documented pytest fixture swallows the error, so this log line is
	// the only thing that explains why per-test scoping quietly stopped.
	require.Contains(t, nextStepFor("/agent/scope/begin"), "KEPLOY_MOCK_AGENT_TOKEN")
	require.Contains(t, nextStepFor("/agent/scope/end"), "Authorization: Bearer")
	require.Contains(t, nextStepFor("/agent/app/start"), "KEPLOY_MOCK_AGENT_TOKEN")
	require.NotContains(t, nextStepFor("/agent/stop"), "KEPLOY_MOCK_AGENT_TOKEN",
		"a rejected /agent/stop is not a test-runner problem; pointing the operator at the scope variable would misdirect them")
}

// TestAuthProbe_AnswersDifferentlyDependingOnWhetherTheGuardIsLive is the
// assumption the CLI's self-check rests on, asserted against the REAL router
// rather than a stub: a guarded agent refuses the probe with 401, and an
// unguarded one falls through to chi's 404.
//
// If those two ever coincide the check silently stops working — or worse,
// reports every healthy run as unauthenticated and fails CI everywhere. Note
// chi's Mux.ServeHTTP has a fast path that skips middleware entirely when no
// route is registered, which is exactly the shape that would break it, so this
// builds the router the way production does.
//
// The only unguarded agent is one started with the explicit opt-out
// (serveWithoutToken); an agent with no token and no opt-out has a guard that
// refuses everything but health, the probe included.
func TestAuthProbe_AnswersDifferentlyDependingOnWhetherTheGuardIsLive(t *testing.T) {
	probe := func(guard func(http.Handler) http.Handler) int {
		router := chi.NewRouter()
		router.Use(guard)
		DefaultRoutes{}.New(router, nil, zap.NewNop())

		req := httptest.NewRequest(http.MethodGet, agentRoutePrefix+token.ProbePath, nil)
		req.Header.Set("Authorization", "Bearer not-the-session-token")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	require.Equal(t, http.StatusUnauthorized, probe(Authenticate(zap.NewNop(), testToken)),
		"a guarded agent must refuse the probe; the CLI reads anything else as an unauthenticated control plane")
	require.Equal(t, http.StatusUnauthorized, probe(Authenticate(zap.NewNop(), "")),
		"an agent with no token must refuse the probe too: its guard serves health and nothing else")
	require.Equal(t, http.StatusNotFound, probe(serveWithoutToken(zap.NewNop())),
		"an unguarded agent must fall through to chi's 404; if it answered 401 the check could never detect one")
}

// TestAuthenticate_DoesNotReportTheCLIsOwnProbeAsAnIntruder pins the other half.
// The CLI sends a deliberately wrong token once per agent, so refusing it is
// expected — reported as an intruder it would print a security warning on the
// user's terminal on every healthy run.
func TestAuthenticate_DoesNotReportTheCLIsOwnProbeAsAnIntruder(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	h := Authenticate(zap.New(core), testToken)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, agentRoutePrefix+token.ProbePath, nil)
	req.Header.Set("Authorization", "Bearer not-the-session-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code, "the probe must still be refused")
	require.Zero(t, logs.Len(), "the CLI's own probe was reported as a rejected intruder")

	// A genuinely unauthenticated caller on any other route still is.
	core2, logs2 := observer.New(zap.WarnLevel)
	h2 := Authenticate(zap.New(core2), testToken)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	h2.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/agent/stop", nil))
	require.Equal(t, 1, logs2.Len(), "a real unauthenticated request must still be reported")
}

package pkg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// resetProbeOnce clears the record of which agents have already answered, so
// each case starts from a clean slate.
func resetProbeOnce(t *testing.T) {
	t.Helper()
	clear := func() {
		controlPlaneProbed.Range(func(k, _ any) bool {
			controlPlaneProbed.Delete(k)
			return true
		})
	}
	clear()
	t.Cleanup(clear)
}

// TestVerifyControlPlaneGuarded is what makes a broken token handoff loud.
// Every step between minting the token and the agent enforcing it fails OPEN —
// the agent serves everything and this process's requests all succeed — so
// without this check the whole feature can regress to a no-op with every test
// and every CI lane still green.
func TestVerifyControlPlaneGuarded(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantErrLog bool
	}{
		// The agent refused a request carrying the wrong token: it is enforcing.
		{"guarded agent answers 401", http.StatusUnauthorized, false},
		// The middleware let it through to chi, which has no such route.
		{"unguarded agent falls through to 404", http.StatusNotFound, true},
		// Any other success is equally proof that nothing is being enforced.
		{"unguarded agent that serves something", http.StatusOK, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetProbeOnce(t)

			var mu sync.Mutex
			var gotPath, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
				mu.Unlock()
				w.WriteHeader(tt.status)
			}))
			t.Cleanup(srv.Close)

			core, logs := observer.New(zap.ErrorLevel)
			// The real AgentURI carries the /agent prefix the routes are
			// mounted under; probing without it would exercise a URL that
			// never occurs.
			token.RecordLaunch(srv.URL + "/agent")
			verifyControlPlaneGuarded(context.Background(), zap.New(core), srv.URL+"/agent")

			mu.Lock()
			path, auth := gotPath, gotAuth
			mu.Unlock()

			require.Equal(t, "/agent"+token.ProbePath, path)
			// A probe presenting the real token would be accepted by a guarded
			// agent and prove nothing at all.
			require.Equal(t, "Bearer not-the-session-token", auth)

			if tt.wantErrLog {
				require.Equal(t, 1, logs.Len(), "a control plane serving without authentication was not reported")
				require.Contains(t, logs.All()[0].Message, "NOT enforcing control-plane authentication")
			} else {
				require.Zero(t, logs.Len(), "reported an unguarded control plane for an agent that refused the probe")
			}
		})
	}
}

func TestVerifyControlPlaneGuarded_DoesNotReportAProbeThatCouldNotComplete(t *testing.T) {
	// The agent answered its health check a moment before this runs, so a
	// transport failure is not evidence that it is serving unguarded.
	// Reporting one would cry wolf with an error that fails keploy's own CI.
	resetProbeOnce(t)

	// A server that has been closed: its address is real but nothing is
	// listening, which is deterministic in a way "port 1" is not.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	core, logs := observer.New(zap.ErrorLevel)
	token.RecordLaunch(deadURL + "/agent")
	verifyControlPlaneGuarded(context.Background(), zap.New(core), deadURL+"/agent")

	require.Zero(t, logs.Len(), "a probe that never reached the agent was reported as an unauthenticated agent")
}

// TestVerifyControlPlaneGuarded_LeavesAgentsItNeverStartedAlone is the
// regression test for a false alarm. A process that drives an agent something
// else started — a sidecar k8s-proxy injected, a pinned agent image that
// predates the token — made no token handoff of its own, and the check
// reported a broken handoff at ERROR, which keploy's CI lanes treat as fatal.
// A sidecar with no token says for itself that it runs unauthenticated.
func TestVerifyControlPlaneGuarded_LeavesAgentsItNeverStartedAlone(t *testing.T) {
	resetProbeOnce(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound) // a token-less agent: the probe falls through to chi
	}))
	t.Cleanup(srv.Close)

	core, logs := observer.New(zap.ErrorLevel)
	verifyControlPlaneGuarded(context.Background(), zap.New(core), srv.URL+"/agent")

	require.Zero(t, logs.Len(), "blamed a token handoff this process never made")
	require.Zero(t, hits.Load(), "probed an agent this process never launched")
}

func TestVerifyControlPlaneGuarded_RunsOncePerAgent(t *testing.T) {
	// Six readiness gates call AgentHealthTicker across record, replay, mock
	// and the runner; one agent should not be probed once per gate. But every
	// agent must be: a compose `keploy test` starts a new agent for each
	// test-set, at the same address, and a handoff that breaks on a later
	// agent would otherwise go unseen.
	resetProbeOnce(t)

	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	first, second := srv.URL+"/first/agent", srv.URL+"/second/agent"
	probes := func(uri string) int {
		mu.Lock()
		defer mu.Unlock()
		return hits[strings.TrimPrefix(uri, srv.URL)+token.ProbePath]
	}

	token.RecordLaunch(first)
	token.RecordLaunch(second)
	for i := 0; i < 3; i++ {
		verifyControlPlaneGuarded(context.Background(), zap.NewNop(), first)
		verifyControlPlaneGuarded(context.Background(), zap.NewNop(), second)
	}
	require.Equal(t, 1, probes(first), "one agent was probed more than once, or not at all")
	require.Equal(t, 1, probes(second), "a second agent was never checked")

	// The next test-set's agent, at the first one's address.
	token.RecordLaunch(first)
	verifyControlPlaneGuarded(context.Background(), zap.NewNop(), first)
	require.Equal(t, 2, probes(first), "a new agent on a reused port inherited its predecessor's answer and was never checked")
}

// TestAgentHealthTicker_ChecksTheGuardWhenTheAgentBecomesReady pins the call
// site. Deleting the one line in AgentHealthTicker that starts the check — or
// moving it back inside a mode-specific branch, which is how it previously came
// to skip docker compose entirely — leaves every other test in this change
// green, so nothing else would notice.
func TestAgentHealthTicker_ChecksTheGuardWhenTheAgentBecomesReady(t *testing.T) {
	resetProbeOnce(t)

	probed := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`"OK"`))
		case "/agent" + token.ProbePath:
			probed <- r.Header.Get("Authorization")
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token.RecordLaunch(srv.URL + "/agent")
	readyCh := make(chan bool, 1)
	go AgentHealthTicker(ctx, zap.NewNop(), srv.URL+"/agent", readyCh, 10*time.Millisecond)

	select {
	case ready := <-readyCh:
		require.True(t, ready)
	case <-ctx.Done():
		t.Fatal("the agent never became ready")
	}

	// The readiness signal must not wait on the check, so the check is allowed
	// to land after it — but it must land.
	select {
	case auth := <-probed:
		require.Equal(t, "Bearer not-the-session-token", auth,
			"the check presented a credential that a guarded agent would accept, which would prove nothing")
	case <-time.After(5 * time.Second):
		t.Fatal("the agent became ready but its control plane was never checked")
	}
}

// TestAgentHealthTicker_ReadinessDoesNotWaitOnTheCheck is the other half of the
// same line: a check that ran before the signal would spend the caller's
// readiness budget, and every caller races that signal against a deadline whose
// expiry fails the user's whole run.
func TestAgentHealthTicker_ReadinessDoesNotWaitOnTheCheck(t *testing.T) {
	resetProbeOnce(t)

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent"+token.ProbePath {
			<-release // hold the check open
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`"OK"`))
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token.RecordLaunch(srv.URL + "/agent")
	readyCh := make(chan bool, 1)
	go AgentHealthTicker(ctx, zap.NewNop(), srv.URL+"/agent", readyCh, 10*time.Millisecond)

	select {
	case <-readyCh:
	case <-time.After(3 * time.Second):
		t.Fatal("readiness was blocked behind the control-plane check")
	}
}

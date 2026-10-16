package pkg

import (
	"context"
	"net"
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

// recordLaunch records that this process launched the agent at agentURI, a
// test server's, and forgets it when the test ends: the server's port is the
// one the system gave it, and a later test's server may get the same port.
// One that was not forgotten made a later test's agent, which this process
// never launched, look launched by it: the check then probed it and reported
// a broken token handoff (TestVerifyControlPlaneGuarded_LeavesAgentsItNeverStartedAlone,
// about once in 20 runs of the package on a loaded machine).
func recordLaunch(t *testing.T, agentURI string) {
	t.Helper()
	launch := token.RecordLaunch(agentURI)
	t.Cleanup(func() { token.ForgetLaunch(agentURI, launch) })
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
			recordLaunch(t, srv.URL+"/agent")
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
				// It is the only word a doas in front of docker compose gets
				// when it resets the environment the token is in: keploy
				// cannot tell beforehand whether doas.conf keeps it.
				nextStep, _ := logs.All()[0].ContextMap()["next_step"].(string)
				require.Contains(t, nextStep, "keepenv", "the ERROR does not say how a doas can keep the token")
				require.Contains(t, nextStep, "setenv { "+token.Env+" }", "the ERROR does not say how a doas can keep the token")
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

	// An agent whose connection drops before it answers: a listener this
	// test holds, so nothing else can answer at its address, closing every
	// connection it accepts. A closed server's port was used for this, and
	// another server (a later test's, another process's) could take the
	// port and answer the probe: the test then failed a correct run, twice
	// in 2000 on a loaded machine.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	deadURL := "http://" + ln.Addr().String()

	core, logs := observer.New(zap.ErrorLevel)
	recordLaunch(t, deadURL+"/agent")
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
	// Five readiness gates call AgentHealthTicker or AgentHealthTickerWithAnswer
	// across record, replay, mock, the runner and the agent client's own
	// bring-up; one agent should not be probed once per gate. But every
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

	recordLaunch(t, first)
	recordLaunch(t, second)
	for i := 0; i < 3; i++ {
		verifyControlPlaneGuarded(context.Background(), zap.NewNop(), first)
		verifyControlPlaneGuarded(context.Background(), zap.NewNop(), second)
	}
	require.Equal(t, 1, probes(first), "one agent was probed more than once, or not at all")
	require.Equal(t, 1, probes(second), "a second agent was never checked")

	// The next test-set's agent, at the first one's address.
	recordLaunch(t, first)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	recordLaunch(t, srv.URL+"/agent")
	readyCh := make(chan bool, 1)
	go AgentHealthTicker(ctx, zap.NewNop(), srv.URL+"/agent", readyCh, 10*time.Millisecond)

	// No clock bounds either wait: an agent that never becomes ready, or a
	// check that never lands, fails the run at go test's own -timeout,
	// where a budget here would fail a correct run on a starved machine.
	require.True(t, <-readyCh, "the agent never became ready")

	// The readiness signal must not wait on the check, so the check is allowed
	// to land after it — but it must land.
	auth := <-probed
	require.Equal(t, "Bearer not-the-session-token", auth,
		"the check presented a credential that a guarded agent would accept, which would prove nothing")
}

// TestAgentHealthTickerWithAnswer_SendsTheReadyAnswerAndChecksTheGuard holds
// the variant the native and docker-run bring-up waits on to what it promises:
// it sends the headers of the answer that found the agent ready, once, then
// closes the channel; it starts the control-plane check as AgentHealthTicker
// does; and when ctx ends before any healthy answer it closes the channel with
// nothing sent. Every wait here has a definite end, so none has a deadline.
func TestAgentHealthTickerWithAnswer_SendsTheReadyAnswerAndChecksTheGuard(t *testing.T) {
	resetProbeOnce(t)

	probed := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/health":
			w.Header().Set("X-Test-Answer", "the ready one")
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recordLaunch(t, srv.URL+"/agent")
	answers := make(chan http.Header, 1)
	go AgentHealthTickerWithAnswer(ctx, zap.NewNop(), srv.URL+"/agent", answers, 10*time.Millisecond)

	header := <-answers
	require.NotNil(t, header, "the agent answered 200, so the ticker must send its headers")
	require.Equal(t, "the ready one", header.Get("X-Test-Answer"), "the headers sent are not the ready answer's")
	_, open := <-answers
	require.False(t, open, "the ticker must send one answer and close the channel")
	require.Equal(t, "Bearer not-the-session-token", <-probed,
		"the check presented a credential that a guarded agent would accept, which would prove nothing")

	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()
	ended, end := context.WithCancel(context.Background())
	nothing := make(chan http.Header, 1)
	go AgentHealthTickerWithAnswer(ended, zap.NewNop(), goneURL+"/agent", nothing, 10*time.Millisecond)
	end()
	header, open = <-nothing
	require.False(t, open, "a ticker whose ctx ended must close the channel")
	require.Nil(t, header, "a ticker whose ctx ended must send nothing")
}

// TestAgentHealthTicker_ReadinessDoesNotWaitOnTheCheck is the other half of the
// same line: a check that ran before the signal would spend the caller's
// readiness budget, and every caller races that signal against a deadline whose
// expiry fails the user's whole run.
func TestAgentHealthTicker_ReadinessDoesNotWaitOnTheCheck(t *testing.T) {
	resetProbeOnce(t)

	release := make(chan struct{})
	probed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent"+token.ProbePath {
			probed <- struct{}{}
			<-release // hold the check open
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`"OK"`))
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	recordLaunch(t, srv.URL+"/agent")
	readyCh := make(chan bool, 1)
	go AgentHealthTicker(ctx, zap.NewNop(), srv.URL+"/agent", readyCh, 10*time.Millisecond)

	// Readiness comes while the check is held open. No clock bounds the
	// wait: a readiness blocked behind the check never comes, and go
	// test's own -timeout fails that run, where a budget here would fail
	// a correct run on a starved machine.
	<-readyCh
	// The check runs on after readiness. It is waited for here, held open,
	// so that it cannot dial after this test, when a later test's server
	// may have this one's port.
	<-probed
}

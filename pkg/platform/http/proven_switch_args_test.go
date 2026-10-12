package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The native agent is started through sudo, which drops KEPLOY_PROVEN, so the
// switch must reach it as an argv flag; with the switch off the argv must not
// change at all.
func TestNativeAgentArgs_CarryTheProvenSwitch(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	flag := "--" + provenswitch.AgentFlag
	a := New(zap.NewNop(), nil, &config.Config{})
	opts := models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797, AgentURI: "http://localhost:26795/agent", Mode: models.MODE_TEST}

	t.Setenv(provenswitch.Env, "")
	if args := a.nativeAgentArgs(opts); slices.Contains(args, flag) {
		t.Fatalf("switch off, yet the native agent argv carries %s: %v", flag, args)
	}

	t.Setenv(provenswitch.Env, "1")
	if args := a.nativeAgentArgs(opts); !slices.Contains(args, flag) {
		t.Fatalf("KEPLOY_PROVEN=1, yet the native agent argv lacks %s, so the agent's parsers run the old paths: %v", flag, args)
	}
}

// standInAgent is an agent whose /health reports the given proven switch (""
// for an agent that predates the switch and sends no header) with the given
// status, and which counts the POSTs to /agent/ready that release the app.
type standInAgent struct {
	url     string
	readies atomic.Int32
}

func newStandInAgent(t *testing.T, state string, status int) *standInAgent {
	t.Helper()
	sa := &standInAgent{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent/ready") {
			sa.readies.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		if state != "" {
			w.Header().Set(provenswitch.HealthHeader, state)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`"OK"`))
	}))
	t.Cleanup(srv.Close)
	sa.url = srv.URL
	return sa
}

// closedAgentURL is an address nothing answers on.
func closedAgentURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func clientFor(uri string) *AgentClient {
	return New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
}

// The CLI refuses an agent whose switch is not its own, in both directions,
// and, with the switch on, an agent that cannot report one or whose report
// cannot be read; with the switch off it runs as before against today's
// agents, and a failed read changes nothing.
func TestCheckAgentSwitch(t *testing.T) {
	for _, tc := range []struct {
		name, local, agent string
		status             int
		closed             bool
		ok                 bool
	}{
		{"both off", "", "off", http.StatusOK, false, true},
		{"both on", "1", "on", http.StatusOK, false, true},
		{"off, agent predates the switch", "", "", http.StatusOK, false, true},
		{"off, health unreadable", "", "", 0, true, true},
		{"off, health not 200", "", "on", http.StatusServiceUnavailable, false, true},
		{"on, agent off", "1", "off", http.StatusOK, false, false},
		{"off, agent on", "", "on", http.StatusOK, false, false},
		{"on, agent predates the switch", "1", "", http.StatusOK, false, false},
		{"on, health unreadable", "1", "", 0, true, false},
		{"on, health not 200", "1", "on", http.StatusServiceUnavailable, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.local)
			uri := closedAgentURL(t)
			if !tc.closed {
				uri = newStandInAgent(t, tc.agent, tc.status).url
			}
			err := clientFor(uri).checkAgentSwitch(context.Background())
			if (err == nil) != tc.ok {
				t.Fatalf("checkAgentSwitch = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !errors.Is(err, provenswitch.ErrAgentSwitch) {
				t.Fatalf("a refusal must wrap provenswitch.ErrAgentSwitch: %v", err)
			}
		})
	}
}

// The check is wired into the wait for a freshly started agent: an agent that
// answers its health check with a switch that is not the CLI's fails the
// bring-up, naming both sides, and is stopped rather than left running.
func TestWaitForAgentRefusesADisagreeingSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	a := clientFor(newStandInAgent(t, "off", http.StatusOK).url)
	stopped := false
	a.agentCancel = func() { stopped = true }
	err := a.waitForAgent(context.Background(), nil, false, models.SetupOptions{})
	if !errors.Is(err, provenswitch.ErrAgentSwitch) || !strings.Contains(err.Error(), "on in this keploy") || !strings.Contains(err.Error(), "off in its agent") {
		t.Fatalf("waitForAgent = %v; want a refusal naming both sides", err)
	}
	if !stopped {
		t.Fatal("the disagreeing agent was left running")
	}

	t.Setenv(provenswitch.Env, "")
	b := clientFor(newStandInAgent(t, "off", http.StatusOK).url)
	if err := b.waitForAgent(context.Background(), nil, false, models.SetupOptions{}); err != nil {
		t.Fatalf("an agreeing agent was refused: %v", err)
	}
}

// Under docker compose the check comes before the agent is told to release the
// app: a disagreeing agent is refused and never released; an agreeing one is
// released as before.
func TestMakeAgentReadyForDockerComposeChecksTheSwitchFirst(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	bad := newStandInAgent(t, "off", http.StatusOK)
	err := clientFor(bad.url).MakeAgentReadyForDockerCompose(context.Background())
	if !errors.Is(err, provenswitch.ErrAgentSwitch) {
		t.Fatalf("MakeAgentReadyForDockerCompose = %v; want a refusal wrapping provenswitch.ErrAgentSwitch", err)
	}
	if n := bad.readies.Load(); n != 0 {
		t.Fatalf("the disagreeing agent was told to release the app %d time(s)", n)
	}

	good := newStandInAgent(t, "on", http.StatusOK)
	if err := clientFor(good.url).MakeAgentReadyForDockerCompose(context.Background()); err != nil {
		t.Fatalf("an agreeing agent was refused: %v", err)
	}
	if n := good.readies.Load(); n != 1 {
		t.Fatalf("the agreeing agent was released %d times, want 1", n)
	}
}

// wedgingAgent answers its first health check (with the switch on) and then
// holds every later request until the request ends; it tells the test when the
// second request has arrived.
func wedgingAgent(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	var n atomic.Int32
	second := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.Header().Set(provenswitch.HealthHeader, "on")
			w.WriteHeader(http.StatusOK)
			return
		case 2:
			close(second)
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv.URL, second
}

// A Ctrl+C while the switch is being read is reported as the cancellation, not
// as a switch that was never read.
func TestWaitForAgentReportsACancelDuringTheSwitchRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	uri, second := wedgingAgent(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("the user pressed Ctrl+C")
	go func() {
		<-second
		cancel(cause)
	}()
	err := clientFor(uri).waitForAgent(ctx, nil, false, models.SetupOptions{})
	if !errors.Is(err, cause) || errors.Is(err, provenswitch.ErrAgentSwitch) {
		t.Fatalf("waitForAgent = %v; want the cancellation's cause, not a switch refusal", err)
	}
}

// An agent that answers its health check once and then stops answering is not
// ready: the bring-up fails as not ready (which the caller retries with a fresh
// agent), not with a switch refusal, and the agent is stopped. The readiness
// budget is what this tests, so it is short.
func TestWaitForAgentTreatsAWedgedSwitchReadAsNotReady(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	uri, _ := wedgingAgent(t)
	a := clientFor(uri)
	stopped := false
	a.agentCancel = func() { stopped = true }
	err := a.waitForAgent(context.Background(), nil, false, models.SetupOptions{AgentReadyTimeout: 3 * time.Second})
	if !errors.Is(err, pkg.ErrAgentNotReady) || errors.Is(err, provenswitch.ErrAgentSwitch) {
		t.Fatalf("waitForAgent = %v; want pkg.ErrAgentNotReady, not a switch refusal", err)
	}
	if !stopped {
		t.Fatal("the wedged agent was left running")
	}
}

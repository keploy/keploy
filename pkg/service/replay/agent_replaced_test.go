package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.uber.org/zap"
)

// P056: what a test set is judged on (the calls no mock answered, what its
// tests consumed, what keploy could not capture) is held in the agent
// process's memory. An agent started again in its place mid-set (kubelet
// restarting a crashed sidecar, docker restarting a container) answers at
// the same address holding none of it, and its reads say nothing was missed.
// Under the proven switch such a set has no verdict.

// restartingAgent is a keploy agent over HTTP as far as a test's misses go:
// each process holds the calls no mock answered in its memory, and names
// itself in every answer, as an agent with the proven switch on does
// (models.AgentInstanceHeader). restart starts another process in its place,
// at the same address, holding none of them.
type restartingAgent struct {
	mu      sync.Mutex
	process int
	misses  []models.UnmatchedCall
}

func (s *restartingAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set(models.AgentInstanceHeader, fmt.Sprintf("process-%d", s.process))
	switch r.URL.Path {
	case "/agent/test-capture/begin":
		if r.URL.Query().Get("carry") != "1" {
			s.misses = nil
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	case "/agent/mockerrors":
		calls := s.misses
		if calls == nil {
			calls = []models.UnmatchedCall{}
		}
		s.misses = nil
		_ = json.NewEncoder(w).Encode(calls)
	default:
		http.NotFound(w, r)
	}
}

// missed is a call of the app's that no mock answered, held by the running
// process.
func (s *restartingAgent) missed(c models.UnmatchedCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.misses = append(s.misses, c)
}

func (s *restartingAgent) restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.process++
	s.misses = nil
}

// overTheWire is the partial-run harness's instrumentation with the misses
// read from an agent over HTTP through keploy's agent client: every test's
// window opens and its misses are read there. The app makes, during
// missedIn, a call no mock answers; the agent restarts before the read
// numbered restartAt (from 1; 0: never).
type overTheWire struct {
	*prInstr
	client    *httpclient.AgentClient
	agent     *restartingAgent
	mu        sync.Mutex
	opened    int
	reads     int
	missedIn  int
	restartAt int
}

func (o *overTheWire) BeginTestErrorCapture(ctx context.Context) error {
	return o.open(ctx, false)
}

func (o *overTheWire) ContinueTestErrorCapture(ctx context.Context) error {
	return o.open(ctx, true)
}

func (o *overTheWire) open(ctx context.Context, carry bool) error {
	var err error
	if carry {
		err = o.client.ContinueTestErrorCapture(ctx)
	} else {
		err = o.client.BeginTestErrorCapture(ctx)
	}
	o.mu.Lock()
	o.opened++
	test := o.opened
	o.mu.Unlock()
	if test == o.missedIn {
		o.agent.missed(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "audit:8080"})
	}
	return err
}

func (o *overTheWire) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	o.mu.Lock()
	o.reads++
	read := o.reads
	o.mu.Unlock()
	if read == o.restartAt {
		o.agent.restart()
	}
	return o.client.GetMockErrors(ctx)
}

func (o *overTheWire) WatchAgentInstance() models.AgentInstanceWatch {
	return o.client.WatchAgentInstance()
}

// newOverTheWire puts the harness's misses behind an agent over HTTP.
func newOverTheWire(t *testing.T, h *prRun, missedIn, restartAt int) *overTheWire {
	t.Helper()
	agent := &restartingAgent{process: 1}
	srv := httptest.NewServer(agent)
	t.Cleanup(srv.Close)
	h.replayer.config.Agent.AgentURI = srv.URL + "/agent"
	o := &overTheWire{prInstr: h.instr, client: httpclient.New(zap.NewNop(), nil, h.replayer.config), agent: agent, missedIn: missedIn, restartAt: restartAt}
	h.replayer.instrumentation = o
	return o
}

// The control: the agent that saw the app's call no mock answered is the one
// keploy reads it from, and the call fails its test.
func TestRunTestSet_AMissReadFromTheAgentThatSawItFailsItsTest(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	newOverTheWire(t, h, 1, 0)
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s; the call no mock answered during test-1 must fail the set", status)
	}
	if h.replayer.notJudged("test-set-0") {
		t.Fatal("the set is marked not judged; one agent process answered for all of it")
	}
}

// The agent is started again while the set runs: the agent that answers
// keploy's reads after holds none of what the one before saw (test-1's call
// no mock answered, when the restart comes before its read). The set has no
// verdict, and says why; it is neither PASSED nor FAILED. So for a run with
// no command, whose application and agent keploy does not start (a
// Kubernetes sidecar's): its set's evidence is held in the agent all the
// same.
func TestRunTestSet_AnAgentStartedAgainMidSetLeavesTheSetUnjudged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		missedIn  int
		restartAt int
	}{
		{"before the read of the test that missed", 1, 1},
		{"before the read of a later test", 0, 2},
		{"after the last test, before the set's last read", 0, 3},
		// test-1 failed on the miss the agent before held, and was read;
		// what test-2 did is lost. FAILED says more than keploy knows.
		{"after a test failed on a miss", 1, 2},
	} {
		for _, noCommand := range []bool{false, true} {
			name := tc.name
			if noCommand {
				// An application keploy does not start (no -c), up beside
				// an agent it did not start either: a Kubernetes sidecar's.
				name += ", no command"
			}
			t.Run(name, func(t *testing.T) {
				provenOn(t, true)
				h := newPartialRunHarness(t, 2, 0)
				h.replayer.instrument = !noCommand
				o := newOverTheWire(t, h, tc.missedIn, tc.restartAt)
				status := h.run(t)
				if o.reads < tc.restartAt {
					t.Fatalf("fixture: %d reads of the agent's misses, the restart was set before read %d", o.reads, tc.restartAt)
				}
				if status != models.TestSetStatusInternalErr {
					t.Fatalf("status %s; a set whose agent was started again while it ran has no verdict", status)
				}
				if !h.replayer.notJudged("test-set-0") {
					t.Fatal("the set is not recorded as not judged")
				}
				reason := h.report.report.FailureReason
				for _, want := range []string{"agent was started again while the test-set ran", "process-1", "process-2"} {
					if !strings.Contains(reason, want) {
						t.Fatalf("failure reason %q does not say %q", reason, want)
					}
				}
			})
		}
	}
}

// An agent started again before the set began holds all of it: the set is
// judged on it.
func TestRunTestSet_AnAgentStartedAgainBeforeTheSetJudgesIt(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	o := newOverTheWire(t, h, 0, 0)
	// The process before answered this client, before the set.
	if _, err := o.client.GetMockErrors(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.agent.restart()
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED: the agent was started again before the set, and answered for all of it", status)
	}
}

// With the switch off the set is what it was: no watch, no verdict changed.
func TestRunTestSet_AnAgentStartedAgainWithTheSwitchOffIsAsBefore(t *testing.T) {
	provenOn(t, false)
	h := newPartialRunHarness(t, 2, 0)
	newOverTheWire(t, h, 1, 1)
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED as before: with the switch off a miss does not fail a test", status)
	}
	if h.replayer.notJudged("test-set-0") {
		t.Fatal("switch off: the set is marked not judged")
	}
}

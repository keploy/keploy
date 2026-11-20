package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// composeReads is a docker compose stack's stand-in agent, read as keploy's
// agent client reads it (readsAsAnAgentDoes), whose app's container keploy
// stops itself (StopComposeApp), the stack, keploy's agent among it, running
// on. The app makes a lookup no mock answers only as it stops, which the agent
// holds for the set's end read. Its stop says whether the set's post-script
// had run.
type composeReads struct {
	*readsAsAnAgentDoes
	post    string
	stopErr error

	mu      sync.Mutex
	stops   int
	starts  int
	postRan bool
	// stopped: the app's container has stopped, its stop's lookup made.
	stopped bool
}

func (c *composeReads) StopComposeApp(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stops++
	_, err := os.Stat(c.post)
	c.postRan = err == nil
	if c.stopErr == nil {
		c.stopped = true
	}
	return "", c.stopErr
}

func (c *composeReads) StartComposeApp(context.Context) (models.ComposeAppStart, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	c.stopped = false
	return models.ComposeAppStartedAnew, nil
}

// counts is the stops and starts of the app's container, and whether the
// set's post-script had run at its last stop.
func (c *composeReads) counts() (stops, starts int, postRan bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stops, c.starts, c.postRan
}

// composeReadHarness is the harness for a set of a docker compose stack whose
// agent is c (composeReads) and whose post-script makes c's file.
func composeReadHarness(t *testing.T, c *composeReads) *prRun {
	t.Helper()
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.CommandType = string(utils.DockerCompose)
	// A set that brings its stack up afresh waits for the agent in it to
	// answer /health first.
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(agent.Close)
	h.replayer.config.Agent.AgentURI = agent.URL
	withSetDir(t, h)
	c.readsAsAnAgentDoes = &readsAsAnAgentDoes{prInstr: h.instr}
	c.post = filepath.Join(t.TempDir(), "post-ran")
	c.atEnd = func() ([]models.UnmatchedCall, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.stopped {
			// Still running: its stop's lookup is made only as it stops.
			return nil, nil
		}
		return []models.UnmatchedCall{stopLookup}, nil
	}
	h.replayer.instrumentation = c
	h.replayer.testSetConf = postScriptTouches{path: c.post}
	return h
}

// With the switch off keploy stops a docker compose app's container itself,
// its stack running on, after the set's post-script, which runs with the app
// up, and before the set's report: the read for the report takes the lookup
// the app made only as it stopped, which the report names as made after the
// set's last test, by its shape, and the misses file names whole. The set
// passes (the lookup fails nothing). A set the stack brings up for itself is
// stopped so; of a stack kept across sets only the run's last set that runs
// (the last by place, or one after which every set is wholly ignored or has
// no test case), on an attempt no other follows, is, and an earlier one is
// read with the app running, before its post-script, as --keep-app-alive's
// app is.
func TestAComposeAppIsStoppedBeforeTheReportWithTheSwitchOff(t *testing.T) {
	provenOn(t, false)
	const stopNamed = "DNS A {}.{}.{}. @after_last_test;"
	const stopWhole = "DNS A shutdown-audit.staging.internal. ;"
	for _, tc := range []struct {
		name       string
		serveTest  bool
		last       bool
		finalTry   bool
		stops      int
		named      string
		postBefore bool
		// noLater: no later set runs (noLaterSetRuns): each is wholly
		// ignored or has no test case.
		noLater bool
	}{
		{"a stack the set brings up for itself", false, false, false, 1, stopNamed, true, false},
		{"a kept stack's last set, its final attempt", true, true, true, 1, stopNamed, true, false},
		{"a kept stack's earlier set", true, false, true, 0, "", false, false},
		{"a kept stack's last set, an attempt that another follows", true, true, false, 0, "", false, false},
		{"a kept stack's last set that runs, its final attempt", true, false, true, 1, stopNamed, true, true},
		{"a kept stack's last set that runs, an attempt that another follows", true, false, false, 0, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &composeReads{}
			h := composeReadHarness(t, c)
			h.replayer.isLastTestSet, h.replayer.finalAttempt = tc.last, tc.finalTry
			h.replayer.noLaterSetRuns = tc.noLater
			status := h.runTestSet(t, tc.serveTest)
			if status != models.TestSetStatusPassed {
				t.Fatalf("status %s (reason %q), want PASSED: a lookup the app makes as it stops fails nothing", status, h.report.report.FailureReason)
			}
			stops, starts, postRan := c.counts()
			if stops != tc.stops || starts != 0 || (tc.stops > 0 && postRan != tc.postBefore) {
				t.Fatalf("%d stops of the app's container (the post-script had run: %v), %d starts; want %d (after it: %v), none", stops, postRan, starts, tc.stops, tc.postBefore)
			}
			if ends := c.ends.Load(); ends != 1 {
				t.Fatalf("%d set-end reads, want one", ends)
			}
			if got := calledShaped(h.report.report.OutsideTestMisses); got != tc.named {
				t.Fatalf("the report names %q outside every test, want %q", got, tc.named)
			}
			if tc.named != "" {
				if got := calledWhole(readTestMisses(t, h)); got != stopWhole {
					t.Fatalf("the misses file names %q, want %q", got, stopWhole)
				}
				if !h.replayer.composeAppStopped {
					t.Fatal("the stop is not kept: a set that follows would not start the app again")
				}
			}
		})
	}

	// A set that follows a stop (a kept stack's) has the app started again,
	// once it is staged on the agent, whatever the switch.
	c := &composeReads{}
	h := composeReadHarness(t, c)
	h.replayer.isLastTestSet, h.replayer.finalAttempt = true, true
	h.runServing(t)
	h.replayer.isLastTestSet = false
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("the set after the stop: status %s, want PASSED", status)
	}
	if stops, starts, _ := c.counts(); stops != 1 || starts != 1 {
		t.Fatalf("the set after the stop: %d stops, %d starts; want the app the stop ended started again once", stops, starts)
	}
	// A set that brings its stack up for itself after a stop has compose
	// start its app: keploy starts none.
	c = &composeReads{}
	h = composeReadHarness(t, c)
	h.replayer.composeAppStopped = true
	h.run(t)
	if stops, starts, _ := c.counts(); stops != 1 || starts != 0 {
		t.Fatalf("a stack brought up afresh after a stop: %d stops, %d starts; want its own stop, no start of keploy's", stops, starts)
	}

	// Under the switch the app is stopped once, at the set's end, before the
	// set is judged (TestAComposeSetStopsItsAppBeforeItsEndsRead), and not
	// again before the report.
	provenOn(t, true)
	for _, serveTest := range []bool{false, true} {
		c = &composeReads{}
		h = composeReadHarness(t, c)
		h.replayer.isLastTestSet, h.replayer.finalAttempt = true, true
		h.runTestSet(t, serveTest)
		if stops, _, _ := c.counts(); stops != 1 {
			t.Fatalf("under the switch (kept stack: %v): %d stops of the app's container, want one", serveTest, stops)
		}
	}
}

// A docker compose app that exited on its own before keploy's stop at the
// set's end (with the switch off) took keploy's agent down with its stack:
// nothing is read, and the set's status, judged before, stands. A stop that
// failed is said at WARN, and the set's end is read all the same, the agent
// running on in the stack.
func TestAComposeAppsStopBeforeTheReportThatFoundItExitedOrFailed(t *testing.T) {
	provenOn(t, false)
	c := &composeReads{stopErr: fmt.Errorf("the application's container exited with code 3: %w", models.ErrAppExitedOnItsOwn)}
	h := composeReadHarness(t, c)
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("an app that exited on its own after its set was judged: status %s, want PASSED, as judged", status)
	}
	if stops, _, _ := c.counts(); stops != 1 {
		t.Fatalf("%d stops, want one", stops)
	}
	if ends := c.ends.Load(); ends != 0 {
		t.Fatalf("%d set-end reads of an agent the app's exit took down with its stack, want none", ends)
	}

	c = &composeReads{stopErr: errors.New("daemon gone")}
	h = composeReadHarness(t, c)
	logs := observeWarnings(h)
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("a stop that failed: status %s, want PASSED", status)
	}
	if ends := c.ends.Load(); ends != 1 {
		t.Fatalf("a stop that failed: %d set-end reads, want one", ends)
	}
	said := false
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "could not stop the application's container before reading") {
			said = true
		}
	}
	if !said {
		t.Fatal("a stop that failed is not said at WARN")
	}
}

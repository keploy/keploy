package replay

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// captureInstr is the partial-run harness's agent, telling in order when the
// replayer opens a test's mock-error capture window and when it moves the
// agent's mock window (UpdateMockParams).
type captureInstr struct {
	*prInstr
	mu     sync.Mutex
	events []string
}

func (c *captureInstr) note(e string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *captureInstr) BeginTestErrorCapture(context.Context) error {
	c.note("begin")
	return nil
}

func (c *captureInstr) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	c.note("move")
	return c.prInstr.UpdateMockParams(ctx, params)
}

// continueInstr is captureInstr with the continue capability.
type continueInstr struct{ *captureInstr }

func (c continueInstr) ContinueTestErrorCapture(context.Context) error {
	c.note("continue")
	return nil
}

// Each test opens its capture window BEFORE the agent's mock window moves to
// it. Moving it releases what the test recorded the app being pushed (a Pulsar
// MESSAGE), and a call the app makes at once in reaction is this test's: with
// the window opened after the move, that miss landed with no window open and
// the agent discarded it, so a real mismatch was reported against no test
// (pulsar-followup-mismatch, enterprise pipeline 10181). The set's first test
// begins the capture; every later one continues it, carrying in what was
// missed between the two tests — in the streaming phase too.
func TestRunTestSetOpensEachTestsCaptureBeforeMovingItsMockWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		noContinue bool
		later      string // how every test after the first opens its window
	}{
		{name: "an agent that continues the capture", later: "continue"},
		{name: "an agent that predates continue", noContinue: true, later: "begin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 4, 0)
			h.streaming("test-4")
			ci := &captureInstr{prInstr: h.instr}
			if tc.noContinue {
				h.replayer.instrumentation = ci
			} else {
				h.replayer.instrumentation = continueInstr{ci}
			}
			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("precondition: the set reported %q; want PASSED", status)
			}

			// Whatever moves the mock window before the loop (the set's
			// startup) is no test's; from the first capture on, each test
			// opens its window and then moves the mock window to it.
			events := strings.Join(ci.events, " ")
			first := strings.Index(events, "begin")
			if first < 0 {
				t.Fatalf("no test opened a capture window: %s", events)
			}
			want := "begin move" + strings.Repeat(" "+tc.later+" move", 3)
			if got := events[first:]; got != want {
				t.Fatalf("capture and mock-window events from the first test on:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// resetOnceHooks resets the first request of the test named reset, as
// docker's userland proxy does to a connection it has just accepted, and
// answers every other request.
type resetOnceHooks struct {
	prHooks
	reset string
	mu    sync.Mutex
	tries int
}

func (h *resetOnceHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	if tc.Name == h.reset {
		h.mu.Lock()
		h.tries++
		first := h.tries == 1
		h.mu.Unlock()
		if first {
			return nil, io.ErrUnexpectedEOF
		}
	}
	return h.prHooks.SimulateRequest(ctx, tc, testSetID)
}

// A test whose request is reset before it reached the app is sent again with
// its capture window still open: what the window holds — the misses carried in
// from before the test, and those made as its mock window moved — is the
// test's, and reopening the window for the re-send dropped them.
func TestRunTestSetKeepsTheTestsCaptureOpenAcrossAResetResend(t *testing.T) {
	h := newPartialRunHarness(t, 3, 0)
	ci := &captureInstr{prInstr: h.instr}
	h.replayer.instrumentation = continueInstr{ci}
	hooks := &resetOnceHooks{reset: "test-2"}
	h.replayer.hookImpl = hooks
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("precondition: the set reported %q; want PASSED", status)
	}
	if hooks.tries != 2 {
		t.Fatalf("precondition: test-2 was sent %d time(s); want 2 (reset, then re-sent)", hooks.tries)
	}
	events := strings.Join(ci.events, " ")
	first := strings.Index(events, "begin")
	if first < 0 {
		t.Fatalf("no test opened a capture window: %s", events)
	}
	if got := events[first:]; got != "begin move continue move continue move" {
		t.Fatalf("capture and mock-window events from the first test on:\n got %s\nwant begin move continue move continue move", got)
	}
}

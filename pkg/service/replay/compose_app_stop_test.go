package replay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// composeInstr is the harness's instrumentation with the stop and start of a
// compose app's container (AgentClient's), counting each.
type composeInstr struct {
	*prInstr
	stops, starts int
	killed        string
	stopErr       error
	startErr      error
	// order is the agent's readiness and the app's starts, as they came.
	order []string
}

func (c *composeInstr) StopComposeApp(context.Context) (string, error) {
	c.stops++
	return c.killed, c.stopErr
}

func (c *composeInstr) StartComposeApp(context.Context) error {
	c.starts++
	c.order = append(c.order, "start")
	return c.startErr
}

func (c *composeInstr) MakeAgentReadyForDockerCompose(ctx context.Context) error {
	c.order = append(c.order, "agent ready")
	return c.prInstr.MakeAgentReadyForDockerCompose(ctx)
}

// composeHarness is the harness for a set of a docker compose stack kept
// across sets, with c as its instrumentation.
func composeHarness(t *testing.T, c *composeInstr) *prRun {
	t.Helper()
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.CommandType = string(utils.DockerCompose)
	c.prInstr = h.instr
	h.replayer.instrumentation = c
	return h
}

// P101-c: under the proven switch keploy stops a compose app's container at
// the set's end, its stack (keploy's agent among it) running on, and reads the
// set's end with the app stopped: no read with the app running is said. The
// next set of a stack kept across sets starts it again before its tests.
func TestAComposeSetStopsItsAppBeforeItsEndsRead(t *testing.T) {
	provenOn(t, true)
	c := &composeInstr{}
	h := composeHarness(t, c)
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED", status)
	}
	if c.stops != 1 || c.starts != 0 || h.report.report.AppNotStopped != "" || !h.replayer.composeAppStopped {
		t.Fatalf("%d stops, %d starts, app_not_stopped %q, stopped %v; want the app stopped once before the read, nothing said",
			c.stops, c.starts, h.report.report.AppNotStopped, h.replayer.composeAppStopped)
	}
	// The next set starts it again, once the set is staged on the agent (as
	// compose starts the first set's app only then), and stops it at its end.
	c.order = nil
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("the next set: status %s, want PASSED", status)
	}
	if c.starts != 1 || c.stops != 2 {
		t.Fatalf("the next set: %d starts, %d stops; want the app started again once, and stopped at its end", c.starts, c.stops)
	}
	if strings.Join(c.order, ",") != "agent ready,start" {
		t.Fatalf("the next set: %v; want the app started once the agent is ready for the set", c.order)
	}
}

// A compose app's stop that had to kill it, or that failed, leaves the set's
// end not judged, naming why; an app that could not be started again halts
// the set; with the switch off nothing is stopped.
func TestAComposeAppsStopThatKilledOrFailedLeavesTheSetUnjudged(t *testing.T) {
	provenOn(t, true)
	for _, tc := range []struct {
		name   string
		c      *composeInstr
		reason string
	}{
		{"killed", &composeInstr{killed: "its container had not stopped 5s later (the grace keploy's stop gives a docker compose app), so keploy killed it"}, "keploy interrupted the application at the set's end and its container had not stopped"},
		{"failed", &composeInstr{stopErr: errors.New("daemon gone")}, "could not stop the application's container at the set's end (daemon gone)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := composeHarness(t, tc.c)
			status := h.runServing(t)
			if status != models.TestSetStatusInternalErr || !h.replayer.notJudged("test-set-0") || !strings.Contains(h.report.report.FailureReason, tc.reason) {
				t.Fatalf("status %s, not judged %v, reason %q; want INTERNAL_ERR naming %q", status, h.replayer.notJudged("test-set-0"), h.report.report.FailureReason, tc.reason)
			}
		})
	}
	// A compose app that exited on its own before the set's end is a halt,
	// not a stop: the set is APP_HALTED, as compose's abort made it, and its
	// end is not read (the halt took keploy's agent down with the stack).
	halted := &composeInstr{stopErr: fmt.Errorf("the application's container exited with code 3: %w", models.ErrAppExitedOnItsOwn)}
	h0 := composeHarness(t, halted)
	if status := h0.runServing(t); status != models.TestSetStatusAppHalted || h0.replayer.notJudged("test-set-0") {
		t.Fatalf("an app that exited on its own: status %s, not judged %v; want APP_HALTED, its end not read", status, h0.replayer.notJudged("test-set-0"))
	}

	c := &composeInstr{startErr: errors.New("no such container")}
	h := composeHarness(t, c)
	h.replayer.composeAppStopped = true
	status, err := h.replayer.RunTestSet(context.Background(), "test-set-0", "test-run-0", true)
	if status != models.TestSetStatusAppHalted || err == nil || c.stops != 0 {
		t.Fatalf("an app that could not be started again: status %s (%v) after %d stops; want APP_HALTED with its error, no stop", status, err, c.stops)
	}

	provenOn(t, false)
	off := &composeInstr{}
	h = composeHarness(t, off)
	if status := h.runServing(t); status != models.TestSetStatusPassed || off.stops != 0 || off.starts != 0 {
		t.Fatalf("switch off: status %s, %d stops, %d starts; want PASSED, none", status, off.stops, off.starts)
	}
}

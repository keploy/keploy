package replay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// drainInstr is a run's instrumentation whose agent drains (CaptureLossDrainer):
// a close its proxy counts only as it drains (stopLoss, the close of a
// connection the application's stop at the set's end made) is in the set
// end's take after the drain, and in none before it. err is the drain's
// answer; order is the drain and the takes, in order.
type drainInstr struct {
	*prLossInstr
	stopLoss *models.OwnedCaptureLoss
	err      error
	drained  bool
	order    []string
}

func (d *drainInstr) DrainCaptureLosses(context.Context) error {
	d.lmu.Lock()
	defer d.lmu.Unlock()
	d.order = append(d.order, "drain")
	d.drained = true
	return d.err
}

func (d *drainInstr) TakeOwnedCaptureLosses(context.Context) ([]models.OwnedCaptureLoss, error) {
	d.lmu.Lock()
	defer d.lmu.Unlock()
	d.order = append(d.order, "take")
	if d.drained && d.stopLoss != nil {
		l := *d.stopLoss
		d.stopLoss = nil
		return []models.OwnedCaptureLoss{l}, nil
	}
	return nil, nil
}

// A connection that closes as the set's application stops, with bytes the
// application wrote lost: the set's end has the agent count the close of
// every connection the stop ended (drainCaptureLosses) before it reads the
// set's losses, so the loss fails that set, named in its report, and never
// waits for a later set or only a log line. A drain that fails leaves the set
// neither passed nor failed: what the stop's closes lost cannot be told. An
// agent that does not drain leaves the set's reads as they were.
func TestASetsEndCountsTheClosesItsApplicationsStopMade(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	lost := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1,
		What: "from [::ffff:127.0.0.1]:50914, written as the application stopped"}
	run := func(t *testing.T, stopLoss *models.OwnedCaptureLoss, err error) (*prRun, *drainInstr, models.TestSetStatus) {
		h := newPartialRunHarness(t, 3, 0) // every test case passes
		h.replayer.config.Test.Mocking = true
		instr := &drainInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}},
			stopLoss: stopLoss, err: err}
		h.replayer.instrumentation = instr
		h.replayer.markPIDMaps(context.Background())
		return h, instr, h.run(t)
	}

	t.Run("the stop's loss fails its own set", func(t *testing.T) {
		h, instr, status := run(t, &models.OwnedCaptureLoss{Loss: lost}, nil)
		if got := strings.Join(instr.order, ","); got != "take,take,take,drain,take" {
			t.Fatalf("drain and takes %q, want the set's end to drain once, after the test cases' takes and before its own", got)
		}
		if status != models.TestSetStatusFailed {
			t.Fatalf("set %q, want FAILED: bytes the application wrote as it stopped were lost", status)
		}
		cv := h.report.report.CaptureVerification
		if cv == nil || cv.Status != models.CaptureIncompleteStatus || !strings.Contains(cv.Reason, lost.What) {
			t.Fatalf("capture_verification %+v, want incomplete naming %q", cv, lost.What)
		}
	})

	t.Run("a drain that fails leaves the set unjudged", func(t *testing.T) {
		h, _, status := run(t, nil, errors.New("keploy's proxy had not finished counting the close of 1 connection(s)"))
		if status == models.TestSetStatusPassed || status == models.TestSetStatusFailed {
			t.Fatalf("set %q, want neither PASSED nor FAILED: what the stop's closes lost cannot be told", status)
		}
		if cv := h.report.report.CaptureVerification; cv == nil || cv.Status != models.CaptureUnverifiedStatus || !strings.Contains(cv.Reason, "had not finished counting") {
			t.Fatalf("capture_verification %+v, want unverified naming the drain", cv)
		}
	})

	t.Run("a drain that gave up on a stuck handler leaves the set unjudged, named", func(t *testing.T) {
		// The agent's drain owns, as the session's, the close of a
		// connection whose handler never finished closing it: not judged,
		// naming where it went and the session. The set's take has it.
		stuck := models.CaptureLoss{Kind: models.CaptureLossAppBytesUntold, Count: 1,
			What: "to 10.0.0.5:5432 from 127.0.0.1:40000, in session 2, whose handler never finished closing it once the application's end had ended"}
		h, _, status := run(t, &models.OwnedCaptureLoss{Loss: stuck}, nil)
		if status == models.TestSetStatusPassed || status == models.TestSetStatusFailed {
			t.Fatalf("set %q, want neither PASSED nor FAILED: what the stuck close lost cannot be told", status)
		}
		cv := h.report.report.CaptureVerification
		if cv == nil || cv.Status != models.CaptureUnverifiedStatus || !strings.Contains(cv.Reason, "10.0.0.5:5432") || !strings.Contains(cv.Reason, "session 2") {
			t.Fatalf("capture_verification %+v, want unverified naming the stuck connection's destination and session", cv)
		}
	})

	t.Run("an agent that does not drain", func(t *testing.T) {
		_, instr, status := run(t, nil, models.ErrCaptureLossesUnsupported)
		if status != models.TestSetStatusPassed {
			t.Fatalf("set %q, want PASSED: an agent older than the drain leaves the set's reads as they were", status)
		}
		if got := strings.Join(instr.order, ","); got != "take,take,take,drain,take" {
			t.Fatalf("drain and takes %q", got)
		}
	})
}

// The set's end asks the agent to drain only under the proven switch and
// with an agent: with the switch off, or keploy test run with no agent
// (noAgent), it asks nothing, and what a drain answers is the set's.
func TestASetsEndDrainsOnlyUnderTheSwitchWithAnAgent(t *testing.T) {
	failed := errors.New("keploy's proxy had not finished counting the close of 1 connection(s)")
	for _, c := range []struct {
		name    string
		env     string
		noAgent bool
		want    error
	}{
		{name: "the switch on, an agent", env: "1", want: failed},
		{name: "the switch off", env: "0"},
		{name: "no agent", env: "1", noAgent: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, c.env)
			instr := &drainInstr{prLossInstr: &prLossInstr{prInstr: &prInstr{}}, err: failed}
			r := &Replayer{instrumentation: instr, noAgent: c.noAgent}
			if err := r.drainCaptureLosses(context.Background()); !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
				t.Fatalf("drain = %v, want %v", err, c.want)
			}
			if asked := c.want != nil; instr.drained != asked {
				t.Fatalf("the agent asked to drain: %v, want %v", instr.drained, asked)
			}
		})
	}
}

// A set whose application runs on past its end (--keep-app-alive keeps it
// across sets, and this set's end did not stop it) has no stop's closes to
// count: its end asks the agent for no drain, and its reads are as they
// were. The final attempt of the run's last set stops that application, so
// its end drains once, before the set's own take.
func TestASetsEndDrainsOnlyWhenItStoppedItsApplication(t *testing.T) {
	provenOn(t, true)
	for _, c := range []struct {
		name        string
		last, final bool
		drains      bool
	}{
		{name: "another set, its application kept running", last: false, final: true},
		{name: "an earlier attempt of the last set, its application kept running", last: true, final: false},
		{name: "the last set's final attempt, which stops it", last: true, final: true, drains: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0) // every test case passes
			h.replayer.config.Test.Mocking = true
			instr := &drainInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}}}
			h.replayer.instrumentation = instr
			h.replayer.markPIDMaps(context.Background())
			stops := 0
			h.replayer.noLaterSetRuns, h.replayer.finalAttempt = c.last, c.final
			h.replayer.keepAliveStop = func() string { stops++; return "" }
			status := h.runServing(t)
			if status != models.TestSetStatusPassed {
				t.Fatalf("set %q, want PASSED", status)
			}
			if stopped := stops == 1; stopped != c.drains {
				t.Fatalf("premise: the set's end stopped the application %d time(s), want stopped %v", stops, c.drains)
			}
			got := strings.Join(instr.order, ",")
			if instr.drained != c.drains {
				t.Fatalf("the agent asked to drain: %v (drain and takes %q), want %v", instr.drained, got, c.drains)
			}
			if c.drains && !strings.HasSuffix(got, "drain,take") {
				t.Fatalf("drain and takes %q, want the set's end to drain before its own take", got)
			}
		})
	}
}

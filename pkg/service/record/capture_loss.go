package record

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// PIDMapReader is an optional Instrumentation extension: the inserts into the
// agent's PID maps the kernel has refused since its hooks loaded, each of them
// a process the hooks could not track (models.PIDMapRefusals).
type PIDMapReader interface {
	GetPIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error)
}

// captureWatch is, under the proven switch, keploy record's account of what
// the agent could not capture (design v14 §3.2, rule 12): the processes the
// kernel refused a place in the agent's PID maps, counted from the moment
// the agent began capturing. A loss taints the test case being recorded when
// it happened and every later one, so each test case is checked as it is
// saved: the first whose check sees a loss and every one after it are named
// at the end, and the recording fails. A test case whose check could not be
// read is named as unverified, and fails the recording too. With the switch
// off, or an agent that cannot count, it does nothing.
type captureWatch struct {
	logger *zap.Logger
	reader PIDMapReader
	off    bool
	base   models.PIDMapRefusals
	// baseErr is why the count could not be read when capture began; every
	// test case is then unverified.
	baseErr error

	mu         sync.Mutex
	tainted    []string
	unverified []string
	lost       models.PIDMapRefusals
	readErr    error
}

// watchCaptureLosses marks the count the recording's test cases are checked
// against. Called once the agent is capturing and before the app starts.
func watchCaptureLosses(ctx context.Context, logger *zap.Logger, instr Instrumentation) *captureWatch {
	w := &captureWatch{logger: logger}
	reader, ok := instr.(PIDMapReader)
	if !provenswitch.On() || !ok {
		w.off = true
		return w
	}
	w.reader = reader
	// No deadline of its own (rule 15): the agent's answer, the connection's
	// close or ctx ends it.
	base, err := reader.GetPIDMapRefusals(ctx)
	switch {
	case errors.Is(err, models.ErrPIDMapRefusalsUnsupported):
		logger.Debug("the agent cannot say whether it tracked every process of the application; capture completeness is not checked", zap.Error(err))
		w.off = true
	case err != nil:
		w.baseErr = err
	default:
		w.base = base
	}
	return w
}

// afterTestCase checks the count as test case name is saved.
func (w *captureWatch) afterTestCase(ctx context.Context, name string) {
	if w == nil || w.off {
		return
	}
	if w.baseErr != nil {
		w.mu.Lock()
		w.unverified = append(w.unverified, name)
		w.mu.Unlock()
		return
	}
	now, err := w.reader.GetPIDMapRefusals(context.WithoutCancel(ctx))
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.unverified = append(w.unverified, name)
		if w.readErr == nil {
			w.readErr = err
		}
		return
	}
	if refused := now.Since(w.base); refused.Any() {
		w.tainted = append(w.tainted, name)
		w.lost = refused
	}
}

// finish says, at ERROR, which test cases were recorded while the agent could
// not capture every call, or could not be checked, and reports whether the
// recording is to fail for it.
func (w *captureWatch) finish(testSetID string) bool {
	if w == nil || w.off {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	failed := false
	if len(w.tainted) > 0 {
		failed = true
		w.logger.Error("the recording is incomplete: the agent could not track every process the application ran, so these test cases' calls may be missing from the mocks ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Strings("testCases", w.tainted),
			zap.String("reason", w.lost.CaptureIncomplete()),
			zap.String("next_step", "re-record these test cases with fewer live processes on the machine, or run the app in Docker so the agent sees only its container"))
	}
	if len(w.unverified) > 0 {
		failed = true
		why := w.baseErr
		if why == nil {
			why = w.readErr
		}
		w.logger.Error("the recording is not verified: keploy could not read whether the agent tracked every process the application ran while these test cases were recorded ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Strings("testCases", w.unverified),
			zap.String("reason", models.CaptureUnverified(why)),
			zap.String("next_step", "check the agent's log for why its count could not be read, then re-record"))
	}
	return failed
}

// String is a one-line account, for tests.
func (w *captureWatch) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return fmt.Sprintf("tainted=[%s] unverified=[%s]", strings.Join(w.tainted, ","), strings.Join(w.unverified, ","))
}

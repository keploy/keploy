package record

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// CaptureLossReader is an optional Instrumentation extension: every kind of
// capture loss the agent has counted since it started (models.CaptureLosses).
type CaptureLossReader interface {
	GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error)
}

// captureWatch is, under the proven switch, keploy record's account of what
// the agent could not capture (design v14 §3.2, rule 12): processes the
// kernel refused a place in the agent's PID maps, client-side TLS handshakes
// that failed against keploy's CA, TLS upgrades keploy could not make,
// counted from the moment the agent began capturing. A loss taints the test case being recorded when
// it happened and every later one, so each test case is checked as it is
// saved: the first whose check sees a loss and every one after it are named
// at the end, and the recording fails. A test case whose check could not be
// read is named as unverified, and fails the recording too. With the switch
// off, or an agent that cannot count, it does nothing.
type captureWatch struct {
	logger *zap.Logger
	reader CaptureLossReader
	off    bool
	base   models.CaptureLosses
	// baseErr is why the count could not be read when capture began; every
	// test case is then unverified.
	baseErr error

	mu         sync.Mutex
	tainted    []string
	unverified []string
	lost       models.CaptureLosses
	readErr    error
	// uncapturable are the test cases recorded while or after the
	// application called what keploy cannot capture at all (decision 20),
	// and notJudged what that was: said, never a failure.
	uncapturable []string
	notJudged    models.CaptureLosses
}

// watchCaptureLosses marks the count the recording's test cases are checked
// against. Called once the agent is capturing and before the app starts.
func watchCaptureLosses(ctx context.Context, logger *zap.Logger, instr Instrumentation) *captureWatch {
	w := &captureWatch{logger: logger}
	reader, ok := instr.(CaptureLossReader)
	if !provenswitch.On() || !ok {
		w.off = true
		return w
	}
	w.reader = reader
	// No deadline of its own (rule 15): the agent's answer, or the
	// connection's close, ends it. Not the stop: from here a stop drains what
	// the agent captured (Start), and each test case it saves is checked
	// against this count, so a stop that cut the read short would fail the
	// recording the user stopped as "not verified". A second interrupt, which
	// gives the drain up, ends it.
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRead()
	go func() {
		select {
		case <-utils.StopNow():
			cancelRead()
		case <-readCtx.Done():
		}
	}()
	base, err := reader.GetCaptureLosses(readCtx)
	switch {
	case errors.Is(err, models.ErrCaptureLossesUnsupported):
		logger.Debug("the agent cannot say whether it captured every call; capture completeness is not checked", zap.Error(err))
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
	now, err := w.reader.GetCaptureLosses(context.WithoutCancel(ctx))
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.unverified = append(w.unverified, name)
		if w.readErr == nil {
			w.readErr = err
		}
		return
	}
	judged, notJudged := now.Since(w.base).Split()
	if len(judged) > 0 {
		w.tainted = append(w.tainted, name)
		w.lost = judged
	}
	if len(notJudged) > 0 {
		w.uncapturable = append(w.uncapturable, name)
		w.notJudged = notJudged
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
		w.logger.Error("the recording is incomplete: keploy's agent could not capture every call while these test cases were recorded, so their calls may be missing from the mocks ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Strings("testCases", w.tainted),
			zap.String("reason", w.lost.Text()),
			zap.String("next_step", "fix what the reason names, then re-record these test cases"))
	}
	if len(w.uncapturable) > 0 {
		// Not a failure: keploy never had these calls to lose. The test
		// cases are kept; a replay of them makes those calls for real.
		w.logger.Warn("these test cases were recorded while or after the application called what keploy cannot capture: their calls over it are not in the mocks ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Strings("testCases", w.uncapturable),
			zap.String("reason", w.notJudged.NotJudgedText()),
			zap.String("next_step", "reach that dependency over a TCP address (mocked, or listed in mock.real), then re-record these test cases"))
	}
	if len(w.unverified) > 0 {
		failed = true
		why := w.baseErr
		if why == nil {
			why = w.readErr
		}
		w.logger.Error("the recording is not verified: keploy could not read whether its agent captured every call while these test cases were recorded ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.Strings("testCases", w.unverified),
			zap.Error(why),
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

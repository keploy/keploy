//go:build linux

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// What the agent could not capture that no test's end was charged with is
// what was lost after the last test ended: a record's judging fails the run
// on it (design v14 §7.5, evidence charged to no test). A loss a test was
// charged with is not in it; one after the last test is, by its kind; and
// a new session forgets the last one's charges.
func TestUnchargedCaptureLossesAreWhatNoTestWasChargedWith(t *testing.T) {
	ctx := context.Background()
	a, _ := wireAgent(t, models.MODE_RECORD)
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestA").Action)
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "nothing lost")

	// Lost while TestB ran: TestB is charged with it.
	_, serr := a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Target: 2}}
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: "passed"})
	require.Nil(t, serr)
	un, err = a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "a loss charged to a test is no run's loss")

	// Lost after the last test ended: no test is charged with it.
	a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Target: 5}}
	un, err = a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Len(t, un, 1)
	require.Equal(t, models.CaptureLossPIDMap, un[0].Kind)
	require.Equal(t, uint64(3), un[0].Count, "the 3 lost after the last test, not the 2 TestB was charged with")

	// A new session counts from its own start, and charges nothing yet.
	require.NoError(t, a.resetSession(ctx))
	un, err = a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un)
}

// lastEndSettler is a capture that says what it lost since the run's last
// test ended (LastEndLossReporter), once it settled as an end does.
type lastEndSettler struct {
	fakeCaptureSettler
	lost models.CaptureLosses
	err  error
}

func (s *lastEndSettler) CaptureLossesSinceLastEnd(ctx context.Context) (models.CaptureLosses, error) {
	if err := s.SettleCapture(ctx); err != nil {
		return nil, err
	}
	return s.lost, s.err
}

// What the capture a proven record's tests settle through lost after the
// last test ended is in what no test was charged with, which the record
// reads for its summary and judging reads as the run's evidence, never only
// in the agent's log. What it cannot tell is an error, never "nothing". A
// replay settles through keploy's proxy and never asks it.
func TestARecordsUnchargedLossesHoldWhatItsCaptureLostAfterTheLastTest(t *testing.T) {
	ctx := context.Background()
	a, _ := wireAgent(t, models.MODE_RECORD)
	cs := &lastEndSettler{}
	withCaptureSettler(t, cs)
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestA").Action)
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "nothing lost")

	lost := models.CaptureLoss{Kind: models.CaptureLossCaptureEvents, Count: 2, What: "the capture ring"}
	cs.lost = models.CaptureLosses{lost}
	un, err = a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, models.CaptureLosses{lost}, un, "what the capture lost after the last test")
	judged, _ := un.Split()
	require.Len(t, judged, 1, "the run's evidence, which judging reads")

	cs.err = errors.New("the capture ring's count could not be read")
	_, err = a.UnchargedCaptureLosses(ctx)
	require.ErrorIs(t, err, cs.err)
	require.ErrorContains(t, err, "keploy cannot tell what the capture lost after the last test")

	replay, _ := wireAgent(t, models.MODE_TEST)
	before := cs.settles.Load()
	_, _ = replay.UnchargedCaptureLosses(ctx)
	require.Equal(t, before, cs.settles.Load(), "a replay never asks the capture")
}

// The read of what the capture lost after the last test settles under the
// settle's watchdog, as an end does: a wait whose counts stop moving fails
// at settleStallLimit wake-ups, never hangs the record's last read.
func TestTheReadOfWhatTheCaptureLostAfterTheLastTestRunsUnderTheWatchdog(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	wake := manualWake(t)
	prev := settleStallLimit
	settleStallLimit = 2
	t.Cleanup(func() { settleStallLimit = prev })
	a := &Agent{logger: zap.NewNop(), config: &config.Config{}}
	cs := &lastEndSettler{fakeCaptureSettler: fakeCaptureSettler{block: make(chan struct{}), entered: make(chan struct{})}}
	withCaptureSettler(t, cs)
	done := make(chan error, 1)
	go func() {
		_, err := a.lostSinceLastEnd(context.Background())
		done <- err
	}()
	select {
	case <-cs.entered:
	case err := <-done:
		t.Fatalf("the read did not settle through the capture: %v", err)
	}
	wake()
	wake()
	err := <-done
	require.ErrorIs(t, err, errNoProgress)
	require.ErrorContains(t, err, "keploy cannot tell what the capture lost after the last test")
}

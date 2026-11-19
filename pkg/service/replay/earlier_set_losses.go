package replay

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The capture losses of an earlier test set that the agent found only after
// that set had ended (under the proven switch): with the application kept
// across sets, a connection a set used can close, with bytes it wrote lost or
// untold, after the set's last read. keploy test starts each set's session
// with the set's name and the run's token (models.TestSetSession), and the
// agent stamps such a loss with the set (models.OwnedCaptureLoss.
// EarlierSession, Set), so its take returns it as that set's. It is no loss of
// the set whose take returned it, nor of that set's tests, and the set it was
// made in has its verdict already, which came too late: the run ends not
// judged for that set, naming it and its test, and no set's verdict counts it.

// earlierSetLosses are the losses of earlier sets the run's takes returned,
// in the order they were taken.
type earlierSetLosses struct {
	mu     sync.Mutex
	losses []earlierSetLoss
}

// earlierSetLoss is a loss of an earlier set, with the set whose take
// returned it.
type earlierSetLoss struct {
	readIn string
	loss   models.OwnedCaptureLoss
}

// take keeps the losses of owned that an earlier set made
// (models.OwnedCaptureLoss.EarlierSession), read in readIn, and returns the
// others, and those it kept.
func (e *earlierSetLosses) take(owned []models.OwnedCaptureLoss, readIn string) (rest []models.OwnedCaptureLoss, taken []earlierSetLoss) {
	for _, o := range owned {
		if !o.EarlierSession {
			rest = append(rest, o)
			continue
		}
		taken = append(taken, earlierSetLoss{readIn: readIn, loss: o})
	}
	e.mu.Lock()
	e.losses = append(e.losses, taken...)
	e.mu.Unlock()
	return rest, taken
}

// all is every loss of an earlier set the run took.
func (e *earlierSetLosses) all() []earlierSetLoss {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]earlierSetLoss(nil), e.losses...)
}

// reset forgets every loss, for a new run.
func (e *earlierSetLosses) reset() {
	e.mu.Lock()
	e.losses = nil
	e.mu.Unlock()
}

// madeWhere is where l was made, as a line says it: "test-2 of test-set-a",
// or that its bytes were written in no test of the set (between two of its
// tests, or after its last).
func (l earlierSetLoss) madeWhere() string {
	set := l.loss.Set
	if set == "" {
		set = "an earlier test set"
	}
	test := l.loss.Test
	if test == "" {
		test = "no test (between two of its tests, or after its last)"
	}
	return test + " of " + set
}

// newSetSessionRun is the token keploy test names a run's sessions by
// (models.TestSetSession.Run): drawn as each run starts, so an agent kept
// from an earlier run tells that run's sets from this one's.
func newSetSessionRun() string {
	return rand.Text()
}

// withTestSetSession is ctx naming testSetID, of this run, as the set the
// agent's session started under it serves (models.TestSetSession).
func (r *Replayer) withTestSetSession(ctx context.Context, testSetID string) context.Context {
	return models.WithTestSetSession(ctx, models.TestSetSession{Run: r.setSessionRun, Set: testSetID})
}

// takeEarlierSetLosses is owned, a take of readIn's, without the losses an
// earlier set made (earlierSetLosses.take), which the run keeps to end not
// judged with. It says each at ERROR as it takes it, naming the set it was
// made in, its test, where one is named, and the set whose take returned it.
func (r *Replayer) takeEarlierSetLosses(owned []models.OwnedCaptureLoss, readIn string) []models.OwnedCaptureLoss {
	rest, taken := r.earlierSetLosses.take(owned, readIn)
	for _, l := range taken {
		r.logger.Error("test run not judged: keploy found, only after an earlier test set had ended, that "+lossOfASet(l.loss.Loss)+" ("+provenswitch.Env+"=1)",
			zap.String("made-in-test", l.loss.Test),
			zap.String("made-in-test-set", l.loss.Set),
			zap.String("read-in-test-set", l.readIn),
			zap.String("kind", string(l.loss.Loss.Kind)),
			zap.Uint64("count", l.loss.Loss.Count),
			zap.String("what", l.loss.Loss.LogWhat()),
			zap.String("next_step", "the loss is that set's (and that test's, where one is named), not the set whose read returned it: run that test set again; check the agent's log for why the connection ended"))
	}
	return rest
}

// lossOfASet says what l, a loss of a set's found after its end, lost.
func lossOfASet(l models.CaptureLoss) string {
	if l.Kind.NotJudged() {
		return "it cannot tell whether a connection of that set's carried a call it did not capture"
	}
	return "it could not capture a call on a connection of that set's"
}

// earlierSetLossesWhy says why the losses leave the run not judged, as the
// run's log line says it: each loss by its kind and where it was made.
func earlierSetLossesWhy(losses []earlierSetLoss) string {
	var named []string
	for _, l := range losses {
		named = append(named, fmt.Sprintf("%s x%d (made in %s)", l.loss.Loss.Kind, l.loss.Loss.Count, l.madeWhere()))
	}
	return fmt.Sprintf("keploy found %d capture loss(es) of earlier test sets only after those sets had ended, so their verdicts are not known: %s",
		len(losses), strings.Join(named, "; "))
}

// passesEarlierSetLosses is the run's verdict pass, false where a loss an
// earlier set made was found after that set's end (earlierSetLosses.all):
// that set is not judged, which it says at ERROR, naming each set and test.
// Each such set is shown failing in the run's summary
// (earlierSetLossesSummary says why).
func (r *Replayer) passesEarlierSetLosses(pass bool) bool {
	late := r.earlierSetLosses.all()
	if len(late) == 0 {
		return pass
	}
	r.logger.Error("test run not judged: "+earlierSetLossesWhy(late)+" ("+provenswitch.Env+"=1)",
		zap.String("next_step", "run each test set named again; check the agent's log for why each connection ended"))
	r.completeTestReportMu.Lock()
	for _, l := range late {
		if v, ok := r.completeTestReport[l.loss.Set]; ok {
			v.status = false
			r.completeTestReport[l.loss.Set] = v
		}
	}
	r.completeTestReportMu.Unlock()
	return false
}

// earlierSetLossesSummary is the run summary's list of the test sets not
// judged for a loss found after their end, each with its test and what was
// lost; "" for none.
func (r *Replayer) earlierSetLossesSummary() string {
	late := r.earlierSetLosses.all()
	if len(late) == 0 {
		return ""
	}
	sets := map[string]bool{}
	for _, l := range late {
		sets[l.loss.Set] = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n\tTest sets not judged, a capture loss of theirs found after their end: %d\n", len(sets))
	for _, l := range late {
		fmt.Fprintf(&b, "\t\t%s: %s\n", l.madeWhere(), models.CaptureLosses{l.loss.Loss}.LogText())
	}
	return b.String()
}

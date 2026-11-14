package mock

import (
	"context"
	"slices"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// seriesStallLimit is the count of wake-ups in a row, with none of the
// series the run owes answered, after which the wait for them stops
// (awaitRequiredSeries), as keploy test's wait for its worker scope stops
// (replay's workerStallLimit). It counts wake-ups, not seconds: a starved run
// starves its wait equally, so load alone cannot end it. At seriesWakeAfter's
// delays, 6000 wake-ups span about 600 s.
var seriesStallLimit = 6000

// seriesWakeFirst is how soon the wait first reads what the agent consumed,
// and seriesWakeMost the longest it sleeps between two reads: a series
// answered as the wait begins is read within about seriesWakeFirst, one
// answered later at most seriesWakeMost after its answer.
const (
	seriesWakeFirst = 10 * time.Millisecond
	seriesWakeMost  = 100 * time.Millisecond
)

// seriesWakeAfter is how long the wait sleeps before its n-th wake-up, from
// 0: seriesWakeFirst, twice as long at each wake-up after it, at most
// seriesWakeMost.
func seriesWakeAfter(n int) time.Duration {
	d := seriesWakeFirst
	for ; n > 0 && d < seriesWakeMost; n-- {
		d *= 2
	}
	return min(d, seriesWakeMost)
}

// seriesWake is how the wait wakes: next is its next wake-up, which comes
// seriesWakeAfter(n) after the wait asks for it, n the count of wake-ups it
// asked for before. A test replaces it.
var seriesWake = func(context.Context) (func() <-chan time.Time, func()) {
	n := 0
	return func() <-chan time.Time {
		d := seriesWakeAfter(n)
		n++
		return time.After(d)
	}, func() {}
}

// awaitRequiredSeries waits, once the test command has exited and while
// what it left of the application runs on (running), until every series a
// declared rule requires that is due (owedSeries) has answered: an
// application makes a clean-up in its own time, after its last test too, as
// keploy test waits for its worker scope (replay's awaitWorkerScope). It is
// the run's models.RunOptions.BeforeStop: keploy stops what the application
// left only once it returns, and it holds no call. It stops waiting when
// every owed series answered, when the application no longer runs, when the
// run is stopped (ctx), or when none of them answers across seriesStallLimit
// wake-ups in a row: the wake-ups count the application's progress on them,
// not keploy's. What it reads of the consumed recordings is kept in
// m.seriesRead, which the run's end reads with the rest (reportOutcome): a
// read hands back only what was consumed since the read before. A read that
// fails ends the wait; the run's end reads again, and judges on its read.
// The tests that ran are those of the run's windows as the agent has them;
// a series trailing one of them holds the wait only if that test passed
// (seriesDue.holdsWait).
func (m *mockService) awaitRequiredSeries(ctx context.Context, running func() bool) {
	if len(m.series) == 0 {
		return
	}
	reader, ok := m.instrumentation.(ScopeReader)
	if !ok {
		return
	}
	windows, err := reader.GetScopeWindows(ctx)
	if err != nil {
		m.logger.Debug("keploy could not read which tests ran to wait for the series they owe; the run's end judges them as they are", zap.Error(err))
		return
	}
	ran, passed := map[string]bool{}, map[string]bool{}
	for _, w := range testWindows(windows) {
		if ranOutcome(w.Outcome) {
			ran[w.Name] = true
		}
		if w.Outcome == models.ScopeOutcomePassed {
			passed[w.Name] = true
		}
	}
	owed := m.readOwedSeries(ctx, ran, passed)
	if len(owed) == 0 {
		return
	}
	m.logger.Info("waiting for the series of declared rules the run owes: the application runs on after the test command exited, and makes them in its own time ("+provenswitch.Env+"=1)",
		zap.Int("series", len(owed)), zap.Strings("owed", owed[:min(len(owed), 20)]))
	next, stop := seriesWake(ctx)
	defer stop()
	stalls := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-next():
		}
		if !running() {
			return
		}
		now := m.readOwedSeries(ctx, ran, passed)
		if len(now) == 0 {
			return
		}
		if len(now) < len(owed) {
			stalls = 0
		} else {
			stalls++
		}
		owed = now
		if stalls >= seriesStallLimit {
			return
		}
	}
}

// readOwedSeries reads what the agent consumed since its last read into
// m.seriesRead and returns the series still owed by ran that hold the wait
// (seriesDue.holdsWait, by the tests that passed): none when the read failed,
// which ends the wait as none owed does.
func (m *mockService) readOwedSeries(ctx context.Context, ran, passed map[string]bool) []string {
	read, err := m.instrumentation.GetConsumedMocks(ctx)
	if err != nil {
		m.logger.Debug("keploy could not read the recordings consumed while it waited for the series the run owes; the run's end reads them again", zap.Error(err))
		return nil
	}
	m.seriesRead = append(m.seriesRead, read...)
	return slices.DeleteFunc(owedSeries(m.series, ran, usedNames(m.seriesRead)), func(id string) bool {
		return !m.series[id].holdsWait(passed)
	})
}

// holdsWait is whether s, owed, holds the wait for the series the run owes,
// by the tests that passed: a background series always does, as it is due
// wherever the run reached its place; a trailing one only when the test it
// trails passed. One trailing a test that failed is still owed, and the
// run's end names it not made if it is, but waiting for it changes no
// verdict: its test fails whether the application makes it later or not.
// Held, a run whose test command died by its own timeout waited out the
// whole stall for the series of the tests it left failed.
func (s seriesDue) holdsWait(passed map[string]bool) bool {
	return s.Trails == "" || passed[s.Trails]
}

// withSeriesRead is consumed, the run's end's read, after what the wait for
// the owed series read before it (m.seriesRead): the agent handed those to
// the wait, and no later read.
func (m *mockService) withSeriesRead(consumed []models.MockState) []models.MockState {
	if len(m.seriesRead) == 0 {
		return consumed
	}
	return append(append([]models.MockState(nil), m.seriesRead...), consumed...)
}

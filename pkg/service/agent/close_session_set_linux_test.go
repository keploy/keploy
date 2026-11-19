//go:build linux

package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// inSet is ctx naming set of keploy test's run run (models.TestSetSession).
func inSet(run, set string) context.Context {
	return models.WithTestSetSession(context.Background(), models.TestSetSession{Run: run, Set: set})
}

// keploy test begins a session at each test set, naming the set and its run
// (models.TestSetSession). With the application kept across sets, a loss of
// an earlier session of the same run -- counted after that session's reset,
// on a connection it used (late), or counted in it and never read by it
// (left) -- is a verdict of that set's that came after its end: the next
// owned take returns it, once, as that set's (EarlierSession, Set), naming
// its test where it was counted in one, and the agent's log still says it.
// The control: the open session's own loss is returned as before, naming no
// set.
func TestAnEarlierSetsLossIsReturnedAsThatSets(t *testing.T) {
	for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
		for _, c := range []struct {
			name, why string
			loss      coreAgent.OwnedLoss
			want      string
		}{
			{"late, a cut-short end's", lateLoss, coreAgent.OwnedLoss{Test: "test-2"}, "test-2"},
			{"late, a close of a connection the set used", lateLoss, coreAgent.OwnedLoss{Open: true}, ""},
			{"left unread, a cut-short end's", leftLoss, coreAgent.OwnedLoss{Test: "test-2"}, "test-2"},
		} {
			t.Run(string(kind)+"/"+c.name, func(t *testing.T) {
				a, sw := switchAgent(t, models.MODE_TEST)
				logs := observe(a)
				ctx := context.Background()
				require.NoError(t, a.resetSession(inSet("run-a", "test-set-0"))) // session 1
				o := c.loss
				o.Loss = models.CaptureLoss{Kind: kind, Count: 1, What: "from 127.0.0.1:62766"}
				if c.why == leftLoss {
					sw.put(o) // counted in session 1, never read by it
				}
				require.NoError(t, a.resetSession(inSet("run-a", "test-set-1"))) // session 2
				if c.why == lateLoss {
					sw.putFrom(1, o) // counted in session 2, on a connection session 1 used
				}
				own := models.CaptureLoss{Kind: kind, Count: 1, What: "from 127.0.0.1:62767"}
				sw.put(coreAgent.OwnedLoss{Test: "test-1", Loss: own})
				got, err := a.TakeOwnedCaptureLosses(ctx)
				require.NoError(t, err)
				require.ElementsMatch(t, []models.OwnedCaptureLoss{
					{Test: "test-1", Loss: own},
					{Test: c.want, Loss: o.Loss, EarlierSession: true, Set: "test-set-0"},
				}, got, "the set before's loss is returned as that set's, and this set's own as before")
				got, err = a.TakeOwnedCaptureLosses(ctx)
				require.NoError(t, err)
				require.Empty(t, got, "each is taken once")
				require.Len(t, earlierLogged(logs, c.why), 1, "the agent's log says it once")
			})
		}
	}
}

// A loss of an earlier session is returned as a set's only within one keploy
// test run's sessions: one of an earlier run's session (an agent kept across
// runs, whose sets may have the same names), or of a session that named no
// set (a record, keploy mock, an older keploy test, whose take would apply
// it by test name to the open set), is only logged, as before, and taken by
// no later read.
func TestAnEarlierSessionsLossOfNoSetOfThisRunIsOnlyLogged(t *testing.T) {
	for _, c := range []struct {
		name          string
		first, second context.Context
	}{
		{"an earlier run's set", inSet("run-a", "test-set-0"), inSet("run-b", "test-set-0")},
		{"sessions naming no set", context.Background(), context.Background()},
		{"a set after a session naming none", context.Background(), inSet("run-a", "test-set-0")},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, sw := switchAgent(t, models.MODE_TEST)
			logs := observe(a)
			ctx := context.Background()
			require.NoError(t, a.resetSession(c.first)) // session 1
			require.NoError(t, a.resetSession(c.second))
			lost := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: "from 127.0.0.1:62766"}
			sw.putFrom(1, coreAgent.OwnedLoss{Test: "test-2", Loss: lost})
			got, err := a.TakeOwnedCaptureLosses(ctx)
			require.NoError(t, err)
			require.Empty(t, got, "no take of this session's reports it")
			require.NoError(t, a.resetSession(c.second))
			got, err = a.TakeOwnedCaptureLosses(ctx)
			require.NoError(t, err)
			require.Empty(t, got, "nor a later one's")
			require.Len(t, earlierLogged(logs, lateLoss), 1, "the agent's log says it once")
		})
	}
}

// An earlier set's losses the takes leave are bounded: past
// coreAgent.MaxOwnedLosses, one entry per set and kind, naming no test,
// counts the rest. A loss of a session whose set's name the agent no longer
// holds (models.MaxSessionSets sessions back) is still returned as an
// earlier set's, naming none: keploy test names it an earlier test set.
func TestAnEarlierSetsLossesAreBounded(t *testing.T) {
	t.Run("past the bound", func(t *testing.T) {
		a, sw := switchAgent(t, models.MODE_TEST)
		ctx := context.Background()
		require.NoError(t, a.resetSession(inSet("run-a", "test-set-0"))) // session 1
		require.NoError(t, a.resetSession(inSet("run-a", "test-set-1"))) // session 2
		require.NoError(t, a.resetSession(inSet("run-a", "test-set-2"))) // session 3
		lost := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: "from 127.0.0.1:62766"}
		untold := models.CaptureLoss{Kind: models.CaptureLossAppBytesUntold, Count: 1, What: "from 127.0.0.1:62766"}
		for i := 0; i < coreAgent.MaxOwnedLosses; i++ {
			sw.putFrom(1, coreAgent.OwnedLoss{Test: fmt.Sprintf("test-%d", i), Loss: lost})
		}
		for _, o := range []struct {
			session uint64
			loss    models.CaptureLoss
		}{{1, lost}, {1, lost}, {1, untold}, {2, lost}} {
			sw.putFrom(o.session, coreAgent.OwnedLoss{Test: "test-x", Loss: o.loss})
		}
		got, err := a.TakeOwnedCaptureLosses(ctx)
		require.NoError(t, err)
		require.Len(t, got, coreAgent.MaxOwnedLosses+3)
		require.Equal(t, models.OwnedCaptureLoss{Test: "test-0", Loss: lost, EarlierSession: true, Set: "test-set-0"}, got[0])
		past := func(set string, kind models.CaptureLossKind, n uint64) models.OwnedCaptureLoss {
			return models.OwnedCaptureLoss{EarlierSession: true, Set: set,
				Loss: models.CaptureLoss{Kind: kind, Count: n, What: coreAgent.OwnedLossesPastTheBound}}
		}
		require.Equal(t, []models.OwnedCaptureLoss{
			past("test-set-0", models.CaptureLossAppBytesLost, 2),
			past("test-set-0", models.CaptureLossAppBytesUntold, 1),
			past("test-set-1", models.CaptureLossAppBytesLost, 1),
		}, got[coreAgent.MaxOwnedLosses:], "one entry per set and kind counts the rest")
	})
	t.Run("a set whose name is no longer held", func(t *testing.T) {
		a, sw := switchAgent(t, models.MODE_TEST)
		ctx := context.Background()
		require.NoError(t, a.resetSession(inSet("run-a", "test-set-0"))) // session 1
		a.provenMu.Lock()
		a.session = models.MaxSessionSets
		a.provenMu.Unlock()
		require.NoError(t, a.resetSession(inSet("run-a", "test-set-1"))) // session MaxSessionSets+1
		lost := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: "from 127.0.0.1:62766"}
		sw.putFrom(1, coreAgent.OwnedLoss{Test: "test-2", Loss: lost})
		got, err := a.TakeOwnedCaptureLosses(ctx)
		require.NoError(t, err)
		require.Equal(t, []models.OwnedCaptureLoss{{Test: "test-2", Loss: lost, EarlierSession: true}}, got,
			"still an earlier set's, naming none")
	})
}

//go:build linux

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// observe gives a its logger's lines from here on.
func observe(a *Agent) *observer.ObservedLogs {
	core, logs := observer.New(zap.DebugLevel)
	a.logger = zap.New(core)
	return logs
}

// leftLogged is each line resetSession logged for a loss left unread, by its
// fields, with its level as "_level".
func leftLogged(logs *observer.ObservedLogs) []map[string]any {
	return earlierLogged(logs, leftLoss)
}

// earlierLogged is each line logged for an earlier session's loss that
// reached none of its verdicts for why (logEarlierSessionsLoss), by its
// fields, with its level as "_level".
func earlierLogged(logs *observer.ObservedLogs, why string) []map[string]any {
	var out []map[string]any
	for _, e := range logs.All() {
		if m := e.ContextMap(); m["why"] == why {
			m["_level"] = e.Level
			out = append(out, m)
		}
	}
	return out
}

// A loss a session counted and never read (its command gone before its last
// read, a test still open) charges no later session: the next session's
// uncharged read (what a judged mock record fails on) and its own count
// (what mock replay judges) both leave it out, and the reset logs it once,
// with its kind, its connection, the session it was counted in, and the
// test it was counted in: the test open at its epoch, none for a close in
// the gap before the test still open began. The control: the same close in
// the next session's own gap is that session's run's.
func TestAnEarlierSessionsUnreadLossIsNotTheNextRecords(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			for _, where := range []string{"", "o::TestOne", "gap"} {
				earlier := where != ""
				name := "control: this session's own gap close"
				switch where {
				case "o::TestOne":
					name = "an earlier session's unread close in its open test"
				case "gap":
					name = "an earlier session's unread close in a gap, a test open after it"
				}
				t.Run(string(mode)+"/"+string(kind)+"/"+name, func(t *testing.T) {
					ctx := context.Background()
					a, sw := switchAgent(t, mode)
					logs := observe(a)
					// The agent's first session begins.
					require.NoError(t, a.resetSession(ctx))
					if where == "o::TestOne" {
						claimSelf(t, a)
						_, serr := a.ProvenBegin(ctx, begin("TestOne"))
						require.Nil(t, serr)
						sw.closeOf(0, kind) // stamped with the open TestOne's epoch; no last read follows
					}
					if where == "gap" {
						claimSelf(t, a)
						endPassed(t, a, "TestOne")
						sw.closeOf(0, kind) // stamped with the gap's epoch
						_, serr := a.ProvenBegin(ctx, begin("TestTwo"))
						require.Nil(t, serr) // open at the reset; no last read follows
					}
					require.NoError(t, a.resetSession(ctx))
					left := leftLogged(logs)
					if earlier {
						require.Len(t, left, 1, "the reset logs the earlier session's loss once")
						require.Equal(t, zap.WarnLevel, left[0]["_level"], "at warning level")
						require.NotEmpty(t, left[0]["next_step"])
						require.Equal(t, string(kind), left[0]["kind"])
						require.EqualValues(t, 1, left[0]["count"])
						require.Contains(t, left[0]["what"], "62999", "the line names the connection")
						require.EqualValues(t, 1, left[0]["session"], "and the session: the agent's first")
						if where == "gap" {
							require.NotContains(t, left[0], "test", "a close in a gap was counted in no test, not in the test open after it")
						} else {
							require.Equal(t, where, left[0]["test"], "and the test it was counted in")
						}
					} else {
						require.Empty(t, left, "a session that left nothing logs nothing")
					}
					base, err := a.CaptureLosses(ctx)
					require.NoError(t, err)
					claimSelf(t, a)
					endPassed(t, a, "TestOne")
					if !earlier {
						sw.closeOf(0, kind) // this session's gap
					}
					endPassed(t, a, "TestTwo")
					for _, id := range []string{"o::TestOne", "o::TestTwo"} {
						j, nj := kindOf(windowOf(t, a, id), kind)
						require.Zero(t, j+nj, "%s never used the connection", id)
					}
					run, err := a.UnchargedCaptureLosses(ctx)
					require.NoError(t, err)
					now, err := a.CaptureLosses(ctx)
					require.NoError(t, err)
					j, nj := lossesIn(now.Since(base), kind)
					if earlier {
						require.Zero(t, kindCount(run, kind), "an earlier session's loss is not this session's run's, which a judged mock record fails on: %+v", run)
						require.Zero(t, j+nj, "nor in this session's own count")
					} else {
						require.EqualValues(t, 1, kindCount(run, kind))
						require.EqualValues(t, 1, j+nj)
					}
					// The next reset logs nothing of it again.
					require.NoError(t, a.resetSession(ctx))
					require.Len(t, leftLogged(logs), len(left), "logged once")
				})
			}
		}
	}
}

// A loss a session's cut-short end owned, or one kept for its worker's test
// still open, that no read of it took is logged at the next reset and
// charges nothing of the next session: keploy test's owned take, at the next
// set's test case ends and its set's end, reads none of it, and the next
// session's uncharged read none. Within its own session the take reads it.
func TestKeployTestsOwnedTakeReadsOnlyItsOwnSessions(t *testing.T) {
	t.Run("a cut-short end's", func(t *testing.T) {
		a, p := wireAgent(t, models.MODE_TEST)
		var owned []coreAgent.OwnedLoss
		a.Proxy = ownedLossProxy{lossProxy: lossProxy{wireProxy: p}, owned: &owned}
		logs := observe(a)
		ctx := context.Background()
		lost := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1,
			What: "from [::ffff:127.0.0.1]:50912, written in test-1, whose end could not wait for them"}
		owned = []coreAgent.OwnedLoss{{Test: "test-1", Loss: lost}}
		got, err := a.TakeOwnedCaptureLosses(ctx)
		require.NoError(t, err)
		require.Equal(t, []models.OwnedCaptureLoss{{Test: "test-1", Loss: lost}}, got, "its own session's take reads it")

		owned = []coreAgent.OwnedLoss{{Test: "test-2", Loss: lost}}
		require.NoError(t, a.resetSession(ctx))
		got, err = a.TakeOwnedCaptureLosses(ctx)
		require.NoError(t, err)
		require.Empty(t, got, "the next session's take reads none of the session before's")
		un, err := a.UnchargedCaptureLosses(ctx)
		require.NoError(t, err)
		require.Zero(t, kindCount(un, models.CaptureLossAppBytesLost), "nor its run: %+v", un)
		left := leftLogged(logs)
		require.Len(t, left, 1)
		require.Equal(t, "test-2", left[0]["test"], "the line names the test whose end owned it")
		require.Equal(t, string(models.CaptureLossAppBytesLost), left[0]["kind"])
	})
	t.Run("kept for a worker's test still open", func(t *testing.T) {
		a, src, inst1, _, endTwo := keptAgent(t)
		logs := observe(a)
		ctx := context.Background()
		src.put(coreAgent.OwnedLoss{Worker: inst1, Open: true, Loss: untoldFrom("62766")})
		endTwo() // kept for TestOne, still open
		require.NoError(t, a.resetSession(ctx))
		left := leftLogged(logs)
		require.Len(t, left, 1, "the reset logs the loss kept for the earlier session's open test")
		require.Equal(t, "a::TestOne", left[0]["test"])
		require.Equal(t, string(models.CaptureLossAppBytesUntold), left[0]["kind"])
		require.Contains(t, left[0]["what"], "62766")
		claimSelf(t, a)
		endPassed(t, a, "TestOne")
		j, nj := kindOf(windowOf(t, a, "o::TestOne"), models.CaptureLossAppBytesUntold)
		require.Zero(t, j+nj, "no test of the next session is charged with it")
		un, err := a.UnchargedCaptureLosses(ctx)
		require.NoError(t, err)
		require.Zero(t, kindCount(un, models.CaptureLossAppBytesUntold), "nor its run: %+v", un)
	})
}

// The run's own count of a kind is named by its newest loss, of this
// session: a long-lived agent whose earlier session closed a connection in
// its gap names this session's only loss, a counted close, by its own
// connection; within a session, a counted close after a loss made the run's
// names the count, and a loss made the run's after a counted close names it.
func TestTheRunsOwnCountIsNamedByItsNewestLoss(t *testing.T) {
	const old, counted, gap, tail = "from 10.0.0.1:1111", "from 10.0.0.2:2222", "from 10.0.0.3:3333", "from 10.0.0.4:4444"
	const gap2 = "from 10.0.0.5:5555"
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			for _, c := range []struct {
				name    string
				earlier bool
				order   []string // this session's losses, oldest first
				count   uint64
				want    string
			}{
				{"control: no earlier session", false, []string{counted}, 1, counted},
				{"after an earlier session's gap close", true, []string{counted}, 1, counted},
				{"a counted close after the gap's", true, []string{gap, counted}, 2, counted},
				{"the gap's after a counted close", true, []string{counted, gap}, 2, gap},
				{"a second gap close after a counted close after the first", true, []string{gap, counted, gap2}, 3, gap2},
				{"a close after the last test, taken by the last read, after a counted close", true, []string{counted, tail}, 2, tail},
			} {
				t.Run(string(mode)+"/"+string(kind)+"/"+c.name, func(t *testing.T) {
					ctx := context.Background()
					a, sw := switchAgent(t, mode)
					countClose := func(what string) {
						// chargeOpen's p.losses.Add: a close with the
						// one-process accounting off.
						sw.cmu.Lock()
						sw.counted = sw.counted.Plus(models.CaptureLosses{{Kind: kind, Count: 1, What: what}})
						sw.cmu.Unlock()
					}
					if c.earlier {
						claimSelf(t, a)
						endPassed(t, a, "TestOne")
						sw.put(coreAgent.OwnedLoss{Open: true, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: old}})
						endPassed(t, a, "TestTwo")
						_, err := a.UnchargedCaptureLosses(ctx)
						require.NoError(t, err)
						require.NoError(t, a.resetSession(ctx))
					}
					base, err := a.CaptureLosses(ctx)
					require.NoError(t, err)
					claimSelf(t, a)
					endPassed(t, a, "TestOne")
					for _, what := range c.order {
						if what == counted {
							countClose(what)
							continue
						}
						if what == tail {
							// After the last test: the last read takes it.
							sw.put(coreAgent.OwnedLoss{Open: true, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: what}})
							continue
						}
						// The process's connection closes in its gap; the
						// next end makes it the run's.
						sw.put(coreAgent.OwnedLoss{Open: true, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: what}})
						endPassed(t, a, "TestGap")
					}
					_, err = a.UnchargedCaptureLosses(ctx)
					require.NoError(t, err)
					now, err := a.CaptureLosses(ctx)
					require.NoError(t, err)
					var what string
					var n uint64
					for _, l := range now.Since(base) {
						if l.Kind == kind {
							what, n = l.What, l.Count
						}
					}
					require.Equal(t, c.count, n, "this session's losses")
					require.Equal(t, c.want, what, "the count is named by this session's newest loss")
				})
			}
		}
	}
}

// A loss made the run's that names nothing leaves a count's words as they
// were: of a kind no count holds, it gives no words of an earlier session's
// loss of that kind, whose words the run's own count still keeps.
func TestARunsLossThatNamesNothingTakesNoEarlierSessionsWords(t *testing.T) {
	for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			a, sw := switchAgent(t, models.MODE_RECORD)
			claimSelf(t, a)
			endPassed(t, a, "TestOne")
			sw.put(coreAgent.OwnedLoss{Open: true, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: "from 10.0.0.1:1111"}})
			endPassed(t, a, "TestTwo")
			_, err := a.UnchargedCaptureLosses(ctx)
			require.NoError(t, err)
			require.NoError(t, a.resetSession(ctx))
			a.provenMu.Lock()
			a.runsLocked(models.CaptureLosses{{Kind: kind, Count: 1}}, nil, true)
			a.provenMu.Unlock()
			now, err := a.CaptureLosses(ctx)
			require.NoError(t, err)
			for _, l := range now {
				if l.Kind == kind {
					require.EqualValues(t, 2, l.Count)
					require.Empty(t, l.What, "no words of the earlier session's loss")
					return
				}
			}
			t.Fatalf("no count of %s: %+v", kind, now)
		})
	}
}

// Each read a session's tests are charged from -- the session's mark, a
// claim's, an overlapping begin's -- reads the counts alone, never the run's
// own count: on an agent whose earlier session made a loss the run's (a
// close in its gap) and counted one (a close after its last read), the next
// session's clean tests are charged nothing, and its uncharged read holds
// nothing. A run of one test process, and a run of workers whose tests
// overlap.
func TestTheReadsATestIsChargedFromLeaveTheRunsOwnOut(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			for _, workers := range []bool{false, true} {
				name := "one test process"
				if workers {
					name = "overlapping workers"
				}
				t.Run(string(mode)+"/"+string(kind)+"/"+name, func(t *testing.T) {
					ctx := context.Background()
					a, sw := switchAgent(t, mode)
					claimSelf(t, a)
					endPassed(t, a, "TestOne")
					sw.closeOf(0, kind) // in the gap: the run's own
					endPassed(t, a, "TestTwo")
					_, err := a.UnchargedCaptureLosses(ctx)
					require.NoError(t, err)
					sw.closeOf(0, kind) // after the last read: counted
					require.NoError(t, a.resetSession(ctx))
					a.provenMu.Lock()
					own := kindCount(a.runsOwn, kind)
					a.provenMu.Unlock()
					counts, err := a.captureCounts(ctx)
					require.NoError(t, err)
					require.EqualValues(t, 1, own, "premise: the run's own count holds the earlier gap's close")
					require.EqualValues(t, 1, kindCount(counts, kind), "premise: the counts hold the close after the last read")

					var ids []string
					if workers {
						w1, w2 := child(t), child(t)
						claimWorker(t, a, w1.Process.Pid, "a", 2)
						claimWorker(t, a, w2.Process.Pid, "b", 2)
						_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
						require.Nil(t, serr)
						_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
						require.Nil(t, serr)
						_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
						require.Nil(t, serr)
						_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
						require.Nil(t, serr)
						ids = []string{"a::TestOne", "b::TestTwo"}
					} else {
						claimSelf(t, a)
						endPassed(t, a, "TestOne")
						ids = []string{"o::TestOne"}
					}
					for _, id := range ids {
						j, nj := kindOf(windowOf(t, a, id), kind)
						require.Zero(t, j+nj, "%s is clean: %+v", id, windowOf(t, a, id).Evidence)
					}
					run, err := a.UnchargedCaptureLosses(ctx)
					require.NoError(t, err)
					require.Zero(t, kindCount(run, kind), "nothing is the next session's run's: %+v", run)
				})
			}
		}
	}
}

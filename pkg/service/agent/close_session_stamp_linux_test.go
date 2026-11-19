//go:build linux

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// failNextCountRead makes a's next capture-loss count read fail.
func failNextCountRead(a *Agent) { a.Hooks.(*instanceHooks).failReads.Store(1) }

// The run's last read keeps what it took when its count read fails: a close
// counted in the process's gap after its last test, taken by that read, is
// in the next read of the run's own count since its base (what mock replay
// judges, and mock record falls back to), never dropped with the read.
func TestTheLastReadKeepsWhatItTookWhenItsCountReadFails(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			t.Run(string(mode)+"/"+string(kind), func(t *testing.T) {
				ctx := context.Background()
				a, sw := switchAgent(t, mode)
				base, err := a.CaptureLosses(ctx)
				require.NoError(t, err)
				claimSelf(t, a)
				endPassed(t, a, "TestOne")
				sw.closeOf(0, kind) // the process's gap after its last test
				failNextCountRead(a)
				_, err = a.UnchargedCaptureLosses(ctx)
				require.Error(t, err, "the last read's count read failed")
				now, err := a.CaptureLosses(ctx)
				require.NoError(t, err)
				j, nj := lossesIn(now.Since(base), kind)
				require.EqualValues(t, 1, j+nj, "the close the failed read took is the run's: %+v", now.Since(base))
			})
		}
	}
}

// A read that failed notes nothing of the run's count: the note an earlier
// read of the session made stands, so the run's count of a kind, which an
// earlier session's counted close named, is named by this session's newest
// loss made the run's, also when the read that made it the run's failed.
func TestAReadThatFailedKeepsTheRunsCountsNote(t *testing.T) {
	const old, first, second = "from 10.0.0.1:1111", "from 10.0.0.3:3333", "from 10.0.0.5:5555"
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			t.Run(string(mode)+"/"+string(kind), func(t *testing.T) {
				ctx := context.Background()
				a, sw := switchAgent(t, mode)
				// An earlier session's close, counted with the one-process
				// accounting off.
				sw.cmu.Lock()
				sw.counted = models.CaptureLosses{{Kind: kind, Count: 1, What: old}}
				sw.cmu.Unlock()
				require.NoError(t, a.resetSession(ctx))
				claimSelf(t, a)
				endPassed(t, a, "TestOne")
				sw.put(coreAgent.OwnedLoss{Open: true, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: first}})
				endPassed(t, a, "TestGap") // makes it the run's, noted with the count read then
				sw.put(coreAgent.OwnedLoss{Open: true, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: second}})
				failNextCountRead(a)
				_, err := a.UnchargedCaptureLosses(ctx)
				require.Error(t, err)
				now, err := a.CaptureLosses(ctx)
				require.NoError(t, err)
				var what string
				for _, l := range now {
					if l.Kind == kind {
						what = l.What
					}
				}
				require.Equal(t, second, what, "named by this session's newest loss, not the earlier session's close")
			})
		}
	}
}

// A loss the proxy stamps with an earlier session, counted after that
// session's reset, charges no run or test of a later one: a close of an
// ended worker's connection, or of one the session before used, is in
// neither the run's uncharged read nor its own count; a loss a cut-short end
// of the session before owned is never charged to this session's test of the
// same name. Each is logged once, at warning level, with its session. The
// control: the same loss stamped with the session open now is charged.
func TestAnEarlierSessionsLossCountedAfterItsResetChargesNoLaterSession(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			for _, c := range []struct {
				name string
				loss coreAgent.OwnedLoss
			}{
				{"an ended worker's close", coreAgent.OwnedLoss{Worker: 7, Open: true}},
				{"a close of a connection the session before used", coreAgent.OwnedLoss{Open: true}},
				{"a cut-short end's of a test of the same name", coreAgent.OwnedLoss{Test: "o::TestOne"}},
			} {
				for _, earlier := range []bool{true, false} {
					name := string(mode) + "/" + string(kind) + "/" + c.name
					if !earlier {
						name += "/control: this session's"
					}
					t.Run(name, func(t *testing.T) {
						ctx := context.Background()
						a, sw := switchAgent(t, mode)
						logs := observe(a)
						require.NoError(t, a.resetSession(ctx)) // session 1 begins
						base, err := a.CaptureLosses(ctx)
						require.NoError(t, err)
						claimSelf(t, a)
						_, serr := a.ProvenBegin(ctx, begin("TestOne"))
						require.Nil(t, serr)
						o := c.loss
						o.Loss = models.CaptureLoss{Kind: kind, Count: 1, What: "from 127.0.0.1:62766"}
						session := uint64(1)
						if earlier {
							session = 0
						}
						sw.putFrom(session, o)
						_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestOne", Outcome: "passed"})
						require.Nil(t, serr)
						j, nj := kindOf(windowOf(t, a, "o::TestOne"), kind)
						run, err := a.UnchargedCaptureLosses(ctx)
						require.NoError(t, err)
						now, err := a.CaptureLosses(ctx)
						require.NoError(t, err)
						rj, rnj := lossesIn(now.Since(base), kind)
						late := earlierLogged(logs, lateLoss)
						if earlier {
							require.Zero(t, j+nj, "TestOne of this session is not charged")
							require.Zero(t, kindCount(run, kind), "nor its run's uncharged read: %+v", run)
							require.Zero(t, rj+rnj, "nor its own count")
							require.Len(t, late, 1, "logged once")
							require.Equal(t, zap.WarnLevel, late[0]["_level"])
							require.EqualValues(t, 0, late[0]["session"], "naming the session it was made in")
							require.NotEmpty(t, late[0]["next_step"])
							return
						}
						require.Empty(t, late)
						if c.loss.Test != "" || c.loss.Worker == 0 {
							require.EqualValues(t, 1, j+nj, "this session's TestOne is charged its own")
						} else {
							require.EqualValues(t, 1, rj+rnj, "a close of no open test's is this session's run's")
						}
					})
				}
			}
		}
	}
}

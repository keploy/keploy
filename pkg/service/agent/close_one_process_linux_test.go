//go:build linux

package agent

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// oneProcessAgent is a record or a replay that a run of one test process
// claimed (claimSelf), whose proxy reports owned losses (src) and keeps
// worker 0's test epoch, as the proxy does once that claim tells it.
func oneProcessAgent(t *testing.T, mode models.Mode) (*Agent, *ownedSource) {
	t.Helper()
	a, wp := wireAgent(t, mode)
	src := &ownedSource{}
	a.Proxy = &ownedWire{wireProxy: wp, ownedSource: src}
	a.proven.losses = a.markCaptureLosses(context.Background())
	claimSelf(t, a)
	return a, src
}

// oneProcessClose is a close of the one test process's connection the
// proxy counted (chargeOpen): worker 0's open test's, stamped with its epoch.
func oneProcessClose(kind models.CaptureLossKind) coreAgent.OwnedLoss {
	return coreAgent.OwnedLoss{Open: true, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: "from 127.0.0.1:62766"}}
}

// A close of a connection of the one test process of a run (worker 0)
// counted in the gap between its tests, after one test's end and before the
// next one's begin, is the run's: reported, charged to no test. The next
// test did not exist when it was counted and never used the connection, so
// it is untouched; the same close counted once the next test is open is that
// test's. The epoch rule of a run of workers, for a run of one process: in a
// record and a replay, for a close keploy cannot account for and for bytes
// keploy saw lost.
func TestACloseInTheOneProcessGapSparesItsNextTest(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			for _, inGap := range []bool{true, false} {
				where := "counted in the gap"
				if !inGap {
					where = "counted in the next test"
				}
				t.Run(string(mode)+"/"+string(kind)+"/"+where, func(t *testing.T) {
					a, src := oneProcessAgent(t, mode)
					ctx := context.Background()
					endPassed(t, a, "TestOne")
					if inGap {
						// The process has no test open: its connection closes now.
						src.put(oneProcessClose(kind))
					}
					_, serr := a.ProvenBegin(ctx, begin("TestTwo"))
					require.Nil(t, serr)
					if !inGap {
						src.put(oneProcessClose(kind))
					}
					_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
					require.Nil(t, serr)
					j, nj := kindOf(windowOf(t, a, "o::TestOne"), kind)
					require.Zero(t, j+nj, "TestOne, ended before the close, is untouched")
					two := windowOf(t, a, "o::TestTwo")
					j, nj = kindOf(two, kind)
					run, err := a.UnchargedCaptureLosses(ctx)
					require.NoError(t, err)
					if inGap {
						if j != 0 || nj != 0 {
							t.Errorf("TestTwo, which began after the close was counted and never used the connection, is charged %d judged and %d unjudged %s; its outcome %s", j, nj, kind, two.Outcome)
						}
						require.NotEqual(t, models.ScopeOutcomeFailed, two.Outcome)
						require.EqualValues(t, 1, kindCount(run, kind), "the close counted while the process had no test open is the run's: %+v", run)
						return
					}
					if kind == models.CaptureLossAppBytesUntold {
						require.EqualValues(t, 1, nj, "TestTwo, open at the close, is not judged")
						require.Zero(t, j)
					} else {
						require.EqualValues(t, 1, j, "TestTwo, open at the close, is charged the loss")
						if mode == models.MODE_TEST {
							require.Equal(t, models.ScopeOutcomeFailed, two.Outcome, "a loss keploy saw fails TestTwo in a replay")
						}
					}
					require.Zero(t, kindCount(run, kind), "TestTwo's close is not the run's too: %+v", run)
				})
			}
		}
	}
}

// The claim of a run of one test process tells the proxy so before it reads
// the counts the process's first test counts from: a close counted before
// that read is in it, and one after is stamped, at an epoch the claim moved,
// which no test began at, so a close counted between the claim and the first
// begin is the run's, never the first test's. A session's reset tells the
// proxy no such run holds it any more.
func TestTheOneProcessClaimTellsTheProxyBeforeItReadsTheCounts(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		t.Run(string(mode), func(t *testing.T) {
			a, wp := wireAgent(t, mode)
			src := &ownedSource{}
			a.Proxy = &ownedWire{wireProxy: wp, ownedSource: src}
			a.proven.losses = a.markCaptureLosses(context.Background())
			src.toldOf()
			claimSelf(t, a)
			told := src.toldOf()
			on := slices.Index(told, "on")
			require.GreaterOrEqual(t, on, 0, "the claim tells the proxy a run of one process holds the session: %v", told)
			require.Greater(t, slices.Index(told[on:], "read"), 0, "the claim reads the counts its first test counts from after it tells the proxy: %v", told)
			require.NotContains(t, told, "off", "the claim turns nothing off: %v", told)

			src.put(oneProcessClose(models.CaptureLossAppBytesUntold))
			endPassed(t, a, "TestOne")
			j, nj := kindOf(windowOf(t, a, "o::TestOne"), models.CaptureLossAppBytesUntold)
			require.Zero(t, j+nj, "TestOne, begun after the close was counted, is untouched")
			run, err := a.UnchargedCaptureLosses(context.Background())
			require.NoError(t, err)
			require.EqualValues(t, 1, kindCount(run, models.CaptureLossAppBytesUntold), "the close counted before the first begin is the run's: %+v", run)

			src.toldOf()
			require.NoError(t, a.resetSession(context.Background()))
			require.Contains(t, src.toldOf(), "off", "a session's reset tells the proxy no run of one process holds it")
		})
	}
}

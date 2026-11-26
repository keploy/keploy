//go:build linux

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.keploy.io/server/v3/pkg/models"
)

// A close of the proxy's that keploy cannot account for (app_bytes_untold) is
// charged by the proxy only to the tests the connection could have served
// (the proxy's rows say which). Charged to a test, it leaves it NOT JUDGED,
// never failed, and a test keploy saw miss a call stays FAILED on that miss,
// in either order of the miss test and the close:
//   - the close while the miss test is open, charged to it: it stays failed
//     with its miss as its evidence, not capture_incomplete, and the close
//     only named in its reason; the next test passes;
//   - the close after the miss test ended, charged to the next test (one
//     that used the connection): that test is not judged (cannot capture),
//     still passed and not failed by keploy; the miss test stays failed.
func TestAnUntoldCloseBesideASeenMiss(t *testing.T) {
	untold := models.CaptureLosses{{Kind: models.CaptureLossAppBytesUntold, Count: 1, What: "from [::ffff:127.0.0.1]:62766"}}
	miss := []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /reserve/book", Destination: "127.0.0.1:20218"}}
	for _, during := range []bool{true, false} {
		name := "close during the miss test"
		if !during {
			name = "close after the miss test, charged to the next"
		}
		t.Run(name, func(t *testing.T) {
			a, wp := wireAgent(t, models.MODE_TEST)
			lp := lossProxy{wireProxy: wp}
			a.Proxy = lp
			a.proven.losses = a.markCaptureLosses(context.Background())
			claimSelf(t, a)
			ctx := context.Background()
			_, serr := a.ProvenBegin(ctx, begin("TestStock"))
			require.Nil(t, serr)
			wp.mu.Lock()
			wp.misses = miss
			wp.mu.Unlock()
			if during {
				lp.losses = untold
				a.Proxy = lp
			}
			e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestStock", Outcome: "failed"})
			require.Nil(t, serr)
			require.Equal(t, "fail", e.Action, "the miss test must end failed")
			wp.mu.Lock()
			wp.misses = nil
			wp.mu.Unlock()
			if !during {
				lp.losses = untold
				a.Proxy = lp
			}
			e2 := endPassed(t, a, "TestClock")

			stock := windowOf(t, a, "o::TestStock")
			require.Equal(t, models.ScopeOutcomeFailed, stock.Outcome)
			require.NotNil(t, stock.Evidence)
			require.Len(t, stock.Evidence.Misses, 1, "the seen miss is the miss test's evidence")
			require.False(t, stock.Evidence.Unread, "a test keploy saw miss is not unread")
			require.Empty(t, stock.Evidence.CaptureIncomplete, "an untold close is no capture loss keploy saw: never capture_incomplete")
			require.Empty(t, stock.Evidence.CannotCapture, "a seen miss is not left without a verdict by an untold close")
			if during {
				require.Contains(t, stock.Evidence.Reason, "keploy also cannot tell whether bytes the application wrote to 1 connection(s) this test used")
				require.True(t, strings.HasSuffix(stock.Evidence.Reason, "the call keploy saw miss fails the test whatever they held."), "nothing is left to say after the untold close: %q", stock.Evidence.Reason)
				// Said on a line of its own: the quarantine command before it
				// keeps its words.
				require.Equal(t, [][]string{
					{"keploy", "mock", "record", "-c", "<your command, selecting o::TestStock>"},
					{"keploy", "mock", "quarantine", "o::TestStock", "--reason", "<why>"},
				}, commandsIn(t, stock.Evidence.Reason), stock.Evidence.Reason)
			} else {
				require.NotContains(t, stock.Evidence.Reason, "keploy also cannot tell")
			}

			clock := windowOf(t, a, "o::TestClock")
			require.Equal(t, models.ScopeOutcomePassed, clock.Outcome, "an untold close never fails a test")
			require.Empty(t, e2.Action, "TestClock's end must not answer it failed")
			if during {
				require.True(t, clock.Evidence == nil || clock.Evidence.Empty(), "TestClock is untouched by a close charged to the miss test: %+v", clock.Evidence)
				return
			}
			require.NotNil(t, clock.Evidence)
			require.Len(t, clock.Evidence.CannotCapture, 1, "the test the close is charged to is not judged")
			require.Equal(t, models.CaptureLossAppBytesUntold, clock.Evidence.CannotCapture[0].Kind)
			require.Empty(t, clock.Evidence.CaptureIncomplete, "never a judged loss")
			require.Empty(t, clock.Evidence.Misses)
			require.True(t, strings.HasPrefix(clock.Evidence.Reason, "keploy: not judged: keploy's proxy closed 1 connection(s) this test used"), clock.Evidence.Reason)
		})
	}
}

// Only the untold close is taken out of a seen-miss test's unjudged losses:
// another loss that leaves a test without a verdict stays as it was, the
// untold close is counted on its own, and a seen-miss test with no untold
// close gets no word of one.
func TestAnUntoldCloseBesideASeenMissLeavesItsOtherLossesAsTheyWere(t *testing.T) {
	serverFirst := models.CaptureLoss{Kind: models.CaptureLossServerFirst, Count: 1, What: "to 10.0.0.9:25"}
	untold := models.CaptureLoss{Kind: models.CaptureLossAppBytesUntold, Count: 1, What: "from [::ffff:127.0.0.1]:62766"}
	miss := []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /reserve/book", Destination: "127.0.0.1:20218"}}
	for _, tc := range []struct {
		name   string
		losses models.CaptureLosses
		untold bool
	}{
		{"another loss, no untold close", models.CaptureLosses{serverFirst}, false},
		{"another loss beside an untold close", models.CaptureLosses{serverFirst, untold}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, wp := wireAgent(t, models.MODE_TEST)
			lp := lossProxy{wireProxy: wp}
			a.Proxy = lp
			a.proven.losses = a.markCaptureLosses(context.Background())
			claimSelf(t, a)
			ctx := context.Background()
			_, serr := a.ProvenBegin(ctx, begin("TestStock"))
			require.Nil(t, serr)
			wp.mu.Lock()
			wp.misses = miss
			wp.mu.Unlock()
			lp.losses = tc.losses
			a.Proxy = lp
			_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestStock", Outcome: "failed"})
			require.Nil(t, serr)

			stock := windowOf(t, a, "o::TestStock")
			require.NotNil(t, stock.Evidence)
			require.Len(t, stock.Evidence.Misses, 1, "the seen miss is the test's evidence")
			require.Len(t, stock.Evidence.CannotCapture, 1, "%+v", stock.Evidence.CannotCapture)
			require.Equal(t, models.CaptureLossServerFirst, stock.Evidence.CannotCapture[0].Kind, "the other loss is kept, and the untold close taken out")
			if tc.untold {
				require.Contains(t, stock.Evidence.Reason, "keploy also cannot tell whether bytes the application wrote to 1 connection(s) this test used")
			} else {
				require.NotContains(t, stock.Evidence.Reason, "keploy also cannot tell whether bytes")
			}
		})
	}
}

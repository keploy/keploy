//go:build linux

package agent

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// The losses cut-short ends owned that each session's reset leaves unread
// for the next run are bounded while no read takes them (an older keploy on
// a long-lived agent), as the proxy bounds those it keeps: the oldest
// coreAgent.MaxOwnedLosses are kept, naming no test, and one entry of each
// kind, the run's, counts every one past them. None is dropped, and no kind
// is counted as another.
func TestTheOwnedLossesSessionsLeaveUnreadAreBounded(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	var owned []coreAgent.OwnedLoss
	a.Proxy = ownedLossProxy{lossProxy: lossProxy{wireProxy: p}, owned: &owned}
	ctx := context.Background()
	from := func(s, i int) string {
		return "from 127.0.0.1:" + strconv.Itoa(40000+100*s+i) + ", written in test-" + strconv.Itoa(i)
	}
	const sessions = 3
	var first []coreAgent.OwnedLoss
	want := map[models.CaptureLossKind]uint64{}
	for s := range sessions {
		// What a proxy at its own bound hands: its oldest, each naming a
		// test, then its entry past them, and one loss of another kind.
		owned = nil
		for i := range coreAgent.MaxOwnedLosses {
			owned = append(owned, coreAgent.OwnedLoss{Test: "test-" + strconv.Itoa(i),
				Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: from(s, i)}})
		}
		owned = append(owned,
			coreAgent.OwnedLoss{Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 3, What: coreAgent.OwnedLossesPastTheBound}},
			coreAgent.OwnedLoss{Test: "test-0", Loss: models.CaptureLoss{Kind: models.CaptureLossTLSAbort, Count: 2, What: from(s, 99)}})
		if s == 0 {
			first = append([]coreAgent.OwnedLoss(nil), owned[:coreAgent.MaxOwnedLosses]...)
		}
		for _, o := range owned {
			want[o.Loss.Kind] += o.Loss.Count
		}
		require.NoError(t, a.resetSession(ctx))
		a.provenMu.Lock()
		left := append([]coreAgent.OwnedLoss(nil), a.proven.ownedLeft...)
		a.provenMu.Unlock()
		require.Len(t, left, coreAgent.MaxOwnedLosses+2, "session %d: the oldest, then one entry past them of each kind", s)
	}

	got, err := a.TakeOwnedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Len(t, got, coreAgent.MaxOwnedLosses+2)
	sum := map[models.CaptureLossKind]uint64{}
	for i, o := range got {
		require.Empty(t, o.Test, "%d: an earlier session's names no test of this one", i)
		sum[o.Loss.Kind] += o.Loss.Count
		if i < coreAgent.MaxOwnedLosses {
			require.Equal(t, first[i].Loss, o.Loss, "%d: the oldest are kept in their words", i)
		}
	}
	require.Equal(t, want, sum, "every loss counted, each as its kind")
	require.Equal(t, models.OwnedCaptureLoss{Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost,
		Count: want[models.CaptureLossAppBytesLost] - coreAgent.MaxOwnedLosses, What: coreAgent.OwnedLossesPastTheBound}}, got[coreAgent.MaxOwnedLosses])
	require.Equal(t, models.OwnedCaptureLoss{Loss: models.CaptureLoss{Kind: models.CaptureLossTLSAbort,
		Count: want[models.CaptureLossTLSAbort], What: coreAgent.OwnedLossesPastTheBound}}, got[coreAgent.MaxOwnedLosses+1])

	got, err = a.TakeOwnedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, got, "each once")

	// One past the bound is counted past it too, in the words of the rest.
	owned = nil
	for i := range coreAgent.MaxOwnedLosses + 1 {
		owned = append(owned, coreAgent.OwnedLoss{Test: "test-" + strconv.Itoa(i),
			Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: from(8, i)}})
	}
	require.NoError(t, a.resetSession(ctx))
	got, err = a.TakeOwnedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Len(t, got, coreAgent.MaxOwnedLosses+1)
	require.Equal(t, models.OwnedCaptureLoss{Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost,
		Count: 1, What: coreAgent.OwnedLossesPastTheBound}}, got[coreAgent.MaxOwnedLosses])

	// Under the bound, a session's are kept as they were.
	loss := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: from(9, 1)}
	owned = []coreAgent.OwnedLoss{{Test: "test-1", Loss: loss}}
	require.NoError(t, a.resetSession(ctx))
	got, err = a.TakeOwnedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, []models.OwnedCaptureLoss{{Loss: loss}}, got)
}

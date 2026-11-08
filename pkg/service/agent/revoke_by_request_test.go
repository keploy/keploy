package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
)

// The mock stream tells the recording's manager whether its client finds a
// revoked test case sent with no name by its request time
// (SupportsRevokeByRequest), each time it opens, so a later client that does
// not is never sent such a name.
func TestGetOutgoingTellsTheManagerWhetherItsClientRevokesByRequestTime(t *testing.T) {
	useHooks(t, &clockHooks{})
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	t.Cleanup(func() {
		syncMock.Get().SetRevokeByRequest(false)
		syncMock.Get().SetRevokeCapable(false)
	})
	for _, want := range []bool{true, false} {
		_, err := a.GetOutgoing(context.Background(), models.OutgoingOptions{SupportsDroppedRevoke: true, SupportsRevokeByRequest: want})
		require.NoError(t, err)
		require.Equal(t, want, syncMock.Get().RevokesByRequest(), "the stream opened with SupportsRevokeByRequest %v", want)
	}
}

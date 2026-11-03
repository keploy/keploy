package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
)

// connectingProxy records as the proxy does: each connection it accepts sets
// the session's outgoing channel on the package's syncMock manager.
type connectingProxy struct{ workerPool }

func (p *connectingProxy) Record(_ context.Context, m chan<- *models.Mock, _ models.OutgoingOptions) error {
	syncMock.Get().SetOutputChannel(m)
	return nil
}

// The outgoing stream's end is its mocks' consumer's exit: once the stream's
// request is done, nothing reads its channel, and the proxy's stop, which
// waits for what is queued for that channel (CloseOutChan), ends at it, the
// queue given up. Here the out queue's clock is frozen, so its stall verdict
// never comes: only the consumer's exit, told before the proxy records, can
// end that wait.
func TestTheProxysStopEndsAtTheOutgoingStreamsEndWhileItsStallVerdictIsPending(t *testing.T) {
	sm := syncMock.Get()
	t.Cleanup(sm.SetOutStallForTest(&stall.ManualClock{}, time.Hour))
	a := newClockAgent(t)
	a.Proxy = &connectingProxy{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := a.GetOutgoing(ctx, models.OutgoingOptions{})
	require.NoError(t, err)
	// The channel full, and one more mock queued behind it, which only the
	// stream could take.
	for i := 0; i <= cap(m); i++ {
		sm.SendConfigMock(&models.Mock{Name: fmt.Sprint("m", i), Kind: models.HTTP})
	}
	require.True(t, sm.OutQueued(), "setup: no mock waits behind the full channel")
	drops := sm.DropCount()
	closed := make(chan struct{})
	go func() { sm.CloseOutChan(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("the proxy's stop ended while the stream still read and the queue's stall verdict could not come")
	case <-time.After(50 * time.Millisecond): // a look, not a bound: the verdict cannot come
	}
	cancel() // the stream's handler returned
	select {
	case <-closed:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the proxy's stop still waits on a stream that ended, its queue's stall verdict pending")
	}
	require.Equal(t, drops+1, sm.DropCount(), "the mock no stream took was not given up at the stream's end")
	require.False(t, sm.OutQueued(), "mocks still wait for a stream that ended")
}

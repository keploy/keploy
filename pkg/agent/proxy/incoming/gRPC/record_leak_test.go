package proxy

import (
	"context"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/internal/leakcheck"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A gRPC conversation that has ended leaves nothing of RecordIncoming behind,
// though the ingress's context it was given lives on: no goroutine parked on
// that context, and no registration on it. A goroutine was left per
// conversation, parked on the context and holding its closed connections,
// until the agent stopped; the close on shutdown registered instead must be
// stopped as the call returns, or it leaves a registration per conversation.
func TestRecordIncomingLeavesNothingBehindAnEndedConversation(t *testing.T) {
	ctx := leakcheck.NewContext(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	defer ctx.Cancel()
	tcCh := make(chan *models.TestCase, 8)
	const conversations = 20
	for i := 0; i < conversations; i++ {
		cliApp, cliProxy := net.Pipe()
		appProxy, appApp := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = RecordIncoming(ctx, zap.NewNop(), cliProxy, appProxy, tcCh, 50051, "", false, false)
		}()
		_ = cliApp.Close()
		_ = appApp.Close()
		<-done
	}
	if n, g := leakcheck.Parked("gRPC.RecordIncoming"); n != 0 {
		t.Errorf("%d goroutines of RecordIncoming are parked after the %d conversations it recorded all ended; the first:\n%s", n, conversations, g)
	}
	if live := ctx.Live(); live != 0 {
		t.Errorf("%d registrations on the ingress's context outlive the %d conversations", live, conversations)
	}
}

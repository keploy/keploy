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

// The close on shutdown is still registered while a conversation is
// recorded, and it still runs when the ingress stops, so an idle conversation
// does not keep RecordIncoming blocked for good after StopIngress; the row
// above passes with no close on shutdown at all. It waits until the first
// byte of the client's preface reaches the app, which RecordIncoming forwards
// after registering the close, then checks the registration is live, stops
// the ingress and waits for the call to return. Nothing reads a clock: go
// test's -timeout is the hang guard.
func TestRecordIncomingOfAnIdleConversationReturnsWhenTheIngressStops(t *testing.T) {
	ctx := leakcheck.NewContext(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	cliApp, cliProxy := net.Pipe()
	appProxy, appApp := net.Pipe()
	t.Cleanup(func() { _ = cliApp.Close(); _ = appApp.Close() })
	tcCh := make(chan *models.TestCase, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RecordIncoming(ctx, zap.NewNop(), cliProxy, appProxy, tcCh, 50051, "", false, false)
	}()
	// The preface's first byte, then the conversation stays idle.
	if _, err := cliApp.Write([]byte("P")); err != nil {
		t.Fatal(err)
	}
	if _, err := appApp.Read(make([]byte, 1)); err != nil {
		t.Fatalf("the preface never reached the app: %v", err)
	}
	if live := ctx.Live(); live != 1 {
		t.Fatalf("RecordIncoming of an idle conversation has %d closes registered on the ingress's context, want 1", live)
	}
	ctx.Cancel()
	<-done
}

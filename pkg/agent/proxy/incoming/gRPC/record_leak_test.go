package proxy

import (
	"context"
	"io"
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

// A client that goes away with its call unanswered leaves no window open, with
// --sync or without. The parser reads its channel to its end and does not give
// up, so the connection's end is the only thing that ends that call's window
// (callWindows.stop). Left open, the window would hold the call's mocks until
// the bound gave it up, and a window given up while in flight turns the
// outside-requests marking off for the agent's life. Nothing reads a clock:
// go test's -timeout is the hang guard.
func TestRecordIncomingEndsAnUnansweredCallsWindowAtTheConnectionsEnd(t *testing.T) {
	for _, synchronous := range []bool{true, false} {
		t.Run(map[bool]string{true: "sync", false: "async"}[synchronous], func(t *testing.T) {
			m := syncMock.New(zap.NewNop())
			ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), m))
			defer cancel()
			cliApp, cliProxy := net.Pipe()
			appProxy, appApp := net.Pipe()
			tcCh := make(chan *models.TestCase, 4)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = RecordIncoming(ctx, zap.NewNop(), cliProxy, appProxy, tcCh, 50051, "", synchronous, false)
			}()
			req, _ := pingCall(t)
			go func() { _, _ = cliApp.Write(req) }()
			// The forwarder opens the call's window before it writes the
			// call's bytes on, so once the app has read them it is open.
			if _, err := io.ReadFull(appApp, make([]byte, len(req))); err != nil {
				t.Fatal(err)
			}
			if n := m.OpenWindows(); n != 1 {
				t.Fatalf("%d windows open as the app read the call, want 1", n)
			}
			_ = cliApp.Close()
			_ = appApp.Close()
			<-done
			if n := m.OpenWindows(); n != 0 {
				t.Fatalf("%d windows left open after the connection ended with its call unanswered", n)
			}
		})
	}
}

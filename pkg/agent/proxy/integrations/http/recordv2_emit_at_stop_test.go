package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// A mock the session refuses as the recording's stop comes (the context ends
// between emitV2's check of it and EmitMock's own; here in the hook EmitMock
// runs before its check) is left out, counted and spanned as every mock left
// out is, not dropped. The mock after it, handed over once the context has
// ended, is recorded.
func TestEmitV2_AMockRefusedAsTheStopComesIsLeftOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := syncMock.New(nil)
	out := make(chan *models.Mock, 4)
	mgr.SetOutputChannel(out)
	spans := &spanLog{}
	sess := &supervisor.Session{
		Ctx:                   ctx,
		Mgr:                   mgr,
		RouteMocksViaSyncMock: true,
		OnMockRecorded:        func(*models.Mock) { cancel() },
		Orphans:               spans,
	}
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	mock := func(name string) *models.Mock {
		return &models.Mock{Kind: models.HTTP, Name: name, Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}
	}
	logger := zaptest.NewLogger(t)
	// Each says whether it was lost (a recordOne mark is given back for one
	// that was): the refused one was, the one after the stop was taken.
	if !emitV2(sess, logger, mock("refused")) {
		t.Fatal("emitV2 said the refused mock was not lost")
	}
	if emitV2(sess, logger, mock("after the stop")) {
		t.Fatal("emitV2 said the mock handed over after the stop was lost")
	}
	// And one the manager drops after the stop is lost: the output closed.
	defer func() {
		mgr.CloseOutChan()
		if !emitV2(sess, logger, mock("dropped after the stop")) {
			t.Error("emitV2 said a mock the manager dropped after the stop was not lost")
		}
	}()
	if n, s := mgr.MocksLeftOut(), len(spans.all()); n != 1 || s != 1 {
		t.Fatalf("%d mock(s) left out, %d span(s); want the refused mock left out once, with its span", n, s)
	}
	select {
	case m := <-out:
		if m.Name != "after the stop" {
			t.Fatalf("recorded %q, want the mock handed over after the stop", m.Name)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the mock handed over after the stop was not recorded")
	}
	select {
	case m := <-out:
		t.Fatalf("recorded %q as well, want only the mock handed over after the stop", m.Name)
	default:
	}
}

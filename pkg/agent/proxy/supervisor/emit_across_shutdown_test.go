package supervisor

import (
	"context"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
)

// EmitMockAcrossShutdown delivers the mock of an exchange its parser read
// whole though the session's context ends after any check its caller could
// make and before the emit's own: here the post-record hook, which the emit
// runs before it asks the context, ends it. EmitMock drops that mock with
// nothing reporting it; the session decides here, by its own check, and
// takes the shutdown path. On the direct channel the send then does not
// wait; while the context lasts it waits on the channel, as EmitMock's does,
// so a consumer that takes the mock later still gets it.
func TestEmitMockAcrossShutdownDeliversAMockAsTheContextEnds(t *testing.T) {
	session := func(viaSyncMock bool, mocks chan *models.Mock) (*Session, *syncMock.SyncMockManager, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		mgr := syncMock.New(nil)
		return &Session{Mgr: mgr, Mocks: mocks, Ctx: ctx, ClientConnID: "c", RouteMocksViaSyncMock: viaSyncMock}, mgr, cancel
	}
	mock := func() *models.Mock {
		m := &models.Mock{Name: "read-whole", Kind: models.HTTP}
		m.Spec.ReqTimestampMock = time.Now()
		return m
	}

	t.Run("the sync-mock manager", func(t *testing.T) {
		s, mgr, cancel := session(true, nil)
		s.AddPostRecordHook(func(*models.Mock) { cancel() })
		s.EmitMockAcrossShutdown(mock())
		if _, _, added, _ := mgr.GetDropStats(); added != 1 {
			t.Fatalf("the manager was handed %d mocks, want the one read before the context ended", added)
		}
	})
	t.Run("the direct channel", func(t *testing.T) {
		// Each emit races the send against the context's end, which the
		// hook has made; whichever the select takes, the mock is sent.
		for i := 0; i < 64; i++ {
			mocks := make(chan *models.Mock, 1)
			s, _, cancel := session(false, mocks)
			s.AddPostRecordHook(func(*models.Mock) { cancel() })
			s.EmitMockAcrossShutdown(mock())
			if len(mocks) != 1 {
				t.Fatalf("emit %d: %d mocks sent, want the one read before the context ended", i, len(mocks))
			}
		}
	})
	t.Run("the direct channel, the context alive", func(t *testing.T) {
		mocks := make(chan *models.Mock)
		s, _, _ := session(false, mocks)
		got := make(chan *models.Mock, 1)
		go func() {
			// A consumer that comes to the channel after the emit has.
			time.Sleep(20 * time.Millisecond)
			got <- <-mocks
		}()
		s.EmitMockAcrossShutdown(mock())
		select {
		case <-got:
		case <-time.After(10 * time.Second):
			t.Fatal("the mock was not delivered to a consumer that came to the channel after the emit")
		}
	})
}

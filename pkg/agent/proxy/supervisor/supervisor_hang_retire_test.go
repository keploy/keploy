package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestAHungParserRecordsNothingAfterItsRetirement: a parser the supervisor
// retires for a hang has its streams closed under it, and whatever it hands
// over after was read up to that close: an answer it was in the middle of is
// cut there. Each mock it hands over after the abort is left out and reported,
// for the hang: the ordinary emit, made as the abort closes the streams and
// before the parser's context is cancelled, and the shutdown emit, made once
// it is cancelled, which skips the context check (as the Generic recorder
// flushes what it holds when its context ends), and every one after the
// first, so a parser with several exchanges in flight records none of them.
// Clearing the flag (MarkMockComplete) does not undo the retirement. Ordered by
// events: the abort holds until the parser has made its first emit, so the
// cancel comes after it.
func TestAHungParserRecordsNothingAfterItsRetirement(t *testing.T) {
	ResetLeftOutWarningsForTest()
	core, logs := observer.New(zapcore.DebugLevel)
	mocks := make(chan *models.Mock, 8)
	mgr := syncMock.New(nil)
	sess := &Session{Mocks: mocks, Mgr: mgr, Logger: zap.New(core), Ctx: context.Background()}

	s := New(shortCfg(t))
	aborted := make(chan struct{})
	firstEmitted := make(chan error, 1)
	s.SessionOnAbort = func() {
		close(aborted)
		// The supervisor is held between the abort and the cancel.
		<-firstEmitted
	}
	s.MarkPendingWork()
	at := time.Unix(1_700_000_000, 0)
	mock := func(name string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP,
			Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}
	}
	after := make(chan []error, 1)
	res := s.Run(context.Background(), func(ctx context.Context, sess *Session) error {
		// Hung until the abort, which retires it before it cancels it.
		<-aborted
		firstEmitted <- sess.EmitMock(mock("cut-as-the-abort-closed"))
		<-ctx.Done()
		sess.MarkMockComplete()
		after <- []error{
			sess.EmitMockOnShutdown(mock("held-when-cancelled")),
			sess.EmitMockOnShutdown(mock("second-in-flight")),
		}
		return ctx.Err()
	}, sess)
	if res.Status != StatusHung {
		t.Fatalf("status = %s, want hung", res.Status)
	}
	// Run returns at the abort; the parser's shutdown emits follow its cancel.
	errs := <-after
	for _, err := range errs {
		if err != nil {
			t.Fatalf("EmitMockOnShutdown = %v, want nil for a mock left out", err)
		}
	}
	select {
	case m := <-mocks:
		t.Fatalf("mock %q was recorded after the hang retired its parser", m.Name)
	default:
	}
	if n := mgr.MocksLeftOut(); n != 3 {
		t.Fatalf("mocks left out = %d, want 3: each one the retired parser handed over", n)
	}
	lines := append(logs.FilterMessage(LeftOutWarnMsg).All(), logs.FilterMessage(LeftOutDebugMsg).All()...)
	if len(lines) != 3 {
		t.Fatalf("left-out lines = %d, want 3", len(lines))
	}
	for _, e := range lines {
		if r, _ := e.ContextMap()["reason"].(string); !strings.HasPrefix(r, "parser_hung:") {
			t.Fatalf("left-out reason = %q, want the hang's (parser_hung)", r)
		}
	}
}

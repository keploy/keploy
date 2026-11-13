package http

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// emitAtStopMock is a mock with the times of an exchange, so a mock left out
// gets its span.
func emitAtStopMock(name string) *models.Mock {
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	return &models.Mock{Kind: models.HTTP, Name: name, Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}
}

// leftOutReasons is the reason of each mock the session's log says it left
// out.
func leftOutReasons(logs *observer.ObservedLogs) []string {
	var why []string
	for _, e := range logs.All() {
		if e.Message == supervisor.LeftOutWarnMsg || e.Message == supervisor.LeftOutDebugMsg {
			why = append(why, fmt.Sprint(e.ContextMap()["reason"]))
		}
	}
	return why
}

// A mock handed over as the recording's stop comes (here in the post-record
// hook EmitMock runs before its look at the context) is recorded through the
// sync-mock manager, as the agent records, with nothing left out: its
// exchange was captured before the stop, and the supervisor's stop waits for
// the parser to read what was captured. So is the mock handed over after it.
//
// Before, EmitMock refused the mock and it was left out with its span, and
// the test cases over it with it.
func TestEmitV2_AMockHandedOverAsTheStopComesIsRecorded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := syncMock.New(nil)
	out := make(chan *models.Mock, 4)
	mgr.SetOutputChannel(out)
	spans := &spanLog{}
	core, logs := observer.New(zapcore.DebugLevel)
	sess := &supervisor.Session{
		Ctx:                   ctx,
		Mgr:                   mgr,
		RouteMocksViaSyncMock: true,
		OnMockRecorded:        func(*models.Mock) { cancel() },
		Orphans:               spans,
		Logger:                zap.New(core),
		// No room: a mock sent here instead of through the manager is
		// not recorded.
		Mocks: make(chan *models.Mock),
	}
	logger := zaptest.NewLogger(t)
	// Each says whether it was lost (a recordOne mark is given back for one
	// that was): neither was.
	for _, name := range []string{"as the stop comes", "after the stop"} {
		if emitV2(sess, logger, emitAtStopMock(name)) {
			t.Fatalf("emitV2 said the mock handed over %s was lost", name)
		}
	}
	if n, s, why := mgr.MocksLeftOut(), len(spans.all()), leftOutReasons(logs); n != 0 || s != 0 || len(why) != 0 {
		t.Fatalf("%d mock(s) left out (%q), %d span(s); want none", n, why, s)
	}
	for _, want := range []string{"as the stop comes", "after the stop"} {
		select {
		case m := <-out:
			if m.Name != want {
				t.Fatalf("recorded %q, want %q", m.Name, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the mock handed over %s was not recorded", want)
		}
	}
	// And one the manager drops after the stop is lost: the output closed.
	mgr.CloseOutChan()
	if !emitV2(sess, logger, emitAtStopMock("dropped after the stop")) {
		t.Error("emitV2 said a mock the manager dropped after the stop was not lost")
	}
}

// Straight to the session's channel, a mock EmitMock refuses as the stop
// comes (the context ends between emitV2's look at it and EmitMock's own,
// here in the hook EmitMock runs before it, with no room on the channel) is
// left out with its reason, counted and spanned as every mock left out is,
// not dropped with nothing said.
func TestEmitV2_AMockRefusedAsTheStopComesIsLeftOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := syncMock.New(nil)
	spans := &spanLog{}
	core, logs := observer.New(zapcore.DebugLevel)
	sess := &supervisor.Session{
		Ctx:            ctx,
		Mgr:            mgr,
		Mocks:          make(chan *models.Mock),
		OnMockRecorded: func(*models.Mock) { cancel() },
		Orphans:        spans,
		Logger:         zap.New(core),
	}
	if !emitV2(sess, zaptest.NewLogger(t), emitAtStopMock("refused")) {
		t.Fatal("emitV2 said the refused mock was not lost")
	}
	why := leftOutReasons(logs)
	if n, s := mgr.MocksLeftOut(), len(spans.all()); n != 1 || s != 1 || len(why) != 1 || why[0] != leftOutRefusedAtStop {
		t.Fatalf("%d mock(s) left out (%q), %d span(s); want the refused mock left out once for %q, with its span", n, why, s, leftOutRefusedAtStop)
	}
}

// Straight to the session's channel, every mock handed over after the stop
// is recorded while the channel has room: EmitMock would refuse each one
// whose send it did not pick (a select between the send and the context's
// end, both ready), so emitV2 uses EmitMockOnShutdown once the context has
// ended.
func TestEmitV2_MocksHandedOverAfterTheStopAreRecorded(t *testing.T) {
	const n = 24
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mgr := syncMock.New(nil)
	out := make(chan *models.Mock, n)
	core, logs := observer.New(zapcore.DebugLevel)
	sess := &supervisor.Session{Ctx: ctx, Mgr: mgr, Mocks: out, Orphans: &spanLog{}, Logger: zap.New(core)}
	for i := 0; i < n; i++ {
		if emitV2(sess, zaptest.NewLogger(t), emitAtStopMock(fmt.Sprint("after the stop ", i))) {
			t.Fatalf("emitV2 said mock %d, handed over after the stop, was lost", i)
		}
	}
	if got, left, why := len(out), mgr.MocksLeftOut(), leftOutReasons(logs); got != n || left != 0 || len(why) != 0 {
		t.Fatalf("recorded %d of the %d mocks handed over after the stop, %d left out (%q); want every one recorded", got, n, left, why)
	}
}

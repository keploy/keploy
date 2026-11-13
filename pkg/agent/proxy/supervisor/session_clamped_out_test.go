package supervisor

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// clampedOutRun runs a parser that emits each of mocks, in order, on a session
// whose post-record hook is hook (none when nil), under a supervisor, and
// returns the session's WARNs once Run has returned.
func clampedOutRun(t *testing.T, hook PostRecordHook, mocks ...*models.Mock) []observer.LoggedEntry {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	ch := make(chan *models.Mock, len(mocks))
	sess := &Session{Mocks: ch, Ctx: context.Background(), Logger: zap.New(core), ClientConnID: "conn-2"}
	if hook != nil {
		sess.AddPostRecordHook(hook)
	}
	sv := New(Config{Logger: zap.NewNop()})
	res := sv.Run(context.Background(), func(_ context.Context, sess *Session) error {
		for _, m := range mocks {
			if err := sess.EmitMock(m); err != nil {
				return err
			}
		}
		if n := logs.FilterMessage(ClampedOutMsg).Len(); n != 0 {
			t.Errorf("%d WARNs while the parser runs; want none: the clamp's loss is said once, as the parser returns", n)
		}
		return nil
	}, sess)
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	if got := len(ch); got != len(mocks) {
		t.Fatalf("%d mocks recorded; want all %d", got, len(mocks))
	}
	sess.ReportClampedOut() // said once: a second call says nothing more
	return logs.FilterLevelExact(zapcore.WarnLevel).All()
}

// zkAt is a ZooKeeper mock asked at ms and answered at ms+1 (milliseconds past
// a base), named name.
func zkAt(name string, kind models.Kind, ms int) *models.Mock {
	base := time.Unix(1_763_000_000, 0).UTC()
	at := base.Add(time.Duration(ms) * time.Millisecond)
	return &models.Mock{Name: name, Kind: kind,
		Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}
}

// A parser that emits in answer order (zookeeper serves a later xid before an
// earlier one) gets its later-emitted, earlier-asked mocks' request stamps
// raised to keep the connection's requests in order, which leaves them
// answered before they were asked: a replay without KEPLOY_PROVEN leaves them
// out. The recording says so once for the connection, as its parser returns,
// with how many and the largest inversion, not once for each mock (990 WARNs
// in one CI run).
func TestTheRequestClampsLossIsSaidOnceForTheConnectionWithACount(t *testing.T) {
	warns := clampedOutRun(t, nil,
		zkAt("zk-0", models.Kind("ZooKeeper"), 100),
		zkAt("zk-1", models.Kind("ZooKeeper"), 50), // raised to 100ms+1ns, answered at 51ms
		zkAt("zk-2", models.Kind("ZooKeeper"), 90), // raised to 100ms+2ns, answered at 91ms
	)
	if len(warns) != 1 {
		t.Fatalf("%d WARNs; want 1 for the connection: %v", len(warns), warns)
	}
	f := warns[0].ContextMap()
	if warns[0].Message != ClampedOutMsg {
		t.Errorf("the WARN says %q; want %q", warns[0].Message, ClampedOutMsg)
	}
	if f["mocks"] != int64(2) || f["firstMock"] != "zk-1" || f["connID"] != "conn-2" {
		t.Errorf("the WARN counts %v mocks, first %v, on %v; want 2, zk-1, conn-2", f["mocks"], f["firstMock"], f["connID"])
	}
	if want := 49*time.Millisecond + time.Nanosecond; f["mostAnsweredBeforeAskedBy"] != want {
		t.Errorf("the WARN's largest inversion is %v; want %v", f["mostAnsweredBeforeAskedBy"], want)
	}
}

// ZooKeeper's post-record hook carries a mock's response stamp up to its
// clamped request stamp: the replay keeps the mock, and nothing is said.
func TestAMockAHookCarriesBackIntoOrderIsNotSaidOf(t *testing.T) {
	carry := func(m *models.Mock) {
		if m.Spec.ResTimestampMock.Before(m.Spec.ReqTimestampMock) {
			m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock
		}
	}
	if warns := clampedOutRun(t, carry,
		zkAt("zk-0", models.Kind("ZooKeeper"), 100),
		zkAt("zk-1", models.Kind("ZooKeeper"), 50),
	); len(warns) != 0 {
		t.Fatalf("%d WARNs for a mock its hook kept in order; want none: %v", len(warns), warns)
	}
}

// Only what the clamp did is counted: a mock its parser stamped answered before
// it was asked is warnAnsweredBeforeAsked's (said as it is recorded, and not
// again here), and a connection failure is placed by its window alone, never
// left out for its stamps' order.
func TestOnlyTheClampsOwnInversionsAreCounted(t *testing.T) {
	answeredBeforeAskedWarns.Reset()
	inverted := zkAt("zk-own", models.Kind("ZooKeeper"), 50)
	inverted.Spec.ResTimestampMock = inverted.Spec.ReqTimestampMock.Add(-time.Millisecond)
	warns := clampedOutRun(t, nil,
		zkAt("zk-0", models.Kind("ZooKeeper"), 100),
		inverted,
		zkAt("refused", models.ConnectionFailure, 60),
	)
	for _, w := range warns {
		if w.Message == ClampedOutMsg {
			t.Fatalf("the clamp's WARN for mocks it did not invert, or a replay keeps: %v", w.ContextMap())
		}
	}
}

// A session with no logger, or none at all, says nothing and does not fall
// over.
func TestTheRequestClampsReportWithNoLoggerSaysNothing(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ReportClampedOut panicked: %v", r)
		}
	}()
	sess := &Session{Mocks: make(chan *models.Mock, 2), Ctx: context.Background()}
	for _, m := range []*models.Mock{zkAt("zk-0", models.Kind("ZooKeeper"), 100), zkAt("zk-1", models.Kind("ZooKeeper"), 50)} {
		if err := sess.EmitMock(m); err != nil {
			t.Fatalf("EmitMock: %v", err)
		}
	}
	sess.ReportClampedOut()
	(*Session)(nil).ReportClampedOut()
}

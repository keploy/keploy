package manager

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// noRequestTimeSaid begins the line warnNoRequestTime says.
const noRequestTimeSaid = "a parser handed over a test's mock with no request time"

// noRequestTimeMock is a mock of kind its recorder states the lifetime of, as
// every current recorder does, requested at req (zero: no request time).
func noRequestTimeMock(kind models.Kind, name, lifetime string, req time.Time) *models.Mock {
	mk := &models.Mock{Kind: kind, Name: name, ConnectionID: "conn-" + name}
	mk.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: lifetime}
	mk.Spec.ReqTimestampMock = req
	mk.Spec.ResTimestampMock = req.Add(time.Millisecond)
	return mk
}

// noRequestTimeLines hands mk to a fresh manager's AddMock and counts the
// lines warnNoRequestTime said for it, at any level.
func noRequestTimeLines(t *testing.T, mk *models.Mock) int {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	m.SetOutputChannel(make(chan *models.Mock, 8))
	m.AddMock(mk)
	n := 0
	for _, e := range logs.All() {
		if strings.HasPrefix(e.Message, noRequestTimeSaid) {
			n++
		}
	}
	return n
}

// guardShape derives a copy of mk's lifetime, as AddMock derives it, and
// returns it with whether the loss reads mk as a mock any test may be served
// (attributedReusable) and whether it is reusable only by its statement,
// leaving mk as it was. A row checks its mock's shape first, so a row whose
// mock stops being the case it names fails on that line, not by passing.
func guardShape(mk *models.Mock) (lt models.Lifetime, anyTests, statementOnly bool) {
	c := *mk
	c.Spec.Metadata = make(map[string]string, len(mk.Spec.Metadata))
	for k, v := range mk.Spec.Metadata {
		c.Spec.Metadata[k] = v
	}
	c.DeriveLifetime()
	return c.TestModeInfo.Lifetime, attributedReusable(&c), c.ReusableByStatementOnly()
}

// A test's mock handed over with no request time is said at WARN as AddMock
// takes it, naming its parser by kind and the mock; the first of each kind and
// every 1024th after it at WARN, the rest at Debug. A test's mock with its
// request time, a session's handshake (any test may be served it) and an
// answer the client never read are not said. Under either switch.
func TestATestsMockWithNoRequestTimeIsSaidNamingItsParser(t *testing.T) {
	eachProvenSwitch(t, func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		m := New(zap.New(core))
		out := make(chan *models.Mock, 64)
		m.SetOutputChannel(out)
		said := func(level zapcore.Level) []observer.LoggedEntry {
			var got []observer.LoggedEntry
			for _, e := range logs.FilterLevelExact(level).All() {
				if strings.HasPrefix(e.Message, noRequestTimeSaid) {
					got = append(got, e)
				}
			}
			return got
		}
		at := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)

		m.AddMock(noRequestTimeMock(models.HTTP, "stamped", models.MetadataLifetimePerTest, at))
		handshake := noRequestTimeMock(models.MySQL, "handshake", models.MetadataLifetimeSession, time.Time{})
		handshake.Spec.Metadata["type"] = "config"
		if lt, anyTests, _ := guardShape(handshake); lt != models.LifetimeSession || !anyTests {
			t.Fatalf("the handshake's shape: lifetime %v, any test's %v; want session, true", lt, anyTests)
		}
		m.AddMock(handshake)
		undelivered := noRequestTimeMock(models.HTTP, "unread", models.MetadataLifetimePerTest, time.Time{})
		undelivered.Spec.Metadata[models.MetadataUndeliveredKey] = models.MetadataUndelivered
		m.AddMock(undelivered)
		if w, d := said(zapcore.WarnLevel), said(zapcore.DebugLevel); len(w)+len(d) != 0 {
			t.Fatalf("said %d WARN and %d Debug lines for a stamped test's mock, a session's handshake and an "+
				"unread answer; want none", len(w), len(d))
		}

		m.AddMock(noRequestTimeMock(models.HTTP, "mock-1", models.MetadataLifetimePerTest, time.Time{}))
		w := said(zapcore.WarnLevel)
		if len(w) != 1 {
			t.Fatalf("%d WARN lines for a test's mock with no request time, want 1", len(w))
		}
		f := w[0].ContextMap()
		if f["parser"] != string(models.HTTP) || f["mock"] != "mock-1" || f["connection"] != "conn-mock-1" || f["of_this_parser"] != uint64(1) {
			t.Fatalf("the WARN said parser %v, mock %v, connection %v, of_this_parser %v; want %q, mock-1, conn-mock-1, 1",
				f["parser"], f["mock"], f["connection"], f["of_this_parser"], models.HTTP)
		}
		m.AddMock(noRequestTimeMock(models.HTTP, "mock-2", models.MetadataLifetimePerTest, time.Time{}))
		if w, d := said(zapcore.WarnLevel), said(zapcore.DebugLevel); len(w) != 1 || len(d) != 1 {
			t.Fatalf("the second of a kind: %d WARN and %d Debug lines in all, want 1 and 1", len(w), len(d))
		}
		m.AddMock(noRequestTimeMock(models.MySQL, "mock-3", models.MetadataLifetimePerTest, time.Time{}))
		if w := said(zapcore.WarnLevel); len(w) != 2 || w[1].ContextMap()["parser"] != string(models.MySQL) {
			t.Fatalf("the first of another kind was not said at WARN naming it: %d WARN lines", len(w))
		}
		for i := 3; i <= 1024; i++ {
			m.warnNoRequestTime(noRequestTimeMock(models.HTTP, "mock-n", models.MetadataLifetimePerTest, time.Time{}))
		}
		if w := said(zapcore.WarnLevel); len(w) != 3 || w[2].ContextMap()["of_this_parser"] != uint64(1024) {
			t.Fatalf("%d WARN lines after 1024 of a kind, want 3 (its first, another kind's first, its 1024th)", len(w))
		}
	})
}

// The guard says a mock with no request time by the rule the loss reads
// (servedToAnyTest), not by its lifetime: one any test may be served is not
// said, and one the loss reads as a test's is. A connection's mock (its
// recorder stated connection, with the connection's id, tagged config) is
// any test's under either switch: not said.
func TestAConnectionsMockWithNoRequestTimeIsNotSaid(t *testing.T) {
	eachProvenSwitch(t, func(t *testing.T) {
		mk := noRequestTimeMock(models.MySQL, "conn-setup", models.MetadataLifetimeConnection, time.Time{})
		mk.Spec.Metadata["type"] = "config"
		mk.Spec.Metadata["connID"] = "c1"
		if lt, anyTests, _ := guardShape(mk); lt != models.LifetimeConnection || !anyTests {
			t.Fatalf("the mock's shape: lifetime %v, any test's %v; want connection, true", lt, anyTests)
		}
		if n := noRequestTimeLines(t, mk); n != 0 {
			t.Fatalf("said %d lines for a connection's mock with no request time; want none: any test may be served it", n)
		}
	})
}

// A MySQL mock its recorder stated session but tagged "mocks", as a SET, a
// BEGIN or a COMMIT is, is reusable only by its statement
// (ReusableByStatementOnly): the loss reads it as the test's whose window
// holds its request, so with no request time as needed by every test case of
// its recording. Said under either switch.
func TestAStatementOnlySessionMockWithNoRequestTimeIsSaid(t *testing.T) {
	eachProvenSwitch(t, func(t *testing.T) {
		mk := noRequestTimeMock(models.MySQL, "begin", models.MetadataLifetimeSession, time.Time{})
		mk.Spec.Metadata["type"] = "mocks"
		if lt, anyTests, stmtOnly := guardShape(mk); lt != models.LifetimeSession || anyTests || !stmtOnly {
			t.Fatalf("the mock's shape: lifetime %v, any test's %v, statement-only %v; want session, false, true",
				lt, anyTests, stmtOnly)
		}
		if n := noRequestTimeLines(t, mk); n != 1 {
			t.Fatalf("said %d lines for a statement-only session mock with no request time; want 1: its loss "+
				"leaves out every later test case", n)
		}
	})
}

// An HTTP mock its recorder stated no lifetime for is session-tier by its
// kind. With the proven switch off any test may be served it: not said. With
// it on it is a test's mock, owned by its window, so with no request time it
// is read as needed by every test case of its recording: said.
func TestASessionTierTestMockWithNoRequestTimeIsSaidUnderTheProvenSwitch(t *testing.T) {
	eachProvenSwitch(t, func(t *testing.T) {
		on := provenswitch.On()
		mk := &models.Mock{Kind: models.HTTP, Name: "untagged", ConnectionID: "conn-untagged",
			Spec: models.MockSpec{Metadata: map[string]string{}, ResTimestampMock: time.Time{}.Add(time.Millisecond)}}
		if lt, anyTests, _ := guardShape(mk); lt != models.LifetimeSession || anyTests == on {
			t.Fatalf("the mock's shape with the switch on %v: lifetime %v, any test's %v; want session, %v",
				on, lt, anyTests, !on)
		}
		want := 0
		if on {
			want = 1
		}
		if n := noRequestTimeLines(t, mk); n != want {
			t.Fatalf("said %d lines with the switch on %v; want %d", n, on, want)
		}
	})
}

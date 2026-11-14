package manager

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// noRequestTimeMock is a mock of kind its recorder states the lifetime of, as
// every current recorder does, requested at req (zero: no request time).
func noRequestTimeMock(kind models.Kind, name, lifetime string, req time.Time) *models.Mock {
	mk := &models.Mock{Kind: kind, Name: name, ConnectionID: "conn-" + name}
	mk.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: lifetime}
	mk.Spec.ReqTimestampMock = req
	mk.Spec.ResTimestampMock = req.Add(time.Millisecond)
	return mk
}

// A test's mock handed over with no request time is said at WARN as AddMock
// takes it, naming its parser by kind and the mock; the first of each kind and
// every 1024th after it at WARN, the rest at Debug. A test's mock with its
// request time, a session's mock (any test may be served it, in both switch
// modes) and an answer the client never read are not said.
func TestATestsMockWithNoRequestTimeIsSaidNamingItsParser(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	out := make(chan *models.Mock, 64)
	m.SetOutputChannel(out)
	const msg = "a parser handed over a test's mock with no request time"
	said := func(level zapcore.Level) []observer.LoggedEntry {
		var got []observer.LoggedEntry
		for _, e := range logs.FilterLevelExact(level).All() {
			if len(e.Message) >= len(msg) && e.Message[:len(msg)] == msg {
				got = append(got, e)
			}
		}
		return got
	}
	at := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)

	m.AddMock(noRequestTimeMock(models.HTTP, "stamped", models.MetadataLifetimePerTest, at))
	handshake := noRequestTimeMock(models.MySQL, "handshake", models.MetadataLifetimeSession, time.Time{})
	handshake.Spec.Metadata["type"] = "config"
	m.AddMock(handshake)
	undelivered := noRequestTimeMock(models.HTTP, "unread", models.MetadataLifetimePerTest, time.Time{})
	undelivered.Spec.Metadata[models.MetadataUndeliveredKey] = models.MetadataUndelivered
	m.AddMock(undelivered)
	if w, d := said(zapcore.WarnLevel), said(zapcore.DebugLevel); len(w)+len(d) != 0 {
		t.Fatalf("said %d WARN and %d Debug lines for a stamped test's mock, a session's mock and an unread "+
			"answer; want none", len(w), len(d))
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
}

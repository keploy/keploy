package http

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// stampedSend is a recorded POST /send whose body.content a client test
// named by its own clock (`<p>E2E email body ${Date.now()}</p>`), which the
// learn step learned as a read of the clock: the word at bytes 18-31, read
// since its test began, and body.since, a plain read.
func stampedSend() *models.Mock {
	const content = "<p>E2E email body 1730000000000</p>"
	rec := recorded("mock-6", "/send", map[string]string{"Content-Type": "application/json"},
		`{"content":"`+content+`","since":"2026-09-01T09:00:00Z","to":"a@b.c"}`)
	rec.Spec.Clocks = map[string]models.ClockOffset{
		"body.content": {Res: time.Millisecond, Layout: "epoch-ms", Since: models.SinceTestBegin, Word: models.ClockWordOf(content, 18, 31)},
		"body.since":   {Res: time.Second},
	}
	return rec
}

// clockMissOf matches in, a call no recording is, against mocks under the
// proven switch, and returns its miss report and what the agent logged
// that a learned read of the clock does not hold.
func clockMissOf(t *testing.T, in *req, mocks ...*models.Mock) (*models.MockMismatchReport, []observer.LoggedEntry) {
	t.Helper()
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	in.arrived = base.Add(2 * time.Second)
	core, logs := observer.New(zap.WarnLevel)
	h := &HTTP{Logger: zap.New(core)}
	db := &mockMemDb{mocks: mocks, updateUnFilteredReturn: true}
	ok, _, diag, err := h.match(clockConn(true, base), in, db, models.ProvenFlakyHeaderNoise(), nil, nil, true, false, false, true, true)
	if err != nil || ok || diag == nil {
		t.Fatalf("match ok=%v diag=%v err=%v; want a miss", ok, diag, err)
	}
	r := newHTTP().buildHTTPMismatchReport(&http.Request{Method: in.method, URL: in.url, Host: "api.example", Header: in.header}, in.body, db, nil, nil, nil, diag)
	if r == nil {
		t.Fatal("no miss report")
	}
	return r, logs.FilterMessage("a learned read of the application's clock does not hold").All()
}

// A call of another route than every recording's (an extra POST /search a
// replay makes that no recording holds) is closest to a recording of
// another route, POST /send, which learned reads of the clock. That
// recording is not the call, so its learned reads are no reads the call
// made: the agent logs no read of the clock that does not hold, and the
// miss report names the fields that differ (the path, the recording's
// body.content and body.since the call lacks, its own body.txt) with no
// reason of a clock read, which would blame the clock for a call no
// recording is.
func TestAClockReadOfARecordingOfAnotherRouteIsNoReasonOfAMiss(t *testing.T) {
	in := sent("/search", map[string]string{"Content-Type": "application/json"}, `{"txt":"E2E"}`)
	r, said := clockMissOf(t, in, stampedSend())
	if len(said) != 0 {
		t.Fatalf("the agent logged %d reads of the clock that do not hold, first %v; want none for a recording of another route", len(said), said[0].ContextMap())
	}
	for _, f := range []string{"path", "body.content", "body.since", "body.txt"} {
		if !hasField(r.Differs, f) {
			t.Fatalf("Differs = %v; want %s named", r.Differs, f)
		}
	}
	if len(r.FieldReasons) != 0 {
		t.Fatalf("FieldReasons = %+v; want none: the closest recording is of another route", r.FieldReasons)
	}
}

// A call of the recording's route that carries no value at a place the
// learn step learned as a read of the clock is refused, the place named,
// and its reason says the call carries no value there, of a word read and
// of a plain read alike: never that the text is too short to hold the
// word's sides, or how a value there fails, which names a value the call
// never sent. The agent logs it with that reason.
func TestAClockReadTheCallDoesNotCarrySaysSo(t *testing.T) {
	in := sent("/send", map[string]string{"Content-Type": "application/json"}, `{"to":"a@b.c"}`)
	r, said := clockMissOf(t, in, stampedSend())
	whys := map[string]string{}
	for _, fr := range r.FieldReasons {
		whys[fr.Field] = fr.Why
	}
	logged := map[string]string{}
	for _, e := range said {
		c := e.ContextMap()
		f, _ := c["field"].(string)
		logged[f], _ = c["reason"].(string)
	}
	for _, f := range []string{"body.content", "body.since"} {
		why := whys[f]
		if !hasField(r.Differs, f) || !strings.HasPrefix(why, f+" was learned as ") || !strings.HasSuffix(why, ", and this call carries no value at "+f+": it changed") {
			t.Fatalf("Differs %v, the reason of %s is %q; want it named, and that the call carries no value there", r.Differs, f, why)
		}
		if strings.Contains(why, "too short") {
			t.Fatalf("the reason of %s is %q; want no text the call never sent", f, why)
		}
		if logged[f] != why {
			t.Fatalf("the agent logged %q for %s; want %q", logged[f], f, why)
		}
	}
}

// A call of the recording's route that carries the place, its word read
// from a clock thirty days back, is still refused for how the word fails,
// not for being absent.
func TestAClockReadTheCallCarriesIsCheckedNotCalledAbsent(t *testing.T) {
	stale := "<p>E2E email body 1727800000000</p>"
	in := sent("/send", map[string]string{"Content-Type": "application/json"}, `{"content":"`+stale+`","since":"2026-09-01T09:00:00Z","to":"a@b.c"}`)
	in.url = &url.URL{Path: "/send"}
	r, _ := clockMissOf(t, in, stampedSend())
	for _, fr := range r.FieldReasons {
		if strings.Contains(fr.Why, "carries no value") {
			t.Fatalf("the reason of %s is %q; want the carried value checked", fr.Field, fr.Why)
		}
	}
	if !hasField(r.Differs, "body.content") {
		t.Fatalf("Differs = %v; want body.content refused", r.Differs)
	}
}

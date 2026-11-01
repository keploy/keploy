package learn

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// olderTestAnswer is api-server's acceptance run 5 at mock-645 and mock-646,
// trimmed: in an older test the application inserted an app's audit (mock-645,
// its updatedAt and auditedAt a read of its clock, S) and found it again
// (mock-646, whose answer gives the application S whole); in the open test it
// inserts another audit whose app.createdAt it reads from its clock in the
// same second S in the recording, and at sLive in record's full round, where
// the older test's insert sent fOps. Every instant is seconds since the epoch,
// as the driver wrote them ($numberLong).
type olderTestAnswer struct {
	x645, view645, x646 *models.Mock
	found               func(created, updated string) string
	call                time.Time
	w                   *Window
}

const (
	olderS     = "1793277534"
	olderSLive = "1793277613"
	olderFOps  = "1793277609"
	olderPath  = "body.docs.msgs.0.app.createdAt.$numberLong"
	// olderCreated is the found app's own createdAt, which no run moved.
	olderCreated = "1793277533"
)

func newOlderTestAnswer() olderTestAnswer {
	ts := func(sec, nsec int) time.Time { return time.Date(2026, 10, 29, 12, 38, sec, nsec, time.UTC) }
	fts := func(sec, nsec int) time.Time { return time.Date(2026, 10, 29, 12, 40, sec, nsec, time.UTC) }
	ins := func(sec string) []string {
		return []string{`{ SectionSingle msg: {"insert":"app_config_audits","ordered":true,"$db":"api-server"} }`,
			`{ SectionSingle identifier: documents , msgs: [ {"_id":"c43d5d9a","app":{"_id":"f819baf2","createdAt":{"$numberLong":"1793277533"},"updatedAt":{"$numberLong":"` + sec + `"}},"auditedAt":{"$numberLong":"` + sec + `"}} ] }`}
	}
	const wrote = `{ SectionSingle msg: {"n":{"$numberInt":"1"},"ok":{"$numberDouble":"1.0"}} }`
	found := func(created, updated string) string {
		return `{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"c43d5d9a","app":{"_id":"f819baf2","createdAt":{"$numberLong":"` + created + `"},"updatedAt":{"$numberLong":"` + updated + `"}}}],"id":{"$numberLong":"0"},"ns":"api-server.app_config_audits"},"ok":{"$numberDouble":"1.0"}} }`
	}
	findReq := []string{`{ SectionSingle msg: {"find":"app_config_audits","filter":{"appId":"f819baf2"},"$db":"api-server"} }`}
	return olderTestAnswer{
		x645:    r2Mongo("mock-645", ts(54, 2923055), ts(54, 3063614), ins(olderS), wrote),
		view645: r2Mongo("mock-645", ts(54, 2923055), ts(54, 3063614), ins(olderFOps), wrote),
		x646:    r2Mongo("mock-646", ts(54, 4000000), ts(54, 5000000), findReq, found(olderCreated, olderS)),
		found:   found,
		call:    ts(54, 114470582),
		w: &Window{
			Rec: Span{From: ts(54, 114278574), To: ts(54, 114470582), Began: ts(54, 109684321)},
			F:   Span{From: fts(14, 700000000), To: fts(14, 770000000), Began: fts(13, 900000000)},
		},
	}
}

// view646 is mock-646 as F's application was given it, its answer's
// app.createdAt at created and updatedAt at updated.
func (o olderTestAnswer) view646(created, updated string) *models.Mock {
	v := *o.x646
	v.Spec.MongoResponses = []models.MongoResponse{{Header: o.x646.Spec.MongoResponses[0].Header, Message: &models.MongoOpMessage{Sections: []string{o.found(created, updated)}}}}
	return &v
}

// classify reads the open test's call, whose places are mock-645 and
// mock-646 as the tracker's Order shows them (Order.Of, Order.ServedView).
func (o olderTestAnswer) classify(tr *Tracker) Learned {
	p := tr.Placed()
	dc := clockedDrifts()
	dc.clientTests = true
	var raw []Raw
	for _, m := range []*models.Mock{o.x645, o.x646} {
		r := dc.RawOf(m, false)
		if servedInSet(p, m) {
			r = p.Order.ServedView(m, dc.RawOf(m, true).ServedIn(p.Order.Of(m)))
		}
		raw = append(raw, r)
	}
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{olderPath: {Path: olderPath, Noise: "body.docs.msgs.0.app.createdAt", Type: String, Value: v}}}
	}
	return ClassifyFollowing(dc, []string{olderPath}, shape(olderSLive), shape(olderS), o.call, nil, raw, nil, o.w, nil)[0]
}

// servedInSet reports whether F served m in the set p was read of.
func servedInSet(p Placed, m *models.Mock) bool {
	for _, x := range p.Set {
		if MockID(x) == MockID(m) {
			return true
		}
	}
	return false
}

// An instant whole that an answer gave the application in an older test is
// set aside as the source of a value F's stated view shows moved apart from
// it, as an instant the application sent is (a founder ruling, 2026-10-29):
// api-server's run 5 refused the audit's createdAt three times because a find
// of an older test answered the recording's second whole. The value is then
// a read of the clock since the test began, checked so at every replay, and
// what it learned names the answer it set aside and why. Where F's view
// holds the value F saw there too, the value moved with it, and stays a
// variable of it. With no view stated, served in an order keploy cannot
// tell, never served, or served in the open test itself (whatever F gave
// there: a data-plane answer's instant is a variable, which every replay
// checks), the answer stays the value's source, and the value is learned as
// before: a variable of it (which the Mongo adapter cannot follow into a
// request, and refuses), or refused.
func TestClassifyFollowing_AnOlderTestsAnswerTheValueMovedApartFromIsNoSource(t *testing.T) {
	o := newOlderTestAnswer()
	insertMovedApart := func(tr *Tracker) {
		tr.ServedMock(o.x645)
		tr.ServedMockLive(o.x645, "fOps", func() (*models.Mock, error) { return o.view645, nil })
	}
	asRecorded := func(tr *Tracker) { tr.ServedMock(o.x646); tr.ServedMockAsRecorded(o.x646) }
	viewOf := func(created, updated string) func(tr *Tracker) {
		return func(tr *Tracker) {
			tr.ServedMock(o.x646)
			tr.ServedMockLive(o.x646, created+updated, func() (*models.Mock, error) { return o.view646(created, updated), nil })
		}
	}
	viewAt := func(sec string) func(tr *Tracker) { return viewOf(olderCreated, sec) }
	none := func(*Tracker) {}
	for _, c := range []struct {
		name           string
		older, open    func(tr *Tracker)
		want           Kind
		since, inOlder bool // want: a read since the test began; mock-646 read as served in an older test
		refuted        string
	}{
		{"an older test's answer, as recorded", asRecorded, none, KindClockRead, true, true, "which record's full round sent and gave the application as recorded, byte for byte"},
		{"an older test's answer, its view moved apart", viewAt(olderFOps), none, KindClockRead, true, true, "where record's full round gave the application another value"},
		{"an older test's answer, its view moved with the value", viewAt(olderSLive), none, KindVariable, false, true, ""},
		{"an older test's answer, its view moved apart there and giving the value elsewhere", viewOf(olderSLive, olderFOps), none, KindVariable, false, true, ""},
		{"an older test's answer, no view stated", func(tr *Tracker) { tr.ServedMock(o.x646) }, none, KindVariable, false, true, ""},
		{"an older test's answer, served in an order keploy cannot tell", func(tr *Tracker) {
			a, b := tr.Lift(), tr.Lift()
			tr.ServedMock(o.x646)
			tr.ServedMockAsRecorded(o.x646)
			a.Land()
			b.Land()
		}, none, KindUnaccounted, false, false, ""},
		{"never served", none, none, KindVariable, false, false, ""},
		{"the open test's own answer, as recorded", none, asRecorded, KindUnaccounted, false, false, ""},
		{"the open test's own answer, its view moved apart", none, viewAt(olderFOps), KindUnaccounted, false, false, ""},
		{"served in an older test and again in the open test, as recorded", asRecorded, asRecorded, KindUnaccounted, false, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := NewTracker()
			tr.Begin()
			insertMovedApart(tr)
			c.older(tr)
			tr.End()
			tr.Begin()
			c.open(tr)
			p := tr.Placed()
			if first, last := p.Order.Of(o.x646); servedInSet(p, o.x646) {
				r := p.Order.ServedView(o.x646, clockedDrifts().RawOf(o.x646, true).ServedIn(first, last))
				if r.InAnOlderTest() != c.inOlder {
					t.Fatalf("mock-646 (served at %d,%d) read as served in an older test: %v; want %v", first, last, r.InAnOlderTest(), c.inOlder)
				}
			}
			ln := o.classify(tr)
			if ln.Kind != c.want {
				t.Fatalf("FALSE GREEN? learned %s from %q (%q); want %s", ln.Kind, ln.From.Mock, ln.Why, c.want)
			}
			switch c.want {
			case KindClockRead:
				if ln.Read == nil || ln.Read.Since != models.SinceTestBegin {
					t.Fatalf("learned a read %+v; want one since the test began", ln.Read)
				}
			case KindVariable:
				if ln.From.Mock != "mock-646" {
					t.Fatalf("a variable of %q; want of mock-646, still the value's source", ln.From.Mock)
				}
			}
			if c.refuted == "" {
				if ln.Kind == KindClockRead && strings.Contains(ln.Refuted, "mock-646") {
					t.Fatalf("names mock-646 as set aside (%q)", ln.Refuted)
				}
				return
			}
			t.Logf("set aside: %s", ln.Refuted)
			if !strings.Contains(ln.Refuted, "mock-646") || !strings.Contains(ln.Refuted, c.refuted) || !strings.Contains(ln.Refuted, "a copy moves with its source") {
				t.Fatalf("names what it set aside as %q; want mock-646, %q and why", ln.Refuted, c.refuted)
			}
		})
	}
}

// Order.ServedView reads a recording as served in an older test only where
// F made every serve of it the read saw in a test of the set before the
// running one, in an order keploy can tell: the previous test's, or one two
// tests back; not one served in the running test, nor in an older test and
// again in it, nor one served at the start-up or between tests at all (a
// founder decision keeps such an answer's instant a variable), nor one of
// the previous set, nor before the new set's first test. It reads one as
// served before the running test (before: Raw.BeforeTheTest) where F made
// every serve of it the read saw before that test began, in an order keploy
// can tell: at the start-up, between tests or in an older test; not one
// served in the running test too, nor one of the previous set, nor any
// before the set's first test began, nor one served on parallel requests
// aloft together.
func TestOrderServedViewMarksARecordingServedOnlyBeforeTheRunningTest(t *testing.T) {
	m := &models.Mock{Name: "mock-1", Kind: models.Mongo}
	for _, c := range []struct {
		name   string
		run    func(tr *Tracker)
		want   bool
		before bool
	}{
		{"served at the start-up, read in the first test", func(tr *Tracker) { tr.ServedMock(m); tr.Begin() }, false, true},
		{"served at the start-up, read before any test", func(tr *Tracker) { tr.ServedMock(m) }, false, false},
		{"served at the start-up on parallel requests aloft together, read in the first test", func(tr *Tracker) {
			a, b := tr.Lift(), tr.Lift()
			tr.ServedMock(m)
			tr.ServedMock(&models.Mock{Name: "mock-0", Kind: models.Mongo})
			a.Land()
			b.Land()
			tr.Begin()
		}, false, false},
		{"served in the running test", func(tr *Tracker) { tr.Begin(); tr.ServedMock(m) }, false, false},
		{"served in the previous test", func(tr *Tracker) { tr.Begin(); tr.ServedMock(m); tr.End(); tr.Begin() }, true, true},
		{"served two tests back", func(tr *Tracker) {
			tr.Begin()
			tr.ServedMock(m)
			tr.End()
			tr.Begin()
			tr.End()
			tr.Begin()
		}, true, true},
		{"served between tests", func(tr *Tracker) { tr.Begin(); tr.End(); tr.ServedMock(m); tr.Begin() }, false, true},
		{"served at the start-up and in the previous test", func(tr *Tracker) {
			tr.ServedMock(m)
			tr.Begin()
			tr.ServedMock(m)
			tr.End()
			tr.Begin()
		}, false, true},
		{"served in the previous test and between tests", func(tr *Tracker) {
			tr.Begin()
			tr.ServedMock(m)
			tr.End()
			tr.ServedMock(m)
			tr.Begin()
		}, false, true},
		{"served in the previous test and the running one", func(tr *Tracker) {
			tr.Begin()
			tr.ServedMock(m)
			tr.End()
			tr.Begin()
			tr.ServedMock(m)
		}, false, false},
		{"served in the previous set", func(tr *Tracker) { tr.Begin(); tr.ServedMock(m); tr.NewSet(); tr.Begin() }, false, false},
		{"served in a new set before its first test", func(tr *Tracker) {
			tr.Begin()
			tr.ServedMock(&models.Mock{Name: "mock-0", Kind: models.Mongo})
			tr.ServedMock(&models.Mock{Name: "mock-2", Kind: models.Mongo})
			tr.End()
			tr.Begin()
			tr.NewSet()
			tr.ServedMock(m)
		}, false, false},
	} {
		tr := NewTracker()
		c.run(tr)
		o := tr.Placed().Order
		first, last := o.Of(m)
		r := RawOf(m, true).ServedIn(first, last)
		if got := o.ServedView(m, r).InAnOlderTest(); got != c.want {
			t.Errorf("%s (at %d,%d): read as served in an older test %v; want %v", c.name, first, last, got, c.want)
		}
		if got := o.ServedView(m, r).BeforeTheTest(); got != c.before {
			t.Errorf("%s (at %d,%d): read as served before the running test %v; want %v", c.name, first, last, got, c.before)
		}
	}
	// Of a Raw F did not serve it says nothing.
	tr := NewTracker()
	tr.Begin()
	tr.ServedMock(m)
	tr.End()
	tr.Begin()
	if tr.Placed().Order.ServedView(m, RawOf(m, false)).InAnOlderTest() || RawOf(m, false).ServedInAnOlderTest().InAnOlderTest() {
		t.Errorf("a recording F did not serve read as served in an older test")
	}
	if tr.Placed().Order.ServedView(m, RawOf(m, false)).BeforeTheTest() || RawOf(m, false).ServedBeforeTheTest().BeforeTheTest() {
		t.Errorf("a recording F did not serve read as served before the running test")
	}
	if RawOf(m, true).ServedIn(1, 1).ServedInAnOlderTest().WithServed(false).ServedIn(2, 2).InAnOlderTest() {
		t.Errorf("WithServed(false) kept served in an older test")
	}
}

// A connection's start-up an older test opened holds the open test's instant
// whole where a swap of start-ups can change it: in its backend secret key (a
// ten-digit key reads as seconds since the epoch), or in a server parameter
// the set's other start-up recorded otherwise. F gave that start-up as
// recorded in the older test while the value moved. An older test's answer
// in its place is set aside as no source, and the value learned as a read of
// the clock (the row above). A start-up's word is never set aside so
// (swapHolds): F may have given the application's connection another
// connection's start-up, whose word the value then copied, and a clock read
// is checked only against its call's window, which a copied word passes too.
// The value stays refused, naming the start-up, under keploy test's rules and
// a client test's.
func TestClassifyFollowing_AnOlderTestsStartUpTheValueMovedApartFromStaysRefused(t *testing.T) {
	o := newOlderTestAnswer()
	ts := func(sec, nsec int) time.Time { return time.Date(2026, 10, 29, 12, 38, sec, nsec, time.UTC) }
	s, err := strconv.ParseInt(olderS, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	session := func(name string, nsec int, key int32, app string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: ts(54, nsec),
			ResTimestampMock: ts(54, nsec+1000000),
			Metadata:         map[string]string{"type": "config"},
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeSession, Session: &models.PostgresV3SessionSpec{
				ServerVersion: "15.2", BackendProcessID: 81, BackendSecretKey: key,
				ParameterStatus: map[string]string{"application_name": app, "TimeZone": "UTC"},
			}},
		}}
	}
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{olderPath: {Path: olderPath, Noise: "body.docs.msgs.0.app.createdAt", Type: String, Value: v}}}
	}
	for _, c := range []struct {
		name, field string
		older       *models.Mock
		others      []*models.Mock
	}{
		{"its secret key", "backendSecretKey", session("mock-hs-81", 4000000, int32(s), "api"), nil},
		{"a server parameter the other start-up recorded otherwise", "parameterStatus.application_name",
			session("mock-hs-81", 4000000, 7081, olderS), []*models.Mock{session("mock-hs-82", 3000000, 7082, "api")}},
	} {
		for _, client := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, client tests %v", c.name, client), func(t *testing.T) {
				tr := NewTracker()
				for _, m := range c.others {
					tr.ServedMock(m)
					tr.ServedMockAsRecorded(m)
				}
				tr.Begin()
				tr.ServedMock(c.older)
				tr.ServedMockAsRecorded(c.older)
				tr.End()
				tr.Begin()
				p := tr.Placed()
				dc := clockedDrifts()
				dc.clientTests = client
				var raw []Raw
				for _, m := range append([]*models.Mock{c.older}, c.others...) {
					raw = append(raw, p.Order.ServedView(m, dc.RawOf(m, true).ServedIn(p.Order.Of(m))))
				}
				if !raw[0].InAnOlderTest() {
					t.Fatalf("%s not read as served in an older test; the row needs it so", c.older.Name)
				}
				ln := ClassifyFollowing(dc, []string{olderPath}, shape(olderSLive), shape(olderS), o.call, nil, raw, nil, o.w, nil)[0]
				if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "mock-hs-81 at spec.postgresV3.session."+c.field+", a connection's recorded start-up") || !strings.Contains(ln.Why, "never varies") {
					t.Fatalf("FALSE GREEN? learned %s from %q (%q, set aside %q); want refused, naming mock-hs-81's %s", ln.Kind, ln.From.Mock, ln.Why, ln.Refuted, c.field)
				}
			})
		}
	}
}

package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// tiedBigMock is sentInTurnMock's eventsV2 insert whose document also
// carries about 100 KB of notes, many fields of words: api-server's
// TestTestRunReportOnClient wrote such documents (105 KB and 265 KB texts
// in the w10 stall dumps), and each was read into its field view
// (fieldsWith) to ask whether its request alone held the call's word.
func tiedBigMock(name, id string, req, res time.Time) *models.Mock {
	var notes strings.Builder
	for i := 0; notes.Len() < 100<<10; i++ {
		if i > 0 {
			notes.WriteString(",")
		}
		notes.WriteString(`"n` + strconv.Itoa(i) + `":"step ` + strconv.Itoa(i) + ` of the suite ran its checks and wrote its report"`)
	}
	doc := `{ SectionSingle identifier: documents , msgs: [ {"eventDetails":{"test_suite_run_id":"` + id + `","run_result":"failure","notes":{` + notes.String() + `}}} ] }`
	return &models.Mock{Name: name, Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"insert":"eventsV2","ordered":true,"$db":"api-server"} }`, doc}}}},
		MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"n":{"$numberInt":"1"},"ok":{"$numberDouble":"1.0"}} }`}}}},
		ReqTimestampMock: req, ResTimestampMock: res,
	}}
}

// tiedInput is the call of TestAValueTwoCallsSentInTheOtherOrderInEachRunIsBoundAtEither
// (the run's id sent in either order) beside raw, every recording of the
// call's own test, read through r (nil: none).
func tiedInput(r *Reader, dc *DriftCache, raw ...Raw) Input {
	in := oInput(sentInTurnPath, sentInTurnRec, sentInTurnLive, nil, raw)
	in.Drifts, in.Reader = dc, r
	in.Scope = func(string) Scope { return ScopeTest("test-2") }
	return in
}

// integrations' learn step judges a call that drifts alike from many
// recordings against each in recorded order (the tied loop), a Decide each,
// over one read of the call's earlier places. api-server's
// app_config_audits find in TestTestRunReportOnClient was tied with its
// test's alike finds, and each Decide built its own index: every place's
// text was read into its field view again, once a Decide, and the call took
// 6.6 min to miss while its test's end waited for it (round F's 7m16s end).
// With one Reader for the call, the place's text is read into its view once
// for the call, and every Decide decides as with an index of its own. The
// row counts the views built (work, never time): a Reader per Decide, as
// each Decide built before, builds one a Decide; one Reader for the call,
// one in all.
func TestATiedCallsDecidesReadEachPlaceIntoItsViewOnceForTheCall(t *testing.T) {
	const tied = 40
	place := RawOf(tiedBigMock("mock-1784", sentInTurnRec, oAt.Add(-8*time.Microsecond), oAt.Add(220*time.Microsecond)), false)
	dc := oDrifts()

	// Each Decide with its own index, as before.
	var want Decision
	perDecide, oneDecide := 0, 0
	for i := range tied {
		r := NewReader(dc)
		x := r.x
		d := oWord(t, Decide(tiedInput(r, dc, place)), "95ad0386", "4fbd76ad")
		r.Release()
		if i == 0 {
			want, oneDecide = d, x.views
		}
		perDecide += x.views
	}
	if want.Outcome != OutcomeValue || want.From.Mock != "mock-1784" || !want.EitherOrder {
		t.Fatalf("the call alone: %s (either order %v); want bound to mock-1784 in either order", oDescribe([]Decision{want}), want.EitherOrder)
	}
	if oneDecide < 1 || perDecide != tied*oneDecide {
		t.Fatalf("%d Decides with an index each built %d field views, one Decide %d; want at least 1 for one Decide (the place's view), %d in all", tied, perDecide, oneDecide, tied*oneDecide)
	}

	// One Reader for the call: the same decision at every Decide, and the
	// place's view built once.
	r := NewReader(dc)
	x := r.x
	for i := range tied {
		d := oWord(t, Decide(tiedInput(r, dc, place)), "95ad0386", "4fbd76ad")
		if d.Outcome != want.Outcome || d.From.Mock != want.From.Mock || d.EitherOrder != want.EitherOrder || d.Line != want.Line {
			t.Fatalf("Decide %d of the call's Reader: %s; want as with an index of its own: %s", i+1, oDescribe([]Decision{d}), oDescribe([]Decision{want}))
		}
	}
	if x.views != oneDecide {
		t.Errorf("%d Decides through one Reader built %d field views; want %d, one Decide's: each text is read into its view once for the call", tied, x.views, oneDecide)
	}
	r.Release()
	if x.used != 0 || r.x != nil {
		t.Errorf("released, the Reader keeps %d bytes (index kept %v); want none, its index let go", x.used, r.x != nil)
	}
	// Released, a Decide given it builds its own index, as with none.
	if d := oWord(t, Decide(tiedInput(r, dc, place)), "95ad0386", "4fbd76ad"); d.Outcome != want.Outcome || !d.EitherOrder {
		t.Errorf("a Decide given a released Reader: %s; want as with an index of its own", oDescribe([]Decision{d}))
	}
	// A Reader made for another cache is not this Decide's: it builds its own.
	other := NewReader(NewDriftCache())
	if d := oWord(t, Decide(tiedInput(other, dc, place)), "95ad0386", "4fbd76ad"); d.Outcome != want.Outcome || !d.EitherOrder || other.x.views != 0 {
		t.Errorf("a Decide given another cache's Reader: %s, that Reader built %d views; want as with an index of its own, the other Reader untouched", oDescribe([]Decision{d}), other.x.views)
	}
	other.Release()
}

// Each Decide through a Reader refuses for a recording keploy could not
// read only where it asked about that recording itself (decider.unreadHere).
// The Reader's index has counted a text a question of the call could not
// read (a recording read lazily whose parked payload could not be loaded
// again: Raw{} with no payload, as reader_test's unread row asks it); a
// Decide whose places hold no such recording binds the word as a Decide
// with an index of its own does, and one whose places hold one is still
// refused as unread, naming it.
func TestATiedCallsDecideIsRefusedUnreadOnlyForARecordingItAsked(t *testing.T) {
	dc := oDrifts()
	place := RawOf(tiedBigMock("mock-1784", sentInTurnRec, oAt.Add(-8*time.Microsecond), oAt.Add(220*time.Microsecond)), false)
	r := NewReader(dc)
	defer r.Release()
	if _, read := r.x.textOf(Raw{mock: "mock-unread"}).holdsWord("95ad0386"); read || r.x.unreadTexts == 0 {
		t.Fatalf("an earlier question of a recording with no payload: read %v, %d unread texts counted; want unread, counted", read, r.x.unreadTexts)
	}
	if d := oWord(t, Decide(tiedInput(r, dc, place)), "95ad0386", "4fbd76ad"); d.Outcome != OutcomeValue || d.From.Mock != "mock-1784" || !d.EitherOrder {
		t.Errorf("FALSE RED: a Decide of the call whose places hold no unread recording, after the Reader counted another's: %s (either order %v); want bound to mock-1784 in either order, as with an index of its own", oDescribe([]Decision{d}), d.EitherOrder)
	}
	unread := unreadPayload(sentInTurnMock("mock-1790", sentInTurnRec, oAt.Add(-time.Millisecond), oAt.Add(-900*time.Microsecond)))
	oRefused(t, "a Decide of the call whose places hold an unread recording", oWord(t, Decide(tiedInput(r, dc, place, unread)), "95ad0386", "4fbd76ad"), RefusalUnread, "mock-1790")
}

// No Reader, and a Reader let go, are no index: Release of either does
// nothing, and a Decide given either builds its own index and decides as
// with none. Each is asked under recover, so a guard that let one through
// fails here by its line, never by a panic.
func TestANilOrReleasedReaderIsNoIndex(t *testing.T) {
	dc := oDrifts()
	place := RawOf(tiedBigMock("mock-1784", sentInTurnRec, oAt.Add(-8*time.Microsecond), oAt.Add(220*time.Microsecond)), false)
	noPanic := func(what string, f func()) {
		t.Helper()
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("%s panicked: %v; want nothing done, or an index of its own", what, p)
			}
		}()
		f()
	}
	decided := func(what string, r *Reader) {
		t.Helper()
		noPanic(what, func() {
			if d := oWord(t, Decide(tiedInput(r, dc, place)), "95ad0386", "4fbd76ad"); d.Outcome != OutcomeValue || d.From.Mock != "mock-1784" || !d.EitherOrder {
				t.Errorf("%s: %s (either order %v); want bound to mock-1784 in either order, as with an index of its own", what, oDescribe([]Decision{d}), d.EitherOrder)
			}
		})
	}
	var none *Reader
	noPanic("Release of no Reader", none.Release)
	decided("a Decide given no Reader", none)
	r := NewReader(dc)
	r.Release()
	noPanic("a second Release of a Reader", r.Release)
	decided("a Decide given a released Reader", r)
}

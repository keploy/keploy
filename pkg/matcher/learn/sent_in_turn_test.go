package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// sentInTurnPath is the jobEvents insert's field that holds the suite run's
// id, and sentInTurnRec/Live the id in each run (api-server's
// TestSlowStepFailsOnlyItself: the application publishes one event to
// jobEvents and to eventsV2 at once, on two connections).
const (
	sentInTurnPath = "body.docs.msgs.0.event.testsuitreport._id"
	sentInTurnRec  = "95ad0386-71d9-4ccc-822e-618e3aa21789"
	sentInTurnLive = "4fbd76ad-1c2e-4f5a-9b3d-7e8f9a0b1c2d"
)

// sentInTurnMock is the eventsV2 insert that sends id at its
// eventDetails.test_suite_run_id, asked at req and answered at res: its
// request alone holds the id (its answer is {n: 1}).
func sentInTurnMock(name, id string, req, res time.Time) *models.Mock {
	return sentInTurnAnswering(name, id, "", req, res)
}

// sentInTurnAnswering is sentInTurnMock whose answer also holds answered
// at its "upserted" field ("": an answer of {n: 1} alone).
func sentInTurnAnswering(name, id, answered string, req, res time.Time) *models.Mock {
	answer := `{ SectionSingle msg: {"n":{"$numberInt":"1"},"ok":{"$numberDouble":"1.0"}} }`
	if answered != "" {
		answer = `{ SectionSingle msg: {"n":{"$numberInt":"1"},"upserted":"` + answered + `","ok":{"$numberDouble":"1.0"}} }`
	}
	doc := `{ SectionSingle identifier: documents , msgs: [ {"eventDetails":{"test_suite_run_id":"` + id + `","run_result":"failure"}} ] }`
	return &models.Mock{Name: name, Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"insert":"eventsV2","ordered":true,"$db":"api-server"} }`, doc}}}},
		MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			answer}}}},
		ReqTimestampMock: req, ResTimestampMock: res,
	}}
}

// sentInTurnDecide decides the call's first word beside the one recording
// raw of its own test.
func sentInTurnDecide(t *testing.T, raw ...Raw) Decision {
	t.Helper()
	return sentInTurnDecideIn(t, ScopeTest("test-2"), raw...)
}

// sentInTurnDecideIn is sentInTurnDecide with every recording of scope
// (ScopeUnknown: of no scope keploy was told).
func sentInTurnDecideIn(t *testing.T, scope Scope, raw ...Raw) Decision {
	t.Helper()
	in := oInput(sentInTurnPath, sentInTurnRec, sentInTurnLive, nil, raw)
	if scope != ScopeUnknown {
		in.Scope = func(string) Scope { return scope }
	}
	return oWord(t, Decide(in), "95ad0386", "4fbd76ad")
}

// The application sends one id it made at two calls at once (two
// connections): the recording sent the eventsV2 insert 8 µs before the
// call, round F only after the call reached keploy. A request gives the
// application nothing, so neither call is where the other's value came
// from: the id is the application's, sent at both, in another order in
// each run. keploy binds the word to the other call's place as a value
// sent in either order (Decision.EitherOrder): a replay binds it at
// whichever call comes first and checks the other holds it, so a replay
// that sends two values misses (fail closed). Before, the word was refused
// as unknown: "both values hold it, or only part of a word".
func TestAValueTwoCallsSentInTheOtherOrderInEachRunIsBoundAtEither(t *testing.T) {
	us := time.Microsecond
	p := func(req, res time.Duration) Raw {
		return RawOf(sentInTurnMock("mock-1784", sentInTurnRec, oAt.Add(req), oAt.Add(res)), true).ServedAs(6).ServedAsRecorded()
	}
	after := func(r Raw) Raw { r.servedAfter = true; return r }

	d := sentInTurnDecide(t, after(p(-8*us, 220*us)))
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-1784" || !d.EitherOrder {
		t.Fatalf("FALSE RED: the recording sent the other call 8 µs before, round F only after: %s (either order %v); want bound to mock-1784 in either order", oDescribe([]Decision{d}), d.EitherOrder)
	}
	// Answered a millisecond before the call, it is still a send.
	if d := sentInTurnDecide(t, after(p(-2*time.Millisecond, -time.Millisecond))); d.Outcome != OutcomeValue || !d.EitherOrder {
		t.Errorf("the other call asked and answered before the call, round F's only after: %s; want bound in either order", oDescribe([]Decision{d}))
	}

	// Round F had not served the other call by the call's read: it gave
	// the application nothing there before the call, as one served after
	// the call's arrival, so it is bound in either order too (the shape of
	// the next row).
	never := RawOf(sentInTurnMock("mock-1784", sentInTurnRec, oAt.Add(-8*us), oAt.Add(220*us)), false)
	if d := sentInTurnDecide(t, never); d.Outcome != OutcomeValue || d.From.Mock != "mock-1784" || !d.EitherOrder {
		t.Errorf("FALSE RED: the recording sent the other call before, round F not by the call's read: %s (either order %v); want bound to mock-1784 in either order", oDescribe([]Decision{d}), d.EitherOrder)
	}

	// Controls. Both runs sent it after the call: it shows nothing, and the word is
	// made at the call.
	costFresh(t, "both runs sent the other call after the call", sentInTurnDecide(t, after(p(8*us, 220*us))))
	// Round F served it before the call, as recorded: no case of two
	// orders, decided as before.
	if d := sentInTurnDecide(t, p(-8*us, 220*us)); d.EitherOrder {
		t.Errorf("round F served the other call before the call too: %s; want no value in either order", oDescribe([]Decision{d}))
	}
}

// runIDPath is where the call (an eventsV2 insert) carries the suite
// run's id, runIDRec and runIDLive the id in each run.
const (
	runIDPath = "body.docs.msgs.0.eventDetails.test_run_id"
	runIDRec  = "89f3de6b-155c-4386-83b9-f645004943d6"
	runIDLive = "ffcf0510-2b7e-4c1a-9d3f-5e6a7b8c9d0e"
)

// runIDJobEvents is the jobEvents insert that carries id at
// event.testsuitreport.test_suite_run_id, asked at req and answered at res:
// its request alone holds the id (its answer is {n: 1}).
func runIDJobEvents(name, id string, req, res time.Time) *models.Mock {
	doc := `{ SectionSingle identifier: documents , msgs: [ {"_id":"958a9ba2-1e04-49cf-8ff5-63f9d6877825","kind":"TEST","event":{"status":"RUNNING","testsuitreport":{"_id":"7b10ad05-08ab-4c6e-925f-00297d50e5a9","test_suite_run_id":"` + id + `"}}} ] }`
	return &models.Mock{Name: name, Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"insert":"jobEvents","ordered":true,"$db":"api-server"} }`, doc}}}},
		MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"n":{"$numberInt":"1"},"ok":{"$numberDouble":"1.0"}} }`}}}},
		ReqTimestampMock: req, ResTimestampMock: res,
	}}
}

// api-server's TestSlowStepFailsOnlyItself, as a run recorded it: the
// application sent the jobEvents insert 517 µs before the eventsV2 insert
// (the call), and it was answered 401 µs after the call. In round F the
// call reached keploy and its learn step read the round while the
// jobEvents insert was still in its own learn step (it learned 63 fields
// and was served 16 ms after the call was refused): round F had served
// none of it by the call's read. Its request alone holds the run's id, so
// round F gave the application nothing there before the call, as where
// round F served it only after the call's arrival, and neither request is
// the other's source: the id is bound in either order. Before, each word
// was refused as unknown ("keploy cannot tell what round F gave at
// mock-1785 before this call (both values hold it, or only part of a
// word)"), and the test was set aside in every run.
func TestAValueTheOtherCallSentFirstButRoundFHadNotServedIsBoundAtEither(t *testing.T) {
	us := time.Microsecond
	other := func(served bool) Raw {
		return RawOf(runIDJobEvents("mock-1785", runIDRec, oAt.Add(-517*us), oAt.Add(401*us)), served)
	}
	decide := func(scope Scope, raw ...Raw) Decision {
		in := oInput(runIDPath, runIDRec, runIDLive, nil, raw)
		in.Mock = "mock-1780"
		in.Scope = func(string) Scope { return scope }
		return oWord(t, Decide(in), "89f3de6b", "ffcf0510")
	}
	d := decide(ScopeTest("test-2"), other(false))
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-1785" || !d.EitherOrder {
		t.Fatalf("FALSE RED: the other call sent first in the recording, not served by round F at the call's read: %s (either order %v); want bound to mock-1785 in either order", oDescribe([]Decision{d}), d.EitherOrder)
	}
	// Controls. Of a scope keploy was not told, another test's or the
	// start-up's, it stays unknown (fail closed).
	for _, s := range []Scope{ScopeUnknown, ScopeTest("test-1"), ScopeStartup} {
		oRefused(t, "the other call of scope "+string(s)+", not served by the call's read", decide(s, other(false)), RefusalUnknown, "mock-1785")
	}
	// Served by round F before the call reached keploy, stating no view of
	// it: round F may have given the application the value there.
	if d := decide(ScopeTest("test-2"), other(true).ServedAs(6)); d.EitherOrder || d.Outcome == OutcomeValue {
		t.Errorf("FALSE GREEN? round F served the other call before the call, stating no view: %s; want no bind", oDescribe([]Decision{d}))
	}
}

// Only a request is a send: a recording whose answer holds the recorded id
// gave it to the application, which may have copied it, so round F's serve
// after the call leaves it unknown (fail closed). So does another test's
// recording, and one the start-up sent: outside the call's scope.
func TestAValueSentInTheOtherOrderIsOnlyARequestOfTheCallsOwnTest(t *testing.T) {
	us := time.Microsecond
	answer := RawOf(receiveOf("mock-1784", sentInTurnRec, oAt.Add(-8*us)), true).ServedAs(6).ServedAsRecorded()
	answer.servedAfter = true
	if d := sentInTurnDecide(t, answer); d.EitherOrder || d.Outcome == OutcomeValue {
		t.Errorf("FALSE GREEN? an answer that holds the id, served only after the call: %s; want no bind", oDescribe([]Decision{d}))
	}
	other := RawOf(sentInTurnMock("mock-1784", sentInTurnRec, oAt.Add(-8*us), oAt.Add(220*us)), true).ServedAs(6).ServedAsRecorded()
	other.servedAfter, other.elsewhere = true, true
	if d := sentInTurnDecideIn(t, ScopeUnknown, other); d.EitherOrder {
		t.Errorf("FALSE GREEN? another test's send, served only after the call: %s; want no value in either order", oDescribe([]Decision{d}))
	}
	other.elsewhere = false
	for _, s := range []Scope{ScopeTest("test-1"), ScopeStartup, ScopeWorker, ScopeUnknown} {
		if d := sentInTurnDecideIn(t, s, other); d.EitherOrder {
			t.Errorf("FALSE GREEN? a send of scope %q, served only after the call: %s; want no value in either order", s, oDescribe([]Decision{d}))
		}
	}
}

// Each place below leaves the word unknown, and stays so: the call is
// bound in either order only where the other call's request alone holds
// both runs' values whole, sent before the call in the recording at a time
// keploy reads and not served by round F before it. A view round F stated
// that sent its own value there binds it so too.
func TestAValueSentInTheOtherOrderNeedsEveryPartOfTheRule(t *testing.T) {
	us := time.Microsecond
	req, res := oAt.Add(-8*us), oAt.Add(220*us)
	raw := func(id, answered string, req time.Time) Raw {
		return RawOf(sentInTurnAnswering("mock-1784", id, answered, req, res), true)
	}
	after := func(r Raw) Raw { r.servedAfter = true; return r }
	viewed := func(id, answered string) Raw {
		return after(raw(sentInTurnRec, "", req).ServedAs(6).ServedLive(raw(id, answered, req)))
	}

	if d := sentInTurnDecide(t, viewed(sentInTurnLive, "")); d.Outcome != OutcomeValue || !d.EitherOrder || d.From.Mock != "mock-1784" {
		t.Errorf("FALSE RED: round F's view of the other call sent round F's value, only after the call: %s; want bound to mock-1784 in either order", oDescribe([]Decision{d}))
	}
	untimed := after(raw(sentInTurnRec, "", req).ServedAs(6).ServedAsRecorded())
	untimed.noTime = true
	another := "95ad0386-0000-4000-8000-000000000000"
	twin := after(raw(sentInTurnRec, "", req).ServedAs(6).ServedAsRecorded())
	for _, c := range []struct {
		name string
		raw  []Raw
	}{
		{"the recording sent the other call at the call's own instant", []Raw{after(raw(sentInTurnRec, "", oAt).ServedAs(6).ServedAsRecorded())}},
		{"keploy cannot read when the recording sent the other call", []Raw{untimed}},
		{"round F served the other call before the call, stating no view of it", []Raw{raw(sentInTurnRec, "", req).ServedAs(6)}},
		{"round F's view of the other call sent the recorded value (its answer changed)", []Raw{viewed(sentInTurnRec, "zz")}},
		{"round F's view of the other call holds round F's value only in its answer", []Raw{viewed(another, sentInTurnLive)}},
		{"round F's view of the other call sent only round F's first group, of another id", []Raw{viewed("4fbd76ad-0000-4000-8000-000000000000", "")}},
		{"round F's view of the other call holds round F's value glued inside a longer one", []Raw{viewed("x"+sentInTurnLive, "")}},
		{"the recording's other call sent only the recorded first group, of another id", []Raw{after(raw(another, "", req).ServedAs(6).ServedAsRecorded())}},
		{"beside the other call, a recording round F served before the call, stating no view, holds the recorded value", []Raw{twin, RawOf(sentInTurnMock("mock-1786", sentInTurnRec, req, res), true).ServedAs(7)}},
	} {
		if d := sentInTurnDecide(t, c.raw...); d.EitherOrder || d.Outcome == OutcomeValue {
			t.Errorf("FALSE GREEN? %s: %s; want no bind", c.name, oDescribe([]Decision{d}))
		}
	}
	oRefused(t, "beside the other call, a recording round F served before the call, stating no view", sentInTurnDecide(t, twin, RawOf(sentInTurnMock("mock-1786", sentInTurnRec, req, res), true).ServedAs(7)), RefusalUnknown, "mock-1786")
}

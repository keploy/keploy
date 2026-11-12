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

	// Controls. Round F never served the other call: nothing shows round
	// F's application sent it at all, and keploy cannot tell what it held.
	never := RawOf(sentInTurnMock("mock-1784", sentInTurnRec, oAt.Add(-8*us), oAt.Add(220*us)), false)
	oRefused(t, "the recording sent the other call before, round F never", sentInTurnDecide(t, never), RefusalUnknown, "mock-1784")
	// Both runs sent it after the call: it shows nothing, and the word is
	// made at the call.
	costFresh(t, "both runs sent the other call after the call", sentInTurnDecide(t, after(p(8*us, 220*us))))
	// Round F served it before the call, as recorded: no case of two
	// orders, decided as before.
	if d := sentInTurnDecide(t, p(-8*us, 220*us)); d.EitherOrder {
		t.Errorf("round F served the other call before the call too: %s; want no value in either order", oDescribe([]Decision{d}))
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
// keploy reads and served by round F only after it. A view round F stated
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
		{"beside the other call, a recording round F never served holds the recorded value", []Raw{twin, RawOf(sentInTurnMock("mock-1786", sentInTurnRec, req, res), false)}},
	} {
		if d := sentInTurnDecide(t, c.raw...); d.EitherOrder || d.Outcome == OutcomeValue {
			t.Errorf("FALSE GREEN? %s: %s; want no bind", c.name, oDescribe([]Decision{d}))
		}
	}
	oRefused(t, "beside the other call, a recording round F never served", sentInTurnDecide(t, twin, RawOf(sentInTurnMock("mock-1786", sentInTurnRec, req, res), false)), RefusalUnknown, "mock-1786")
}
